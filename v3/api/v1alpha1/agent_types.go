package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Agent is a reusable runtime, instruction, tool, and model configuration.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=agwagent
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Harness",type="string",JSONPath=".spec.runtime.harness"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentSpec      `json:"spec"`
	Status            ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// AgentList contains a list of Agents.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}
