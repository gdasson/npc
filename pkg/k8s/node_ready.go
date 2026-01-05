package k8s

import corev1 "k8s.io/api/core/v1"

func IsNodeReady(n *corev1.Node) bool {
    for _, c := range n.Status.Conditions {
        if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
            return true
        }
    }
    return false
}
