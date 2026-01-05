package k8s

import (
    "context"
    "strconv"
    "time"

    appsv1 "k8s.io/api/apps/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
)

const (
    AnnCAOrigReplicas = "nodepatcher.io/ca-orig-replicas"
    AnnCALockRunUID   = "nodepatcher.io/ca-lock-runuid"
    AnnCALockTS       = "nodepatcher.io/ca-lock-ts"
)

func FindAutoscalerDeployments(ctx context.Context, c client.Client, spec *v1alpha1.AutoscalerControlSpec) ([]appsv1.Deployment, error) {
    if spec == nil || spec.Enabled == nil || !*spec.Enabled || spec.DeploymentSelector == nil {
        return nil, nil
    }
    sel, err := metav1.LabelSelectorAsSelector(spec.DeploymentSelector)
    if err != nil { return nil, err }
    list := &appsv1.DeploymentList{}
    opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.Selector(sel)}}
    if spec.Namespace != "" { opts = append(opts, client.InNamespace(spec.Namespace)) }
    if err := c.List(ctx, list, opts...); err != nil { return nil, err }
    return list.Items, nil
}

func ScaleDownAndAnnotateAutoscaler(ctx context.Context, c client.Client, runUID string, deployments []appsv1.Deployment) ([]v1alpha1.AutoscalerTargetStatus, error) {
    targets := make([]v1alpha1.AutoscalerTargetStatus, 0, len(deployments))
    for i := range deployments {
        d := deployments[i]

        orig := int32(1)
        if d.Spec.Replicas != nil { orig = *d.Spec.Replicas }

        patch := client.MergeFrom(d.DeepCopy())
        if d.Annotations == nil { d.Annotations = map[string]string{} }
        d.Annotations[AnnCAOrigReplicas] = strconv.Itoa(int(orig))
        d.Annotations[AnnCALockRunUID] = runUID
        d.Annotations[AnnCALockTS] = time.Now().UTC().Format(time.RFC3339)

        zero := int32(0)
        d.Spec.Replicas = &zero
        if err := c.Patch(ctx, &d, patch); err != nil { return nil, err }

        targets = append(targets, v1alpha1.AutoscalerTargetStatus{
            Namespace: d.Namespace,
            Name: d.Name,
            OriginalReplicas: orig,
            LastScaledAt: metav1.Now(),
        })
    }
    return targets, nil
}

func RestoreAutoscalerFromStatusOrAnnotation(ctx context.Context, c client.Client, spec *v1alpha1.AutoscalerControlSpec, status *v1alpha1.AutoscalerControlStatus) error {
    if spec == nil || spec.Enabled == nil || !*spec.Enabled { return nil }
    force := true
    if spec.ForceRestore != nil { force = *spec.ForceRestore }

    if status != nil && len(status.Targets) > 0 {
        for _, t := range status.Targets {
            d := &appsv1.Deployment{}
            if err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, d); err != nil {
                continue
            }
            cur := int32(1)
            if d.Spec.Replicas != nil { cur = *d.Spec.Replicas }
            if !force && cur != 0 { continue }

            if cur != t.OriginalReplicas {
                patch := client.MergeFrom(d.DeepCopy())
                desired := t.OriginalReplicas
                d.Spec.Replicas = &desired
                if err := c.Patch(ctx, d, patch); err != nil { return err }
            }
            patch2 := client.MergeFrom(d.DeepCopy())
            if d.Annotations != nil {
                delete(d.Annotations, AnnCAOrigReplicas)
                delete(d.Annotations, AnnCALockRunUID)
                delete(d.Annotations, AnnCALockTS)
            }
            _ = c.Patch(ctx, d, patch2)
        }
        return nil
    }

    dpls, err := FindAutoscalerDeployments(ctx, c, spec)
    if err != nil { return err }
    for i := range dpls {
        d := &dpls[i]
        if d.Annotations == nil { continue }
        origStr := d.Annotations[AnnCAOrigReplicas]
        if origStr == "" { continue }
        orig, err := strconv.Atoi(origStr)
        if err != nil { continue }

        cur := int32(1)
        if d.Spec.Replicas != nil { cur = *d.Spec.Replicas }
        if !force && cur != 0 { continue }

        if cur != int32(orig) {
            patch := client.MergeFrom(d.DeepCopy())
            desired := int32(orig)
            d.Spec.Replicas = &desired
            if err := c.Patch(ctx, d, patch); err != nil { return err }
        }
        patch2 := client.MergeFrom(d.DeepCopy())
        delete(d.Annotations, AnnCAOrigReplicas)
        delete(d.Annotations, AnnCALockRunUID)
        delete(d.Annotations, AnnCALockTS)
        _ = c.Patch(ctx, d, patch2)
    }
    return nil
}
