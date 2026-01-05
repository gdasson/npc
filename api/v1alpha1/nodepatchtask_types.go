package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:validation:Enum=Pending;Locked;Cordoned;ReplacementEnsured;NewNodeReady;LabelsTransferred;Drained;OldDeleted;Succeeded;Failed
type TaskPhase string

const (
    TaskPhasePending            TaskPhase = "Pending"
    TaskPhaseLocked             TaskPhase = "Locked"
    TaskPhaseCordoned           TaskPhase = "Cordoned"
    TaskPhaseReplacementEnsured TaskPhase = "ReplacementEnsured"
    TaskPhaseNewNodeReady       TaskPhase = "NewNodeReady"
    TaskPhaseLabelsTransferred  TaskPhase = "LabelsTransferred"
    TaskPhaseDrained            TaskPhase = "Drained"
    TaskPhaseOldDeleted         TaskPhase = "OldDeleted"
    TaskPhaseSucceeded          TaskPhase = "Succeeded"
    TaskPhaseFailed             TaskPhase = "Failed"
)

type MachineRef struct {
    Provider string            `json:"provider,omitempty"`
    ID       string            `json:"id,omitempty"`
    Location map[string]string `json:"location,omitempty"`
}

type AWSDetails struct {
    Region                string `json:"region,omitempty"`
    ASGName               string `json:"asgName,omitempty"`
    SubnetID              string `json:"subnetId,omitempty"`
    LaunchTemplateID      string `json:"launchTemplateId,omitempty"`
    LaunchTemplateVersion string `json:"launchTemplateVersion,omitempty"`

    NewProtected bool `json:"newProtected,omitempty"`
}

type ProviderDetails struct {
    AWS *AWSDetails `json:"aws,omitempty"`
}

type NodePatchTaskSpec struct {
    RunName string `json:"runName"`
    RunUID  string `json:"runUID"`

    Iteration int    `json:"iteration"`
    NodeName  string `json:"nodeName"`
    Zone      string `json:"zone,omitempty"`

    Important bool `json:"important"`

    Provider ProviderRef `json:"provider"`

    // copied from run
    // +optional
    ImportantPodSelectors []NamespacedLabelSelector `json:"importantPodSelectors,omitempty"`
    // +optional
    RequiredNodeDaemonPodSelectors []NamespacedLabelSelector `json:"requiredNodeDaemonPodSelectors,omitempty"`
    // +optional
    Stability StableGate `json:"stability,omitempty"`
    // +optional
    LabelTransfer *LabelTransferSpec `json:"labelTransfer,omitempty"`
}

type NodePatchTaskStatus struct {
    Phase TaskPhase `json:"phase,omitempty"`

    OldMachine MachineRef `json:"oldMachine,omitempty"`
    NewMachine MachineRef `json:"newMachine,omitempty"`

    NewNodeName string `json:"newNodeName,omitempty"`

    ProviderDetails *ProviderDetails `json:"providerDetails,omitempty"`

    StableSince   *metav1.Time `json:"stableSince,omitempty"`
    StabilityNote string       `json:"stabilityNote,omitempty"`

    LastError string      `json:"lastError,omitempty"`
    UpdatedAt metav1.Time `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type NodePatchTask struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              NodePatchTaskSpec   `json:"spec,omitempty"`
    Status            NodePatchTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NodePatchTaskList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []NodePatchTask `json:"items"`
}

func init() { SchemeBuilder.Register(&NodePatchTask{}, &NodePatchTaskList{}) }
