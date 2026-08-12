package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Gate is an independent verification and acceptance policy.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=agwgate
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Gate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GateSpec       `json:"spec"`
	Status            ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// GateList contains a list of Gates.
type GateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Gate `json:"items"`
}
