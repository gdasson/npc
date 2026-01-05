package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
	"github.com/myorg/nodepatcher/pkg/k8s"
	"github.com/myorg/nodepatcher/pkg/partition"
	"github.com/myorg/nodepatcher/pkg/provider"
)

type NodePatchTaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	ProviderFactory *ProviderFactory
	LockNamespace   string

	Clientset kubernetes.Interface
}

// +kubebuilder:rbac:groups=nodepatcher.myorg.io,resources=nodepatchtasks,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=nodepatcher.myorg.io,resources=nodepatchtasks/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func (r *NodePatchTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	task := &v1alpha1.NodePatchTask{}
	if err := r.Get(ctx, req.NamespacedName, task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if task.Status.Phase == v1alpha1.TaskPhaseSucceeded || task.Status.Phase == v1alpha1.TaskPhaseFailed {
		return ctrl.Result{}, nil
	}

	holder := fmt.Sprintf("%s/%s", task.Namespace, task.Name)
	if p := os.Getenv("POD_NAME"); p != "" {
		holder = holder + "|" + p
	}

	lock := &k8s.LeaseLock{Client: r.Client, Namespace: r.LockNamespace, TTL: 5 * time.Minute}
	ok, err := lock.Acquire(ctx, holder, task.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	// release only when terminal
	defer func() {
		if task.Status.Phase == v1alpha1.TaskPhaseSucceeded || task.Status.Phase == v1alpha1.TaskPhaseFailed {
			_ = lock.Release(context.Background(), holder, task.Spec.NodeName)
		}
	}()

	prov, err := r.ProviderFactory.Get(ctx, task.Spec.Provider)
	if err != nil {
		return r.fail(ctx, task, fmt.Errorf("provider init: %w", err))
	}

	// Old node might still exist for most of the workflow.
	oldNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.NodeName}, oldNode); err != nil {
		if apierrors.IsNotFound(err) {
			// if we already replaced and drained, old node might be gone; allow progress if past drain
			if task.Status.Phase == v1alpha1.TaskPhaseDrained || task.Status.Phase == v1alpha1.TaskPhaseOldDeleted {
				oldNode = nil
			} else {
				return r.fail(ctx, task, fmt.Errorf("old node %q not found", task.Spec.NodeName))
			}
		} else {
			return ctrl.Result{}, err
		}
	}

	// Resolve old machine once
	if task.Status.OldMachine.ID == "" && oldNode != nil {
		mref, details, err := prov.Resolve(ctx, oldNode.Spec.ProviderID)
		if err != nil {
			return r.fail(ctx, task, err)
		}
		if err := r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
			s.OldMachine.Provider = mref.Provider
			s.OldMachine.ID = mref.ID
			s.OldMachine.Location = mref.Location
			if aws, ok := details.(*v1alpha1.AWSDetails); ok && aws != nil {
				if s.ProviderDetails == nil {
					s.ProviderDetails = &v1alpha1.ProviderDetails{}
				}
				s.ProviderDetails.AWS = aws
			}
			if s.Phase == "" {
				s.Phase = v1alpha1.TaskPhasePending
			}
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Pending -> Cordon
	if task.Status.Phase == "" || task.Status.Phase == v1alpha1.TaskPhasePending {
		if oldNode == nil {
			return r.fail(ctx, task, fmt.Errorf("cannot cordon: old node missing"))
		}
		if !oldNode.Spec.Unschedulable {
			patch := client.MergeFrom(oldNode.DeepCopy())
			oldNode.Spec.Unschedulable = true
			if err := r.Patch(ctx, oldNode, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		_ = prov.MarkState(ctx, provider.MachineRef{Provider: task.Status.OldMachine.Provider, ID: task.Status.OldMachine.ID}, "cordoned", nil)
		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseCordoned, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Cordoned -> Ensure replacement
	if task.Status.Phase == v1alpha1.TaskPhaseCordoned {
		oldM := provider.MachineRef{Provider: task.Status.OldMachine.Provider, ID: task.Status.OldMachine.ID, Location: task.Status.OldMachine.Location}
		oldDetails := any(nil)
		if task.Status.ProviderDetails != nil && task.Status.ProviderDetails.AWS != nil {
			oldDetails = task.Status.ProviderDetails.AWS
		}

		strictLoc := true
		extraTags := map[string]string{}
		clusterID := ""
		if task.Spec.Provider.AWS != nil {
			if task.Spec.Provider.AWS.StrictAZ != nil {
				strictLoc = *task.Spec.Provider.AWS.StrictAZ
			}
			extraTags = task.Spec.Provider.AWS.ExtraTags
			clusterID = task.Spec.Provider.AWS.ClusterID
		}

		newM, newDetails, err := prov.EnsureReplacement(ctx, oldM, oldDetails, provider.ReplacementSpec{
			RunUID:         task.Spec.RunUID,
			TaskName:       task.Name,
			NodeName:       task.Spec.NodeName,
			StrictLocation: strictLoc,
			ExtraTags:      extraTags,
			ClusterID:      clusterID,
		})
		if err != nil {
			return r.fail(ctx, task, err)
		}

		_ = prov.MarkState(ctx, newM, "waiting-for-node", nil)
		_ = prov.SetProtection(ctx, newM, newDetails, true)

		if err := r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
			s.NewMachine.Provider = newM.Provider
			s.NewMachine.ID = newM.ID
			s.NewMachine.Location = newM.Location
			if aws, ok := newDetails.(*v1alpha1.AWSDetails); ok && aws != nil {
				if s.ProviderDetails == nil {
					s.ProviderDetails = &v1alpha1.ProviderDetails{}
				}
				s.ProviderDetails.AWS = aws
				s.ProviderDetails.AWS.NewProtected = true
			}
		}); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseReplacementEnsured, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// ReplacementEnsured -> wait for new node
	if task.Status.Phase == v1alpha1.TaskPhaseReplacementEnsured {
		newNode, why, err := r.findNewNode(ctx, prov, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if newNode == nil {
			lg.Info("waiting for new node", "task", task.Name, "why", why)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		strictAZ := true
		if task.Spec.Provider.AWS != nil && task.Spec.Provider.AWS.StrictAZ != nil {
			strictAZ = *task.Spec.Provider.AWS.StrictAZ
		}
		if strictAZ && oldNode != nil {
			oz := oldNode.Labels[partition.ZoneLabel]
			nz := newNode.Labels[partition.ZoneLabel]
			if oz != "" && nz != "" && oz != nz {
				return r.fail(ctx, task, fmt.Errorf("strictAZ violated: old zone=%q new zone=%q", oz, nz))
			}
		}

		if !k8s.IsNodeReady(newNode) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		ok, note, err := k8s.RequiredDaemonPodsReadyOnNode(ctx, r.Client, newNode.Name, task.Spec.RequiredNodeDaemonPodSelectors)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			_ = r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
				s.NewNodeName = newNode.Name
				s.StabilityNote = note
			})
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		_ = prov.MarkState(ctx, provider.MachineRef{Provider: task.Status.NewMachine.Provider, ID: task.Status.NewMachine.ID}, "node-ready", map[string]string{"node": newNode.Name})

		if err := r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
			s.NewNodeName = newNode.Name
			s.StabilityNote = note
		}); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseNewNodeReady, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// NewNodeReady -> label transfer
	if task.Status.Phase == v1alpha1.TaskPhaseNewNodeReady {
		if task.Status.NewNodeName == "" {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		newNode := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: task.Status.NewNodeName}, newNode); err != nil {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if oldNode == nil {
			return r.fail(ctx, task, fmt.Errorf("old node missing before label transfer"))
		}
		delta := k8s.ComputeLabelDelta(oldNode.Labels, newNode.Labels, task.Spec.LabelTransfer)
		if len(delta.Set) > 0 || len(delta.Remove) > 0 {
			nodePatch := client.MergeFrom(newNode.DeepCopy())
			if newNode.Labels == nil {
				newNode.Labels = map[string]string{}
			}
			for k, v := range delta.Set {
				newNode.Labels[k] = v
			}
			for _, k := range delta.Remove {
				delete(newNode.Labels, k)
			}
			if err := r.Patch(ctx, newNode, nodePatch); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseLabelsTransferred, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// LabelsTransferred -> stability gate (optional) -> drain
	if task.Status.Phase == v1alpha1.TaskPhaseLabelsTransferred {
		if oldNode == nil {
			return r.fail(ctx, task, fmt.Errorf("old node missing before drain"))
		}

		if okGate, note, err := r.stabilityGate(ctx, task); err != nil {
			return ctrl.Result{}, err
		} else if !okGate {
			_ = r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) { s.StabilityNote = note })
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		if err := k8s.DrainNode(ctx, r.Clientset, oldNode, 30*time.Minute); err != nil {
			return r.fail(ctx, task, fmt.Errorf("drain failed: %w", err))
		}
		_ = prov.MarkState(ctx, provider.MachineRef{Provider: task.Status.OldMachine.Provider, ID: task.Status.OldMachine.ID}, "drained", nil)
		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseDrained, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Drained -> delete old machine + unprotect new + succeed
	if task.Status.Phase == v1alpha1.TaskPhaseDrained {
		oldM := provider.MachineRef{Provider: task.Status.OldMachine.Provider, ID: task.Status.OldMachine.ID, Location: task.Status.OldMachine.Location}
		oldDetails := any(nil)
		if task.Status.ProviderDetails != nil && task.Status.ProviderDetails.AWS != nil {
			oldDetails = task.Status.ProviderDetails.AWS
		}

		if err := prov.DeleteMachine(ctx, oldM, oldDetails); err != nil {
			lg.Error(err, "DeleteMachine failed; will retry")
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		newM := provider.MachineRef{Provider: task.Status.NewMachine.Provider, ID: task.Status.NewMachine.ID, Location: task.Status.NewMachine.Location}
		newDetails := any(nil)
		if task.Status.ProviderDetails != nil && task.Status.ProviderDetails.AWS != nil {
			newDetails = task.Status.ProviderDetails.AWS
		}
		_ = prov.SetProtection(ctx, newM, newDetails, false)
		_ = prov.MarkState(ctx, newM, "completed", nil)

		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseOldDeleted, ""); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setPhase(ctx, task, v1alpha1.TaskPhaseSucceeded, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *NodePatchTaskReconciler) stabilityGate(ctx context.Context, task *v1alpha1.NodePatchTask) (bool, string, error) {
	stableFor := task.Spec.Stability.StableFor.Duration
	if stableFor <= 0 {
		return true, "no stability gate configured", nil
	}
	importantOnly := true
	if task.Spec.Stability.ImportantOnly != nil {
		importantOnly = *task.Spec.Stability.ImportantOnly
	}
	if importantOnly && !task.Spec.Important {
		return true, "stability gate skipped for non-important task", nil
	}

	okNow, note, err := k8s.ImportantPodsHealthyNow(ctx, r.Client, task.Spec.ImportantPodSelectors)
	if err != nil {
		return false, "", err
	}
	now := metav1.Now()
	if okNow {
		if task.Status.StableSince == nil {
			_ = r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
				s.StableSince = &now
				s.StabilityNote = "stable started: " + note
			})
			return false, "stability window started", nil
		}
		if time.Since(task.Status.StableSince.Time) >= stableFor {
			return true, "stability gate passed", nil
		}
		return false, "stability accumulating", nil
	}

	_ = r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
		s.StableSince = nil
		s.StabilityNote = "unstable: " + note
	})
	return false, "unstable", nil
}

func (r *NodePatchTaskReconciler) findNewNode(ctx context.Context, prov provider.Provider, task *v1alpha1.NodePatchTask) (*corev1.Node, string, error) {
	if task.Status.NewMachine.ID == "" {
		return nil, "new machine not recorded", nil
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return nil, "", err
	}
	m := provider.MachineRef{Provider: task.Status.NewMachine.Provider, ID: task.Status.NewMachine.ID, Location: task.Status.NewMachine.Location}
	for _, n := range nodes.Items {
		ok, _ := prov.NodeMatchesMachine(n.Spec.ProviderID, m)
		if ok {
			nn := n
			return &nn, "", nil
		}
	}
	return nil, "no node with matching providerID", nil
}

func (r *NodePatchTaskReconciler) setPhase(ctx context.Context, task *v1alpha1.NodePatchTask, phase v1alpha1.TaskPhase, note string) error {
	return r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
		s.Phase = phase
		s.UpdatedAt = metav1.Now()
		if note != "" {
			s.StabilityNote = note
		}
		s.LastError = ""
	})
}

func (r *NodePatchTaskReconciler) fail(ctx context.Context, task *v1alpha1.NodePatchTask, err error) (ctrl.Result, error) {
	_ = r.patchStatus(ctx, task, func(s *v1alpha1.NodePatchTaskStatus) {
		s.Phase = v1alpha1.TaskPhaseFailed
		s.LastError = err.Error()
		s.UpdatedAt = metav1.Now()
	})
	return ctrl.Result{}, nil
}

func (r *NodePatchTaskReconciler) patchStatus(ctx context.Context, task *v1alpha1.NodePatchTask, mutate func(*v1alpha1.NodePatchTaskStatus)) error {
	latest := &v1alpha1.NodePatchTask{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, latest); err != nil {
		return err
	}
	patch := client.MergeFrom(latest.DeepCopy())
	mutate(&latest.Status)
	if err := r.Status().Patch(ctx, latest, patch); err != nil {
		return err
	}
	task.Status = latest.Status
	return nil
}

func (r *NodePatchTaskReconciler) SetupWithManager(mgr ctrl.Manager, cfg *rest.Config) error {
	if r.Clientset == nil {
		r.Clientset = kubernetes.NewForConfigOrDie(cfg)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		For(&v1alpha1.NodePatchTask{}).
		Complete(r)
}
