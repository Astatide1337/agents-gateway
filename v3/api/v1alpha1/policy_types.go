package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PolicySeverity determines whether a deterministic policy rule can reject a
// run. Advisory rules are context and self-check hints only.
type PolicySeverity string

const (
	PolicySeverityBlocking PolicySeverity = "blocking"
	PolicySeverityAdvisory PolicySeverity = "advisory"
)

// PolicyCheck is the only deterministic check form supported by the first
// stable API. The script is resolved from the pristine repository revision.
type PolicyCheck struct {
	// +kubebuilder:validation:Enum=script
	Kind string `json:"kind"`
	// A safe repository-relative path. Absolute paths, empty components, and
	// dot-directory traversal are not representable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^(?:[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)?)(?:/(?:[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)?))*$`
	Script string `json:"script"`
	// +kubebuilder:validation:Enum=exit0
	Expect string `json:"expect"`
}

// PolicyRule is one bounded, portable quality rule.
// +kubebuilder:validation:XValidation:rule="self.severity != 'blocking' || has(self.check)",message="blocking policy rules require a deterministic check"
// +kubebuilder:validation:XValidation:rule="!has(self.check) || self.check.kind == 'script'",message="policy checks must use kind=script"
// +kubebuilder:validation:XValidation:rule="!has(self.check) || self.check.expect == 'exit0'",message="policy checks must use expect=exit0"
type PolicyRule struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=blocking;advisory
	Severity PolicySeverity `json:"severity"`
	// +kubebuilder:validation:MaxLength=8192
	Context string       `json:"context,omitempty"`
	Check   *PolicyCheck `json:"check,omitempty"`
}

// PolicySpec is a namespaced, reusable quality contract.
type PolicySpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=id
	Rules []PolicyRule `json:"rules"`
}

// Policy is a reusable quality contract consumed by Agent and Gate.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=agwpolicy
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Policy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PolicySpec     `json:"spec"`
	Status            ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// PolicyList contains a list of Policies.
type PolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Policy `json:"items"`
}
