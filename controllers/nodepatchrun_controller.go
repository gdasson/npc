package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
	"github.com/myorg/nodepatcher/pkg/k8s"
	"github.com/myorg/nodepatcher/pkg/partition"
	"github.com/myorg/nodepatcher/pkg/provider"
	"github.com/myorg/nodepatcher/pkg/util"
)

const (
	RunFinalizer     = "nodepatcher.io/finalizer"
	TaskLabelRunUID  = "nodepatcher.io/run-uid"
	TaskLabelRunName = "nodepatcher.io/run-name"
)

type NodePatchRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	ProviderFactory *ProviderFactory
	LockNamespace   string
}

// +kubebuilder:rbac:groups=nodepatcher.myorg.io,resources=nodepatchruns,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=nodepatcher.myorg.io,resources=nodepatchruns/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=nodepatcher.myorg.io,resources=nodepatchtasks,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=nodepatcher.myorg.io,resources=nodepatchtasks/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch;patch;update

func (r *NodePatchRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	run := &v1alpha1.NodePatchRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if run.DeletionTimestamp == nil && !controllerutil.ContainsFinalizer(run, RunFinalizer) {
		patch := client.MergeFrom(run.DeepCopy())
		controllerutil.AddFinalizer(run, RunFinalizer)
		if err := r.Patch(ctx, run, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	if run.DeletionTimestamp != nil {
		return r.finalize(ctx, run)
	}

	if run.Spec.Paused != nil && *run.Spec.Paused {
		if !run.Status.Paused {
			patch := client.MergeFrom(run.DeepCopy())
			run.Status.Paused = true
			run.Status.PauseReason = "spec.paused=true"
			_ = r.Status().Patch(ctx, run, patch)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if run.Status.Paused {
		patch := client.MergeFrom(run.DeepCopy())
		run.Status.Paused = false
		run.Status.PauseReason = ""
		_ = r.Status().Patch(ctx, run, patch)
	}

	// Global run lock: ensure only one NodePatchRun is actively scheduling/driving tasks in the cluster at a time.
	// This complements MaxConcurrentReconciles=1 and prevents multiple Run CRs from racing each other.
	holder := fmt.Sprintf("%s/%s|%s", run.Namespace, run.Name, string(run.UID))
	runLock := &k8s.LeaseLock{Client: r.Client, Namespace: r.LockNamespace, TTL: 10 * time.Minute}
	got, err := runLock.Acquire(ctx, holder, "global-run")
	if err != nil {
		return ctrl.Result{}, err
	}
	if !got {
		// Another Run is active; back off.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := r.ensureAutoscalerScaledDown(ctx, run); err != nil {
		lg.Error(err, "autoscaler scale-down failed")
		r.setErrorStatus(ctx, run, err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.ensurePlan(ctx, run); err != nil {
		lg.Error(err, "ensurePlan failed")
		r.setErrorStatus(ctx, run, err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if run.Status.Plan == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	tasks, err := r.listTasksForRun(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.updateRunCounters(ctx, run, tasks)

	if r.shouldPauseForFailures(run) {
		if !run.Status.Paused {
			patch := client.MergeFrom(run.DeepCopy())
			run.Status.Paused = true
			run.Status.PauseReason = "failurePolicy.pauseAfterFailures reached"
			_ = r.Status().Patch(ctx, run, patch)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if run.Status.CurrentIteration >= len(run.Status.Plan.Partitions) {
		_ = runLock.Release(ctx, holder, "global-run")

		if err := k8s.RestoreAutoscalerFromStatusOrAnnotation(ctx, r.Client, run.Spec.AutoscalerControl, run.Status.Autoscaler); err != nil {
			lg.Error(err, "autoscaler restore failed")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.setCondition(ctx, run, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionTrue,
			Reason:             "AllPartitionsDone",
			Message:            "node patch run completed",
			ObservedGeneration: run.Generation,
			LastTransitionTime: metav1.Now(),
		})
		return ctrl.Result{}, nil
	}

	part := run.Status.Plan.Partitions[run.Status.CurrentIteration]
	partNodes := append([]string{}, part.Nodes...)
	sort.Strings(partNodes)

	succeeded := map[string]struct{}{}
	failed := map[string]struct{}{}
	inflightImp := 0
	inflightNon := 0
	inflightByZone := map[string]int{}
	existingTaskForNode := map[string]*v1alpha1.NodePatchTask{}

	for i := range tasks {
		t := &tasks[i]
		if t.Spec.Iteration != run.Status.CurrentIteration {
			continue
		}
		existingTaskForNode[t.Spec.NodeName] = t
		switch t.Status.Phase {
		case v1alpha1.TaskPhaseSucceeded:
			succeeded[t.Spec.NodeName] = struct{}{}
		case v1alpha1.TaskPhaseFailed:
			failed[t.Spec.NodeName] = struct{}{}
		default:
			if t.Spec.Important {
				inflightImp++
			} else {
				inflightNon++
			}
			if t.Spec.Zone != "" {
				inflightByZone[t.Spec.Zone]++
			}
		}
	}

	allDone := true
	for _, n := range partNodes {
		if _, ok := succeeded[n]; !ok {
			allDone = false
			break
		}
	}
	if allDone {
		patch := client.MergeFrom(run.DeepCopy())
		run.Status.CurrentIteration++
		_ = r.Status().Patch(ctx, run, patch)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	impSet, err := k8s.ImportantNodesNow(ctx, r.Client, run.Spec.ImportantPodSelectors)
	if err != nil {
		r.setErrorStatus(ctx, run, err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	bsImp := defaultInt(run.Spec.BatchSizeImportant, 1)
	bsNon := defaultInt(run.Spec.BatchSizeNonImportant, 2)
	slotsImp := bsImp - inflightImp
	slotsNon := bsNon - inflightNon
	if slotsImp < 0 {
		slotsImp = 0
	}
	if slotsNon < 0 {
		slotsNon = 0
	}

	maxPerZone := 0
	if run.Spec.MaxConcurrentPerZone != nil {
		maxPerZone = *run.Spec.MaxConcurrentPerZone
	}

	createdAny := false
	for _, nodeName := range partNodes {
		if slotsImp == 0 && slotsNon == 0 {
			break
		}
		if _, ok := succeeded[nodeName]; ok {
			continue
		}
		if _, ok := failed[nodeName]; ok {
			continue
		}
		if _, ok := existingTaskForNode[nodeName]; ok {
			continue
		}

		importantNow := false
		if _, ok := impSet[nodeName]; ok && len(run.Spec.ImportantPodSelectors) > 0 {
			importantNow = true
		}

		zone := ""
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err == nil {
			zone = node.Labels[partition.ZoneLabel]
		}

		if maxPerZone > 0 && zone != "" && inflightByZone[zone] >= maxPerZone {
			continue
		}

		if importantNow {
			if slotsImp == 0 {
				continue
			}
		} else {
			if slotsNon == 0 {
				continue
			}
		}

		if err := r.createTask(ctx, run, nodeName, zone, importantNow); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			r.setErrorStatus(ctx, run, err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		createdAny = true
		if importantNow {
			slotsImp--
		} else {
			slotsNon--
		}
		if zone != "" {
			inflightByZone[zone]++
		}
	}

	if createdAny {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func defaultInt(p *int, d int) int {
	if p == nil || *p <= 0 {
		return d
	}
	return *p
}

func (r *NodePatchRunReconciler) ensureAutoscalerScaledDown(ctx context.Context, run *v1alpha1.NodePatchRun) error {
	spec := run.Spec.AutoscalerControl
	if spec == nil || spec.Enabled == nil || !*spec.Enabled {
		return nil
	}
	if run.Status.Autoscaler != nil && run.Status.Autoscaler.ScaledDown {
		return nil
	}
	dpls, err := k8s.FindAutoscalerDeployments(ctx, r.Client, spec)
	if err != nil {
		return err
	}
	if len(dpls) == 0 {
		return nil
	}
	targets, err := k8s.ScaleDownAndAnnotateAutoscaler(ctx, r.Client, string(run.UID), dpls)
	if err != nil {
		return err
	}
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Autoscaler = &v1alpha1.AutoscalerControlStatus{ScaledDown: true, Targets: targets}
	return r.Status().Patch(ctx, run, patch)
}

func (r *NodePatchRunReconciler) ensurePlan(ctx context.Context, run *v1alpha1.NodePatchRun) error {
	if run.Status.Plan != nil && len(run.Status.Plan.Partitions) > 0 {
		return nil
	}

	nodes, err := r.listCandidateNodes(ctx, run)
	if err != nil {
		return err
	}
	p0, p1, pv := partition.MixedAZHalves(nodes)

	candidates := make([]v1alpha1.PlannedNode, 0, len(nodes))
	for _, n := range nodes {
		candidates = append(candidates, v1alpha1.PlannedNode{Name: n.Name, Zone: n.Labels[partition.ZoneLabel]})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

	plan := &v1alpha1.RunPlan{
		PlanVersion: pv,
		Candidates:  candidates,
		Partitions: []v1alpha1.Partition{
			{Index: 0, Nodes: p0},
			{Index: 1, Nodes: p1},
		},
	}

	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Plan = plan
	run.Status.CurrentIteration = 0
	return r.Status().Patch(ctx, run, patch)
}

func (r *NodePatchRunReconciler) listCandidateNodes(ctx context.Context, run *v1alpha1.NodePatchRun) ([]corev1.Node, error) {
	list := &corev1.NodeList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}

	sel, err := selectorOrAll(run.Spec.NodeSelector)
	if err != nil {
		return nil, err
	}
	exSel, err := selectorOrNone(run.Spec.ExcludeNodeSelector)
	if err != nil {
		return nil, err
	}

	excludeControlPlane := true
	if run.Spec.ExcludeControlPlane != nil {
		excludeControlPlane = *run.Spec.ExcludeControlPlane
	}

	out := make([]corev1.Node, 0, len(list.Items))
	for _, n := range list.Items {
		if excludeControlPlane && isControlPlane(&n) {
			continue
		}
		if sel != nil && !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		if exSel != nil && exSel.Matches(labels.Set(n.Labels)) {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func isControlPlane(n *corev1.Node) bool {
	for k := range n.Labels {
		if k == "node-role.kubernetes.io/control-plane" || k == "node-role.kubernetes.io/master" {
			return true
		}
	}
	return false
}

func selectorOrAll(ls *metav1.LabelSelector) (labels.Selector, error) {
	if ls == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(ls)
}
func selectorOrNone(ls *metav1.LabelSelector) (labels.Selector, error) {
	if ls == nil {
		return nil, nil
	}
	return metav1.LabelSelectorAsSelector(ls)
}

func (r *NodePatchRunReconciler) listTasksForRun(ctx context.Context, run *v1alpha1.NodePatchRun) ([]v1alpha1.NodePatchTask, error) {
	list := &v1alpha1.NodePatchTaskList{}
	if err := r.List(ctx, list,
		client.InNamespace(run.Namespace),
		client.MatchingLabels{TaskLabelRunUID: string(run.UID)},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *NodePatchRunReconciler) createTask(ctx context.Context, run *v1alpha1.NodePatchRun, nodeName, zone string, important bool) error {
	t := &v1alpha1.NodePatchTask{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: run.Namespace,
			Name:      util.SafeTaskName(run.Name, fmt.Sprintf("%d-%s", run.Status.CurrentIteration, nodeName)),
			Labels: map[string]string{
				TaskLabelRunUID:  string(run.UID),
				TaskLabelRunName: run.Name,
			},
		},
		Spec: v1alpha1.NodePatchTaskSpec{
			RunName: run.Name,
			RunUID:  string(run.UID),

			Iteration: run.Status.CurrentIteration,
			NodeName:  nodeName,
			Zone:      zone,

			Important: important,

			Provider: run.Spec.Provider,

			ImportantPodSelectors:          run.Spec.ImportantPodSelectors,
			RequiredNodeDaemonPodSelectors: run.Spec.RequiredNodeDaemonPodSelectors,
			Stability:                      run.Spec.Stability,
			LabelTransfer:                  run.Spec.LabelTransfer,
		},
	}
	if err := controllerutil.SetControllerReference(run, t, r.Scheme); err != nil {
		return err
	}
	t.Status.Phase = v1alpha1.TaskPhasePending
	return r.Create(ctx, t)
}

func (r *NodePatchRunReconciler) updateRunCounters(ctx context.Context, run *v1alpha1.NodePatchRun, tasks []v1alpha1.NodePatchTask) {
	inflightImp := 0
	inflightNon := 0
	completed := 0
	failed := 0
	for i := range tasks {
		t := &tasks[i]
		switch t.Status.Phase {
		case v1alpha1.TaskPhaseSucceeded:
			completed++
		case v1alpha1.TaskPhaseFailed:
			failed++
		default:
			if t.Spec.Important {
				inflightImp++
			} else {
				inflightNon++
			}
		}
	}
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.InFlightImportant = inflightImp
	run.Status.InFlightNonImportant = inflightNon
	run.Status.Completed = completed
	run.Status.Failed = failed
	_ = r.Status().Patch(ctx, run, patch)
}

func (r *NodePatchRunReconciler) shouldPauseForFailures(run *v1alpha1.NodePatchRun) bool {
	fp := run.Spec.FailurePolicy
	if fp == nil || fp.Enabled == nil || !*fp.Enabled {
		return false
	}
	thresh := 1
	if fp.PauseAfterFailures != nil && *fp.PauseAfterFailures > 0 {
		thresh = *fp.PauseAfterFailures
	}
	return run.Status.Failed >= thresh
}

func (r *NodePatchRunReconciler) setErrorStatus(ctx context.Context, run *v1alpha1.NodePatchRun, err error) {
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.LastError = err.Error()
	_ = r.Status().Patch(ctx, run, patch)
}

func (r *NodePatchRunReconciler) setCondition(ctx context.Context, run *v1alpha1.NodePatchRun, cond metav1.Condition) {
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Conditions = upsertCondition(run.Status.Conditions, cond)
	_ = r.Status().Patch(ctx, run, patch)
}

func upsertCondition(conds []metav1.Condition, c metav1.Condition) []metav1.Condition {
	for i := range conds {
		if conds[i].Type == c.Type {
			conds[i] = c
			return conds
		}
	}
	return append(conds, c)
}

func (r *NodePatchRunReconciler) finalize(ctx context.Context, run *v1alpha1.NodePatchRun) (ctrl.Result, error) {
	holder := fmt.Sprintf("%s/%s|%s", run.Namespace, run.Name, string(run.UID))
	runLock := &k8s.LeaseLock{Client: r.Client, Namespace: r.LockNamespace, TTL: 10 * time.Minute}
	_ = runLock.Release(ctx, holder, "global-run")

	lg := log.FromContext(ctx)

	if err := k8s.RestoreAutoscalerFromStatusOrAnnotation(ctx, r.Client, run.Spec.AutoscalerControl, run.Status.Autoscaler); err != nil {
		lg.Error(err, "autoscaler restore in finalizer failed")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if run.Spec.Provider.Type == "aws" && run.Spec.Provider.AWS != nil {
		clusterID := run.Spec.Provider.AWS.ClusterID
		if clusterID != "" && r.ProviderFactory != nil {
			prov, err := r.ProviderFactory.Get(ctx, run.Spec.Provider)
			if err == nil && prov != nil {
				hasNode := func(instanceID string) (bool, error) {
					nodes := &corev1.NodeList{}
					if err := r.List(ctx, nodes); err != nil {
						return false, err
					}
					m := provider.MachineRef{Provider: "aws", ID: instanceID}
					for _, n := range nodes.Items {
						ok, _ := prov.NodeMatchesMachine(n.Spec.ProviderID, m)
						if ok {
							return true, nil
						}
					}
					return false, nil
				}
				_ = prov.CleanupRunOrphans(ctx, clusterID, string(run.UID), hasNode)
			}
		}
	}

	if controllerutil.ContainsFinalizer(run, RunFinalizer) {
		patch := client.MergeFrom(run.DeepCopy())
		controllerutil.RemoveFinalizer(run, RunFinalizer)
		if err := r.Patch(ctx, run, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *NodePatchRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapFn := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		t, ok := obj.(*v1alpha1.NodePatchTask)
		if !ok {
			return nil
		}
		runName := t.Labels[TaskLabelRunName]
		if runName == "" {
			runName = t.Spec.RunName
		}
		if runName == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: t.Namespace, Name: runName}}}
	})

	pred := predicate.NewPredicateFuncs(func(obj client.Object) bool { return true })

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		For(&v1alpha1.NodePatchRun{}).
		Watches(&v1alpha1.NodePatchTask{}, mapFn, builder.WithPredicates(pred)).
		Complete(r)
}
