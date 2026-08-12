package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ContextRepoMapSpec configures the structural repository map tier.
type ContextRepoMapSpec struct {
	// +kubebuilder:validation:Enum=tree-sitter
	Kind string `json:"kind"`
	// Hard token budget for the generated repository map.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100000
	Budget int32 `json:"budget"`
}

// ContextSymbolsSpec configures the language-server symbol tier.
type ContextSymbolsSpec struct {
	// +kubebuilder:validation:Enum=lsp-serena
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Enum=go;typescript;javascript;python;rust;java;dotnet
	// +listType=set
	Languages []string `json:"languages"`
}

// ContextHistorySpec configures bounded history for touched paths.
type ContextHistorySpec struct {
	// +kubebuilder:validation:Enum=git-blame-touched
	Kind string `json:"kind"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Depth int32 `json:"depth"`
}

// ContextConventionsSpec enables Policy-derived conventions in AGENTS.md.
type ContextConventionsSpec struct {
	FromPolicy bool `json:"fromPolicy"`
}

// ContextSemanticSpec reserves the semantic tier without enabling it in the
// first stable release.
type ContextSemanticSpec struct {
	Enabled bool `json:"enabled"`
}

// ContextBudgetSpec is the hard aggregate context budget.
type ContextBudgetSpec struct {
	// +kubebuilder:validation:Minimum=1
	// Keep the aggregate context below the point where attention quality starts
	// thinning in the first stable release. Larger budgets require measured
	// evidence and a future API revision.
	// +kubebuilder:validation:Maximum=131072
	TotalTokens int32 `json:"totalTokens"`
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=67108864
	MaxBytes int64 `json:"maxBytes"`
}

// ContextStrategySpec describes deterministic context assembly. All tiers are
// optional so the schema can grow without inventing fake settings, but the CEL
// invariant below rejects a completely empty/no-op strategy.
// +kubebuilder:validation:XValidation:rule="has(self.repoMap) || has(self.symbols) || has(self.history) || (has(self.conventions) && self.conventions.fromPolicy)",message="ContextStrategy must enable at least one context tier"
// +kubebuilder:validation:XValidation:rule="!has(self.repoMap) || self.repoMap.budget <= self.budget.totalTokens",message="repoMap budget cannot exceed the aggregate totalTokens budget"
// +kubebuilder:validation:XValidation:rule="!has(self.semantic) || !self.semantic.enabled",message="semantic context is disabled in this release"
type ContextStrategySpec struct {
	RepoMap     *ContextRepoMapSpec     `json:"repoMap,omitempty"`
	Symbols     *ContextSymbolsSpec     `json:"symbols,omitempty"`
	History     *ContextHistorySpec     `json:"history,omitempty"`
	Conventions *ContextConventionsSpec `json:"conventions,omitempty"`
	Semantic    *ContextSemanticSpec    `json:"semantic,omitempty"`
	Budget      ContextBudgetSpec       `json:"budget"`
}

// ContextStrategy is a reusable, bounded context assembly contract.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=agwcontext
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ContextStrategy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContextStrategySpec `json:"spec"`
	Status            ResourceStatus      `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ContextStrategyList contains a list of ContextStrategies.
type ContextStrategyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContextStrategy `json:"items"`
}
