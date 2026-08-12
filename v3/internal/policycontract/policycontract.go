// Package policycontract compiles resolved Policy objects into one immutable
// quality contract with three projections: ContextPack, harness self-checks,
// and independent Gate checks.
//
// The compiler is intentionally pure with respect to the repository and
// cluster. It does not open files, invoke Git, contact Kubernetes, or make
// network calls. Callers provide a loader that has already been given the
// authority to read a script from the pristine base revision. The loader is
// called only with the immutable base SHA and a validated repository-relative
// path; all returned bytes and metadata are verified and copied before they
// enter the compiled contract.
package policycontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// SchemaVersion identifies the canonical manifest and descriptor contract.
	SchemaVersion = "agents.astatide.com/policy-contract/v1"

	// ContextPackPolicyRoot is the stable output root shared with contextpack.
	ContextPackPolicyRoot = ".agents/policies"
	// ManifestPath is the sealed, credential-free policy contract consumed by
	// the broker and the independent verifier. It is materialized inside the
	// immutable ContextPack volume by contextmaterializer.
	ManifestPath = contextpack.PolicyContractPath
	// ScriptRunner is the only command placed in generated argv. Scripts are
	// intentionally invoked as arguments, never interpolated into a shell.
	ScriptRunner = "sh"

	// These bounds are compiler ceilings, independent of Kubernetes API bounds.
	MaxPolicies           = 16
	MaxRulesPerPolicy     = 64
	MaxTotalRules         = 512
	MaxPolicyContextBytes = 8192
	MaxScriptPathBytes    = 512
	MaxScriptBytes        = 256 << 10
	MaxTotalScriptBytes   = 4 << 20
	MaxInputBytes         = 8 << 20
	MaxManifestBytes      = 8 << 20
	MaxArgvItems          = 2
	MaxArgBytes           = 1024
	MaxPolicyNameBytes    = 253
	MaxProvenanceBytes    = 128
)

var (
	// ErrInvalidInput identifies malformed policy data or compiler input.
	ErrInvalidInput = errors.New("policycontract: invalid input")
	// ErrMalformedProvenance identifies a missing or malformed Kubernetes
	// revision identity. A policy is never compiled without this provenance.
	ErrMalformedProvenance = errors.New("policycontract: malformed policy provenance")
	// ErrDuplicatePolicy identifies multiple snapshots for one policy name.
	ErrDuplicatePolicy = errors.New("policycontract: duplicate policy provenance")
	// ErrDuplicateRule identifies a rule ID repeated across the complete input.
	ErrDuplicateRule = errors.New("policycontract: duplicate rule id")
	// ErrScriptMissing identifies a required script that the loader could not
	// supply, including a nil loader or an empty returned file.
	ErrScriptMissing = errors.New("policycontract: script missing")
	// ErrScriptLoad identifies an error returned by the external loader.
	ErrScriptLoad = errors.New("policycontract: script load failed")
	// ErrUnsafeScript identifies non-regular files or unsafe loader metadata.
	ErrUnsafeScript = errors.New("policycontract: unsafe script")
	// ErrScriptMismatch identifies a base/path/byte/digest mismatch.
	ErrScriptMismatch = errors.New("policycontract: script mismatch")
	// ErrTooLarge identifies an input or output beyond a hard compiler bound.
	ErrTooLarge = errors.New("policycontract: input or output exceeds bounds")
)

var (
	policyIDPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	scriptPathPattern = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)?)(?:/(?:[A-Za-z0-9_-]+(?:[.][A-Za-z0-9_-]+)?))*$`)
)

// Input is the immutable material required by Compile. ResolvedSpecDigest is
// required so every policy manifest is bound to the final source-pinned run
// contract.
type Input struct {
	BaseSHA            string                    `json:"baseSHA"`
	ResolvedSpecDigest string                    `json:"resolvedSpecDigest"`
	Policies           []resolved.PolicySnapshot `json:"policies"`
}

// FileKind is supplied by the loader so symlinks and special files cannot be
// mistaken for regular policy scripts.
type FileKind string

const (
	FileKindRegular FileKind = "regular"
	FileKindSymlink FileKind = "symlink"
	FileKindSpecial FileKind = "special"
)

// LoadedScript is the only data accepted from a ScriptLoader. Digest is the
// loader's digest of the pristine file bytes; the compiler recomputes it and
// requires exact equality. Bytes are copied and never retained by reference.
type LoadedScript struct {
	BaseSHA string   `json:"baseSHA"`
	Path    string   `json:"path"`
	Kind    FileKind `json:"kind"`
	Bytes   []byte   `json:"-"`
	Digest  string   `json:"digest"`
}

// ScriptLoader loads one exact path from the pristine base revision. The
// implementation may use Git, a verified checkout, or an object store, but
// those concerns remain outside this package and Compile never performs them.
type ScriptLoader interface {
	Load(baseSHA, sourcePath string) (LoadedScript, error)
}

// ScriptLoaderFunc adapts a pure callback to ScriptLoader.
type ScriptLoaderFunc func(baseSHA, sourcePath string) (LoadedScript, error)

func (f ScriptLoaderFunc) Load(baseSHA, sourcePath string) (LoadedScript, error) {
	if f == nil {
		return LoadedScript{}, ErrScriptMissing
	}
	return f(baseSHA, sourcePath)
}

// Severity is the policy enforcement level shared by every projection.
type Severity string

const (
	SeverityBlocking Severity = "blocking"
	SeverityAdvisory Severity = "advisory"
)

// CheckKind identifies the deterministic check implementation.
type CheckKind string

const CheckKindScript CheckKind = "script"

// ExitExpectation is deliberately narrow in v1. The API currently exposes
// only exit0, and the compiler retains its numeric meaning explicitly.
type ExitExpectation string

const (
	ExpectExit0      ExitExpectation = "exit0"
	ExpectedExitCode                 = 0
)

// FailureMode is the action taken when a check does not satisfy its
// expectation. Advisory rules can execute but can never reject a run.
type FailureMode string

const (
	FailureReject   FailureMode = "reject"
	FailureAdvisory FailureMode = "advisory"
)

// PolicySource is immutable Kubernetes revision provenance for one Policy.
type PolicySource struct {
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Generation      int64  `json:"generation"`
}

// Check is the canonical deterministic-check description carried by Rule.
// A nil Rule.Check means an advisory rule with no executable check.
type Check struct {
	Kind             CheckKind       `json:"kind"`
	Expect           ExitExpectation `json:"expect"`
	ExpectedExitCode int             `json:"expectedExitCode"`
}

// Rule is the one canonical compiled policy rule. Its projections are derived
// from this value; no projection contains an independently compiled policy.
// Script bytes are private and can only be obtained through a defensive copy.
type Rule struct {
	ID               string       `json:"id"`
	Severity         Severity     `json:"severity"`
	Context          string       `json:"context,omitempty"`
	Source           PolicySource `json:"source"`
	Check            *Check       `json:"check,omitempty"`
	ScriptSourcePath string       `json:"scriptSourcePath,omitempty"`
	ContextPackPath  string       `json:"contextPackOutputPath,omitempty"`
	ScriptDigest     string       `json:"scriptDigest,omitempty"`
	scriptBytes      []byte
	scriptPresent    bool
}

// ScriptBytes returns a defensive copy of the exact bytes loaded from the
// pristine base revision.
func (r Rule) ScriptBytes() []byte {
	return append([]byte(nil), r.scriptBytes...)
}

// HasScript reports whether the rule has an executable deterministic check.
func (r Rule) HasScript() bool {
	return r.scriptPresent || len(r.scriptBytes) > 0
}

// CheckDescriptor is the common bounded projection used by both self-check
// and Gate consumers. It contains argv as data, never a shell command string.
type CheckDescriptor struct {
	ContractDigest        string          `json:"contractDigest"`
	RuleID                string          `json:"ruleId"`
	Severity              Severity        `json:"severity"`
	Blocking              bool            `json:"blocking"`
	HasScript             bool            `json:"hasScript"`
	Argv                  []string        `json:"argv,omitempty"`
	ScriptSourcePath      string          `json:"scriptSourcePath,omitempty"`
	ContextPackOutputPath string          `json:"contextPackOutputPath,omitempty"`
	ScriptDigest          string          `json:"scriptDigest,omitempty"`
	Kind                  CheckKind       `json:"kind,omitempty"`
	Expect                ExitExpectation `json:"expect,omitempty"`
	ExpectedExitCode      int             `json:"expectedExitCode"`
	FailureMode           FailureMode     `json:"failureMode"`
}

// SelfCheckDescriptor is the bounded descriptor exposed to the broker or
// harness. It is a named type so callers can keep consumer boundaries clear.
type SelfCheckDescriptor = CheckDescriptor

// GateCheckDescriptor is the independent verifier projection. RejectsOnFailure
// is false for every advisory rule, including advisory scripts.
type GateCheckDescriptor struct {
	CheckDescriptor
	RejectsOnFailure bool `json:"rejectsOnFailure"`
}

// CheckObservation is the bounded result emitted by either the broker
// self-check or the independent verifier. The contract and script digests are
// repeated deliberately: a consumer must prove it executed the exact
// immutable projection it was admitted with before a result is actionable.
type CheckObservation struct {
	ContractDigest string `json:"contractDigest"`
	RuleID         string `json:"ruleId"`
	ScriptDigest   string `json:"scriptDigest,omitempty"`
	ExitCode       *int32 `json:"exitCode,omitempty"`
	Skipped        bool   `json:"skipped,omitempty"`
}

// Manifest is the canonical credential-free policy provenance document. It
// contains no script bytes; the manifest digest binds all rules and all script
// digests through the canonical bytes returned by Compile.
type Manifest struct {
	SchemaVersion      string         `json:"schemaVersion"`
	BaseSHA            string         `json:"baseSHA"`
	ResolvedSpecDigest string         `json:"resolvedSpecDigest"`
	Rules              []ManifestRule `json:"rules"`
}

// ManifestRule is the non-secret representation of a compiled Rule.
type ManifestRule struct {
	ID          string         `json:"id"`
	Severity    Severity       `json:"severity"`
	Context     string         `json:"context,omitempty"`
	Source      PolicySource   `json:"source"`
	FailureMode FailureMode    `json:"failureMode"`
	Check       *ManifestCheck `json:"check,omitempty"`
}

// ManifestCheck binds every script-related input without embedding bytes.
type ManifestCheck struct {
	Kind                  CheckKind       `json:"kind"`
	Expect                ExitExpectation `json:"expect"`
	ExpectedExitCode      int             `json:"expectedExitCode"`
	ScriptSourcePath      string          `json:"scriptSourcePath"`
	ContextPackOutputPath string          `json:"contextPackOutputPath"`
	ScriptDigest          string          `json:"scriptDigest"`
}

// Compiled is an immutable result. All accessors return copies of mutable
// slices, including nested script bytes and argv values.
type Compiled struct {
	rules        []Rule
	manifest     Manifest
	manifestJSON []byte
	digest       string
}

// Compile validates and compiles the resolved policy snapshots. It is
// deterministic for all permutations of snapshot and rule order. The loader
// is called in canonical policy/rule order and is the only external callback.
func Compile(input Input, loader ScriptLoader) (Compiled, error) {
	policies, err := normalizeInput(input)
	if err != nil {
		return Compiled{}, err
	}

	canonicalInput, err := marshalInput(input.BaseSHA, input.ResolvedSpecDigest, policies)
	if err != nil {
		return Compiled{}, err
	}
	if len(canonicalInput) > MaxInputBytes {
		return Compiled{}, tooLarge("canonical policy input", len(canonicalInput), MaxInputBytes)
	}

	rules := make([]Rule, 0, countRules(policies))
	var totalScriptBytes int
	for _, policy := range policies {
		source, err := sourceFromObjectVersion(policy.Reference)
		if err != nil {
			return Compiled{}, err
		}
		for _, raw := range policy.Spec.Rules {
			rule, err := compileRule(input.BaseSHA, source, raw, loader)
			if err != nil {
				return Compiled{}, fmt.Errorf("policy %q rule %q: %w", source.Name, raw.ID, err)
			}
			totalScriptBytes += len(rule.scriptBytes)
			if totalScriptBytes > MaxTotalScriptBytes {
				return Compiled{}, tooLarge("total script bytes", totalScriptBytes, MaxTotalScriptBytes)
			}
			rules = append(rules, rule)
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })

	manifest := makeManifest(input.BaseSHA, input.ResolvedSpecDigest, rules)
	manifestJSON, err := marshalManifest(manifest)
	if err != nil {
		return Compiled{}, err
	}
	if len(manifestJSON) > MaxManifestBytes {
		return Compiled{}, tooLarge("canonical policy manifest", len(manifestJSON), MaxManifestBytes)
	}

	return Compiled{
		rules:        cloneRules(rules),
		manifest:     cloneManifest(manifest),
		manifestJSON: append([]byte(nil), manifestJSON...),
		digest:       digest(manifestJSON),
	}, nil
}

// Rules returns defensive copies of the canonical rules.
func (c Compiled) Rules() []Rule {
	return cloneRules(c.rules)
}

// Manifest returns a defensive copy of the canonical manifest value.
func (c Compiled) Manifest() Manifest {
	return cloneManifest(c.manifest)
}

// ManifestBytes returns the exact canonical JSON bytes hashed by Digest.
func (c Compiled) ManifestBytes() []byte {
	return append([]byte(nil), c.manifestJSON...)
}

// Digest returns sha256:<hex> over ManifestBytes.
func (c Compiled) Digest() string {
	return c.digest
}

// ContextPackRules projects every canonical rule into contextpack's existing
// PolicyRule. Script bytes, digest, context, and source path are copied exactly.
func (c Compiled) ContextPackRules() []contextpack.PolicyRule {
	output := make([]contextpack.PolicyRule, 0, len(c.rules))
	for _, rule := range c.rules {
		output = append(output, rule.contextPackRule())
	}
	return output
}

// SelfChecks returns the broker/harness projection in canonical rule order.
func (c Compiled) SelfChecks() []SelfCheckDescriptor {
	output := make([]SelfCheckDescriptor, 0, len(c.rules))
	for _, rule := range c.rules {
		descriptor := cloneCheckDescriptor(rule.selfCheck())
		descriptor.ContractDigest = c.digest
		output = append(output, descriptor)
	}
	return output
}

// GateChecks returns the independent verifier projection in canonical rule
// order. Advisory failures are explicitly demoted and can never reject.
func (c Compiled) GateChecks() []GateCheckDescriptor {
	output := make([]GateCheckDescriptor, 0, len(c.rules))
	for _, rule := range c.rules {
		descriptor := cloneGateDescriptor(rule.gateCheck())
		descriptor.ContractDigest = c.digest
		output = append(output, descriptor)
	}
	return output
}

// ValidateGateChecks validates the descriptor-only projection before it is
// handed to a separate verifier. The verifier may not treat a malformed or
// reordered projection as an empty policy: callers must fail closed instead.
func ValidateGateChecks(checks []GateCheckDescriptor) error {
	if len(checks) > MaxTotalRules {
		return tooLarge("gate check count", len(checks), MaxTotalRules)
	}
	seen := make(map[string]struct{}, len(checks))
	for index, gateCheck := range checks {
		check := gateCheck.CheckDescriptor
		if check.RuleID == "" || !policyIDPattern.MatchString(check.RuleID) {
			return fmt.Errorf("%w: gate check[%d] has an invalid rule id", ErrInvalidInput, index)
		}
		if _, exists := seen[check.RuleID]; exists {
			return fmt.Errorf("%w: gate checks contain duplicate rule %q", ErrDuplicateRule, check.RuleID)
		}
		seen[check.RuleID] = struct{}{}
		if index > 0 && checks[index-1].RuleID >= check.RuleID {
			return fmt.Errorf("%w: gate checks are not sorted", ErrInvalidInput)
		}
		if !validDigest(check.ContractDigest) || (check.Severity != SeverityBlocking && check.Severity != SeverityAdvisory) || check.Blocking != (check.Severity == SeverityBlocking) || gateCheck.RejectsOnFailure != check.Blocking {
			return fmt.Errorf("%w: gate check %q has invalid contract identity or severity", ErrInvalidInput, check.RuleID)
		}
		if check.FailureMode != FailureReject && check.FailureMode != FailureAdvisory {
			return fmt.Errorf("%w: gate check %q has invalid failure mode", ErrInvalidInput, check.RuleID)
		}
		if check.HasScript {
			if check.Kind != CheckKindScript || check.Expect != ExpectExit0 || check.ExpectedExitCode != ExpectedExitCode || len(check.Argv) != 2 || check.Argv[0] != ScriptRunner || check.Argv[1] != check.ContextPackOutputPath || check.ScriptSourcePath == "" || check.ContextPackOutputPath != contextPackOutputPath(check.RuleID, check.ScriptSourcePath) || !validDigest(check.ScriptDigest) {
				return fmt.Errorf("%w: gate check %q has invalid script binding", ErrInvalidInput, check.RuleID)
			}
			if err := validateScriptPath(check.ScriptSourcePath); err != nil {
				return fmt.Errorf("gate check %q: %w", check.RuleID, err)
			}
		} else if check.Kind != "" || check.Expect != "" || check.ExpectedExitCode != 0 || len(check.Argv) != 0 || check.ScriptSourcePath != "" || check.ContextPackOutputPath != "" || check.ScriptDigest != "" {
			return fmt.Errorf("%w: gate check %q contains script fields without a script", ErrInvalidInput, check.RuleID)
		}
	}
	return nil
}

// DecodeManifest reconstructs the descriptor-only form of a compiled policy
// contract from the exact canonical manifest emitted by Compile. Script bytes
// are intentionally absent: consumers that execute a check use the sealed
// ContextPack copy or the pristine verifier checkout, while all rule identity,
// severity, paths, and digests come from this one immutable document.
func DecodeManifest(body []byte) (Compiled, error) {
	if len(body) == 0 || len(body) > MaxManifestBytes || strictjson.ValidateObject(body) != nil {
		return Compiled{}, fmt.Errorf("%w: policy manifest is invalid or oversized", ErrInvalidInput)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) {
		return Compiled{}, fmt.Errorf("%w: policy manifest is not canonical", ErrInvalidInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Compiled{}, fmt.Errorf("%w: decode policy manifest: %v", ErrInvalidInput, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Compiled{}, fmt.Errorf("%w: policy manifest contains trailing JSON", ErrInvalidInput)
	}
	if manifest.SchemaVersion != SchemaVersion || !validBaseSHA(manifest.BaseSHA) || !validDigest(manifest.ResolvedSpecDigest) {
		return Compiled{}, fmt.Errorf("%w: policy manifest identity is invalid", ErrInvalidInput)
	}
	if len(manifest.Rules) > MaxTotalRules {
		return Compiled{}, tooLarge("policy manifest rule count", len(manifest.Rules), MaxTotalRules)
	}
	rules := make([]Rule, 0, len(manifest.Rules))
	seen := make(map[string]struct{}, len(manifest.Rules))
	for index, entry := range manifest.Rules {
		if entry.ID == "" || !policyIDPattern.MatchString(entry.ID) {
			return Compiled{}, fmt.Errorf("%w: policy manifest rule[%d] has an invalid id", ErrInvalidInput, index)
		}
		if _, exists := seen[entry.ID]; exists {
			return Compiled{}, fmt.Errorf("%w: policy manifest contains duplicate rule %q", ErrDuplicateRule, entry.ID)
		}
		seen[entry.ID] = struct{}{}
		if index > 0 && manifest.Rules[index-1].ID >= entry.ID {
			return Compiled{}, fmt.Errorf("%w: policy manifest rules are not sorted", ErrInvalidInput)
		}
		if err := validateText("policy manifest context", entry.Context, MaxPolicyContextBytes); err != nil {
			return Compiled{}, err
		}
		source, err := normalizeSource(entry.Source)
		if err != nil {
			return Compiled{}, fmt.Errorf("policy manifest rule %q: %w", entry.ID, err)
		}
		if entry.Severity != SeverityBlocking && entry.Severity != SeverityAdvisory {
			return Compiled{}, fmt.Errorf("%w: policy manifest rule %q has invalid severity", ErrInvalidInput, entry.ID)
		}
		wantMode := FailureAdvisory
		if entry.Severity == SeverityBlocking {
			wantMode = FailureReject
		}
		if entry.FailureMode != wantMode {
			return Compiled{}, fmt.Errorf("%w: policy manifest rule %q has inconsistent failure mode", ErrInvalidInput, entry.ID)
		}
		rule := Rule{ID: entry.ID, Severity: entry.Severity, Context: entry.Context, Source: source, scriptPresent: entry.Check != nil}
		if entry.Check != nil {
			check := entry.Check
			if check.Kind != CheckKindScript || check.Expect != ExpectExit0 || check.ExpectedExitCode != ExpectedExitCode || check.ScriptSourcePath == "" || check.ContextPackOutputPath == "" || check.ContextPackOutputPath != contextPackOutputPath(entry.ID, check.ScriptSourcePath) || !validDigest(check.ScriptDigest) {
				return Compiled{}, fmt.Errorf("%w: policy manifest rule %q has an invalid script binding", ErrInvalidInput, entry.ID)
			}
			if err := validateScriptPath(check.ScriptSourcePath); err != nil {
				return Compiled{}, fmt.Errorf("policy manifest rule %q: %w", entry.ID, err)
			}
			rule.Check = &Check{Kind: check.Kind, Expect: check.Expect, ExpectedExitCode: check.ExpectedExitCode}
			rule.ScriptSourcePath = check.ScriptSourcePath
			rule.ContextPackPath = check.ContextPackOutputPath
			rule.ScriptDigest = check.ScriptDigest
		}
		rules = append(rules, rule)
	}
	return Compiled{
		rules:        cloneRules(rules),
		manifest:     cloneManifest(manifest),
		manifestJSON: append([]byte(nil), body...),
		digest:       digest(body),
	}, nil
}

func normalizeInput(input Input) ([]resolved.PolicySnapshot, error) {
	if !validBaseSHA(input.BaseSHA) {
		return nil, fmt.Errorf("%w: baseSHA must be 40 or 64 lowercase hexadecimal characters", ErrInvalidInput)
	}
	if !validDigest(input.ResolvedSpecDigest) {
		return nil, fmt.Errorf("%w: resolvedSpecDigest is required and must be a valid SHA-256 digest", ErrInvalidInput)
	}
	if len(input.Policies) > MaxPolicies {
		return nil, tooLarge("policy count", len(input.Policies), MaxPolicies)
	}

	policies := cloneSnapshots(input.Policies)
	sort.Slice(policies, func(i, j int) bool {
		left, right := policies[i].Reference, policies[j].Reference
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.UID != right.UID {
			return left.UID < right.UID
		}
		if left.ResourceVersion != right.ResourceVersion {
			return left.ResourceVersion < right.ResourceVersion
		}
		return left.Generation < right.Generation
	})

	seenPolicies := make(map[string]struct{}, len(policies))
	seenRules := make(map[string]struct{}, countRules(policies))
	totalRules := 0
	for index := range policies {
		if _, err := sourceFromObjectVersion(policies[index].Reference); err != nil {
			return nil, fmt.Errorf("policy[%d]: %w", index, err)
		}
		name := policies[index].Reference.Name
		if _, exists := seenPolicies[name]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicatePolicy, name)
		}
		seenPolicies[name] = struct{}{}
		if len(policies[index].Spec.Rules) == 0 || len(policies[index].Spec.Rules) > MaxRulesPerPolicy {
			return nil, tooLargeOrInvalidRuleCount(name, len(policies[index].Spec.Rules))
		}
		sort.Slice(policies[index].Spec.Rules, func(i, j int) bool {
			return policies[index].Spec.Rules[i].ID < policies[index].Spec.Rules[j].ID
		})
		for ruleIndex := range policies[index].Spec.Rules {
			rule := policies[index].Spec.Rules[ruleIndex]
			if err := validateRuleShape(rule); err != nil {
				return nil, fmt.Errorf("policy %q rule[%d]: %w", name, ruleIndex, err)
			}
			if _, exists := seenRules[rule.ID]; exists {
				return nil, fmt.Errorf("%w: %q", ErrDuplicateRule, rule.ID)
			}
			seenRules[rule.ID] = struct{}{}
		}
		totalRules += len(policies[index].Spec.Rules)
		if totalRules > MaxTotalRules {
			return nil, tooLarge("total rule count", totalRules, MaxTotalRules)
		}
	}
	return policies, nil
}

func tooLargeOrInvalidRuleCount(policy string, count int) error {
	if count > MaxRulesPerPolicy {
		return tooLarge(fmt.Sprintf("rules in policy %q", policy), count, MaxRulesPerPolicy)
	}
	return fmt.Errorf("%w: policy %q must contain at least one rule", ErrInvalidInput, policy)
}

func sourceFromObjectVersion(reference resolved.ObjectVersion) (PolicySource, error) {
	return normalizeSource(PolicySource{
		Name:            reference.Name,
		UID:             reference.UID,
		ResourceVersion: reference.ResourceVersion,
		Generation:      reference.Generation,
	})
}

func normalizeSource(source PolicySource) (PolicySource, error) {
	if source.Name == "" || len(source.Name) > MaxPolicyNameBytes || len(validation.IsDNS1123Subdomain(source.Name)) != 0 {
		return PolicySource{}, fmt.Errorf("%w: name %q is not a DNS subdomain", ErrMalformedProvenance, source.Name)
	}
	if err := validateOpaque("UID", source.UID, MaxProvenanceBytes); err != nil {
		return PolicySource{}, fmt.Errorf("%w: %v", ErrMalformedProvenance, err)
	}
	if err := validateOpaque("resourceVersion", source.ResourceVersion, MaxProvenanceBytes); err != nil {
		return PolicySource{}, fmt.Errorf("%w: %v", ErrMalformedProvenance, err)
	}
	if source.Generation < 1 {
		return PolicySource{}, fmt.Errorf("%w: generation must be positive", ErrMalformedProvenance)
	}
	return source, nil
}

func validateRuleShape(rule v1alpha1.PolicyRule) error {
	if !policyIDPattern.MatchString(rule.ID) {
		return fmt.Errorf("%w: rule ID is invalid", ErrInvalidInput)
	}
	if rule.Severity != v1alpha1.PolicySeverityBlocking && rule.Severity != v1alpha1.PolicySeverityAdvisory {
		return fmt.Errorf("%w: unknown severity %q", ErrInvalidInput, rule.Severity)
	}
	if err := validateText("rule context", rule.Context, MaxPolicyContextBytes); err != nil {
		return err
	}
	if rule.Severity == v1alpha1.PolicySeverityBlocking && rule.Check == nil {
		return fmt.Errorf("%w: blocking rule requires a deterministic script", ErrInvalidInput)
	}
	if rule.Check == nil {
		return nil
	}
	if rule.Check.Kind != string(CheckKindScript) {
		return fmt.Errorf("%w: unknown check kind %q", ErrInvalidInput, rule.Check.Kind)
	}
	if rule.Check.Expect != string(ExpectExit0) {
		return fmt.Errorf("%w: unknown check expectation %q", ErrInvalidInput, rule.Check.Expect)
	}
	if err := validateScriptPath(rule.Check.Script); err != nil {
		return err
	}
	return nil
}

func compileRule(baseSHA string, source PolicySource, raw v1alpha1.PolicyRule, loader ScriptLoader) (Rule, error) {
	rule := Rule{
		ID:       raw.ID,
		Severity: Severity(raw.Severity),
		Context:  raw.Context,
		Source:   source,
	}
	if raw.Check == nil {
		return rule, nil
	}

	sourcePath := raw.Check.Script
	loaded, err := loadScript(loader, baseSHA, sourcePath)
	if err != nil {
		return Rule{}, err
	}
	if loaded.BaseSHA != baseSHA || loaded.Path != sourcePath {
		return Rule{}, fmt.Errorf("%w: loader returned base/path %q/%q for %q", ErrScriptMismatch, loaded.BaseSHA, loaded.Path, sourcePath)
	}
	if loaded.Kind != FileKindRegular {
		return Rule{}, fmt.Errorf("%w: %s is %s, not regular", ErrUnsafeScript, sourcePath, loaded.Kind)
	}
	if len(loaded.Bytes) == 0 {
		return Rule{}, fmt.Errorf("%w: %s is empty", ErrScriptMissing, sourcePath)
	}
	if len(loaded.Bytes) > MaxScriptBytes {
		return Rule{}, tooLarge(fmt.Sprintf("script %q", sourcePath), len(loaded.Bytes), MaxScriptBytes)
	}
	if !utf8.Valid(loaded.Bytes) || bytes.IndexByte(loaded.Bytes, 0) >= 0 {
		return Rule{}, fmt.Errorf("%w: script %q is not valid text", ErrUnsafeScript, sourcePath)
	}
	if !validDigest(loaded.Digest) {
		return Rule{}, fmt.Errorf("%w: loader digest for %q is malformed", ErrScriptMismatch, sourcePath)
	}
	computed := digest(loaded.Bytes)
	if computed != loaded.Digest {
		return Rule{}, fmt.Errorf("%w: loader digest for %q does not match returned bytes", ErrScriptMismatch, sourcePath)
	}

	copyBytes := append([]byte(nil), loaded.Bytes...)
	rule.Check = &Check{Kind: CheckKindScript, Expect: ExpectExit0, ExpectedExitCode: ExpectedExitCode}
	rule.ScriptSourcePath = sourcePath
	rule.ContextPackPath = contextPackOutputPath(rule.ID, sourcePath)
	if len(rule.ContextPackPath) > MaxArgBytes {
		return Rule{}, tooLarge(fmt.Sprintf("ContextPack output path for %q", rule.ID), len(rule.ContextPackPath), MaxArgBytes)
	}
	rule.ScriptDigest = computed
	rule.scriptBytes = copyBytes
	rule.scriptPresent = true
	return rule, nil
}

func loadScript(loader ScriptLoader, baseSHA, sourcePath string) (LoadedScript, error) {
	if loader == nil {
		return LoadedScript{}, fmt.Errorf("%w: no loader for %q", ErrScriptMissing, sourcePath)
	}
	loaded, err := loader.Load(baseSHA, sourcePath)
	if err != nil {
		for _, sentinel := range []error{ErrScriptMissing, ErrScriptMismatch, ErrUnsafeScript} {
			if errors.Is(err, sentinel) {
				return LoadedScript{}, fmt.Errorf("%w: %v", sentinel, err)
			}
		}
		return LoadedScript{}, fmt.Errorf("%w: %v", ErrScriptLoad, err)
	}
	if loaded.BaseSHA == "" && loaded.Path == "" && loaded.Kind == "" && len(loaded.Bytes) == 0 && loaded.Digest == "" {
		return LoadedScript{}, fmt.Errorf("%w: loader returned no file", ErrScriptMissing)
	}
	return loaded, nil
}

func validateScriptPath(value string) error {
	if len(value) == 0 || len(value) > MaxScriptPathBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: script path is empty, oversized, or invalid UTF-8", ErrInvalidInput)
	}
	if strings.ContainsRune(value, '\\') || path.IsAbs(value) || path.Clean(value) != value || !scriptPathPattern.MatchString(value) {
		return fmt.Errorf("%w: unsafe script path %q", ErrInvalidInput, value)
	}
	return nil
}

func validateOpaque(label, value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is empty, oversized, or not normalized", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", label)
		}
	}
	return nil
}

func validateText(label, value string, maxBytes int) error {
	if len(value) > maxBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s is oversized or invalid UTF-8", ErrInvalidInput, label)
	}
	for _, r := range value {
		if r == '\r' || r == '\n' || r == '\t' || !unicode.IsControl(r) {
			continue
		}
		return fmt.Errorf("%w: %s contains control character U+%04X", ErrInvalidInput, label, r)
	}
	return nil
}

func marshalInput(baseSHA, resolvedSpecDigest string, policies []resolved.PolicySnapshot) ([]byte, error) {
	body, err := json.Marshal(struct {
		BaseSHA            string                    `json:"baseSHA"`
		ResolvedSpecDigest string                    `json:"resolvedSpecDigest"`
		Policies           []resolved.PolicySnapshot `json:"policies"`
	}{BaseSHA: baseSHA, ResolvedSpecDigest: resolvedSpecDigest, Policies: policies})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal input: %v", ErrInvalidInput, err)
	}
	return body, nil
}

func makeManifest(baseSHA, resolvedSpecDigest string, rules []Rule) Manifest {
	manifestRules := make([]ManifestRule, 0, len(rules))
	for _, rule := range rules {
		manifestRule := ManifestRule{
			ID:          rule.ID,
			Severity:    rule.Severity,
			Context:     rule.Context,
			Source:      rule.Source,
			FailureMode: rule.failureMode(),
		}
		if rule.Check != nil {
			manifestRule.Check = &ManifestCheck{
				Kind:                  rule.Check.Kind,
				Expect:                rule.Check.Expect,
				ExpectedExitCode:      rule.Check.ExpectedExitCode,
				ScriptSourcePath:      rule.ScriptSourcePath,
				ContextPackOutputPath: rule.ContextPackPath,
				ScriptDigest:          rule.ScriptDigest,
			}
		}
		manifestRules = append(manifestRules, manifestRule)
	}
	return Manifest{SchemaVersion: SchemaVersion, BaseSHA: baseSHA, ResolvedSpecDigest: resolvedSpecDigest, Rules: manifestRules}
}

func marshalManifest(manifest Manifest) ([]byte, error) {
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal manifest: %v", ErrInvalidInput, err)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil {
		return nil, fmt.Errorf("%w: normalize manifest: %v", ErrInvalidInput, err)
	}
	if !json.Valid(normalized) || bytes.IndexByte(normalized, '\n') >= 0 || bytes.IndexByte(normalized, '\r') >= 0 || bytes.IndexByte(normalized, '\t') >= 0 {
		return nil, fmt.Errorf("%w: manifest is not canonical JSON", ErrInvalidInput)
	}
	return normalized, nil
}

func (r Rule) contextPackRule() contextpack.PolicyRule {
	return contextpack.PolicyRule{
		ID:           r.ID,
		Severity:     contextpack.Severity(r.Severity),
		Context:      r.Context,
		ScriptPath:   r.ScriptSourcePath,
		ScriptBytes:  r.ScriptBytes(),
		ScriptDigest: r.ScriptDigest,
	}
}

func (r Rule) selfCheck() CheckDescriptor {
	return r.checkDescriptor()
}

func (r Rule) gateCheck() GateCheckDescriptor {
	return GateCheckDescriptor{CheckDescriptor: r.checkDescriptor(), RejectsOnFailure: r.Severity == SeverityBlocking}
}

func (r Rule) checkDescriptor() CheckDescriptor {
	descriptor := CheckDescriptor{
		ContractDigest:        "",
		RuleID:                r.ID,
		Severity:              r.Severity,
		Blocking:              r.Severity == SeverityBlocking,
		HasScript:             r.HasScript(),
		ScriptSourcePath:      r.ScriptSourcePath,
		ContextPackOutputPath: r.ContextPackPath,
		ScriptDigest:          r.ScriptDigest,
		FailureMode:           r.failureMode(),
	}
	if r.Check != nil {
		descriptor.Argv = []string{ScriptRunner, r.ContextPackPath}
		descriptor.Kind = r.Check.Kind
		descriptor.Expect = r.Check.Expect
		descriptor.ExpectedExitCode = r.Check.ExpectedExitCode
	}
	return descriptor
}

func (r Rule) failureMode() FailureMode {
	if r.Severity == SeverityBlocking {
		return FailureReject
	}
	return FailureAdvisory
}

func contextPackOutputPath(ruleID, sourcePath string) string {
	// Keep this expression byte-for-byte aligned with contextpack's policy
	// renderer: path.Join(".agents/policies", policy.id, path.Base(scriptPath)).
	return path.Join(ContextPackPolicyRoot, ruleID, path.Base(sourcePath))
}

func cloneSnapshots(input []resolved.PolicySnapshot) []resolved.PolicySnapshot {
	output := make([]resolved.PolicySnapshot, len(input))
	for index, snapshot := range input {
		output[index] = snapshot
		output[index].Spec.Rules = append([]v1alpha1.PolicyRule(nil), snapshot.Spec.Rules...)
		for ruleIndex := range output[index].Spec.Rules {
			if snapshot.Spec.Rules[ruleIndex].Check != nil {
				check := *snapshot.Spec.Rules[ruleIndex].Check
				output[index].Spec.Rules[ruleIndex].Check = &check
			}
		}
	}
	return output
}

func countRules(policies []resolved.PolicySnapshot) int {
	total := 0
	for _, policy := range policies {
		total += len(policy.Spec.Rules)
	}
	return total
}

func cloneRules(input []Rule) []Rule {
	output := make([]Rule, len(input))
	for index, rule := range input {
		output[index] = rule
		output[index].scriptBytes = append([]byte(nil), rule.scriptBytes...)
		if rule.Check != nil {
			check := *rule.Check
			output[index].Check = &check
		}
	}
	return output
}

func cloneManifest(input Manifest) Manifest {
	output := input
	output.Rules = append([]ManifestRule(nil), input.Rules...)
	for index := range output.Rules {
		if input.Rules[index].Check != nil {
			check := *input.Rules[index].Check
			output.Rules[index].Check = &check
		}
	}
	return output
}

func cloneCheckDescriptor(input CheckDescriptor) CheckDescriptor {
	input.Argv = append([]string(nil), input.Argv...)
	return input
}

func cloneGateDescriptor(input GateCheckDescriptor) GateCheckDescriptor {
	input.CheckDescriptor = cloneCheckDescriptor(input.CheckDescriptor)
	return input
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range value[len("sha256:"):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validBaseSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func tooLarge(label string, actual, limit int) error {
	return fmt.Errorf("%w: %s=%d exceeds limit=%d", ErrTooLarge, label, actual, limit)
}
