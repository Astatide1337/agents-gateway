// Package resolved turns an AgentRun and its mutable references into one
// immutable, content-addressed execution contract.
package resolved

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/toolsecurity"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SchemaVersion increments when the immutable snapshot shape changes. The
	// critic route is now part of the resolved execution boundary, so older
	// snapshots must not be replayed under the new contract.
	SchemaVersion      = 3
	TaskConfigMapKey   = "task"
	InstructionsMapKey = "instructions"
)

var (
	ErrInvalidRun       = errors.New("invalid AgentRun")
	ErrReferenceMissing = errors.New("referenced resource is missing")
	ErrReferenceUnsafe  = errors.New("referenced resource is unsafe")
	ErrConfiguration    = errors.New("invalid resolved configuration")
)

// Snapshot is the exact immutable input used to create execution children.
// It contains logical credential references, never credential values.
type Snapshot struct {
	SchemaVersion    int                          `json:"schemaVersion"`
	Run              RunIdentity                  `json:"run"`
	Spec             v1alpha1.AgentRunSpec        `json:"spec"`
	BaseSHA          string                       `json:"baseSHA"`
	Task             string                       `json:"task"`
	Agent            v1alpha1.AgentSpec           `json:"agent"`
	Instructions     string                       `json:"instructions"`
	Gate             v1alpha1.GateSpec            `json:"gate"`
	ToolSet          v1alpha1.ToolSetSpec         `json:"toolSet"`
	ModelRoute       v1alpha1.ModelRouteSpec      `json:"modelRoute"`
	CriticModelRoute *v1alpha1.ModelRouteSpec     `json:"criticModelRoute,omitempty"`
	ContextStrategy  v1alpha1.ContextStrategySpec `json:"contextStrategy"`
	Policies         []PolicySnapshot             `json:"policies"`
	References       References                   `json:"references"`
}

// PolicySnapshot binds complete policy content to the Kubernetes revision
// from which it was resolved.
type PolicySnapshot struct {
	Reference ObjectVersion       `json:"reference"`
	Spec      v1alpha1.PolicySpec `json:"spec"`
}

type RunIdentity struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// References records the exact Kubernetes versions resolved at admission.
// ResourceVersion is evidence only; semantic content is also in Snapshot.
type References struct {
	Agent            ObjectVersion  `json:"agent"`
	Gate             ObjectVersion  `json:"gate"`
	ToolSet          ObjectVersion  `json:"toolSet"`
	ModelRoute       ObjectVersion  `json:"modelRoute"`
	CriticModelRoute *ObjectVersion `json:"criticModelRoute,omitempty"`
	ContextStrategy  ObjectVersion  `json:"contextStrategy"`
}

type ObjectVersion struct {
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Generation      int64  `json:"generation"`
}

type Result struct {
	Snapshot  Snapshot
	Canonical []byte
	Digest    string
}

// Decode verifies and decodes a canonical immutable snapshot loaded from
// object storage. Unknown fields and non-canonical encodings fail closed.
func Decode(body []byte, digest string) (Snapshot, error) {
	if len(body) == 0 || len(body) > 1<<20 || !canonical.ValidDigest(digest) || strictjson.ValidateObject(body) != nil {
		return Snapshot{}, ErrConfiguration
	}
	computed, err := canonical.ResolvedSpecDigest(body)
	if err != nil || computed != digest {
		return Snapshot{}, ErrConfiguration
	}
	canonicalBody, err := canonical.CanonicalizeResolvedSpec(body)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return Snapshot{}, ErrConfiguration
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil || snapshot.SchemaVersion != SchemaVersion || snapshot.Run.Namespace == "" || snapshot.Run.Name == "" || snapshot.Run.UID == "" || !ValidBaseSHA(snapshot.BaseSHA) {
		return Snapshot{}, ErrConfiguration
	}
	if err := validateSnapshot(snapshot); err != nil {
		return Snapshot{}, err
	}
	if !ValidPinnedImage(snapshot.Agent.Runtime.Image) || !ValidPinnedImage(snapshot.Gate.Verify.Image) {
		return Snapshot{}, ErrReferenceUnsafe
	}
	return snapshot, nil
}

// WithBaseSHA converts the Kubernetes-only preliminary resolution into the
// immutable execution contract persisted by the controller. The commit must
// be resolved through the operator-owned GitHub App before any Sandbox exists.
func WithBaseSHA(input Result, baseSHA string) (Result, error) {
	if !ValidBaseSHA(baseSHA) || input.Snapshot.SchemaVersion != SchemaVersion {
		return Result{}, ErrConfiguration
	}
	snapshot := input.Snapshot
	snapshot.BaseSHA = baseSHA
	if err := validateSnapshot(snapshot); err != nil {
		return Result{}, err
	}
	encoded, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize source-pinned snapshot: %w", err)
	}
	digest, err := canonical.ResolvedSpecDigest(encoded)
	if err != nil {
		return Result{}, fmt.Errorf("digest source-pinned snapshot: %w", err)
	}
	return Result{Snapshot: snapshot, Canonical: encoded, Digest: digest}, nil
}

// ValidBaseSHA accepts Git's current SHA-1 object IDs and SHA-256 repository
// object IDs, always in canonical lowercase hexadecimal form.
func ValidBaseSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Resolve reads every reference through an uncached Reader, resolves prompt
// text, validates digest-pinned execution images, and computes one digest.
func Resolve(ctx context.Context, reader client.Reader, run *v1alpha1.AgentRun) (Result, error) {
	if reader == nil || run == nil || run.Namespace == "" || run.Name == "" || run.UID == "" {
		return Result{}, ErrInvalidRun
	}

	agent := &v1alpha1.Agent{}
	if err := get(ctx, reader, run.Namespace, run.Spec.AgentRef, agent); err != nil {
		return Result{}, fmt.Errorf("resolve Agent: %w", err)
	}
	gate := &v1alpha1.Gate{}
	if err := get(ctx, reader, run.Namespace, run.Spec.GateRef, gate); err != nil {
		return Result{}, fmt.Errorf("resolve Gate: %w", err)
	}
	if err := ValidateGateSignalsSpec(gate.Spec); err != nil {
		return Result{}, fmt.Errorf("resolve Gate signals: %w", err)
	}
	policyRefs, err := NormalizePolicyRefs(agent.Spec.PolicyRefs)
	if err != nil {
		return Result{}, fmt.Errorf("resolve Agent policyRefs: %w", err)
	}
	gatePolicyRefs, err := NormalizePolicyRefs(gate.Spec.PolicyRefs)
	if err != nil {
		return Result{}, fmt.Errorf("resolve Gate policyRefs: %w", err)
	}
	if !reflect.DeepEqual(policyRefs, gatePolicyRefs) {
		return Result{}, fmt.Errorf("%w: Agent and Gate policyRefs must contain the same normalized set", ErrConfiguration)
	}
	if !validReference(agent.Spec.ContextStrategyRef) {
		return Result{}, fmt.Errorf("%w: Agent contextStrategyRef is required and must be a DNS subdomain", ErrConfiguration)
	}
	contextStrategy := &v1alpha1.ContextStrategy{}
	if err := get(ctx, reader, run.Namespace, agent.Spec.ContextStrategyRef, contextStrategy); err != nil {
		return Result{}, fmt.Errorf("resolve ContextStrategy: %w", err)
	}
	contextStrategySpec := normalizeContextStrategySpec(*contextStrategy.Spec.DeepCopy())
	if err := validateContextStrategySpec(contextStrategySpec); err != nil {
		return Result{}, fmt.Errorf("resolve ContextStrategy: %w", err)
	}
	policies := make([]PolicySnapshot, 0, len(policyRefs))
	for _, name := range policyRefs {
		policy := &v1alpha1.Policy{}
		if err := get(ctx, reader, run.Namespace, name, policy); err != nil {
			return Result{}, fmt.Errorf("resolve Policy %q: %w", name, err)
		}
		policySpec := normalizePolicySpec(*policy.Spec.DeepCopy())
		if err := validatePolicySpec(policySpec); err != nil {
			return Result{}, fmt.Errorf("resolve Policy %q: %w", name, err)
		}
		policies = append(policies, PolicySnapshot{Reference: version(policy), Spec: policySpec})
	}
	toolSet := &v1alpha1.ToolSet{}
	if err := get(ctx, reader, run.Namespace, agent.Spec.ToolSetRef, toolSet); err != nil {
		return Result{}, fmt.Errorf("resolve ToolSet: %w", err)
	}
	if violations := toolsecurity.ValidateToolSetSecurity(toolSet); len(violations) != 0 {
		violation := violations[0]
		return Result{}, fmt.Errorf("%w: ToolSet %s at %s", ErrReferenceUnsafe, violation.Code, violation.Field)
	}
	modelRoute := &v1alpha1.ModelRoute{}
	if err := get(ctx, reader, run.Namespace, agent.Spec.ModelRouteRef, modelRoute); err != nil {
		return Result{}, fmt.Errorf("resolve ModelRoute: %w", err)
	}
	if !ValidPinnedImage(agent.Spec.Runtime.Image) {
		return Result{}, fmt.Errorf("%w: agent runtime image must be digest-pinned", ErrReferenceUnsafe)
	}
	if !ValidPinnedImage(gate.Spec.Verify.Image) {
		return Result{}, fmt.Errorf("%w: gate verifier image must be digest-pinned", ErrReferenceUnsafe)
	}
	if err := validateModelRouteSpec(modelRoute.Spec); err != nil {
		return Result{}, fmt.Errorf("resolve ModelRoute: %w", err)
	}
	var criticModelRoute *v1alpha1.ModelRoute
	if CriticModelRouteEnabled(gate.Spec) {
		criticModelRoute = &v1alpha1.ModelRoute{}
		criticRef := gate.Spec.Signals.Critic.ModelRouteRef
		if !validReference(criticRef) {
			return Result{}, fmt.Errorf("%w: critic ModelRoute reference is invalid", ErrConfiguration)
		}
		if err := get(ctx, reader, run.Namespace, criticRef, criticModelRoute); err != nil {
			return Result{}, fmt.Errorf("resolve critic ModelRoute: %w", err)
		}
		if err := validateModelRouteSpec(criticModelRoute.Spec); err != nil {
			return Result{}, fmt.Errorf("resolve critic ModelRoute: %w", err)
		}
		if err := ValidateDistinctModelRoutes(modelRoute.Spec, criticModelRoute.Spec); err != nil {
			return Result{}, fmt.Errorf("resolve critic ModelRoute: %w", err)
		}
	}

	task, err := resolveText(ctx, reader, run.Namespace, run.Spec.Task.Inline, run.Spec.Task.ConfigMapRef, TaskConfigMapKey)
	if err != nil {
		return Result{}, fmt.Errorf("resolve task: %w", err)
	}
	instructions, err := resolveText(ctx, reader, run.Namespace, agent.Spec.Instructions.Inline, agent.Spec.Instructions.ConfigMapRef, InstructionsMapKey)
	if err != nil {
		return Result{}, fmt.Errorf("resolve instructions: %w", err)
	}

	resolvedSpec := *run.Spec.DeepCopy()
	if resolvedSpec.Output == nil {
		resolvedSpec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputPatch}
	} else if resolvedSpec.Output.Mode == "" {
		resolvedSpec.Output.Mode = v1alpha1.OutputPatch
	}
	if err := validateOutputPublication(resolvedSpec.Output, resolvedSpec.Publish.Mode); err != nil {
		return Result{}, fmt.Errorf("resolve output: %w", err)
	}
	agentSpec := normalizeAgentSpec(*agent.Spec.DeepCopy())
	gateSpec := normalizeGateSpec(*gate.Spec.DeepCopy())
	toolSetSpec := normalizeToolSetSpec(*toolSet.Spec.DeepCopy())
	modelRouteSpec := normalizeModelRouteSpec(*modelRoute.Spec.DeepCopy())
	var criticModelRouteSpec *v1alpha1.ModelRouteSpec
	var criticModelRouteReference *ObjectVersion
	if criticModelRoute != nil {
		normalized := normalizeModelRouteSpec(*criticModelRoute.Spec.DeepCopy())
		criticModelRouteSpec = &normalized
		reference := version(criticModelRoute)
		criticModelRouteReference = &reference
	}

	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Run:           RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID), Generation: run.Generation},
		Spec:          resolvedSpec, Task: task,
		Agent: agentSpec, Instructions: instructions,
		Gate: gateSpec, ToolSet: toolSetSpec, ModelRoute: modelRouteSpec, CriticModelRoute: criticModelRouteSpec,
		ContextStrategy: contextStrategySpec, Policies: policies,
		References: References{
			Agent: version(agent), Gate: version(gate), ToolSet: version(toolSet), ModelRoute: version(modelRoute), CriticModelRoute: criticModelRouteReference, ContextStrategy: version(contextStrategy),
		},
	}
	if err := validateSnapshot(snapshot); err != nil {
		return Result{}, fmt.Errorf("validate resolved snapshot: %w", err)
	}
	encoded, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize resolved snapshot: %w", err)
	}
	digest, err := canonical.ResolvedSpecDigest(encoded)
	if err != nil {
		return Result{}, fmt.Errorf("digest resolved snapshot: %w", err)
	}
	return Result{Snapshot: snapshot, Canonical: encoded, Digest: digest}, nil
}

func get(ctx context.Context, reader client.Reader, namespace, name string, object client.Object) error {
	if strings.TrimSpace(name) == "" {
		return ErrConfiguration
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrReferenceMissing
		}
		return err
	}
	if object.GetDeletionTimestamp() != nil {
		return ErrReferenceUnsafe
	}
	return nil
}

func resolveText(ctx context.Context, reader client.Reader, namespace string, inline, ref *string, key string) (string, error) {
	if (inline == nil) == (ref == nil) {
		return "", ErrConfiguration
	}
	if inline != nil {
		if err := validateResolvedText(*inline); err != nil {
			return "", ErrConfiguration
		}
		return *inline, nil
	}
	configMap := &corev1.ConfigMap{}
	if err := get(ctx, reader, namespace, *ref, configMap); err != nil {
		return "", err
	}
	value, ok := configMap.Data[key]
	if !ok {
		return "", fmt.Errorf("%w: ConfigMap must contain %q", ErrConfiguration, key)
	}
	if err := validateResolvedText(value); err != nil {
		return "", fmt.Errorf("%w: ConfigMap value %q is invalid", ErrConfiguration, key)
	}
	return value, nil
}

func validateResolvedText(value string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > v1alpha1.MaxTaskLength {
		return ErrConfiguration
	}
	return nil
}

// ValidPinnedImage reports whether value is a non-placeholder OCI image
// reference pinned to one lowercase SHA-256 manifest digest.
func ValidPinnedImage(value string) bool {
	parts := strings.Split(value, "@sha256:")
	return len(parts) == 2 && parts[0] != "" && parts[1] != strings.Repeat("0", 64) && canonical.ValidDigest("sha256:"+parts[1])
}

var (
	policyIDPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	policyScriptPattern = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)?)(?:/(?:[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)?))*$`)
	familyPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// NormalizePolicyRefs validates and sorts policy names so the Agent/Gate
// contract is a true set while the resolved JSON remains deterministic.
func NormalizePolicyRefs(refs []string) ([]string, error) {
	if len(refs) > 16 {
		return nil, fmt.Errorf("%w: at most 16 policy references are allowed", ErrConfiguration)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(refs))
	normalized := make([]string, 0, len(refs))
	for _, ref := range refs {
		if !validReference(ref) {
			return nil, fmt.Errorf("%w: policy reference %q is not a DNS subdomain", ErrConfiguration, ref)
		}
		if _, ok := seen[ref]; ok {
			return nil, fmt.Errorf("%w: duplicate policy reference %q", ErrConfiguration, ref)
		}
		seen[ref] = struct{}{}
		normalized = append(normalized, ref)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func validReference(value string) bool {
	return value != "" && len(validation.IsDNS1123Subdomain(value)) == 0
}

// NormalizeToolSetSpec returns a defensive, deterministic copy of a ToolSet.
// Server and tool definitions are sorted by their names, profiles by phase,
// and profile references by the complete server/tool binding. Validation is
// intentionally separate so callers can distinguish normalization from the
// fail-closed resolved boundary.
func NormalizeToolSetSpec(spec v1alpha1.ToolSetSpec) v1alpha1.ToolSetSpec {
	if copy := spec.DeepCopy(); copy != nil {
		spec = *copy
	}
	if spec.Servers != nil {
		sort.SliceStable(spec.Servers, func(i, j int) bool { return spec.Servers[i].Name < spec.Servers[j].Name })
		for index := range spec.Servers {
			if spec.Servers[index].Tools != nil {
				sort.SliceStable(spec.Servers[index].Tools, func(i, j int) bool { return spec.Servers[index].Tools[i].Name < spec.Servers[index].Tools[j].Name })
			}
		}
	}
	if spec.Profiles != nil {
		sort.SliceStable(spec.Profiles, func(i, j int) bool { return spec.Profiles[i].Name < spec.Profiles[j].Name })
		for index := range spec.Profiles {
			if spec.Profiles[index].Tools != nil {
				sort.SliceStable(spec.Profiles[index].Tools, func(i, j int) bool {
					if spec.Profiles[index].Tools[i].Server != spec.Profiles[index].Tools[j].Server {
						return spec.Profiles[index].Tools[i].Server < spec.Profiles[index].Tools[j].Server
					}
					return spec.Profiles[index].Tools[i].Tool < spec.Profiles[index].Tools[j].Tool
				})
			}
		}
	}
	return spec
}

// ValidateToolSetSpec validates the phase-scoped ToolSet contract before it
// enters a resolved snapshot. It never supplies a compatibility default:
// omitted profiles or a zero maxToolsPerPhase cannot silently expose every
// server tool.
func ValidateToolSetSpec(spec v1alpha1.ToolSetSpec) error {
	if len(spec.Servers) == 0 || len(spec.Servers) > 32 {
		return fmt.Errorf("%w: ToolSet must contain 1..32 servers", ErrConfiguration)
	}
	if len(spec.Profiles) == 0 || len(spec.Profiles) > 3 {
		return fmt.Errorf("%w: ToolSet must contain 1..3 profiles", ErrConfiguration)
	}
	if spec.MaxToolsPerPhase < 1 || spec.MaxToolsPerPhase > v1alpha1.MaxToolsPerPhase {
		return fmt.Errorf("%w: maxToolsPerPhase must be between 1 and %d", ErrConfiguration, v1alpha1.MaxToolsPerPhase)
	}

	servers := make(map[string]map[string]struct{}, len(spec.Servers))
	for serverIndex, server := range spec.Servers {
		if !validToolSetName(server.Name, 128) {
			return fmt.Errorf("%w: server name at index %d is unsafe", ErrConfiguration, serverIndex)
		}
		if _, exists := servers[server.Name]; exists {
			return fmt.Errorf("%w: duplicate ToolServer %q", ErrConfiguration, server.Name)
		}
		if strings.TrimSpace(server.Ref) == "" || strings.ContainsAny(server.Ref, "\x00\r\n") {
			return fmt.Errorf("%w: server %q has an invalid endpoint reference", ErrConfiguration, server.Name)
		}
		if len(server.Tools) == 0 || len(server.Tools) > 256 {
			return fmt.Errorf("%w: server %q must contain 1..256 tools", ErrConfiguration, server.Name)
		}
		toolNames := make(map[string]struct{}, len(server.Tools))
		for toolIndex, tool := range server.Tools {
			if !validToolSetName(tool.Name, 253) {
				return fmt.Errorf("%w: tool name at server %q index %d is unsafe", ErrConfiguration, server.Name, toolIndex)
			}
			if _, exists := toolNames[tool.Name]; exists {
				return fmt.Errorf("%w: duplicate tool %q on server %q", ErrConfiguration, tool.Name, server.Name)
			}
			toolNames[tool.Name] = struct{}{}
			switch tool.Effect {
			case v1alpha1.EffectRead, v1alpha1.EffectWrite, v1alpha1.EffectPublish:
			default:
				return fmt.Errorf("%w: tool %q on server %q has an invalid effect", ErrConfiguration, tool.Name, server.Name)
			}
		}
		servers[server.Name] = toolNames
	}

	profiles := make(map[v1alpha1.ToolProfileName]struct{}, len(spec.Profiles))
	for profileIndex, profile := range spec.Profiles {
		switch profile.Name {
		case v1alpha1.ToolProfileExplore, v1alpha1.ToolProfileEdit, v1alpha1.ToolProfileVerify:
		default:
			return fmt.Errorf("%w: profile name at index %d is invalid", ErrConfiguration, profileIndex)
		}
		if _, exists := profiles[profile.Name]; exists {
			return fmt.Errorf("%w: duplicate ToolProfile %q", ErrConfiguration, profile.Name)
		}
		profiles[profile.Name] = struct{}{}
		if len(profile.Tools) == 0 {
			return fmt.Errorf("%w: profile %q cannot be empty", ErrConfiguration, profile.Name)
		}
		if len(profile.Tools) > int(spec.MaxToolsPerPhase) {
			return fmt.Errorf("%w: profile %q contains %d tools, maxToolsPerPhase is %d", ErrConfiguration, profile.Name, len(profile.Tools), spec.MaxToolsPerPhase)
		}
		seenRefs := make(map[string]struct{}, len(profile.Tools))
		for refIndex, reference := range profile.Tools {
			if !validToolSetName(reference.Server, 128) || !validToolSetName(reference.Tool, 253) {
				return fmt.Errorf("%w: profile %q reference at index %d has an unsafe server or tool name", ErrConfiguration, profile.Name, refIndex)
			}
			serverTools, ok := servers[reference.Server]
			if !ok {
				return fmt.Errorf("%w: profile %q references unknown server %q", ErrConfiguration, profile.Name, reference.Server)
			}
			if _, ok := serverTools[reference.Tool]; !ok {
				return fmt.Errorf("%w: profile %q references unknown tool %q on server %q", ErrConfiguration, profile.Name, reference.Tool, reference.Server)
			}
			refKey := reference.Server + "\x00" + reference.Tool
			if _, exists := seenRefs[refKey]; exists {
				return fmt.Errorf("%w: profile %q contains duplicate server/tool reference", ErrConfiguration, profile.Name)
			}
			seenRefs[refKey] = struct{}{}
		}
	}
	for _, required := range []v1alpha1.ToolProfileName{
		v1alpha1.ToolProfileExplore,
		v1alpha1.ToolProfileEdit,
		v1alpha1.ToolProfileVerify,
	} {
		if _, ok := profiles[required]; !ok {
			return fmt.Errorf("%w: ToolSet is missing required %q profile", ErrConfiguration, required)
		}
	}
	return nil
}

// CriticModelRouteEnabled reports whether a Gate explicitly enables the
// execution-free critic signal. Omitted signals and execution-only signals do
// not create a hidden route lookup.
func CriticModelRouteEnabled(spec v1alpha1.GateSpec) bool {
	return spec.Signals != nil && spec.Signals.Critic != nil
}

// EffectiveGateSignals returns the canonical execution-only default when a
// Gate omits its optional signal block. Callers that need to preserve the
// distinction between omitted and explicit configuration should inspect
// GateSpec.Signals before calling this helper.
func EffectiveGateSignals(spec v1alpha1.GateSpec) v1alpha1.GateSignalsSpec {
	if spec.Signals == nil {
		return v1alpha1.GateSignalsSpec{
			ExecutionWeightBasisPoints: v1alpha1.GateScoreScale,
			MinScoreBasisPoints:        v1alpha1.GateScoreScale,
		}
	}
	return *spec.Signals.DeepCopy()
}

// ValidateGateSignalsSpec validates the declarative fixed-point signal
// contract before it can enter a resolved snapshot. Mutation is deliberately
// absent from this contract and therefore cannot be enabled accidentally.
func ValidateGateSignalsSpec(spec v1alpha1.GateSpec) error {
	if spec.Signals == nil {
		return nil
	}
	signals := spec.Signals
	if signals.ExecutionWeightBasisPoints < 0 || signals.ExecutionWeightBasisPoints > v1alpha1.GateScoreScale || signals.MinScoreBasisPoints < 0 || signals.MinScoreBasisPoints > v1alpha1.GateScoreScale {
		return fmt.Errorf("%w: signal score or weight is outside 0..%d", ErrConfiguration, v1alpha1.GateScoreScale)
	}
	if signals.Critic == nil {
		if signals.ExecutionWeightBasisPoints != v1alpha1.GateScoreScale || signals.MinScoreBasisPoints != v1alpha1.GateScoreScale {
			return fmt.Errorf("%w: execution-only signals must use execution=10000 and minimum=10000", ErrConfiguration)
		}
		return nil
	}
	critic := signals.Critic
	if signals.ExecutionWeightBasisPoints < 1 || critic.WeightBasisPoints < 1 || int64(signals.ExecutionWeightBasisPoints)+int64(critic.WeightBasisPoints) != int64(v1alpha1.GateScoreScale) {
		return fmt.Errorf("%w: execution and critic weights must sum exactly to %d", ErrConfiguration, v1alpha1.GateScoreScale)
	}
	if !validReference(critic.ModelRouteRef) {
		return fmt.Errorf("%w: critic ModelRoute reference is invalid", ErrConfiguration)
	}
	if critic.MaxFindings < 1 || critic.MaxFindings > v1alpha1.MaxCriticFindings {
		return fmt.Errorf("%w: critic maxFindings is outside 1..%d", ErrConfiguration, v1alpha1.MaxCriticFindings)
	}
	return nil
}

func validToolSetName(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func validatePolicySpec(spec v1alpha1.PolicySpec) error {
	if len(spec.Rules) == 0 || len(spec.Rules) > 64 {
		return fmt.Errorf("%w: Policy must contain 1..64 rules", ErrConfiguration)
	}
	seen := make(map[string]struct{}, len(spec.Rules))
	for _, rule := range spec.Rules {
		if !policyIDPattern.MatchString(rule.ID) {
			return fmt.Errorf("%w: policy rule id is invalid", ErrConfiguration)
		}
		if _, ok := seen[rule.ID]; ok {
			return fmt.Errorf("%w: duplicate policy rule id %q", ErrConfiguration, rule.ID)
		}
		seen[rule.ID] = struct{}{}
		if len(rule.Context) > 8192 {
			return fmt.Errorf("%w: policy rule context is too large", ErrConfiguration)
		}
		if rule.Severity != v1alpha1.PolicySeverityBlocking && rule.Severity != v1alpha1.PolicySeverityAdvisory {
			return fmt.Errorf("%w: policy rule severity is invalid", ErrConfiguration)
		}
		if rule.Severity == v1alpha1.PolicySeverityBlocking && rule.Check == nil {
			return fmt.Errorf("%w: blocking policy rule %q has no check", ErrConfiguration, rule.ID)
		}
		if rule.Check != nil {
			if rule.Check.Kind != "script" || rule.Check.Expect != "exit0" || !policyScriptPattern.MatchString(rule.Check.Script) {
				return fmt.Errorf("%w: policy rule %q has an invalid script check", ErrConfiguration, rule.ID)
			}
		}
	}
	if !sortedPolicyRules(spec.Rules) {
		return fmt.Errorf("%w: policy rules must be sorted by id", ErrConfiguration)
	}
	return nil
}

func validateContextStrategySpec(spec v1alpha1.ContextStrategySpec) error {
	if spec.Budget.TotalTokens < 1 || spec.Budget.TotalTokens > 131072 || spec.Budget.MaxBytes < 1024 || spec.Budget.MaxBytes > 67108864 {
		return fmt.Errorf("%w: ContextStrategy budget is outside its bounds", ErrConfiguration)
	}
	if spec.Semantic != nil && spec.Semantic.Enabled {
		return fmt.Errorf("%w: semantic context is disabled in this release", ErrConfiguration)
	}
	active := false
	if spec.RepoMap != nil {
		if spec.RepoMap.Kind != "tree-sitter" || spec.RepoMap.Budget < 1 || spec.RepoMap.Budget > 100000 || spec.RepoMap.Budget > spec.Budget.TotalTokens {
			return fmt.Errorf("%w: repoMap configuration is invalid", ErrConfiguration)
		}
		active = true
	}
	if spec.Symbols != nil {
		if spec.Symbols.Kind != "lsp-serena" || len(spec.Symbols.Languages) == 0 || len(spec.Symbols.Languages) > 8 {
			return fmt.Errorf("%w: symbols configuration is invalid", ErrConfiguration)
		}
		allowed := map[string]bool{"go": true, "typescript": true, "javascript": true, "python": true, "rust": true, "java": true, "dotnet": true}
		seen := make(map[string]struct{}, len(spec.Symbols.Languages))
		for _, language := range spec.Symbols.Languages {
			if !allowed[language] {
				return fmt.Errorf("%w: symbols language %q is unsupported", ErrConfiguration, language)
			}
			if _, ok := seen[language]; ok {
				return fmt.Errorf("%w: duplicate symbols language %q", ErrConfiguration, language)
			}
			seen[language] = struct{}{}
		}
		if !sort.StringsAreSorted(spec.Symbols.Languages) {
			return fmt.Errorf("%w: symbols languages must be sorted", ErrConfiguration)
		}
		active = true
	}
	if spec.History != nil {
		if spec.History.Kind != "git-blame-touched" || spec.History.Depth < 1 || spec.History.Depth > 100 {
			return fmt.Errorf("%w: history configuration is invalid", ErrConfiguration)
		}
		active = true
	}
	if spec.Conventions != nil && spec.Conventions.FromPolicy {
		active = true
	}
	if !active {
		return fmt.Errorf("%w: ContextStrategy has no enabled context tier", ErrConfiguration)
	}
	return nil
}

func validateModelRouteSpec(spec v1alpha1.ModelRouteSpec) error {
	if len(spec.Providers) == 0 || len(spec.Providers) > 16 {
		return fmt.Errorf("%w: ModelRoute must contain 1..16 providers", ErrConfiguration)
	}
	seenNames := make(map[string]struct{}, len(spec.Providers))
	for _, provider := range spec.Providers {
		if !validModelProviderName(provider.Name) || !validModelProviderModel(provider.Model) || !validModelProviderKind(provider.Kind) || provider.Priority < 1 || provider.Priority > 1000 {
			return fmt.Errorf("%w: ModelProvider has an invalid name, kind, model, or priority", ErrConfiguration)
		}
		if provider.Pricing != nil && (provider.Pricing.InputMicrosPerToken < 0 || provider.Pricing.OutputMicrosPerToken < 0 || provider.Pricing.InputMicrosPerToken > 1000000000000 || provider.Pricing.OutputMicrosPerToken > 1000000000000) {
			return fmt.Errorf("%w: ModelProvider pricing is outside its bound", ErrConfiguration)
		}
		if !familyPattern.MatchString(provider.Family) {
			return fmt.Errorf("%w: ModelProvider family must be explicit and valid", ErrConfiguration)
		}
		if _, exists := seenNames[provider.Name]; exists {
			return fmt.Errorf("%w: ModelRoute contains duplicate provider name %q", ErrConfiguration, provider.Name)
		}
		seenNames[provider.Name] = struct{}{}
	}
	return nil
}

// ValidateDistinctModelRoutes proves that a worker route and a critic route
// cannot select the same provider/model family, including through failover.
// Families are explicit API data; this function never guesses from a model
// name or provider label. Missing/unknown identity fails closed.
func ValidateDistinctModelRoutes(worker, critic v1alpha1.ModelRouteSpec) error {
	if err := validateModelRouteSpec(worker); err != nil {
		return fmt.Errorf("%w: worker route is not comparable: %v", ErrConfiguration, err)
	}
	if err := validateModelRouteSpec(critic); err != nil {
		return fmt.Errorf("%w: critic route is not comparable: %v", ErrConfiguration, err)
	}
	workerFamilies := make(map[string]struct{}, len(worker.Providers))
	workerModels := make(map[string]struct{}, len(worker.Providers))
	for _, provider := range worker.Providers {
		workerFamilies[provider.Family] = struct{}{}
		workerModels[provider.Kind+"\x00"+provider.Model] = struct{}{}
	}
	for _, provider := range critic.Providers {
		if _, sameFamily := workerFamilies[provider.Family]; sameFamily {
			return fmt.Errorf("%w: worker and critic routes share provider/model family %q", ErrConfiguration, provider.Family)
		}
		if _, sameModel := workerModels[provider.Kind+"\x00"+provider.Model]; sameModel {
			return fmt.Errorf("%w: worker and critic routes share provider/model %q", ErrConfiguration, provider.Kind+"/"+provider.Model)
		}
	}
	return nil
}

func validModelProviderName(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n") && validToolSetName(value, 128)
}

func validModelProviderKind(value string) bool {
	switch value {
	case "openrouter-responses", "openai-responses", "openrouter-anthropic-messages", "anthropic-messages":
		return true
	default:
		return false
	}
}

func validModelProviderModel(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func validateOutputSpec(output *v1alpha1.AgentRunOutputSpec) error {
	if output == nil {
		return nil
	}
	mode := output.Mode
	if mode == "" {
		mode = v1alpha1.OutputPatch
	}
	if mode != v1alpha1.OutputPatch && mode != v1alpha1.OutputFindings && mode != v1alpha1.OutputBoth {
		return fmt.Errorf("%w: output mode is invalid", ErrConfiguration)
	}
	if mode == v1alpha1.OutputFindings && (output.Target == nil || output.Target.PullRequest <= 0) {
		return fmt.Errorf("%w: findings output requires target.pullRequest", ErrConfiguration)
	}
	return nil
}

func validateOutputPublication(output *v1alpha1.AgentRunOutputSpec, publishMode v1alpha1.PublishMode) error {
	if err := validateOutputSpec(output); err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	mode := output.Mode
	if mode == "" {
		mode = v1alpha1.OutputPatch
	}
	if (mode == v1alpha1.OutputFindings || mode == v1alpha1.OutputBoth) && publishMode != v1alpha1.PublishPullRequest {
		return fmt.Errorf("%w: findings output requires pull-request publication", ErrConfiguration)
	}
	return nil
}

func validateObjectVersion(reference ObjectVersion) error {
	if !validReference(reference.Name) || reference.UID == "" || reference.ResourceVersion == "" || reference.Generation < 1 {
		return ErrReferenceUnsafe
	}
	return nil
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.SchemaVersion != SchemaVersion || snapshot.Run.Namespace == "" || snapshot.Run.Name == "" || snapshot.Run.UID == "" || snapshot.Run.Generation < 1 {
		return ErrConfiguration
	}
	if err := validateOutputPublication(snapshot.Spec.Output, snapshot.Spec.Publish.Mode); err != nil {
		return err
	}
	if err := ValidateGateSignalsSpec(snapshot.Gate); err != nil {
		return err
	}
	if snapshot.Agent.ContextStrategyRef == "" || snapshot.References.ContextStrategy.Name != snapshot.Agent.ContextStrategyRef {
		return ErrReferenceUnsafe
	}
	if err := validateObjectVersion(snapshot.References.ContextStrategy); err != nil {
		return err
	}
	agentPolicies, err := NormalizePolicyRefs(snapshot.Agent.PolicyRefs)
	if err != nil {
		return err
	}
	gatePolicies, err := NormalizePolicyRefs(snapshot.Gate.PolicyRefs)
	if err != nil || !reflect.DeepEqual(agentPolicies, gatePolicies) {
		return fmt.Errorf("%w: Agent and Gate policyRefs differ", ErrConfiguration)
	}
	if len(snapshot.Policies) != len(agentPolicies) {
		return ErrReferenceUnsafe
	}
	for index, policy := range snapshot.Policies {
		if policy.Reference.Name != agentPolicies[index] {
			return ErrReferenceUnsafe
		}
		if err := validateObjectVersion(policy.Reference); err != nil {
			return err
		}
		if err := validatePolicySpec(policy.Spec); err != nil {
			return err
		}
	}
	if err := validateContextStrategySpec(snapshot.ContextStrategy); err != nil {
		return err
	}
	if err := ValidateToolSetSpec(snapshot.ToolSet); err != nil {
		return err
	}
	for _, reference := range []ObjectVersion{snapshot.References.Agent, snapshot.References.Gate, snapshot.References.ToolSet, snapshot.References.ModelRoute} {
		if err := validateObjectVersion(reference); err != nil {
			return err
		}
	}
	if err := validateModelRouteSpec(snapshot.ModelRoute); err != nil {
		return err
	}
	if CriticModelRouteEnabled(snapshot.Gate) {
		if snapshot.CriticModelRoute == nil || snapshot.References.CriticModelRoute == nil {
			return ErrReferenceUnsafe
		}
		if err := validateObjectVersion(*snapshot.References.CriticModelRoute); err != nil {
			return err
		}
		if err := ValidateDistinctModelRoutes(snapshot.ModelRoute, *snapshot.CriticModelRoute); err != nil {
			return err
		}
	} else if snapshot.CriticModelRoute != nil || snapshot.References.CriticModelRoute != nil {
		return ErrReferenceUnsafe
	}
	if !reflect.DeepEqual(snapshot.Agent, normalizeAgentSpec(snapshot.Agent)) ||
		!reflect.DeepEqual(snapshot.Gate, normalizeGateSpec(snapshot.Gate)) ||
		!reflect.DeepEqual(snapshot.ToolSet, normalizeToolSetSpec(snapshot.ToolSet)) ||
		!reflect.DeepEqual(snapshot.ModelRoute, normalizeModelRouteSpec(snapshot.ModelRoute)) ||
		!reflect.DeepEqual(snapshot.CriticModelRoute, normalizeOptionalModelRouteSpec(snapshot.CriticModelRoute)) ||
		!reflect.DeepEqual(snapshot.ContextStrategy, normalizeContextStrategySpec(snapshot.ContextStrategy)) {
		return fmt.Errorf("%w: resolved list fields are not normalized", ErrConfiguration)
	}
	return nil
}

func sortedPolicyRules(rules []v1alpha1.PolicyRule) bool {
	for index := 1; index < len(rules); index++ {
		if rules[index-1].ID >= rules[index].ID {
			return false
		}
	}
	return true
}

func normalizeAgentSpec(spec v1alpha1.AgentSpec) v1alpha1.AgentSpec {
	if spec.PolicyRefs != nil {
		refs := append([]string(nil), spec.PolicyRefs...)
		sort.Strings(refs)
		spec.PolicyRefs = refs
	}
	if spec.Skills != nil {
		sort.SliceStable(spec.Skills, func(i, j int) bool { return spec.Skills[i].Name < spec.Skills[j].Name })
	}
	return spec
}

func normalizeGateSpec(spec v1alpha1.GateSpec) v1alpha1.GateSpec {
	if copy := spec.DeepCopy(); copy != nil {
		spec = *copy
	}
	if spec.PolicyRefs != nil {
		refs := append([]string(nil), spec.PolicyRefs...)
		sort.Strings(refs)
		spec.PolicyRefs = refs
	}
	return spec
}

func normalizeOptionalModelRouteSpec(spec *v1alpha1.ModelRouteSpec) *v1alpha1.ModelRouteSpec {
	if spec == nil {
		return nil
	}
	normalized := normalizeModelRouteSpec(*spec.DeepCopy())
	return &normalized
}

func normalizeToolSetSpec(spec v1alpha1.ToolSetSpec) v1alpha1.ToolSetSpec {
	return NormalizeToolSetSpec(spec)
}

func normalizeModelRouteSpec(spec v1alpha1.ModelRouteSpec) v1alpha1.ModelRouteSpec {
	if spec.Providers != nil {
		sort.SliceStable(spec.Providers, func(i, j int) bool { return spec.Providers[i].Name < spec.Providers[j].Name })
	}
	return spec
}

func normalizePolicySpec(spec v1alpha1.PolicySpec) v1alpha1.PolicySpec {
	if spec.Rules != nil {
		sort.SliceStable(spec.Rules, func(i, j int) bool { return spec.Rules[i].ID < spec.Rules[j].ID })
	}
	return spec
}

func normalizeContextStrategySpec(spec v1alpha1.ContextStrategySpec) v1alpha1.ContextStrategySpec {
	if spec.Symbols != nil && spec.Symbols.Languages != nil {
		languages := append([]string(nil), spec.Symbols.Languages...)
		sort.Strings(languages)
		spec.Symbols.Languages = languages
	}
	return spec
}

func version(object metav1.Object) ObjectVersion {
	return ObjectVersion{Name: object.GetName(), UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(), Generation: object.GetGeneration()}
}
