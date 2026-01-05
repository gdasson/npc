package k8s

import (
    "context"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    "sigs.k8s.io/controller-runtime/pkg/client"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
)

func ImportantNodesNow(ctx context.Context, c client.Client, selectors []v1alpha1.NamespacedLabelSelector) (map[string]struct{}, error) {
    out := map[string]struct{}{}
    for _, s := range selectors {
        sel, err := metav1.LabelSelectorAsSelector(&s.LabelSelector)
        if err != nil { return nil, err }
        pods := &corev1.PodList{}
        opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.Selector(sel)}}
        if s.Namespace != "" { opts = append(opts, client.InNamespace(s.Namespace)) }
        if err := c.List(ctx, pods, opts...); err != nil { return nil, err }
        for _, p := range pods.Items {
            if p.Spec.NodeName != "" {
                out[p.Spec.NodeName] = struct{}{}
            }
        }
    }
    return out, nil
}
