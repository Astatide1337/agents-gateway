package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AgentRun is the only resource a user needs to create for a run.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=agwrun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Gate",type="string",JSONPath=".status.gate.verdict"
// +kubebuilder:printcolumn:name="PR",type="string",JSONPath=".status.effect.pullRequestUrl"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="!oldSelf.spec.cancelRequested || self.spec.cancelRequested",message="cancelRequested cannot be cleared once true"
type AgentRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentRunSpec   `json:"spec"`
	Status            AgentRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// AgentRunList contains a list of AgentRuns.
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}
