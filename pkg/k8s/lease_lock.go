package k8s

import (
    "context"
    "fmt"
    "time"

    coordv1 "k8s.io/api/coordination/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type LeaseLock struct {
    Client    client.Client
    Namespace string
    TTL       time.Duration
}

func (l *LeaseLock) Acquire(ctx context.Context, holder, nodeName string) (bool, error) {
    name := fmt.Sprintf("nodepatcher-%s", nodeName)
    now := metav1.NowMicro()

    lease := &coordv1.Lease{}
    err := l.Client.Get(ctx, types.NamespacedName{Namespace: l.Namespace, Name: name}, lease)
    if apierrors.IsNotFound(err) {
        newLease := &coordv1.Lease{
            ObjectMeta: metav1.ObjectMeta{Namespace: l.Namespace, Name: name},
            Spec: coordv1.LeaseSpec{
                HolderIdentity: &holder,
                LeaseDurationSeconds: int32Ptr(int32(l.TTL.Seconds())),
                RenewTime: &now,
            },
        }
        if err := l.Client.Create(ctx, newLease); err != nil { return false, err }
        return true, nil
    }
    if err != nil { return false, err }

    if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder {
        lease.Spec.RenewTime = &now
        return true, l.Client.Update(ctx, lease)
    }

    if lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil {
        exp := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
        if time.Now().After(exp) {
            lease.Spec.HolderIdentity = &holder
            lease.Spec.RenewTime = &now
            lease.Spec.LeaseDurationSeconds = int32Ptr(int32(l.TTL.Seconds()))
            return true, l.Client.Update(ctx, lease)
        }
    }
    return false, nil
}

func (l *LeaseLock) Release(ctx context.Context, holder, nodeName string) error {
    name := fmt.Sprintf("nodepatcher-%s", nodeName)
    lease := &coordv1.Lease{}
    if err := l.Client.Get(ctx, types.NamespacedName{Namespace: l.Namespace, Name: name}, lease); err != nil {
        return client.IgnoreNotFound(err)
    }
    if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder {
        return l.Client.Delete(ctx, lease)
    }
    return nil
}

func int32Ptr(v int32) *int32 { return &v }
