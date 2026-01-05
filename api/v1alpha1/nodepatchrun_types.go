package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type NamespacedLabelSelector struct {
    Namespace     string               `json:"namespace,omitempty"`
    LabelSelector metav1.LabelSelector `json:"labelSelector"`
}

type StableGate struct {
    // +optional
    StableFor metav1.Duration `json:"stableFor,omitempty"`
    // +optional
    ImportantOnly *bool `json:"importantOnly,omitempty"`
}

type LabelTransferSpec struct {
    // +optional
    Keys []string `json:"keys,omitempty"`
    // +optional
    Prefixes []string `json:"prefixes,omitempty"`
    // +optional
    RemoveMissing *bool `json:"removeMissing,omitempty"`
    // +optional
    ExcludeKeys []string `json:"excludeKeys,omitempty"`
}

type AutoscalerControlSpec struct {
    // +optional
    Enabled *bool `json:"enabled,omitempty"`
    // +optional
    DeploymentSelector *metav1.LabelSelector `json:"deploymentSelector,omitempty"`
    // +optional
    Namespace string `json:"namespace,omitempty"`
    // +optional
    ForceRestore *bool `json:"forceRestore,omitempty"`
}

type FailurePolicy struct {
    // +optional
    Enabled *bool `json:"enabled,omitempty"`
    // +optional
    PauseAfterFailures *int `json:"pauseAfterFailures,omitempty"`
}

type ProviderRef struct {
    Type string           `json:"type"` // "aws"
    AWS  *AWSProviderSpec `json:"aws,omitempty"`
}

type AWSProviderSpec struct {
    Region string `json:"region"`
    // +optional
    StrictAZ *bool `json:"strictAZ,omitempty"`
    // +optional
    ExtraTags map[string]string `json:"extraTags,omitempty"`
    // +optional
    ClusterID string `json:"clusterId,omitempty"`
}

type NodePatchRunSpec struct {
    // +optional
    Paused *bool `json:"paused,omitempty"`
    // +optional
    FailurePolicy *FailurePolicy `json:"failurePolicy,omitempty"`

    // +optional
    NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
    // +optional
    ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
    // +optional
    ExcludeControlPlane *bool `json:"excludeControlPlane,omitempty"`

    // +optional
    PartitionIterations *int `json:"partitionIterations,omitempty"`

    // +optional
    BatchSizeImportant *int `json:"batchSizeImportant,omitempty"`
    // +optional
    BatchSizeNonImportant *int `json:"batchSizeNonImportant,omitempty"`
    // +optional
    MaxConcurrentPerZone *int `json:"maxConcurrentPerZone,omitempty"`

    // +optional
    ImportantPodSelectors []NamespacedLabelSelector `json:"importantPodSelectors,omitempty"`
    // +optional
    RequiredNodeDaemonPodSelectors []NamespacedLabelSelector `json:"requiredNodeDaemonPodSelectors,omitempty"`

    // +optional
    Stability StableGate `json:"stability,omitempty"`
    // +optional
    LabelTransfer *LabelTransferSpec `json:"labelTransfer,omitempty"`

    // +optional
    AutoscalerControl *AutoscalerControlSpec `json:"autoscalerControl,omitempty"`

    Provider ProviderRef `json:"provider"`
}

type PlannedNode struct {
    Name string `json:"name"`
    Zone string `json:"zone,omitempty"`
}

type Partition struct {
    Index int      `json:"index"`
    Nodes []string `json:"nodes"`
}

type RunPlan struct {
    PlanVersion string        `json:"planVersion,omitempty"`
    Candidates  []PlannedNode `json:"candidates,omitempty"`
    Partitions  []Partition   `json:"partitions,omitempty"`
}

type AutoscalerTargetStatus struct {
    Namespace        string      `json:"namespace"`
    Name             string      `json:"name"`
    OriginalReplicas int32       `json:"originalReplicas"`
    LastScaledAt     metav1.Time `json:"lastScaledAt,omitempty"`
}

type AutoscalerControlStatus struct {
    ScaledDown bool                     `json:"scaledDown,omitempty"`
    Targets    []AutoscalerTargetStatus `json:"targets,omitempty"`
}

type NodePatchRunStatus struct {
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    Plan             *RunPlan `json:"plan,omitempty"`
    CurrentIteration int      `json:"currentIteration,omitempty"`

    InFlightImportant    int `json:"inFlightImportant,omitempty"`
    InFlightNonImportant int `json:"inFlightNonImportant,omitempty"`
    Completed            int `json:"completed,omitempty"`
    Failed               int `json:"failed,omitempty"`

    Paused      bool   `json:"paused,omitempty"`
    PauseReason string `json:"pauseReason,omitempty"`

    Autoscaler *AutoscalerControlStatus `json:"autoscaler,omitempty"`

    LastError string `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type NodePatchRun struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              NodePatchRunSpec   `json:"spec,omitempty"`
    Status            NodePatchRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NodePatchRunList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []NodePatchRun `json:"items"`
}

func init() { SchemeBuilder.Register(&NodePatchRun{}, &NodePatchRunList{}) }
