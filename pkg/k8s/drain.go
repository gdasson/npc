package k8s

import (
    "context"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/kubectl/pkg/drain"
)

func DrainNode(ctx context.Context, clientset kubernetes.Interface, node *corev1.Node, timeout time.Duration) error {
    helper := &drain.Helper{
        Ctx: ctx,
        Client: clientset,
        Force: false,
        IgnoreAllDaemonSets: true,
        DeleteEmptyDirData: true,
        GracePeriodSeconds: -1,
        Timeout: timeout,
    }
    if err := drain.RunCordonOrUncordon(helper, node, true); err != nil {
        return err
    }
    return drain.RunNodeDrain(helper, node.Name)
}
