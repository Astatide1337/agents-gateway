package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ModelRoute is an ordered, explicitly configured model provider route.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=agwmodelroute
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ModelRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelRouteSpec `json:"spec"`
	Status            ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ModelRouteList contains a list of ModelRoutes.
type ModelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelRoute `json:"items"`
}
