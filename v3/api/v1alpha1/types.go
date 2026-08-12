package v1alpha1

import (
	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	GroupName = "agents.astatide.com"
	Version   = "v1alpha1"

	MaxStatusConditions = 16
	MaxStatusChecks     = 32
	MaxStatusMessage    = 2048
	// MaxTaskLength is the maximum number of UTF-8 code points accepted for
	// either inline task content or the fixed task key in a ConfigMap.
	// Keep this value in sync with the TaskSpec.inline MaxLength marker below.
	MaxTaskLength = 32768
)

// Phase is the lifecycle phase of an AgentRun.
type Phase string

const (
	PhasePending       Phase = "Pending"
	PhaseCloning       Phase = "Cloning"
	PhaseWorking       Phase = "Working"
	PhaseCapturing     Phase = "Capturing"
	PhaseVerifying     Phase = "Verifying"
	PhaseGated         Phase = "Gated"
	PhasePublishing    Phase = "Publishing"
	PhaseSucceeded     Phase = "Succeeded"
	PhaseRejected      Phase = "Rejected"
	PhaseFailed        Phase = "Failed"
	PhaseCancelled     Phase = "Cancelled"
	PhaseUnknownEffect Phase = "UnknownEffect"
)

// ConditionType identifies a bounded condition in a resource status.
type ConditionType string

const (
	ConditionAdmitted     ConditionType = "Admitted"
	ConditionReady        ConditionType = "Ready"
	ConditionWorkReady    ConditionType = "WorkReady"
	ConditionWorkComplete ConditionType = "WorkComplete"
	ConditionVerified     ConditionType = "Verified"
	ConditionPublished    ConditionType = "Published"
	// ConditionFindingsPublished reports the independent review/findings
	// publication. It is intentionally separate from Published, which means a
	// controller-created patch pull request only.
	ConditionFindingsPublished ConditionType = "FindingsPublished"
)

// OrchestrationBackend identifies the optional lifecycle sequencing backend.
// It is persisted only as an execution status observation; it is not user
// input on AgentRun and therefore cannot change an admitted run's contract.
type OrchestrationBackend string

const (
	OrchestrationBackendArgo OrchestrationBackend = "argo"
)

// OrchestrationPhase is deliberately separate from AgentRun Phase. Argo's
// terminal state is never an AGW Gate, publication, or effect verdict.
type OrchestrationPhase string

const (
	OrchestrationPending   OrchestrationPhase = "Pending"
	OrchestrationRunning   OrchestrationPhase = "Running"
	OrchestrationSucceeded OrchestrationPhase = "Succeeded"
	OrchestrationFailed    OrchestrationPhase = "Failed"
	OrchestrationError     OrchestrationPhase = "Error"
	OrchestrationSkipped   OrchestrationPhase = "Skipped"
	OrchestrationOmitted   OrchestrationPhase = "Omitted"
	OrchestrationSuspended OrchestrationPhase = "Suspended"
)

// EffectState is the durable state of an externally visible effect.
type EffectState string

const (
	EffectPending   EffectState = "pending"
	EffectSucceeded EffectState = "succeeded"
	EffectFailed    EffectState = "failed"
	EffectUnknown   EffectState = "unknown"
)

// PublishMode controls whether a gated result is published.
type PublishMode string

const (
	PublishNone        PublishMode = "none"
	PublishPullRequest PublishMode = "pull-request"
)

// OutputMode selects the durable result shape produced by an AgentRun.
type OutputMode string

const (
	// OutputPatch is the backwards-compatible default when output is omitted.
	OutputPatch    OutputMode = "patch"
	OutputFindings OutputMode = "findings"
	OutputBoth     OutputMode = "both"
)

// Harness identifies the supported runtime adapter.
type Harness string

const (
	HarnessCodex      Harness = "codex"
	HarnessClaudeCode Harness = "claude-code"
)

// GateMode controls whether a Gate is advisory or enforcing.
type GateMode string

const (
	GateShadow    GateMode = "shadow"
	GateEnforcing GateMode = "enforcing"
)

// TestStrength describes the minimum evidence required for newly added tests.
type TestStrength string

const (
	TestStrengthNone               TestStrength = "none"
	TestStrengthNewTestsFailOnBase TestStrength = "newTestsMustFailOnBase"
)

// GateAdapter identifies the verifier-owned language adapter used for
// language-specific evidence. Empty means that no adapter is configured.
// Go is deliberately the first adapter; generic test-file or coverage
// inference is not a trusted fallback.
type GateAdapter string

const (
	GateAdapterGo GateAdapter = "go"
)

// EffectKind is the effect class used by a ToolSet entry.
type EffectKind string

const (
	EffectRead    EffectKind = "read"
	EffectWrite   EffectKind = "write"
	EffectPublish EffectKind = "publish"
)

// ArtifactRef points at immutable evidence in object storage.
type ArtifactRef struct {
	// URI is an opaque object-store URI. Credentials must never be embedded.
	// +kubebuilder:validation:MaxLength=1024
	URI string `json:"uri"`
	// Digest is a content digest in sha256:<hex> form.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^$|^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	Kind string `json:"kind,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	MediaType string `json:"mediaType,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1099511627776
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// ChildRef identifies an operator-owned child resource.
type ChildRef struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Enum=Sandbox;Job
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=64
	UID string `json:"uid,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	Role string `json:"role,omitempty"`
	// +kubebuilder:validation:Pattern=`^$|^sha256:[a-f0-9]{64}$`
	SpecDigest string `json:"specDigest,omitempty"`
	// PlanFingerprint binds this child reference to the controller-computed,
	// backend-specific execution plan. It is distinct from SpecDigest, which
	// identifies the resolved AgentRun configuration rather than the child
	// resource's actual immutable specification.
	// +kubebuilder:validation:MaxLength=71
	// +kubebuilder:validation:Pattern=`^$|^sha256:[a-f0-9]{64}$`
	PlanFingerprint string `json:"planFingerprint,omitempty"`
}

// OrchestrationRef is the immutable Kubernetes identity bound to one admitted
// AgentRun generation. Namespace is explicit even though the current API is
// namespaced, so a status reader cannot accidentally treat a same-name
// Workflow from another namespace as this run's child.
type OrchestrationRef struct {
	// +kubebuilder:validation:Pattern=`^argoproj[.]io/v1alpha1$`
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Enum=Workflow
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	UID string `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// +kubebuilder:validation:Minimum=1
	RunGeneration int64 `json:"runGeneration"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	SpecDigest string `json:"specDigest"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	WorkflowTemplateName string `json:"workflowTemplateName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	WorkflowTemplateUID string `json:"workflowTemplateUID"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	WorkflowTemplateDigest string `json:"workflowTemplateDigest"`
}

// OrchestrationStatus is a bounded mirror of the selected backend. Its phase
// is informational and must never be used as an AGW completion contract.
type OrchestrationStatus struct {
	// +kubebuilder:validation:Enum=argo
	Backend OrchestrationBackend `json:"backend"`
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Error;Skipped;Omitted;Suspended
	Phase OrchestrationPhase `json:"phase"`
	// +kubebuilder:validation:MaxLength=2048
	Message   string           `json:"message,omitempty"`
	Reference OrchestrationRef `json:"reference"`
}

// SourceSpec identifies the immutable source revision for a run.
type SourceSpec struct {
	// Repo accepts github.com/owner/repository or an equivalent canonical host
	// form; embedded credentials and arbitrary clone URLs are rejected by the
	// admission layer.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repo string `json:"repo"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	BaseRef string `json:"baseRef"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Depth int32 `json:"depth,omitempty"`
}

// TaskSpec is either inline text or a ConfigMap key reference.
// +kubebuilder:validation:XValidation:rule="has(self.inline) != has(self.configMapRef)",message="exactly one of inline or configMapRef must be set"
type TaskSpec struct {
	// +kubebuilder:validation:MaxLength=32768
	Inline *string `json:"inline,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ConfigMapRef *string `json:"configMapRef,omitempty"`
}

// OutputTarget identifies the immutable review target for findings output.
type OutputTarget struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	PullRequest int64 `json:"pullRequest"`
}

// AgentRunOutputSpec controls the result shape of a run. A nil Output or an
// omitted Mode is interpreted as OutputPatch by the resolver for backwards
// compatibility. Findings-only mode requires a pre-existing pull request
// target; both mode targets the pull request created by patch publication.
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode == 'patch' || self.mode == 'both' || (has(self.target) && self.target.pullRequest > 0)",message="findings output requires target.pullRequest"
type AgentRunOutputSpec struct {
	// +kubebuilder:validation:Enum=patch;findings;both
	Mode   OutputMode    `json:"mode,omitempty"`
	Target *OutputTarget `json:"target,omitempty"`
}

// ScopeSpec limits the paths an agent may change.
type ScopeSpec struct {
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=512
	Paths []string `json:"paths,omitempty"`
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=512
	Forbidden []string `json:"forbidden,omitempty"`
}

// WorkspaceSpec defines disposable per-run storage.
type WorkspaceSpec struct {
	// Size is a Kubernetes resource quantity string, for example 8Gi.
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`
	Size string `json:"size"`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	StorageClassName string `json:"storageClassName,omitempty"`
}

// PublishSpec controls controller-owned publication.
// +kubebuilder:validation:XValidation:rule="self.mode == 'none' || size(self.credentialRef) > 0",message="pull-request publication requires credentialRef"
type PublishSpec struct {
	// +kubebuilder:validation:Enum=none;pull-request
	Mode PublishMode `json:"mode"`
	// This is a logical reference resolved by the operator; it is never a
	// credential value.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	CredentialRef string `json:"credentialRef,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	Title string `json:"title,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=64
	Labels []string `json:"labels,omitempty"`
}

// LimitsSpec bounds time, tool calls, and model spend.
type LimitsSpec struct {
	// +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h)$`
	Timeout string `json:"timeout"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxToolCalls int32 `json:"maxToolCalls"`
	// MaxCostUSD is a decimal string rather than a floating-point number so
	// budgets and resolved-spec digests are identical in every client.
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]{0,5})([.][0-9]{1,6})?$`
	MaxCostUSD string `json:"maxCostUsd"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000000
	MaxModelTokens int64 `json:"maxModelTokens,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1099511627776
	MaxPatchBytes int64 `json:"maxPatchBytes,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1099511627776
	MaxOutputBytes int64 `json:"maxOutputBytes,omitempty"`
}

// AgentRunSpec is the user-authored run contract.
// +kubebuilder:validation:XValidation:rule="size(self.agentRef) > 0 && size(self.gateRef) > 0",message="agentRef and gateRef are required"
// +kubebuilder:validation:XValidation:rule="self.agentRef == oldSelf.agentRef && self.gateRef == oldSelf.gateRef && self.source == oldSelf.source && self.task == oldSelf.task && self.scope == oldSelf.scope && self.workspace == oldSelf.workspace && self.publish == oldSelf.publish && self.limits == oldSelf.limits && self.output == oldSelf.output",message="AgentRun execution spec is immutable after creation"
type AgentRunSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	AgentRef string `json:"agentRef"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	GateRef string `json:"gateRef"`

	Source SourceSpec `json:"source"`
	Task   TaskSpec   `json:"task"`
	Scope  ScopeSpec  `json:"scope,omitempty"`

	Workspace WorkspaceSpec `json:"workspace"`
	Publish   PublishSpec   `json:"publish"`
	Limits    LimitsSpec    `json:"limits"`
	// Output is optional for compatibility with v3 manifests written before
	// findings mode existed. The resolver treats nil and an omitted mode as
	// patch output.
	Output *AgentRunOutputSpec `json:"output,omitempty"`

	// CancelRequested is a one-way operational request. The controller records
	// the terminal Cancelled phase; clients must not clear it.
	CancelRequested bool `json:"cancelRequested,omitempty"`
}

// AgentRuntimeSpec selects the runtime image and adapter.
type AgentRuntimeSpec struct {
	// +kubebuilder:validation:Enum=codex;claude-code
	Harness Harness `json:"harness"`
	// Runtime images must be digest-pinned in the resolved snapshot.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`
	// +kubebuilder:validation:MaxLength=253
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
}

// InstructionsSpec provides agent instructions without embedding credentials.
// +kubebuilder:validation:XValidation:rule="has(self.inline) != has(self.configMapRef)",message="exactly one of inline or configMapRef must be set"
type InstructionsSpec struct {
	// +kubebuilder:validation:MaxLength=32768
	Inline *string `json:"inline,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ConfigMapRef *string `json:"configMapRef,omitempty"`
}

// SkillRef identifies a digest-verified skill bundle.
type SkillRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Ref string `json:"ref"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`
}

// AgentSpec defines a reusable runtime configuration.
type AgentSpec struct {
	Runtime      AgentRuntimeSpec `json:"runtime"`
	Instructions InstructionsSpec `json:"instructions"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ToolSetRef string `json:"toolSetRef"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ModelRouteRef string `json:"modelRouteRef"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`
	ContextStrategyRef string `json:"contextStrategyRef"`
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`
	// +listType=set
	PolicyRefs []string `json:"policyRefs,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Skills []SkillRef `json:"skills,omitempty"`
}

// VerifyCommand is an argv command, or an explicitly requested shell command.
// +kubebuilder:validation:XValidation:rule="(size(self.argv) > 0) != (has(self.shell) && size(self.shell) > 0)",message="exactly one of argv or shell must be set"
type VerifyCommand struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Argv []string `json:"argv,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Shell *string `json:"shell,omitempty"`
}

// VerifySpec defines the independent verification execution.
type VerifySpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	Image             string `json:"image"`
	FromCleanCheckout bool   `json:"fromCleanCheckout"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Commands []VerifyCommand `json:"commands"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h)$`
	Timeout string `json:"timeout"`
}

// GateRequirements are independent machine-checkable acceptance rules.
// +kubebuilder:validation:XValidation:rule="(self.testStrength == 'none' && !has(self.baseTestCommand)) || (self.testStrength == 'newTestsMustFailOnBase' && has(self.adapter) && self.adapter == 'go' && has(self.baseTestCommand))",message="newTestsMustFailOnBase requires the go adapter and one explicit baseTestCommand; other testStrength values must not provide one"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageDelta) || size(self.coverageDelta) == 0 || (has(self.adapter) && self.adapter == 'go')",message="coverageDelta requires the go verifier adapter"
type GateRequirements struct {
	ScopeRespected bool `json:"scopeRespected"`
	// +kubebuilder:validation:Enum=none;newTestsMustFailOnBase
	TestStrength TestStrength `json:"testStrength"`
	// +kubebuilder:validation:Enum=go
	Adapter GateAdapter `json:"adapter,omitempty"`
	// BaseTestCommand is the one verifier command used by
	// newTestsMustFailOnBase. It is intentionally separate from verify.commands
	// so build, lint, and vet commands cannot be interpreted as test evidence.
	BaseTestCommand *VerifyCommand `json:"baseTestCommand,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	CoverageDelta string `json:"coverageDelta,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000
	MaxFilesChanged int32 `json:"maxFilesChanged"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000
	MaxDiffLines  int32 `json:"maxDiffLines"`
	NoBinaryFiles bool  `json:"noBinaryFiles"`
}

// GateSpec defines independent verification and acceptance.
type GateSpec struct {
	Verify  VerifySpec       `json:"verify"`
	Require GateRequirements `json:"require"`
	// Signals is optional for compatibility with execution-only Gates. When
	// present it is the complete fixed-point execution/critic contract; there
	// is deliberately no mutation signal until mutation evidence is implemented.
	Signals *GateSignalsSpec `json:"signals,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`
	// +listType=set
	PolicyRefs []string `json:"policyRefs,omitempty"`
	// +kubebuilder:validation:Enum=Rejected
	OnFail string `json:"onFail"`
	// +kubebuilder:validation:Enum=shadow;enforcing
	Mode GateMode `json:"mode"`
}

// ToolDefinition describes one MCP tool and its effect class.
type ToolDefinition struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=read;write;publish
	Effect EffectKind `json:"effect"`
	// ExactArguments is optional strict JSON matching applied by the broker.
	// +kubebuilder:validation:MaxProperties=64
	ExactArguments map[string]apix.JSON `json:"exactArguments,omitempty"`
}

// ToolServer describes an MCP upstream and its logical credential reference.
type ToolServer struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	Ref string `json:"ref"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	CredentialsRef string `json:"credentialsRef,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=name
	Tools []ToolDefinition `json:"tools"`
}

// ToolProfileName identifies the phase in which a ToolSet profile may be
// selected. The controller owns phase transitions; the ToolSet only declares
// the bounded allowlist for each named phase.
type ToolProfileName string

const (
	ToolProfileExplore ToolProfileName = "explore"
	ToolProfileEdit    ToolProfileName = "edit"
	ToolProfileVerify  ToolProfileName = "verify"

	// MaxToolsPerPhase is the largest profile that the API permits. A ToolSet
	// may configure a lower value for a particular run of the broker.
	MaxToolsPerPhase int32 = 12
)

// ToolRef binds one profile entry to exactly one named server and tool. It
// intentionally contains no endpoint, credential, effect, or argument data;
// those remain authoritative on ToolServer and ToolDefinition.
type ToolRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Server string `json:"server"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Tool string `json:"tool"`
}

// ToolProfile is the explicit, phase-scoped allowlist for one ToolSet phase.
type ToolProfile struct {
	// +kubebuilder:validation:Enum=explore;edit;verify
	Name ToolProfileName `json:"name"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=12
	// +listType=map
	// +listMapKey=server
	// +listMapKey=tool
	Tools []ToolRef `json:"tools"`
}

// ToolSetSpec defines the allowlisted MCP surface.
// +kubebuilder:validation:XValidation:rule="size(self.profiles) == 3 && self.profiles.exists(p, p.name == 'explore') && self.profiles.exists(p, p.name == 'edit') && self.profiles.exists(p, p.name == 'verify')",message="profiles must contain exactly one explore, edit, and verify profile"
// +kubebuilder:validation:XValidation:rule="self.profiles.all(p, size(p.tools) <= self.maxToolsPerPhase)",message="each profile cannot contain more than maxToolsPerPhase tools"
type ToolSetSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Servers []ToolServer `json:"servers"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +listType=map
	// +listMapKey=name
	Profiles []ToolProfile `json:"profiles"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=12
	MaxToolsPerPhase int32 `json:"maxToolsPerPhase"`
}

// ModelProvider is one ordered model backend.
type ModelProvider struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Enum=openrouter-responses;openai-responses;openrouter-anthropic-messages;anthropic-messages
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model"`
	// Family is an explicit provider/model family identifier used for future
	// critic separation. It is intentionally not inferred from Model or Name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,63}$`
	Family string `json:"family"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	CredentialRef string `json:"credentialRef,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Priority int32 `json:"priority"`
}

// ModelBudget bounds provider spend.
type ModelBudget struct {
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]{0,5})([.][0-9]{1,6})?$`
	MaxCostUSD string `json:"maxCostUsd"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000000
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MaxRequests int64 `json:"maxRequests,omitempty"`
	// +kubebuilder:validation:Pattern=`^$|^[0-9]+(s|m|h)$`
	Cooldown string `json:"cooldown,omitempty"`
}

// ModelRouteSpec defines ordered model providers.
type ModelRouteSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Providers []ModelProvider `json:"providers"`
	Budget    ModelBudget     `json:"budget"`
}

// ConditionStatus is intentionally narrower than a free-form string and is
// used by every status condition in this package.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition is a bounded status condition. Full diagnostics belong in the
// event stream or report artifact, not in a Kubernetes status object.
type Condition struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status ConditionStatus `json:"status"`
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=2048
	Message            string      `json:"message,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// ResourceStatus is the bounded status shared by configuration resources.
type ResourceStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:MaxLength=71
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	SpecDigest string `json:"specDigest,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []Condition `json:"conditions,omitempty"`
}

// PatchSummary is a bounded summary of a captured patch.
type PatchSummary struct {
	Ref         *ArtifactRef `json:"ref,omitempty"`
	ManifestRef *ArtifactRef `json:"manifestRef,omitempty"`
	// +kubebuilder:validation:Minimum=0
	FilesChanged int32 `json:"filesChanged"`
	// +kubebuilder:validation:Minimum=0
	LinesChanged int32 `json:"linesChanged"`
}

// GateCheck is one bounded machine-checkable gate observation.
type GateCheck struct {
	// +kubebuilder:validation:MaxLength=128
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// GateResult is the status projection of independent verification.
type GateResult struct {
	// Name, UID, and Generation identify the immutable Gate revision that
	// produced this result. A human shadow review must bind to this revision;
	// the current Gate object may have changed by the time the review is made.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	UID string `json:"uid,omitempty"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation,omitempty"`
	// +kubebuilder:validation:Enum=shadow;enforcing
	Mode GateMode `json:"mode,omitempty"`
	// +kubebuilder:validation:Enum=Accepted;Rejected
	Verdict   string       `json:"verdict,omitempty"`
	ReportRef *ArtifactRef `json:"reportRef,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Checks []GateCheck `json:"checks,omitempty"`
}

// EffectSummary is the bounded projection of a durable effect ledger entry.
type EffectSummary struct {
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Key string `json:"key,omitempty"`
	// +kubebuilder:validation:Enum=pending;succeeded;failed;unknown
	State EffectState `json:"state,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	PullRequestURL string `json:"pullRequestUrl,omitempty"`
}

// FailureStatus is safe to expose in the API and deliberately bounded.
type FailureStatus struct {
	// +kubebuilder:validation:MaxLength=128
	Code string `json:"code"`
	// +kubebuilder:validation:MaxLength=2048
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// AgentRunStatus is a bounded projection; full evidence lives in artifacts.
type AgentRunStatus struct {
	// +kubebuilder:validation:Enum=Pending;Cloning;Working;Capturing;Verifying;Gated;Publishing;Succeeded;Rejected;Failed;Cancelled;UnknownEffect
	Phase              Phase `json:"phase,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	SpecDigest      string       `json:"specDigest,omitempty"`
	ResolvedSpecRef *ArtifactRef `json:"resolvedSpecRef,omitempty"`
	ContextPackRef  *ArtifactRef `json:"contextPackRef,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{40,64}$`
	BaseSHA          string               `json:"baseSHA,omitempty"`
	WorkSandboxRef   *ChildRef            `json:"workSandboxRef,omitempty"`
	VerifySandboxRef *ChildRef            `json:"verifySandboxRef,omitempty"`
	Patch            *PatchSummary        `json:"patch,omitempty"`
	Gate             *GateResult          `json:"gate,omitempty"`
	Effect           *EffectSummary       `json:"effect,omitempty"`
	Orchestration    *OrchestrationStatus `json:"orchestration,omitempty"`
	EventStreamRef   *ArtifactRef         `json:"eventStreamRef,omitempty"`
	// +kubebuilder:validation:Minimum=0
	ToolCallCount int64 `json:"toolCallCount,omitempty"`
	// +kubebuilder:validation:Pattern=`^$|^(0|[1-9][0-9]{0,5})([.][0-9]{1,6})?$`
	CostUSD     string         `json:"costUsd,omitempty"`
	Failure     *FailureStatus `json:"failure,omitempty"`
	StartedAt   *metav1.Time   `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time   `json:"completedAt,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []Condition `json:"conditions,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Children []ChildRef `json:"children,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Artifacts []ArtifactRef `json:"artifacts,omitempty"`
	Published bool          `json:"published,omitempty"`
}
