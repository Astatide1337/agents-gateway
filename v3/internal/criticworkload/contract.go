// Package criticworkload owns the execution boundary for the independent
// execution-free critic.
//
// A critic workload receives one immutable, credential-free contract and may
// emit exactly one thing: canonical findingcorroboration.CorroborationInput
// bytes. It cannot represent a verdict, score, Gate result, or blocking
// decision. The operator derives those values independently.
//
// The workload and evidence source bind the contract to Kubernetes resource
// identity and an authenticated Job/Pod lifecycle. Content digests are
// integrity checks; Kubernetes ownership and authenticated API reads provide
// producer authentication. Neither is trusted by itself.
package criticworkload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// ContractSchemaVersion identifies the canonical workload input.
	ContractSchemaVersion = "agents.astatide.com/critic-workload/v1alpha1"
	// RecordSchemaVersion identifies controller-authored output metadata. It is
	// never emitted by the critic.
	RecordSchemaVersion = "agents.astatide.com/critic-output-record/v1alpha1"
	// InputMediaType is the only media type accepted for critic output.
	InputMediaType = "application/vnd.agents-gateway.corroboration-input.v1alpha1+json"
	InputKind      = "critic-corroboration-input"
	InputName      = "critic-corroboration-input.json"
	// OutputProtocol is the one stdout frame accepted from a critic container.
	OutputProtocol = "AGW_CRITIC_INPUT_V1 "

	// MaxContractBytes keeps the Job manifest and environment contract small.
	// The finding contract itself has a larger bound because it contains the
	// actual bounded finding/evidence set.
	MaxContractBytes = 256 << 10
	MaxRecordBytes   = 512 << 10
	MaxFrameBytes    = 2 << 20
	// MaxPatchBytes is the critic's patch-fetch bound. Keep it tied to the
	// verify-fetch contract so a critic Job cannot drift from the fetcher.
	MaxPatchBytes   = verifyfetch.PatchFetchMaxObjectBytes
	MaxContextBytes = 512 << 10
	// MaxPromptBytes bounds the combined verified patch and ContextPack that
	// may enter the critic request. The builder reserves a small envelope for
	// labels and digests so a Job cannot be admitted only to fail at runtime.
	MaxPromptBytes      = 1 << 20
	PromptEnvelopeBytes = 4 << 10
	MaxOutputBytes      = findingcorroboration.MaxInputBytes
	MaxFindings         = v1alpha1.MaxCriticFindings
	MaxRouteProviders   = 16
)

var (
	ErrInvalidInput     = errors.New("invalid critic workload input")
	ErrInvalidRecord    = errors.New("invalid critic output record")
	ErrNonCanonical     = errors.New("critic workload JSON is not canonical")
	ErrOutputMalformed  = errors.New("critic output is malformed")
	ErrOutputOversized  = errors.New("critic output exceeds its bound")
	ErrOutputIdentity   = errors.New("critic output identity conflict")
	ErrBindingConflict  = errors.New("critic workload binding conflict")
	ErrRouteConflict    = errors.New("critic and worker model routes are not distinct")
	ErrArtifactConflict = errors.New("critic artifact identity conflict")
	ErrMissingOutput    = errors.New("critic output is missing")
	ErrDuplicateOutput  = errors.New("critic output has duplicate producer objects")

	digestPattern = "sha256:"
)

// Input is the complete immutable, credential-free contract passed to one
// critic Job. It contains references and identities only; it never contains a
// credential value, a model token, a verdict, a score, or a Gate result.
type Input struct {
	SchemaVersion string               `json:"schemaVersion"`
	Run           resolved.RunIdentity `json:"run"`
	SpecDigest    string               `json:"specDigest"`
	BaseSHA       string               `json:"baseSHA"`
	Patch         v1alpha1.ArtifactRef `json:"patch"`
	Context       v1alpha1.ArtifactRef `json:"context"`
	Gate          ResourceIdentity     `json:"gate"`
	WorkerRoute   ModelRouteIdentity   `json:"workerRoute"`
	CriticRoute   ModelRouteIdentity   `json:"criticRoute"`
	MaxFindings   int32                `json:"maxFindings"`
}

// ResourceIdentity is a Kubernetes object identity resolved at admission.
// ResourceVersion is deliberately excluded: semantic content and generation
// are the replay binding, while resourceVersion is not stable across restores.
type ResourceIdentity struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// ProviderIdentity is the non-secret portion of one model provider. Every
// failover provider is retained so family separation cannot be bypassed by a
// later route fallback.
type ProviderIdentity struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Model         string `json:"model"`
	Family        string `json:"family"`
	CredentialRef string `json:"credentialRef,omitempty"`
	Priority      int32  `json:"priority"`
}

// ModelRouteIdentity is a fully resolved, credential-free model route.
type ModelRouteIdentity struct {
	Name       string             `json:"name"`
	UID        string             `json:"uid"`
	Generation int64              `json:"generation"`
	Providers  []ProviderIdentity `json:"providers"`
	Selected   ProviderIdentity   `json:"selected"`
}

// Binding is the reduced identity used in the controller-authored output
// record. It contains all values that must match before critic evidence can be
// exposed to the trusted verifier.
type Binding struct {
	RunUID        string             `json:"runUID"`
	RunGeneration int64              `json:"runGeneration"`
	SpecDigest    string             `json:"specDigest"`
	BaseSHA       string             `json:"baseSHA"`
	PatchDigest   string             `json:"patchDigest"`
	ContextDigest string             `json:"contextDigest"`
	Gate          ResourceIdentity   `json:"gate"`
	WorkerRoute   ModelRouteIdentity `json:"workerRoute"`
	CriticRoute   ModelRouteIdentity `json:"criticRoute"`
	MaxFindings   int32              `json:"maxFindings"`
}

// JobIdentity is the authenticated Kubernetes Job identity recorded after
// the API server has assigned a UID.
type JobIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// PodIdentity is the authenticated output-producing Pod identity.
type PodIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// OutputRecord is written by the controller runner after it authenticates the
// Job and Pod and validates the critic frame. The record is an audit binding,
// not critic output. It contains no result, route, verdict, score, or finding
// decision.
type OutputRecord struct {
	SchemaVersion string               `json:"schemaVersion"`
	Input         Input                `json:"input"`
	InputDigest   string               `json:"inputDigest"`
	BindingDigest string               `json:"bindingDigest"`
	Job           JobIdentity          `json:"job"`
	Pod           PodIdentity          `json:"pod"`
	Object        ObjectIdentity       `json:"object"`
	Artifact      v1alpha1.ArtifactRef `json:"artifact"`
}

// ObjectIdentity describes the exact immutable object that contains the raw
// canonical CorroborationInput bytes.
type ObjectIdentity struct {
	Key       string `json:"key"`
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

// EvidenceArtifact is the authenticated source result. Input is copied and
// contains only canonical CorroborationInput bytes. The trusted verifier still
// independently derives CorroborationResult from Input.
type EvidenceArtifact struct {
	Input         []byte
	Authenticated bool
	Binding       Binding
	Record        OutputRecord
}

// Binding returns the full run/evidence/model identity of an input.
func (input Input) Binding() Binding {
	return Binding{
		RunUID: input.Run.UID, RunGeneration: input.Run.Generation,
		SpecDigest: input.SpecDigest, BaseSHA: input.BaseSHA,
		PatchDigest: input.Patch.Digest, ContextDigest: input.Context.Digest,
		Gate: input.Gate, WorkerRoute: input.WorkerRoute, CriticRoute: input.CriticRoute,
		MaxFindings: input.MaxFindings,
	}
}

// FromSnapshot creates the critic contract from the exact resolved snapshot
// and separately persisted immutable patch/context references.
func FromSnapshot(snapshot resolved.Snapshot, specDigest string, patch, context v1alpha1.ArtifactRef) (Input, error) {
	computed, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil || computed != specDigest {
		return Input{}, fmt.Errorf("%w: resolved snapshot digest mismatch", ErrInvalidInput)
	}
	if snapshot.SchemaVersion != resolved.SchemaVersion || !resolved.ValidBaseSHA(snapshot.BaseSHA) {
		return Input{}, fmt.Errorf("%w: snapshot identity is invalid", ErrInvalidInput)
	}
	if snapshot.Run.Generation <= 0 || snapshot.References.Gate.Generation <= 0 || snapshot.References.Gate.Name == "" || snapshot.References.Gate.UID == "" {
		return Input{}, fmt.Errorf("%w: run or Gate identity is incomplete", ErrInvalidInput)
	}
	if !resolved.CriticModelRouteEnabled(snapshot.Gate) || snapshot.CriticModelRoute == nil || snapshot.References.CriticModelRoute == nil {
		return Input{}, fmt.Errorf("%w: critic route is not enabled and resolved", ErrInvalidInput)
	}
	if err := resolved.ValidateDistinctModelRoutes(snapshot.ModelRoute, *snapshot.CriticModelRoute); err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrRouteConflict, err)
	}
	worker, err := routeIdentity(snapshot.ModelRoute, snapshot.References.ModelRoute)
	if err != nil {
		return Input{}, fmt.Errorf("%w: worker route: %v", ErrInvalidInput, err)
	}
	critic, err := routeIdentity(*snapshot.CriticModelRoute, *snapshot.References.CriticModelRoute)
	if err != nil {
		return Input{}, fmt.Errorf("%w: critic route: %v", ErrInvalidInput, err)
	}
	maxFindings := snapshot.Gate.Signals.Critic.MaxFindings
	input := Input{
		SchemaVersion: ContractSchemaVersion,
		Run:           snapshot.Run, SpecDigest: specDigest, BaseSHA: snapshot.BaseSHA,
		Patch: patch, Context: context,
		Gate:        ResourceIdentity{Name: snapshot.References.Gate.Name, UID: snapshot.References.Gate.UID, Generation: snapshot.References.Gate.Generation},
		WorkerRoute: worker, CriticRoute: critic, MaxFindings: maxFindings,
	}
	if err := ValidateInput(input); err != nil {
		return Input{}, err
	}
	return input, nil
}

// ValidateInput validates all semantic bounds. It rejects any contract that
// could be reinterpreted as a different run, route, patch, or context.
func ValidateInput(input Input) error {
	if input.SchemaVersion != ContractSchemaVersion {
		return fmt.Errorf("%w: unsupported schemaVersion", ErrInvalidInput)
	}
	if !validPathSegment(input.Run.Namespace) || !validPathSegment(input.Run.Name) || !validPathSegment(input.Run.UID) || input.Run.Generation <= 0 {
		return fmt.Errorf("%w: run identity is invalid", ErrInvalidInput)
	}
	if !canonical.ValidDigest(input.SpecDigest) || !resolved.ValidBaseSHA(input.BaseSHA) {
		return fmt.Errorf("%w: spec or base identity is invalid", ErrInvalidInput)
	}
	if err := validateArtifact(input.Patch, "patch", MaxPatchBytes); err != nil {
		return err
	}
	if err := validateArtifact(input.Context, "context-pack", MaxContextBytes); err != nil {
		return err
	}
	if err := validateResource(input.Gate, "Gate"); err != nil {
		return err
	}
	if err := validateRoute(input.WorkerRoute, "workerRoute"); err != nil {
		return err
	}
	if err := validateRoute(input.CriticRoute, "criticRoute"); err != nil {
		return err
	}
	if routesOverlap(input.WorkerRoute, input.CriticRoute) {
		return fmt.Errorf("%w: worker and critic routes share a family or provider/model", ErrRouteConflict)
	}
	if input.MaxFindings < 1 || input.MaxFindings > MaxFindings {
		return fmt.Errorf("%w: maxFindings is outside 1..%d", ErrInvalidInput, MaxFindings)
	}
	return nil
}

// CanonicalInputBytes returns the only accepted JSON representation of the
// workload contract. It is separate from the critic's CorroborationInput.
func CanonicalInputBytes(input Input) ([]byte, error) {
	if err := ValidateInput(input); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal input: %v", ErrInvalidInput, err)
	}
	normalized, err := strictjson.Normalize(raw)
	if err != nil || len(normalized) > MaxContractBytes || strictjson.ValidateObject(normalized) != nil {
		return nil, fmt.Errorf("%w: input exceeds canonical bound", ErrInvalidInput)
	}
	return normalized, nil
}

// ParseCanonicalInput strictly decodes a workload contract. Duplicate keys,
// unknown fields, trailing JSON, and noncanonical ordering are rejected.
func ParseCanonicalInput(body []byte) (Input, error) {
	var zero Input
	if len(body) == 0 || len(body) > MaxContractBytes || strictjson.ValidateObject(body) != nil {
		return zero, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var input Input
	if err := decoder.Decode(&input); err != nil {
		return zero, fmt.Errorf("%w: decode input: %v", ErrInvalidInput, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return zero, ErrNonCanonical
	}
	canonicalBody, err := CanonicalInputBytes(input)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonicalBody, body) {
		return zero, ErrNonCanonical
	}
	return input, nil
}

// InputDigest returns the content digest of canonical workload input.
func InputDigest(input Input) (string, error) {
	body, err := CanonicalInputBytes(input)
	if err != nil {
		return "", err
	}
	return digestBytes(body), nil
}

// BindingDigest returns the content digest of the reduced identity binding.
func BindingDigest(input Input) (string, error) {
	raw, err := json.Marshal(input.Binding())
	if err != nil {
		return "", err
	}
	body, err := strictjson.Normalize(raw)
	if err != nil || len(body) > MaxContractBytes {
		return "", fmt.Errorf("%w: binding is oversized", ErrInvalidInput)
	}
	return digestBytes(body), nil
}

func routeIdentity(spec v1alpha1.ModelRouteSpec, ref resolved.ObjectVersion) (ModelRouteIdentity, error) {
	if ref.Name == "" || ref.UID == "" || ref.Generation <= 0 || len(spec.Providers) == 0 || len(spec.Providers) > MaxRouteProviders {
		return ModelRouteIdentity{}, ErrInvalidInput
	}
	providers := make([]ProviderIdentity, 0, len(spec.Providers))
	for _, provider := range spec.Providers {
		if provider.Name == "" || provider.Kind == "" || provider.Model == "" || provider.Family == "" || provider.Priority <= 0 {
			return ModelRouteIdentity{}, ErrInvalidInput
		}
		providers = append(providers, ProviderIdentity{Name: provider.Name, Kind: provider.Kind, Model: provider.Model, Family: provider.Family, CredentialRef: provider.CredentialRef, Priority: provider.Priority})
	}
	sort.SliceStable(providers, func(left, right int) bool {
		if providers[left].Priority != providers[right].Priority {
			return providers[left].Priority < providers[right].Priority
		}
		return providers[left].Name < providers[right].Name
	})
	return ModelRouteIdentity{Name: ref.Name, UID: ref.UID, Generation: ref.Generation, Providers: providers, Selected: providers[0]}, nil
}

func validateRoute(route ModelRouteIdentity, field string) error {
	if err := validateResource(ResourceIdentity{Name: route.Name, UID: route.UID, Generation: route.Generation}, field); err != nil {
		return err
	}
	if len(route.Providers) == 0 || len(route.Providers) > MaxRouteProviders {
		return fmt.Errorf("%w: %s providers are outside bounds", ErrInvalidInput, field)
	}
	seenNames := make(map[string]struct{}, len(route.Providers))
	for index, provider := range route.Providers {
		if !validIdentity(provider.Name) || !validIdentity(provider.Kind) || !validIdentity(provider.Model) || !validIdentity(provider.Family) || (provider.CredentialRef != "" && !validPathSegment(provider.CredentialRef)) || provider.Priority <= 0 {
			return fmt.Errorf("%w: %s provider %d is invalid", ErrInvalidInput, field, index)
		}
		if _, exists := seenNames[provider.Name]; exists {
			return fmt.Errorf("%w: %s contains duplicate provider %q", ErrInvalidInput, field, provider.Name)
		}
		seenNames[provider.Name] = struct{}{}
		if index > 0 {
			previous := route.Providers[index-1]
			if previous.Priority > provider.Priority || previous.Priority == provider.Priority && previous.Name >= provider.Name {
				return fmt.Errorf("%w: %s providers are not canonical", ErrInvalidInput, field)
			}
		}
	}
	if route.Selected != route.Providers[0] {
		return fmt.Errorf("%w: %s selected provider is not the deterministic primary", ErrInvalidInput, field)
	}
	return nil
}

func routesOverlap(worker, critic ModelRouteIdentity) bool {
	families := make(map[string]struct{}, len(worker.Providers))
	models := make(map[string]struct{}, len(worker.Providers))
	for _, provider := range worker.Providers {
		families[provider.Family] = struct{}{}
		models[provider.Kind+"\x00"+provider.Model] = struct{}{}
	}
	for _, provider := range critic.Providers {
		if _, ok := families[provider.Family]; ok {
			return true
		}
		if _, ok := models[provider.Kind+"\x00"+provider.Model]; ok {
			return true
		}
	}
	return false
}

func validateResource(identity ResourceIdentity, field string) error {
	if !validPathSegment(identity.Name) || !validPathSegment(identity.UID) || identity.Generation <= 0 {
		return fmt.Errorf("%w: %s identity is invalid", ErrInvalidInput, field)
	}
	return nil
}

func validateArtifact(ref v1alpha1.ArtifactRef, kind string, maxBytes int64) error {
	if ref.Kind != kind || ref.Name == "" || ref.MediaType == "" || !canonical.ValidDigest(ref.Digest) || ref.SizeBytes <= 0 || ref.SizeBytes > maxBytes || !safeURI(ref.URI) {
		return fmt.Errorf("%w: %s artifact identity is invalid", ErrInvalidInput, kind)
	}
	return nil
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= 253 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func safeURI(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "s3" && u.Scheme != "https") {
		return false
	}
	if u.Path == "" || strings.Contains(u.Path, "//") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validPathSegment(value string) bool {
	return validIdentity(value) && value != "." && value != ".." && !strings.ContainsAny(value, "/\\")
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return digestPattern + hex.EncodeToString(sum[:])
}
