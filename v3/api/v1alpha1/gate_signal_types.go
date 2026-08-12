package v1alpha1

// GateScoreScale is the fixed-point scale used by Gate signal weights and
// scores. Ten thousand basis points represent one whole signal. Integer
// arithmetic keeps the resolved contract and every decision reproducible
// across languages and architectures.
const GateScoreScale int32 = 10000

// MaxCriticFindings is the largest critic finding set a Gate may ask the
// verifier to route for one candidate. The finding-corroboration package has
// the same ceiling; keeping the API ceiling here prevents a caller from
// requesting a larger result than the downstream contract can hold.
const MaxCriticFindings int32 = 256

// GateSignalsSpec declares the signal mix used by the independent Gate.
//
// Signals is optional for compatibility with execution-only Gates. An omitted
// value has the explicit effective configuration of execution=10000,
// critic=0, and minimum=10000. When Critic is present, the two weights must
// sum exactly to GateScoreScale and the critic route must be resolved and
// proven distinct from the worker route before a run is admitted.
//
// Mutation testing is intentionally not represented here. It remains deferred
// until it has a verified execution implementation and its own bounded
// evidence contract.
// +kubebuilder:validation:XValidation:rule="self.executionWeightBasisPoints >= 0 && self.executionWeightBasisPoints <= 10000 && self.minScoreBasisPoints >= 0 && self.minScoreBasisPoints <= 10000",message="Gate signal weights and minimum score must be between 0 and 10000 basis points"
// +kubebuilder:validation:XValidation:rule="!has(self.critic) ? (self.executionWeightBasisPoints == 10000 && self.minScoreBasisPoints == 10000) : (self.executionWeightBasisPoints > 0 && self.critic.weightBasisPoints > 0 && self.executionWeightBasisPoints + self.critic.weightBasisPoints == 10000)",message="execution and critic weights must sum exactly to 10000 basis points; an execution-only signal block must use execution=10000 and minimum=10000"
type GateSignalsSpec struct {
	// ExecutionWeightBasisPoints weights the deterministic execution signal.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000
	ExecutionWeightBasisPoints int32 `json:"executionWeightBasisPoints"`
	// Critic is omitted for an explicit execution-only signal block.
	Critic *GateCriticSignalSpec `json:"critic,omitempty"`
	// MinScoreBasisPoints is the minimum weighted score for a candidate that
	// has no blocking deterministic or corroborated critic finding.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000
	MinScoreBasisPoints int32 `json:"minScoreBasisPoints"`
}

// GateCriticSignalSpec configures the execution-free critic signal.
// +kubebuilder:validation:XValidation:rule="size(self.modelRouteRef) > 0 && self.weightBasisPoints > 0 && self.weightBasisPoints <= 10000 && self.maxFindings > 0 && self.maxFindings <= 256",message="critic requires a route, a positive bounded weight, and 1..256 findings"
type GateCriticSignalSpec struct {
	// WeightBasisPoints is the critic's fixed-point weight. Together with
	// GateSignalsSpec.ExecutionWeightBasisPoints it must equal 10000.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	WeightBasisPoints int32 `json:"weightBasisPoints"`
	// ModelRouteRef names a separately resolved critic route. Admission
	// compares every provider family in this route with the worker route.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ModelRouteRef string `json:"modelRouteRef"`
	// MaxFindings bounds critic output before it enters the corroboration
	// router. It is intentionally explicit rather than silently defaulted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	MaxFindings int32 `json:"maxFindings"`
}
