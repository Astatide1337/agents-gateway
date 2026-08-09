package spec

// This package contains the v1alpha1 wire contracts. The structs deliberately
// use only standard-library types so they can be shared by the API, CLI, and
// generated SDKs without coupling the contracts to a runtime implementation.

const (
	APIVersion         = "agents.astatide.com/v1alpha1"
	KindOrganization   = "Organization"
	KindProject        = "Project"
	KindAgent          = "Agent"
	KindSkillSet       = "SkillSet"
	KindToolSet        = "ToolSet"
	KindSandboxProfile = "SandboxProfile"
	KindModelRoute     = "ModelRoute"
	KindWorkflow       = "Workflow"
	KindAgentRun       = "AgentRun"
	KindWorkflowRun    = "WorkflowRun"
	KindApproval       = "Approval"
	KindArtifact       = "Artifact"
	KindRunner         = "Runner"
	KindCredential     = "Credential"
	KindEntitlement    = "Entitlement"
)

// TypeMeta and ObjectMeta form the common Kubernetes-like envelope without
// making the v2 contracts depend on Kubernetes libraries.
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

type ObjectMeta struct {
	Name        string            `json:"name" yaml:"name"`
	Namespace   string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

type ResourceMeta struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta `json:"metadata" yaml:"metadata"`
}

type Resource interface {
	Meta() *ResourceMeta
}

type Organization struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         OrganizationSpec `json:"spec" yaml:"spec"`
}

func (r *Organization) Meta() *ResourceMeta { return &r.ResourceMeta }

type OrganizationSpec struct {
	DisplayName string        `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description string        `json:"description,omitempty" yaml:"description,omitempty"`
	Quotas      ResourceQuota `json:"quotas,omitempty" yaml:"quotas,omitempty"`
	Retention   RetentionSpec `json:"retention,omitempty" yaml:"retention,omitempty"`
}

type Project struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         ProjectSpec `json:"spec" yaml:"spec"`
}

func (r *Project) Meta() *ResourceMeta { return &r.ResourceMeta }

type ProjectSpec struct {
	OrganizationRef string        `json:"organizationRef" yaml:"organizationRef"`
	Description     string        `json:"description,omitempty" yaml:"description,omitempty"`
	Quotas          ResourceQuota `json:"quotas,omitempty" yaml:"quotas,omitempty"`
	Retention       RetentionSpec `json:"retention,omitempty" yaml:"retention,omitempty"`
}

type Agent struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         AgentSpec `json:"spec" yaml:"spec"`
}

func (r *Agent) Meta() *ResourceMeta { return &r.ResourceMeta }

type AgentSpec struct {
	Runtime           RuntimeSpec           `json:"runtime" yaml:"runtime"`
	Instructions      InstructionsSpec      `json:"instructions,omitempty" yaml:"instructions,omitempty"`
	SkillSetRef       string                `json:"skillSetRef,omitempty" yaml:"skillSetRef,omitempty"`
	Skills            []SkillRef            `json:"skills,omitempty" yaml:"skills,omitempty"`
	ToolSetRef        string                `json:"toolSetRef,omitempty" yaml:"toolSetRef,omitempty"`
	ModelRouteRef     string                `json:"modelRouteRef,omitempty" yaml:"modelRouteRef,omitempty"`
	SandboxProfileRef string                `json:"sandboxProfileRef,omitempty" yaml:"sandboxProfileRef,omitempty"`
	Limits            RunLimits             `json:"limits,omitempty" yaml:"limits,omitempty"`
	Verification      VerificationSpec      `json:"verification,omitempty" yaml:"verification,omitempty"`
	Environment       []EnvironmentVariable `json:"environment,omitempty" yaml:"environment,omitempty"`
}

type RuntimeSpec struct {
	Harness string `json:"harness" yaml:"harness"`
	Image   string `json:"image" yaml:"image"`
}

type InstructionsSpec struct {
	Inline string `json:"inline,omitempty" yaml:"inline,omitempty"`
	File   string `json:"file,omitempty" yaml:"file,omitempty"`
}

type SkillSet struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         SkillSetSpec `json:"spec" yaml:"spec"`
}

func (r *SkillSet) Meta() *ResourceMeta { return &r.ResourceMeta }

type SkillSetSpec struct {
	Skills []SkillRef `json:"skills" yaml:"skills"`
}

type SkillRef struct {
	Name   string `json:"name,omitempty" yaml:"name,omitempty"`
	Ref    string `json:"ref" yaml:"ref"`
	Digest string `json:"digest" yaml:"digest"`
}

type ToolSet struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         ToolSetSpec `json:"spec" yaml:"spec"`
}

func (r *ToolSet) Meta() *ResourceMeta { return &r.ResourceMeta }

type ToolSetSpec struct {
	Servers []MCPServer `json:"servers" yaml:"servers"`
}

type MCPServer struct {
	Name        string      `json:"name" yaml:"name"`
	Ref         string      `json:"ref" yaml:"ref"`
	Image       string      `json:"image,omitempty" yaml:"image,omitempty"`
	Credentials string      `json:"credentialsRef,omitempty" yaml:"credentialsRef,omitempty"`
	Tools       []ToolGrant `json:"tools" yaml:"tools"`
}

type ToolGrant struct {
	Name        string         `json:"name" yaml:"name"`
	Resources   []string       `json:"resources,omitempty" yaml:"resources,omitempty"`
	Effect      string         `json:"effect,omitempty" yaml:"effect,omitempty"`
	Approval    string         `json:"approval,omitempty" yaml:"approval,omitempty"`
	Arguments   *JSONArguments `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty" yaml:"inputSchema,omitempty"`
}

type SandboxProfile struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         SandboxProfileSpec `json:"spec" yaml:"spec"`
}

func (r *SandboxProfile) Meta() *ResourceMeta { return &r.ResourceMeta }

type SandboxProfileSpec struct {
	Backend    string         `json:"backend" yaml:"backend"`
	Image      string         `json:"image" yaml:"image"`
	Resources  ResourceLimits `json:"resources" yaml:"resources"`
	Filesystem FilesystemSpec `json:"filesystem" yaml:"filesystem"`
	Network    NetworkSpec    `json:"network" yaml:"network"`
}

type ResourceLimits struct {
	CPU    string `json:"cpu" yaml:"cpu"`
	Memory string `json:"memory" yaml:"memory"`
	Disk   string `json:"disk" yaml:"disk"`
	PIDs   int64  `json:"pids" yaml:"pids"`
}

type FilesystemSpec struct {
	Root      string `json:"root" yaml:"root"`
	Workspace string `json:"workspace" yaml:"workspace"`
}

type NetworkSpec struct {
	Mode           string   `json:"mode" yaml:"mode"`
	DirectInternet bool     `json:"directInternet" yaml:"directInternet"`
	Routes         []string `json:"routes,omitempty" yaml:"routes,omitempty"`
	AllowedHosts   []string `json:"allowedHosts,omitempty" yaml:"allowedHosts,omitempty"`
}

type ModelRoute struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         ModelRouteSpec `json:"spec" yaml:"spec"`
}

func (r *ModelRoute) Meta() *ResourceMeta { return &r.ResourceMeta }

type ModelRouteSpec struct {
	Providers []ModelProvider `json:"providers" yaml:"providers"`
	Budget    BudgetSpec      `json:"budget,omitempty" yaml:"budget,omitempty"`
}

type ModelProvider struct {
	Name       string `json:"name" yaml:"name"`
	Kind       string `json:"kind" yaml:"kind"`
	Model      string `json:"model" yaml:"model"`
	Credential string `json:"credentialRef,omitempty" yaml:"credentialRef,omitempty"`
	Priority   int    `json:"priority,omitempty" yaml:"priority,omitempty"`
	Fallback   bool   `json:"fallback,omitempty" yaml:"fallback,omitempty"`
}

type Workflow struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         WorkflowSpec `json:"spec" yaml:"spec"`
}

func (r *Workflow) Meta() *ResourceMeta { return &r.ResourceMeta }

type WorkflowSpec struct {
	Steps []WorkflowStep `json:"steps" yaml:"steps"`
}

type WorkflowStep struct {
	ID           string         `json:"id" yaml:"id"`
	Agent        string         `json:"agent,omitempty" yaml:"agent,omitempty"`
	Needs        []string       `json:"needs,omitempty" yaml:"needs,omitempty"`
	Approval     *ApprovalStep  `json:"approval,omitempty" yaml:"approval,omitempty"`
	Input        map[string]any `json:"input,omitempty" yaml:"input,omitempty"`
	OutputSchema map[string]any `json:"outputSchema,omitempty" yaml:"outputSchema,omitempty"`
	Timeout      string         `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Retries      int            `json:"retries,omitempty" yaml:"retries,omitempty"`
}

type ApprovalStep struct {
	Reason string `json:"reason" yaml:"reason"`
	Role   string `json:"role,omitempty" yaml:"role,omitempty"`
}

type ResourceQuota struct {
	ConcurrentRuns int64   `json:"concurrentRuns,omitempty" yaml:"concurrentRuns,omitempty"`
	CPU            string  `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory         string  `json:"memory,omitempty" yaml:"memory,omitempty"`
	Storage        string  `json:"storage,omitempty" yaml:"storage,omitempty"`
	ModelSpendUSD  float64 `json:"modelSpendUsd,omitempty" yaml:"modelSpendUsd,omitempty"`
}

type RetentionSpec struct {
	RunsDays     int `json:"runsDays,omitempty" yaml:"runsDays,omitempty"`
	AuditDays    int `json:"auditDays,omitempty" yaml:"auditDays,omitempty"`
	ArtifactDays int `json:"artifactDays,omitempty" yaml:"artifactDays,omitempty"`
}

type RunLimits struct {
	Timeout      string  `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxToolCalls int64   `json:"maxToolCalls,omitempty" yaml:"maxToolCalls,omitempty"`
	MaxCostUSD   float64 `json:"maxCostUsd,omitempty" yaml:"maxCostUsd,omitempty"`
}

type VerificationSpec struct {
	Commands []string `json:"commands,omitempty" yaml:"commands,omitempty"`
}

type EnvironmentVariable struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
	Ref   string `json:"valueFrom,omitempty" yaml:"valueFrom,omitempty"`
}

type BudgetSpec struct {
	MaxCostUSD float64 `json:"maxCostUsd,omitempty" yaml:"maxCostUsd,omitempty"`
	Cooldown   string  `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`
}

type AgentRun struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         RunSpec   `json:"spec" yaml:"spec"`
	Status       RunStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *AgentRun) Meta() *ResourceMeta { return &r.ResourceMeta }

type WorkflowRun struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         WorkflowRunSpec `json:"spec" yaml:"spec"`
	Status       RunStatus       `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *WorkflowRun) Meta() *ResourceMeta { return &r.ResourceMeta }

type RunSpec struct {
	AgentRef string         `json:"agentRef" yaml:"agentRef"`
	Input    map[string]any `json:"input,omitempty" yaml:"input,omitempty"`
	Owner    string         `json:"owner,omitempty" yaml:"owner,omitempty"`
}

type WorkflowRunSpec struct {
	WorkflowRef string         `json:"workflowRef" yaml:"workflowRef"`
	Input       map[string]any `json:"input,omitempty" yaml:"input,omitempty"`
	Owner       string         `json:"owner,omitempty" yaml:"owner,omitempty"`
}

type RunStatus struct {
	Phase      string      `json:"phase" yaml:"phase"`
	Reason     string      `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message    string      `json:"message,omitempty" yaml:"message,omitempty"`
	StartedAt  string      `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	FinishedAt string      `json:"finishedAt,omitempty" yaml:"finishedAt,omitempty"`
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

type Condition struct {
	Type    string `json:"type" yaml:"type"`
	Status  string `json:"status" yaml:"status"`
	Reason  string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
}

type Approval struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         ApprovalSpec   `json:"spec" yaml:"spec"`
	Status       ApprovalStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *Approval) Meta() *ResourceMeta { return &r.ResourceMeta }

type ApprovalSpec struct {
	RunRef string `json:"runRef" yaml:"runRef"`
	Reason string `json:"reason" yaml:"reason"`
	Role   string `json:"role,omitempty" yaml:"role,omitempty"`
}

type ApprovalStatus struct {
	Decision string `json:"decision" yaml:"decision"`
	Actor    string `json:"actor,omitempty" yaml:"actor,omitempty"`
	At       string `json:"at,omitempty" yaml:"at,omitempty"`
}

type Artifact struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         ArtifactSpec `json:"spec" yaml:"spec"`
}

func (r *Artifact) Meta() *ResourceMeta { return &r.ResourceMeta }

type ArtifactSpec struct {
	RunRef    string `json:"runRef" yaml:"runRef"`
	URI       string `json:"uri" yaml:"uri"`
	Digest    string `json:"digest" yaml:"digest"`
	MediaType string `json:"mediaType,omitempty" yaml:"mediaType,omitempty"`
}

type Runner struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         RunnerSpec   `json:"spec" yaml:"spec"`
	Status       RunnerStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *Runner) Meta() *ResourceMeta { return &r.ResourceMeta }

type RunnerSpec struct {
	Endpoint string            `json:"endpoint" yaml:"endpoint"`
	Backends []string          `json:"backends" yaml:"backends"`
	Labels   map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

type RunnerStatus struct {
	Ready         bool   `json:"ready" yaml:"ready"`
	Isolation     string `json:"isolation" yaml:"isolation"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty" yaml:"lastHeartbeat,omitempty"`
}

type Credential struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         CredentialSpec `json:"spec" yaml:"spec"`
}

func (r *Credential) Meta() *ResourceMeta { return &r.ResourceMeta }

type CredentialSpec struct {
	Kind      string `json:"kind" yaml:"kind"`
	Owner     string `json:"owner,omitempty" yaml:"owner,omitempty"`
	SecretRef string `json:"secretRef" yaml:"secretRef"`
}

type Entitlement struct {
	ResourceMeta `json:",inline" yaml:",inline"`
	Spec         EntitlementSpec   `json:"spec" yaml:"spec"`
	Status       EntitlementStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *Entitlement) Meta() *ResourceMeta { return &r.ResourceMeta }

type EntitlementSpec struct {
	Owner    string `json:"owner" yaml:"owner"`
	Provider string `json:"provider" yaml:"provider"`
	Kind     string `json:"kind" yaml:"kind"`
}

type EntitlementStatus struct {
	Available     bool   `json:"available" yaml:"available"`
	CooldownUntil string `json:"cooldownUntil,omitempty" yaml:"cooldownUntil,omitempty"`
	Message       string `json:"message,omitempty" yaml:"message,omitempty"`
}
