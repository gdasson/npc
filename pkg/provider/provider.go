package provider

import "context"

type MachineRef struct {
    Provider string
    ID       string
    Location map[string]string
}

type ReplacementSpec struct {
    RunUID     string
    TaskName   string
    NodeName   string
    StrictLocation bool
    ExtraTags  map[string]string
    ClusterID  string
}

type Provider interface {
    Resolve(ctx context.Context, nodeProviderID string) (MachineRef, any, error)
    EnsureReplacement(ctx context.Context, old MachineRef, oldDetails any, spec ReplacementSpec) (MachineRef, any, error)
    NodeMatchesMachine(nodeProviderID string, machine MachineRef) (bool, error)
    MarkState(ctx context.Context, machine MachineRef, state string, tags map[string]string) error
    SetProtection(ctx context.Context, machine MachineRef, details any, protected bool) error
    DeleteMachine(ctx context.Context, machine MachineRef, details any) error
    CleanupRunOrphans(ctx context.Context, clusterID, runUID string, hasNode func(instanceID string) (bool, error)) error
}
