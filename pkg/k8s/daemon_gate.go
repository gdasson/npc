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

func podReady(p *corev1.Pod) bool {
    for _, c := range p.Status.Conditions {
        if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
            return true
        }
    }
    return false
}

func RequiredDaemonPodsReadyOnNode(ctx context.Context, c client.Client, nodeName string, selectors []v1alpha1.NamespacedLabelSelector) (bool, string, error) {
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

        foundOnNode := false
        foundReady := false
        for _, p := range pods.Items {
            if p.Spec.NodeName != nodeName { continue }
            foundOnNode = true
            if p.DeletionTimestamp != nil { continue }
            if p.Status.Phase == corev1.PodRunning && podReady(&p) {
                foundReady = true
                break
            }
        }
        if !foundOnNode {
            return false, fmt.Sprintf("required daemon pods not scheduled yet (ns=%q)", s.Namespace), nil
        }
        if !foundReady {
            return false, fmt.Sprintf("required daemon pods scheduled but not Ready yet (ns=%q)", s.Namespace), nil
        }
    }
    return true, "all required daemon pods Ready on new node", nil
}
