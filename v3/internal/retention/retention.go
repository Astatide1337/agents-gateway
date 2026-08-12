// Package retention contains the bounded retention policy, inventory, and
// applier used by the v3 operator.
//
// Policy planning remains side-effect free. The live adapter applies only a
// complete plan with Kubernetes UID preconditions and object-store ETags;
// keeping policy and application separate makes the default dry-run behavior
// testable and keeps an inventory or ownership mistake from becoming an
// unbounded delete.
package retention

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/fsm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	DefaultArtifactRetentionDays = 14
	DefaultWorktreeRetentionDays = 7
	DefaultLedgerRetentionDays   = 30
	DefaultMaxActionsPerPlan     = 256
	MaxRetentionActions          = 100000
	MaxInventoryItems            = 100000
	MaxRetentionDays             = 3650

	RunUIDLabelKey          = "agents.astatide.com/run-uid"
	RoleLabelKey            = "agents.astatide.com/role"
	ManagedByLabelKey       = "agents.astatide.com/managed-by"
	RunSecretManagedBy      = "agw-run-secret"
	WorkRole                = "work"
	AgentRunAPIVersion      = "agents.astatide.com/v1alpha1"
	AgentRunKind            = "AgentRun"
	SandboxAPIVersion       = "agents.x-k8s.io/v1beta1"
	SandboxKind             = "Sandbox"
	JobAPIVersion           = "batch/v1"
	JobKind                 = "Job"
	CoreAPIVersion          = "v1"
	SecretKind              = "Secret"
	PersistentVolumeKind    = "PersistentVolumeClaim"
	ObjectStoreKind         = "ObjectStore"
	SpecDigestAnnotation    = "agents.astatide.com/spec-digest"
	LedgerClaimsSegment     = "claims"
	LedgerOutcomesSegment   = "outcomes"
	LedgerTombstonesSegment = effects.TombstonesSegment
)

var (
	ErrInvalidPolicy        = errors.New("retention: invalid policy")
	ErrInvalidNow           = errors.New("retention: planning time is required")
	ErrInvalidRun           = errors.New("retention: invalid AgentRun identity")
	ErrActionLimit          = errors.New("retention: action limit exceeded")
	ErrAmbiguousOwnership   = errors.New("retention: ownership is ambiguous")
	ErrMissingOwnership     = errors.New("retention: controller ownership is missing")
	ErrInvalidObjectKey     = errors.New("retention: invalid object key")
	ErrObjectPrefixMismatch = errors.New("retention: object key is outside the run prefix")
	ErrLedgerInventory      = errors.New("retention: effect-ledger inventory is ambiguous")
)

// SkipReason is deliberately stable: it is suitable for a controller event
// or a bounded metrics label and does not include object content.
type SkipReason string

const (
	SkipInvalidRun             SkipReason = "invalid-run"
	SkipDuplicateRun           SkipReason = "duplicate-run-uid"
	SkipRunNotFound            SkipReason = "run-not-found"
	SkipActiveRun              SkipReason = "active-run"
	SkipUnknownEffect          SkipReason = "unknown-effect"
	SkipMissingCompletion      SkipReason = "missing-completion-time"
	SkipFutureCompletion       SkipReason = "completion-time-in-future"
	SkipRetentionWindow        SkipReason = "retention-window"
	SkipDeletingResource       SkipReason = "resource-already-deleting"
	SkipUnsupportedResource    SkipReason = "unsupported-resource-kind"
	SkipMissingRunLabel        SkipReason = "missing-run-label"
	SkipRunLabelMismatch       SkipReason = "run-label-mismatch"
	SkipRoleLabelMismatch      SkipReason = "role-label-mismatch"
	SkipManagedByMismatch      SkipReason = "managed-by-label-mismatch"
	SkipOwnerMismatch          SkipReason = "owner-mismatch"
	SkipOwnerAmbiguous         SkipReason = "ambiguous-owner"
	SkipWorkSandboxMissing     SkipReason = "work-sandbox-reference-missing"
	SkipArtifactRunMismatch    SkipReason = "artifact-run-mismatch"
	SkipArtifactKeyMismatch    SkipReason = "artifact-prefix-mismatch"
	SkipArtifactKeyInvalid     SkipReason = "artifact-key-invalid"
	SkipArtifactCreatedMissing SkipReason = "artifact-created-at-missing"
	SkipArtifactCreatedFuture  SkipReason = "artifact-created-at-in-future"
	SkipArtifactYoung          SkipReason = "artifact-retention-window"
	SkipDuplicateResource      SkipReason = "duplicate-resource"
	SkipDuplicateArtifact      SkipReason = "duplicate-artifact-key"
	SkipInvalidResource        SkipReason = "invalid-resource"
	SkipSpecDigestMissing      SkipReason = "missing-spec-digest"
	SkipSpecDigestMismatch     SkipReason = "spec-digest-mismatch"
	SkipEvidenceNotFlushed     SkipReason = "evidence-not-flushed"
	SkipEvidenceMissing        SkipReason = "evidence-object-missing"
	SkipLedgerNotFlushed       SkipReason = "ledger-not-flushed"
	SkipArtifactETagMissing    SkipReason = "artifact-etag-missing"
	SkipLedgerIncomplete       SkipReason = "ledger-incomplete"
	SkipLedgerUnknown          SkipReason = "ledger-unknown"
	SkipLedgerRunMismatch      SkipReason = "ledger-run-mismatch"
	SkipLedgerIdentityMismatch SkipReason = "ledger-identity-mismatch"
	SkipLedgerYoung            SkipReason = "ledger-retention-window"
	SkipLedgerArtifacts        SkipReason = "ledger-artifacts-retained"
	SkipLedgerWorkspace        SkipReason = "ledger-workspace-retained"
	SkipLedgerETagMissing      SkipReason = "ledger-etag-missing"
	SkipLedgerTombstone        SkipReason = "ledger-tombstone-mismatch"
)

// Policy carries the two retention windows from ADR-013. DryRun is true in
// DefaultPolicy and must be explicitly changed by the operator configuration.
// Planning remains side-effect free even when DryRun is false; the live
// applier is separately gated by enforcement and lifecycle attestation.
type Policy struct {
	ArtifactRetentionDays int
	WorktreeRetentionDays int
	LedgerRetentionDays   int
	ObjectPrefix          string
	LedgerPrefix          string
	MaxActionsPerPlan     int
	DryRun                bool
}

// DefaultPolicy is intentionally conservative and mirrors the v2 retention
// defaults: 14 days for immutable artifacts, 7 days for worktrees, and a
// dry-run first pass.
func DefaultPolicy() Policy {
	return Policy{
		ArtifactRetentionDays: DefaultArtifactRetentionDays,
		WorktreeRetentionDays: DefaultWorktreeRetentionDays,
		LedgerRetentionDays:   DefaultLedgerRetentionDays,
		ObjectPrefix:          "agents-gateway/v3",
		LedgerPrefix:          "publish-effects",
		MaxActionsPerPlan:     DefaultMaxActionsPerPlan,
		DryRun:                true,
	}
}

func (p Policy) normalized() (Policy, error) {
	if p.ArtifactRetentionDays <= 0 || p.ArtifactRetentionDays > MaxRetentionDays ||
		p.WorktreeRetentionDays <= 0 || p.WorktreeRetentionDays > MaxRetentionDays {
		return Policy{}, fmt.Errorf("%w: retention days must be in [1,%d]", ErrInvalidPolicy, MaxRetentionDays)
	}
	if p.LedgerRetentionDays < DefaultLedgerRetentionDays || p.LedgerRetentionDays > MaxRetentionDays {
		return Policy{}, fmt.Errorf("%w: ledger retention days must be in [%d,%d]", ErrInvalidPolicy, DefaultLedgerRetentionDays, MaxRetentionDays)
	}
	if p.MaxActionsPerPlan == 0 {
		p.MaxActionsPerPlan = DefaultMaxActionsPerPlan
	}
	if p.MaxActionsPerPlan < 1 || p.MaxActionsPerPlan > MaxRetentionActions {
		return Policy{}, fmt.Errorf("%w: MaxActionsPerPlan must be in [1,%d]", ErrInvalidPolicy, MaxRetentionActions)
	}
	if err := validateObjectKey(p.ObjectPrefix, true); err != nil {
		return Policy{}, fmt.Errorf("%w: object prefix: %v", ErrInvalidPolicy, err)
	}
	if err := validateObjectKey(p.LedgerPrefix, false); err != nil {
		return Policy{}, fmt.Errorf("%w: ledger prefix: %v", ErrInvalidPolicy, err)
	}
	return p, nil
}

// Child is a status-bound child identity. UID fencing is mandatory for PVC
// cleanup because agent-sandbox owns PVCs through the intermediate Sandbox.
type Child struct {
	Kind       string
	Name       string
	UID        string
	Role       string
	SpecDigest string
}

// Run is the bounded input needed by the planner. It can be obtained from an
// AgentRun with FromAgentRun; keeping this value small makes inventory tests
// independent of Kubernetes clients.
type Run struct {
	Namespace   string
	Name        string
	UID         string
	SpecDigest  string
	BaseSHA     string
	PatchDigest string
	Phase       v1alpha1.Phase
	CompletedAt time.Time
	Children    []Child
	// EvidenceFlushed is true only when the terminal event stream reference is
	// present and can be checked against the complete object inventory.
	EvidenceFlushed bool
	// RequiredObjectKeys contains immutable status references that must be
	// present before any evidence or workspace action is admissible.
	RequiredObjectKeys []string
	// LedgerFlushed is false for pending/unknown effect state. Settled effects
	// additionally require their claim and outcome objects in the inventory.
	LedgerFlushed bool
	EffectKey     string
}

// FromAgentRun copies only identity, terminal status, completion time, and
// child references. It never retains a pointer to the API object.
func FromAgentRun(run *v1alpha1.AgentRun) (Run, error) {
	if run == nil {
		return Run{}, ErrInvalidRun
	}
	result := Run{
		Namespace:     run.Namespace,
		Name:          run.Name,
		UID:           string(run.UID),
		SpecDigest:    run.Status.SpecDigest,
		BaseSHA:       run.Status.BaseSHA,
		Phase:         run.Status.Phase,
		LedgerFlushed: true,
	}
	if run.Status.CompletedAt != nil {
		result.CompletedAt = run.Status.CompletedAt.Time
	}
	for _, ref := range []*v1alpha1.ChildRef{run.Status.WorkSandboxRef, run.Status.VerifySandboxRef} {
		if ref == nil {
			continue
		}
		result.Children = append(result.Children, Child{Kind: ref.Kind, Name: ref.Name, UID: ref.UID, Role: ref.Role, SpecDigest: ref.SpecDigest})
	}
	for _, ref := range run.Status.Children {
		result.Children = append(result.Children, Child{Kind: ref.Kind, Name: ref.Name, UID: ref.UID, Role: ref.Role, SpecDigest: ref.SpecDigest})
	}
	for _, ref := range artifactRefs(run) {
		if ref == nil {
			continue
		}
		result.RequiredObjectKeys = append(result.RequiredObjectKeys, objectKeyFromURI(ref.URI))
	}
	if run.Status.EventStreamRef != nil {
		result.EvidenceFlushed = true
	} else {
		result.EvidenceFlushed = false
	}
	if run.Status.Effect != nil {
		result.EffectKey = run.Status.Effect.Key
		switch run.Status.Effect.State {
		case v1alpha1.EffectSucceeded, v1alpha1.EffectFailed:
			result.LedgerFlushed = canonical.ValidDigest(run.Status.Effect.Key)
		default:
			result.LedgerFlushed = false
		}
	}
	if run.Status.Patch != nil && run.Status.Patch.Ref != nil {
		result.PatchDigest = run.Status.Patch.Ref.Digest
	}
	if err := validateRunIdentity(result); err != nil {
		return Run{}, err
	}
	result.RequiredObjectKeys = uniqueStrings(result.RequiredObjectKeys)
	return result, nil
}

func artifactRefs(run *v1alpha1.AgentRun) []*v1alpha1.ArtifactRef {
	refs := make([]*v1alpha1.ArtifactRef, 0, 8)
	refs = append(refs, run.Status.ResolvedSpecRef, run.Status.ContextPackRef, run.Status.EventStreamRef)
	if run.Status.Patch != nil {
		refs = append(refs, run.Status.Patch.Ref, run.Status.Patch.ManifestRef)
	}
	if run.Status.Gate != nil {
		refs = append(refs, run.Status.Gate.ReportRef)
	}
	for index := range run.Status.Artifacts {
		refs = append(refs, &run.Status.Artifacts[index])
	}
	return refs
}

// objectKeyFromURI accepts only the URI emitted by the object-store adapter.
// Unknown URI forms become an empty sentinel and therefore fail closed during
// evidence readiness checks.
func objectKeyFromURI(value string) string {
	if !strings.HasPrefix(value, "s3://") || strings.ContainsAny(value, "\x00\r\n?#@") {
		return ""
	}
	rest := strings.TrimPrefix(value, "s3://")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 {
		return ""
	}
	key := rest[slash+1:]
	if validateObjectKey(key, false) != nil {
		return ""
	}
	return key
}

// OwnerReference is intentionally the Kubernetes shape so an adapter can copy
// ObjectMeta.OwnerReferences without introducing a client dependency here.
type OwnerReference = metav1.OwnerReference

// Resource is the minimal metadata inventory required for terminal Secret and
// worktree PVC cleanup. A resource is never selected from a label alone.
type Resource struct {
	Kind            string
	Namespace       string
	Name            string
	UID             string
	SpecDigest      string
	Labels          map[string]string
	Annotations     map[string]string
	OwnerReferences []OwnerReference
	Deleting        bool
}

// Artifact is one immutable object-store entry. Key is the raw bucket key,
// not an s3:// URI; credentials and URLs therefore cannot enter a plan.
type Artifact struct {
	RunUID    string
	Key       string
	ETag      string
	Digest    string
	Kind      string
	CreatedAt time.Time
	SizeBytes int64
}

// LedgerObject is a bounded, body-backed object-store record. The body is
// authoritative for identity and age; Key, ETag, and size metadata are only
// deletion fences and inventory information.
type LedgerObject struct {
	Key       string
	ETag      string
	Body      []byte
	SizeBytes int64
}

type Inventory struct {
	Runs      []Run
	Resources []Resource
	Artifacts []Artifact
	Ledger    []LedgerObject
}

type ActionMode string

const (
	ActionWouldDelete ActionMode = "would-delete"
	ActionDelete      ActionMode = "delete"
)

type ActionTarget struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        string
	ObjectKey  string
	ETag       string
}

type Action struct {
	Mode    ActionMode
	RunUID  string
	Role    string
	PairKey string
	Reason  string
	Target  ActionTarget
}

type Skip struct {
	RunUID    string
	Kind      string
	Namespace string
	Name      string
	UID       string
	ObjectKey string
	Reason    SkipReason
}

type Plan struct {
	DryRun         bool
	ArtifactCutoff time.Time
	WorktreeCutoff time.Time
	LedgerCutoff   time.Time
	Actions        []Action
	LedgerPairs    []LedgerPairPlan
	Skipped        []Skip
}

// Plan constructs a deterministic, bounded deletion plan. All per-object
// uncertainty becomes a Skip entry, never an action. Global policy and clock
// errors return an empty plan and an error.
func (p Policy) Plan(now time.Time, inventory Inventory) (Plan, error) {
	normalized, err := p.normalized()
	if err != nil {
		return Plan{}, err
	}
	if now.IsZero() {
		return Plan{}, ErrInvalidNow
	}
	now = now.UTC()
	plan := Plan{
		DryRun:         normalized.DryRun,
		ArtifactCutoff: now.Add(-days(normalized.ArtifactRetentionDays)),
		WorktreeCutoff: now.Add(-days(normalized.WorktreeRetentionDays)),
		LedgerCutoff:   now.Add(-days(normalized.LedgerRetentionDays)),
	}

	runs, duplicateRuns := indexRuns(inventory.Runs, &plan)
	resourceDuplicates := duplicateResourceKeys(inventory.Resources)
	artifactDuplicates := duplicateArtifactKeys(inventory.Artifacts)
	objectKeys := inventoryKeySet(inventory.Artifacts, inventory.Ledger)
	ledgerIndex, err := indexLedger(normalized, inventory.Ledger)
	if err != nil {
		return Plan{}, err
	}

	for _, resource := range inventory.Resources {
		runUID := resource.Labels[RunUIDLabelKey]
		if runUID == "" {
			plan.skipResource(resource, "", SkipMissingRunLabel)
			continue
		}
		run, ok := runs[runUID]
		if !ok {
			plan.skipResource(resource, runUID, SkipRunNotFound)
			continue
		}
		if duplicateRuns[runUID] {
			plan.skipResource(resource, runUID, SkipDuplicateRun)
			continue
		}
		if resourceDuplicates[resourceKey(resource)] {
			plan.skipResource(resource, runUID, SkipDuplicateResource)
			continue
		}
		if resource.Deleting {
			plan.skipResource(resource, runUID, SkipDeletingResource)
			continue
		}

		switch resource.Kind {
		case SecretKind:
			plan.secret(resource, run, now)
		case PersistentVolumeKind:
			plan.workspacePVC(resource, run, now, normalized.ObjectPrefix, normalized.LedgerPrefix, objectKeys)
		default:
			plan.skipResource(resource, runUID, SkipUnsupportedResource)
		}
	}

	for _, artifact := range inventory.Artifacts {
		runUID := artifact.RunUID
		if runUID == "" {
			plan.skipArtifact(artifact, SkipArtifactRunMismatch)
			continue
		}
		run, ok := runs[runUID]
		if !ok {
			plan.skipArtifact(artifact, SkipRunNotFound)
			continue
		}
		if duplicateRuns[runUID] {
			plan.skipArtifact(artifact, SkipDuplicateRun)
			continue
		}
		if artifactDuplicates[artifact.Key] {
			plan.skipArtifact(artifact, SkipDuplicateArtifact)
			continue
		}
		plan.artifact(artifact, run, normalized.ObjectPrefix, normalized.LedgerPrefix, objectKeys, now)
	}

	for _, pair := range ledgerIndex.Pairs {
		if pair.Claim == nil {
			plan.skipLedger(pair, SkipLedgerIncomplete)
			continue
		}
		run, ok := runs[pair.Claim.Value.RunUID]
		if !ok {
			plan.skipLedger(pair, SkipRunNotFound)
			continue
		}
		if duplicateRuns[pair.Claim.Value.RunUID] {
			plan.skipLedger(pair, SkipDuplicateRun)
			continue
		}
		plan.ledgerPair(pair, run, normalized, inventory, now)
	}

	sort.Slice(plan.Actions, func(i, j int) bool { return actionLess(plan.Actions[i], plan.Actions[j]) })
	sort.Slice(plan.Skipped, func(i, j int) bool { return skipLess(plan.Skipped[i], plan.Skipped[j]) })
	if len(plan.Actions) > normalized.MaxActionsPerPlan {
		return Plan{}, fmt.Errorf("%w: %d actions exceed %d", ErrActionLimit, len(plan.Actions), normalized.MaxActionsPerPlan)
	}
	return plan, nil
}

func (p *Plan) secret(resource Resource, run Run, now time.Time) {
	if ok, reason := terminalEligible(run, now, 0); !ok {
		p.skipResource(resource, run.UID, reason)
		return
	}
	if resource.Namespace != run.Namespace || resource.Labels[RunUIDLabelKey] != run.UID {
		p.skipResource(resource, run.UID, SkipRunLabelMismatch)
		return
	}
	if resource.Labels[ManagedByLabelKey] != RunSecretManagedBy {
		p.skipResource(resource, run.UID, SkipManagedByMismatch)
		return
	}
	if err := requireSpecDigest(resource, run); err != nil {
		p.skipResource(resource, run.UID, specDigestSkipReason(err))
		return
	}
	if err := requireRunOwner(resource.OwnerReferences, run); err != nil {
		p.skipResource(resource, run.UID, ownerSkipReason(err))
		return
	}
	if err := validateResourceIdentity(resource); err != nil {
		p.skipResource(resource, run.UID, SkipInvalidResource)
		return
	}
	p.Actions = append(p.Actions, Action{
		Mode: p.actionMode(), RunUID: run.UID, Role: "credentials",
		Reason: "terminal per-run credentials are no longer needed",
		Target: ActionTarget{APIVersion: CoreAPIVersion, Kind: SecretKind, Namespace: resource.Namespace, Name: resource.Name, UID: resource.UID},
	})
}

func (p *Plan) workspacePVC(resource Resource, run Run, now time.Time, objectPrefix, ledgerPrefix string, objectKeys map[string]struct{}) {
	if ok, reason := terminalEligible(run, now, 0); !ok {
		p.skipResource(resource, run.UID, reason)
		return
	}
	if ok, reason := evidenceEligible(run, objectPrefix, ledgerPrefix, objectKeys); !ok {
		p.skipResource(resource, run.UID, reason)
		return
	}
	if run.CompletedAt.After(p.WorktreeCutoff) {
		p.skipResource(resource, run.UID, SkipRetentionWindow)
		return
	}
	if resource.Namespace != run.Namespace || resource.Labels[RunUIDLabelKey] != run.UID {
		p.skipResource(resource, run.UID, SkipRunLabelMismatch)
		return
	}
	if resource.Labels[RoleLabelKey] != WorkRole {
		p.skipResource(resource, run.UID, SkipRoleLabelMismatch)
		return
	}
	child, ok := workSandbox(run)
	if !ok {
		p.skipResource(resource, run.UID, SkipWorkSandboxMissing)
		return
	}
	if err := requireWorkspaceOwner(resource.OwnerReferences, child, run); err != nil {
		p.skipResource(resource, run.UID, ownerSkipReason(err))
		return
	}
	if err := requireSpecDigest(resource, run); err != nil {
		// PVCs created by the adopted Sandbox controller may not copy the
		// full digest annotation. The status-pinned Sandbox child digest is
		// accepted by requireSpecDigest below; an empty value still fails.
		p.skipResource(resource, run.UID, specDigestSkipReason(err))
		return
	}
	if err := validateResourceIdentity(resource); err != nil {
		p.skipResource(resource, run.UID, SkipInvalidResource)
		return
	}
	p.Actions = append(p.Actions, Action{
		Mode: p.actionMode(), RunUID: run.UID, Role: WorkRole,
		Reason: "terminal worktree PVC exceeded retention window",
		Target: ActionTarget{APIVersion: CoreAPIVersion, Kind: PersistentVolumeKind, Namespace: resource.Namespace, Name: resource.Name, UID: resource.UID},
	})
}

func (p *Plan) artifact(artifact Artifact, run Run, objectPrefix, ledgerPrefix string, objectKeys map[string]struct{}, now time.Time) {
	if ok, reason := terminalEligible(run, now, 0); !ok {
		p.skipArtifact(artifact, reason)
		return
	}
	if !run.EvidenceFlushed {
		p.skipArtifact(artifact, SkipEvidenceNotFlushed)
		return
	}
	if ok, reason := evidenceEligible(run, objectPrefix, ledgerPrefix, objectKeys); !ok {
		p.skipArtifact(artifact, reason)
		return
	}
	if artifact.CreatedAt.IsZero() {
		p.skipArtifact(artifact, SkipArtifactCreatedMissing)
		return
	}
	created := artifact.CreatedAt.UTC()
	if created.After(now) {
		p.skipArtifact(artifact, SkipArtifactCreatedFuture)
		return
	}
	if created.After(p.ArtifactCutoff) {
		p.skipArtifact(artifact, SkipArtifactYoung)
		return
	}
	if artifact.RunUID != run.UID {
		p.skipArtifact(artifact, SkipArtifactRunMismatch)
		return
	}
	if err := validateArtifactKey(objectPrefix, run.UID, artifact.Key); err != nil {
		if errors.Is(err, ErrInvalidObjectKey) {
			p.skipArtifact(artifact, SkipArtifactKeyInvalid)
		} else {
			p.skipArtifact(artifact, SkipArtifactKeyMismatch)
		}
		return
	}
	if artifact.SizeBytes < 0 {
		p.skipArtifact(artifact, SkipInvalidResource)
		return
	}
	if artifact.ETag == "" {
		p.skipArtifact(artifact, SkipArtifactETagMissing)
		return
	}
	p.Actions = append(p.Actions, Action{
		Mode: p.actionMode(), RunUID: run.UID, Reason: "terminal artifact exceeded retention window",
		Target: ActionTarget{Kind: ObjectStoreKind, ObjectKey: artifact.Key, ETag: artifact.ETag},
	})
}

func (p Plan) actionMode() ActionMode {
	if p.DryRun {
		return ActionWouldDelete
	}
	return ActionDelete
}

func indexRuns(input []Run, plan *Plan) (map[string]Run, map[string]bool) {
	runs := make(map[string]Run, len(input))
	duplicates := make(map[string]bool)
	for _, run := range input {
		if err := validateRunIdentity(run); err != nil {
			plan.Skipped = append(plan.Skipped, Skip{RunUID: run.UID, Kind: AgentRunKind, Namespace: run.Namespace, Name: run.Name, Reason: SkipInvalidRun})
			continue
		}
		if _, exists := runs[run.UID]; exists {
			duplicates[run.UID] = true
			continue
		}
		runs[run.UID] = run
	}
	return runs, duplicates
}

func validateRunIdentity(run Run) error {
	if len(validation.IsDNS1123Subdomain(run.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(run.Name)) != 0 || !validUID(run.UID) || !canonical.ValidDigest(run.SpecDigest) {
		return ErrInvalidRun
	}
	if run.Phase != "" && !fsm.IsKnownPhase(run.Phase) {
		return ErrInvalidRun
	}
	return nil
}

func validateResourceIdentity(resource Resource) error {
	if resource.Namespace == "" || len(validation.IsDNS1123Subdomain(resource.Namespace)) != 0 || resource.Name == "" || len(validation.IsDNS1123Subdomain(resource.Name)) != 0 || !validUID(resource.UID) {
		return ErrInvalidRun
	}
	return nil
}

func validUID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func terminalEligible(run Run, now time.Time, retention time.Duration) (bool, SkipReason) {
	if !fsm.IsTerminal(run.Phase) {
		return false, SkipActiveRun
	}
	if run.Phase == v1alpha1.PhaseUnknownEffect {
		return false, SkipUnknownEffect
	}
	if run.CompletedAt.IsZero() {
		return false, SkipMissingCompletion
	}
	completed := run.CompletedAt.UTC()
	if completed.After(now) {
		return false, SkipFutureCompletion
	}
	if completed.Add(retention).After(now) {
		return false, SkipRetentionWindow
	}
	return true, ""
}

func requireSpecDigest(resource Resource, run Run) error {
	if run.SpecDigest == "" {
		return fmt.Errorf("%w: run digest is empty", ErrInvalidRun)
	}
	if resource.SpecDigest == "" {
		if resource.Annotations != nil {
			resource.SpecDigest = resource.Annotations[SpecDigestAnnotation]
		}
	}
	if resource.SpecDigest == "" {
		return errors.New("missing spec digest")
	}
	if resource.SpecDigest != run.SpecDigest {
		return errors.New("spec digest mismatch")
	}
	return nil
}

func specDigestSkipReason(err error) SkipReason {
	if strings.Contains(err.Error(), "missing") {
		return SkipSpecDigestMissing
	}
	return SkipSpecDigestMismatch
}

// evidenceEligible is shared by workspace and object cleanup. It checks the
// controller's durable status references against the same complete object
// inventory used for deletion; a missing reference is never interpreted as a
// successful flush.
func evidenceEligible(run Run, objectPrefix, ledgerPrefix string, objectKeys map[string]struct{}) (bool, SkipReason) {
	if !run.EvidenceFlushed {
		return false, SkipEvidenceNotFlushed
	}
	if !run.LedgerFlushed {
		return false, SkipLedgerNotFlushed
	}
	for _, key := range run.RequiredObjectKeys {
		if key == "" {
			return false, SkipEvidenceNotFlushed
		}
		if err := validateArtifactKey(objectPrefix, run.UID, key); err != nil {
			return false, SkipArtifactKeyMismatch
		}
		if _, ok := objectKeys[key]; !ok {
			return false, SkipEvidenceMissing
		}
	}
	if run.EffectKey != "" {
		if !canonical.ValidDigest(run.EffectKey) {
			return false, SkipLedgerNotFlushed
		}
		hex := strings.TrimPrefix(run.EffectKey, canonical.DigestPrefix)
		for _, segment := range []string{LedgerClaimsSegment, LedgerOutcomesSegment} {
			key := objectPrefix + "/" + ledgerPrefix + "/" + segment + "/" + hex + ".json"
			if _, ok := objectKeys[key]; !ok {
				return false, SkipLedgerNotFlushed
			}
		}
	}
	return true, ""
}

// evidenceReferencesBound is the post-artifact-retention half of the evidence
// contract. Once immutable evidence has been retired, its status references
// are expected to be absent from the live object inventory; the references
// must still be run-scoped so a shared or foreign object can never masquerade
// as this run's flushed evidence.
func evidenceReferencesBound(run Run, objectPrefix string) (bool, SkipReason) {
	if !run.EvidenceFlushed {
		return false, SkipEvidenceNotFlushed
	}
	for _, key := range run.RequiredObjectKeys {
		if key == "" {
			return false, SkipEvidenceNotFlushed
		}
		if err := validateArtifactKey(objectPrefix, run.UID, key); err != nil {
			return false, SkipArtifactKeyMismatch
		}
	}
	return true, ""
}

func inventoryKeySet(artifacts []Artifact, ledger []LedgerObject) map[string]struct{} {
	keys := make(map[string]struct{}, len(artifacts)+len(ledger))
	for _, artifact := range artifacts {
		if artifact.Key != "" {
			keys[artifact.Key] = struct{}{}
		}
	}
	for _, object := range ledger {
		if object.Key != "" {
			keys[object.Key] = struct{}{}
		}
	}
	return keys
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func workSandbox(run Run) (Child, bool) {
	var result Child
	found := false
	for _, child := range run.Children {
		if child.Role != WorkRole {
			continue
		}
		if found || (child.Kind != SandboxKind && child.Kind != JobKind) || child.Name == "" || !validUID(child.UID) {
			return Child{}, false
		}
		result = child
		found = true
	}
	return result, found
}

func requireRunOwner(owners []OwnerReference, run Run) error {
	owner, err := controllerOwner(owners)
	if err != nil {
		return err
	}
	if owner.APIVersion != AgentRunAPIVersion || owner.Kind != AgentRunKind || owner.Name != run.Name || string(owner.UID) != run.UID || !boolValue(owner.BlockOwnerDeletion) {
		return ErrMissingOwnership
	}
	return nil
}

func requireWorkspaceOwner(owners []OwnerReference, child Child, run Run) error {
	if child.Kind == JobKind {
		// JobBackend PVCs are intentionally owned by the AgentRun (not the
		// Job), so their owner reference remains valid after the Job is reaped.
		return requireRunOwner(owners, run)
	}
	if child.Kind != SandboxKind {
		return ErrMissingOwnership
	}
	owner, err := controllerOwner(owners)
	if err != nil {
		return err
	}
	if owner.APIVersion != SandboxAPIVersion || owner.Kind != SandboxKind || owner.Name != child.Name || string(owner.UID) != child.UID || !boolValue(owner.BlockOwnerDeletion) {
		return ErrMissingOwnership
	}
	return nil
}

func controllerOwner(owners []OwnerReference) (OwnerReference, error) {
	var result OwnerReference
	found := false
	for _, owner := range owners {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		if found {
			return OwnerReference{}, ErrAmbiguousOwnership
		}
		result = owner
		found = true
	}
	if !found {
		return OwnerReference{}, ErrMissingOwnership
	}
	return result, nil
}

func boolValue(value *bool) bool { return value != nil && *value }

func ownerSkipReason(err error) SkipReason {
	if errors.Is(err, ErrAmbiguousOwnership) {
		return SkipOwnerAmbiguous
	}
	return SkipOwnerMismatch
}

func validateArtifactKey(prefix, runUID, key string) error {
	if err := validateObjectKey(key, false); err != nil {
		return err
	}
	want := runPrefix(prefix, runUID)
	if !strings.HasPrefix(key, want) || strings.TrimPrefix(key, want) == "" {
		return fmt.Errorf("%w: key is outside the run prefix", ErrObjectPrefixMismatch)
	}
	return nil
}

func runPrefix(prefix, runUID string) string {
	if prefix == "" {
		return "runs/" + runUID + "/"
	}
	return prefix + "/runs/" + runUID + "/"
}

func validateObjectKey(key string, allowEmpty bool) error {
	if key == "" && allowEmpty {
		return nil
	}
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.Contains(key, "//") || path.Clean(key) != key {
		return ErrInvalidObjectKey
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ErrInvalidObjectKey
		}
		for _, c := range segment {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
				continue
			}
			return ErrInvalidObjectKey
		}
	}
	return nil
}

func days(value int) time.Duration { return time.Duration(value) * 24 * time.Hour }

func (p *Plan) skipResource(resource Resource, runUID string, reason SkipReason) {
	p.Skipped = append(p.Skipped, Skip{RunUID: runUID, Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name, UID: resource.UID, Reason: reason})
}

func (p *Plan) skipArtifact(artifact Artifact, reason SkipReason) {
	p.Skipped = append(p.Skipped, Skip{RunUID: artifact.RunUID, ObjectKey: artifact.Key, Reason: reason})
}

func resourceKey(resource Resource) string {
	return resource.Kind + "\x00" + resource.Namespace + "\x00" + resource.Name
}

func duplicateResourceKeys(resources []Resource) map[string]bool {
	counts := make(map[string]int, len(resources))
	for _, resource := range resources {
		counts[resourceKey(resource)]++
	}
	duplicates := make(map[string]bool)
	for key, count := range counts {
		if count > 1 {
			duplicates[key] = true
		}
	}
	return duplicates
}

func duplicateArtifactKeys(artifacts []Artifact) map[string]bool {
	counts := make(map[string]int, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Key != "" {
			counts[artifact.Key]++
		}
	}
	duplicates := make(map[string]bool)
	for key, count := range counts {
		if count > 1 {
			duplicates[key] = true
		}
	}
	return duplicates
}

func actionLess(left, right Action) bool {
	if left.Target.ObjectKey != right.Target.ObjectKey {
		return left.Target.ObjectKey < right.Target.ObjectKey
	}
	if left.Target.Kind != right.Target.Kind {
		return left.Target.Kind < right.Target.Kind
	}
	if left.Target.Namespace != right.Target.Namespace {
		return left.Target.Namespace < right.Target.Namespace
	}
	if left.Target.Name != right.Target.Name {
		return left.Target.Name < right.Target.Name
	}
	if left.Target.UID != right.Target.UID {
		return left.Target.UID < right.Target.UID
	}
	if left.RunUID != right.RunUID {
		return left.RunUID < right.RunUID
	}
	if left.Role != right.Role {
		return left.Role < right.Role
	}
	if left.Mode != right.Mode {
		return left.Mode < right.Mode
	}
	return left.Reason < right.Reason
}

func skipLess(left, right Skip) bool {
	if left.ObjectKey != right.ObjectKey {
		return left.ObjectKey < right.ObjectKey
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Namespace != right.Namespace {
		return left.Namespace < right.Namespace
	}
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.UID != right.UID {
		return left.UID < right.UID
	}
	if left.RunUID != right.RunUID {
		return left.RunUID < right.RunUID
	}
	return left.Reason < right.Reason
}
