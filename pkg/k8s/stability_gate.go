package k8s

import (
    "context"
    "fmt"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    "sigs.k8s.io/controller-runtime/pkg/client"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
)

func ImportantPodsHealthyNow(ctx context.Context, c client.Client, selectors []v1alpha1.NamespacedLabelSelector) (bool, string, error) {
    for _, s := range selectors {
        sel, err := metav1.LabelSelectorAsSelector(&s.LabelSelector)
        if err != nil { return false, "", err }

        pods := &corev1.PodList{}
        opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.Selector(sel)}}
        if s.Namespace != "" {
            opts = append(opts, client.InNamespace(s.Namespace))
        }
        if err := c.List(ctx, pods, opts...); err != nil {
            return false, "", err
        }

        for _, p := range pods.Items {
            if p.DeletionTimestamp != nil { continue }
            if p.Status.Phase != corev1.PodRunning {
                return false, fmt.Sprintf("pod %s/%s not Running", p.Namespace, p.Name), nil
            }
            if !podReady(&p) {
                return false, fmt.Sprintf("pod %s/%s not Ready", p.Namespace, p.Name), nil
            }
        }
    }
    return true, "important pods healthy", nil
}
