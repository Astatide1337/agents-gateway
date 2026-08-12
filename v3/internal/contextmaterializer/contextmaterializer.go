// Package contextmaterializer turns the immutable, credential-free context
// contract into the standard ContextPack file tree used by a work Sandbox.
//
// The package is deliberately the only filesystem-aware layer around the pure
// contextpack and policycontract compilers. It reads only the pristine base
// checkout and the already-verified skills volume, never a Secret or a
// network. It writes into a disposable staging directory, verifies the result
// from disk, and only then promotes it into the dedicated context volume.
package contextmaterializer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// SchemaVersion is the immutable input contract passed to the context init
	// container. It intentionally contains no ToolSet, ModelRoute, or logical
	// credential references.
	SchemaVersion = "agents.astatide.com/context-input/v2"
	// RefSchemaVersion identifies the small controller-readable output marker.
	RefSchemaVersion = "agents.astatide.com/context-ref/v2"

	// RefFileName is outside the ContextPack manifest because adding a file to
	// the manifest would make the manifest digest self-referential.
	RefFileName = "context-pack-ref.json"

	DefaultBaseDir   = "/workspace/base"
	DefaultSkillsDir = "/opt/agw/skills"
	DefaultOutputDir = "/opt/agw/context"

	maxContractBytes  = 1 << 20
	maxExistingAgents = 1 << 20
	maxSkillBytes     = 256 << 10
	maxReadBytes      = 8<<20 + 1
	maxFiles          = 4096
	maxPathBytes      = 512
)

var (
	ErrInvalidInput = errors.New("contextmaterializer: invalid input")
	ErrTampered     = errors.New("contextmaterializer: output is tampered or incomplete")
	ErrOutput       = errors.New("contextmaterializer: output cannot be materialized")
)

// SkillInput is the minimum verified skill identity needed after agw-skills
// has populated the read-only skills volume. The gateway URL is intentionally
// not carried into the context contract.
type SkillInput struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// Contract is the immutable, credential-free input to the context init
// container. Policies retain Kubernetes revision provenance so the policy
// compiler can bind every script to the same resolved snapshot. No Secret
// value or provider credential is representable in this type.
type Contract struct {
	SchemaVersion      string                        `json:"schemaVersion"`
	RunUID             string                        `json:"runUID"`
	BaseSHA            string                        `json:"baseSHA"`
	ResolvedSpecDigest string                        `json:"resolvedSpecDigest"`
	Task               string                        `json:"task"`
	Instructions       string                        `json:"instructions"`
	Boundaries         contextpack.Boundaries        `json:"boundaries"`
	Policies           []resolved.PolicySnapshot     `json:"policies,omitempty"`
	Skills             []SkillInput                  `json:"skills,omitempty"`
	Producers          ProducerSettings              `json:"producers,omitempty"`
	Strategies         contextpack.ContextStrategies `json:"strategies"`
	Budgets            contextpack.Budgets           `json:"budgets"`
}

// Config contains the explicit paths and contract supplied to one init
// container. All paths are absolute, canonical, and bounded. The context
// container receives no Secret mounts and never contacts a service.
type Config struct {
	InputJSON          string
	ExpectedRunUID     string
	ExpectedSpecDigest string
	ExpectedBaseSHA    string
	BaseDir            string
	SkillsDir          string
	OutputDir          string
	// The adapters are in-process seams. The shipped context image leaves them
	// nil unless the context command receives explicit local symbol-adapter
	// configuration; a configured symbol tier therefore still fails closed when
	// its executable or protocol is unavailable.
	GitBinary         string
	StructuralAdapter StructuralAdapter
	SymbolProvider    SymbolProvider
}

// Ref is the run-bound digest pointer written beside the standard pack. The
// controller copies a separately stored evidence artifact into
// AgentRun.status.ContextPackRef only after independently validating it.
type Ref struct {
	SchemaVersion      string `json:"schemaVersion"`
	RunUID             string `json:"runUID"`
	BaseSHA            string `json:"baseSHA"`
	ResolvedSpecDigest string `json:"resolvedSpecDigest"`
	ManifestPath       string `json:"manifestPath"`
	Digest             string `json:"digest"`
}

// Result is the materialization result. Digest is the SHA-256 digest of the
// canonical ContextPack manifest, not a digest of the reference marker.
type Result struct {
	Digest string
	Ref    Ref
}

// FromSnapshot projects only the context-relevant portion of a resolved
// snapshot. It is used by workload construction so the init container's
// environment is generated from the same immutable snapshot as every other
// child. Repository-derived entries are produced later from the sealed base
// checkout; no mutable or credential-bearing value enters this contract.
func FromSnapshot(snapshot resolved.Snapshot, specDigest string) (Contract, error) {
	computed, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil || specDigest == "" || computed != specDigest {
		return Contract{}, fmt.Errorf("%w: resolved spec digest does not match snapshot", ErrInvalidInput)
	}
	if snapshot.SchemaVersion != resolved.SchemaVersion || !resolved.ValidBaseSHA(snapshot.BaseSHA) {
		return Contract{}, fmt.Errorf("%w: snapshot identity is invalid", ErrInvalidInput)
	}
	if strings.TrimSpace(snapshot.Task) == "" || strings.TrimSpace(snapshot.Instructions) == "" {
		return Contract{}, fmt.Errorf("%w: task and instructions are required", ErrInvalidInput)
	}

	policies := clonePolicies(snapshot.Policies)
	skills := make([]SkillInput, 0, len(snapshot.Agent.Skills))
	for _, skill := range snapshot.Agent.Skills {
		skills = append(skills, SkillInput{Name: skill.Name, Digest: skill.Digest})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

	return Contract{
		SchemaVersion:      SchemaVersion,
		RunUID:             snapshot.Run.UID,
		BaseSHA:            snapshot.BaseSHA,
		ResolvedSpecDigest: specDigest,
		Task:               snapshot.Task,
		Instructions:       snapshot.Instructions,
		Boundaries: contextpack.Boundaries{
			AllowedPaths:   append([]string(nil), snapshot.Spec.Scope.Paths...),
			ForbiddenPaths: append([]string(nil), snapshot.Spec.Scope.Forbidden...),
		},
		Policies:   policies,
		Skills:     skills,
		Producers:  producerSettings(snapshot.ContextStrategy),
		Strategies: strategies(snapshot.ContextStrategy),
		Budgets:    budgets(snapshot.ContextStrategy),
	}, nil
}

// Encode returns canonical JSON for a contract. It also validates through the
// same strict decoder used in the init container.
func Encode(contract Contract) ([]byte, error) {
	contract.SchemaVersion = SchemaVersion
	contract.Policies = clonePolicies(contract.Policies)
	contract.Skills = append([]SkillInput(nil), contract.Skills...)
	sort.Slice(contract.Skills, func(i, j int) bool { return contract.Skills[i].Name < contract.Skills[j].Name })
	contract.Producers.SymbolLanguages = normalizeLanguages(contract.Producers.SymbolLanguages)
	body, err := json.Marshal(contract)
	if err != nil {
		return nil, fmt.Errorf("%w: encode contract: %v", ErrInvalidInput, err)
	}
	body, err = strictjson.Normalize(body)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize contract: %v", ErrInvalidInput, err)
	}
	if _, err := Decode(body); err != nil {
		return nil, err
	}
	return body, nil
}

// Decode strictly parses one canonical immutable contract.
func Decode(body []byte) (Contract, error) {
	if len(body) == 0 || len(body) > maxContractBytes || strictjson.ValidateObject(body) != nil {
		return Contract{}, fmt.Errorf("%w: contract JSON is invalid or oversized", ErrInvalidInput)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) {
		return Contract{}, fmt.Errorf("%w: contract JSON is not canonical", ErrInvalidInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var contract Contract
	if err := decoder.Decode(&contract); err != nil {
		return Contract{}, fmt.Errorf("%w: decode contract: %v", ErrInvalidInput, err)
	}
	if decoder.More() {
		return Contract{}, fmt.Errorf("%w: contract has trailing JSON", ErrInvalidInput)
	}
	if contract.SchemaVersion != SchemaVersion || !validRunUID(contract.RunUID) || !resolved.ValidBaseSHA(contract.BaseSHA) || !canonical.ValidDigest(contract.ResolvedSpecDigest) {
		return Contract{}, fmt.Errorf("%w: contract identity is invalid", ErrInvalidInput)
	}
	if err := validateText(contract.Task, true, 1<<20); err != nil {
		return Contract{}, err
	}
	if err := validateText(contract.Instructions, true, 1<<20); err != nil {
		return Contract{}, err
	}
	if len(contract.Skills) > 32 {
		return Contract{}, fmt.Errorf("%w: skill count is bounded", ErrInvalidInput)
	}
	if err := validateProducerSettings(contract); err != nil {
		return Contract{}, err
	}
	seen := make(map[string]struct{}, len(contract.Skills))
	for _, skill := range contract.Skills {
		if !safeName(skill.Name) || !canonical.ValidDigest(skill.Digest) {
			return Contract{}, fmt.Errorf("%w: skill identity is invalid", ErrInvalidInput)
		}
		if _, ok := seen[skill.Name]; ok {
			return Contract{}, fmt.Errorf("%w: duplicate skill name", ErrInvalidInput)
		}
		seen[skill.Name] = struct{}{}
	}
	if _, err := contextpack.Compile(contextpack.Input{
		BaseSHA:            contract.BaseSHA,
		ResolvedSpecDigest: contract.ResolvedSpecDigest,
		Task:               contract.Task,
		Instructions:       contract.Instructions,
		Boundaries:         contract.Boundaries,
		Strategies:         contract.Strategies,
		Budgets:            contract.Budgets,
	}); err != nil {
		// The compiler is used here only to validate the bounded common fields;
		// policy scripts and skill bodies are deliberately loaded later from
		// their independently verified sources.
		return Contract{}, fmt.Errorf("%w: contract bounds: %v", ErrInvalidInput, err)
	}
	return contract, nil
}

// Run materializes one contract into Config.OutputDir and returns the digest
// marker that a later controller can persist. It fails before promotion on any
// missing, malformed, or tampered source/output.
func Run(ctx context.Context, config Config) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	if err := validateConfig(config); err != nil {
		return Result{}, err
	}
	contract, err := Decode([]byte(config.InputJSON))
	if err != nil {
		return Result{}, err
	}
	if contract.RunUID != config.ExpectedRunUID || contract.ResolvedSpecDigest != config.ExpectedSpecDigest || contract.BaseSHA != config.ExpectedBaseSHA {
		return Result{}, fmt.Errorf("%w: contract identity does not match the pod inputs", ErrInvalidInput)
	}
	if err := requireDirectory(config.BaseDir, true); err != nil {
		return Result{}, err
	}
	if err := requireDirectory(config.SkillsDir, true); err != nil {
		return Result{}, err
	}
	if err := requireEmptyDirectory(config.OutputDir); err != nil {
		return Result{}, err
	}

	existingAgents, err := readOptionalText(filepath.Join(config.BaseDir, "AGENTS.md"), maxExistingAgents)
	if err != nil {
		return Result{}, err
	}
	compiledPolicies, err := policycontract.Compile(policycontract.Input{
		BaseSHA:            contract.BaseSHA,
		ResolvedSpecDigest: contract.ResolvedSpecDigest,
		Policies:           contract.Policies,
	}, repositoryLoader{root: config.BaseDir, baseSHA: contract.BaseSHA})
	if err != nil {
		return Result{}, fmt.Errorf("%w: policy compilation failed: %v", ErrInvalidInput, err)
	}

	skills := make([]contextpack.Skill, 0, len(contract.Skills))
	for _, input := range contract.Skills {
		if err := contextError(ctx); err != nil {
			return Result{}, err
		}
		body, err := readRequiredText(filepath.Join(config.SkillsDir, input.Name, "SKILL.md"), maxSkillBytes)
		if err != nil {
			return Result{}, fmt.Errorf("%w: skill %q: %v", ErrInvalidInput, input.Name, err)
		}
		skills = append(skills, contextpack.Skill{Name: input.Name, Digest: input.Digest, Body: string(body)})
	}

	produced, err := Produce(ctx, ProducerConfig{
		Root:              config.BaseDir,
		BaseSHA:           contract.BaseSHA,
		GitBinary:         config.GitBinary,
		Settings:          contract.Producers,
		Strategies:        contract.Strategies,
		Budgets:           contract.Budgets,
		StructuralAdapter: config.StructuralAdapter,
		SymbolProvider:    config.SymbolProvider,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: context producer failed: %v", ErrInvalidInput, err)
	}

	pack, err := contextpack.Compile(contextpack.Input{
		BaseSHA:                contract.BaseSHA,
		ResolvedSpecDigest:     contract.ResolvedSpecDigest,
		Task:                   contract.Task,
		Instructions:           contract.Instructions,
		ExistingAgentsMD:       string(existingAgents),
		Boundaries:             contract.Boundaries,
		Policies:               compiledPolicies.ContextPackRules(),
		PolicyContractManifest: compiledPolicies.ManifestBytes(),
		Skills:                 skills,
		Lexical:                produced.Lexical,
		RepoMap:                produced.RepoMap,
		Symbols:                produced.Symbols,
		History:                produced.History,
		Strategies:             produced.Strategies,
		Budgets:                contract.Budgets,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: context pack compilation failed: %v", ErrInvalidInput, err)
	}
	ref := Ref{SchemaVersion: RefSchemaVersion, RunUID: contract.RunUID, BaseSHA: contract.BaseSHA, ResolvedSpecDigest: contract.ResolvedSpecDigest, ManifestPath: ".agw/context/manifest.json", Digest: pack.Digest}
	if err := materialize(config.OutputDir, pack, ref); err != nil {
		return Result{}, err
	}
	verified, err := VerifyForRun(config.OutputDir, contract.RunUID, pack.Digest, contract.ResolvedSpecDigest, contract.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	return Result{Digest: pack.Digest, Ref: verified}, nil
}

// Verify checks the manifest digest, every listed file, the immutable ref,
// and the exact file set. It is retained as a compatibility wrapper for
// callers that do not yet have the run identity; production callers should
// use VerifyForRun.
func Verify(root, expectedDigest, expectedSpecDigest, expectedBaseSHA string) (Ref, error) {
	return VerifyForRun(root, "", expectedDigest, expectedSpecDigest, expectedBaseSHA)
}

// ReadReference reads only the sealed reference marker. It does not establish
// trust in the pack; callers must follow it with VerifyForRun.
func ReadReference(root string) (Ref, error) {
	if !absoluteClean(root) || root == "/" {
		return Ref{}, fmt.Errorf("%w: root is invalid", ErrTampered)
	}
	body, err := readRequiredText(filepath.Join(root, RefFileName), 4096)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: reference: %v", ErrTampered, err)
	}
	ref, err := decodeRef(body)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: reference: %v", ErrTampered, err)
	}
	if ref.SchemaVersion != RefSchemaVersion || !validRunUID(ref.RunUID) || !resolved.ValidBaseSHA(ref.BaseSHA) || !canonical.ValidDigest(ref.ResolvedSpecDigest) || ref.ManifestPath != ".agw/context/manifest.json" || !canonical.ValidDigest(ref.Digest) {
		return Ref{}, fmt.Errorf("%w: reference identity is invalid", ErrTampered)
	}
	return ref, nil
}

// VerifyForRun checks the exact sealed tree and binds it to the expected
// run/spec/base identities. An empty expectedRunUID is allowed only for the
// compatibility Verify wrapper.
func VerifyForRun(root, expectedRunUID, expectedDigest, expectedSpecDigest, expectedBaseSHA string) (Ref, error) {
	if !canonical.ValidDigest(expectedDigest) || !canonical.ValidDigest(expectedSpecDigest) || !resolved.ValidBaseSHA(expectedBaseSHA) {
		return Ref{}, fmt.Errorf("%w: expected identity is invalid", ErrTampered)
	}
	if err := requireDirectory(root, true); err != nil {
		return Ref{}, fmt.Errorf("%w: %v", ErrTampered, err)
	}
	manifestBody, err := readRequiredText(filepath.Join(root, filepath.FromSlash(".agw/context/manifest.json")), 8<<20)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: manifest: %v", ErrTampered, err)
	}
	if got := digest(manifestBody); got != expectedDigest {
		return Ref{}, fmt.Errorf("%w: manifest digest mismatch", ErrTampered)
	}
	manifest, err := decodeManifest(manifestBody)
	if err != nil || manifest.SchemaVersion != contextpack.SchemaVersion || manifest.BaseSHA != expectedBaseSHA || manifest.ResolvedSpecDigest != expectedSpecDigest {
		return Ref{}, fmt.Errorf("%w: manifest identity is invalid", ErrTampered)
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > maxFiles {
		return Ref{}, fmt.Errorf("%w: manifest file count is invalid", ErrTampered)
	}
	expected := map[string]struct{}{
		filepath.ToSlash(".agw/context/manifest.json"): {},
		RefFileName: {},
	}
	var descriptorBytes int64
	for _, descriptor := range manifest.Files {
		if !safeRelative(descriptor.Path) || descriptor.Path == ".agw/context/manifest.json" || descriptor.Path == RefFileName || descriptor.Digest == "" || !canonical.ValidDigest(descriptor.Digest) || descriptor.SizeBytes < 0 {
			return Ref{}, fmt.Errorf("%w: manifest descriptor is invalid", ErrTampered)
		}
		if _, ok := expected[descriptor.Path]; ok {
			return Ref{}, fmt.Errorf("%w: duplicate manifest path %q", ErrTampered, descriptor.Path)
		}
		body, err := readRequiredText(filepath.Join(root, filepath.FromSlash(descriptor.Path)), descriptor.SizeBytes+1)
		if err != nil || int64(len(body)) != descriptor.SizeBytes || digest(body) != descriptor.Digest {
			return Ref{}, fmt.Errorf("%w: file %q does not match its descriptor", ErrTampered, descriptor.Path)
		}
		if descriptor.SizeBytes > manifest.Budgets.MaxFileBytes {
			return Ref{}, fmt.Errorf("%w: file %q exceeds manifest file budget", ErrTampered, descriptor.Path)
		}
		descriptorBytes += descriptor.SizeBytes
		expected[descriptor.Path] = struct{}{}
	}
	if _, ok := expected["AGENTS.md"]; !ok {
		return Ref{}, fmt.Errorf("%w: AGENTS.md is not present in manifest", ErrTampered)
	}
	if manifest.Usage.FileCount != len(manifest.Files)+1 || manifest.Usage.OutputBytes != descriptorBytes+int64(len(manifestBody)) || manifest.Usage.OutputBytes > manifest.Budgets.MaxOutputBytes {
		return Ref{}, fmt.Errorf("%w: manifest accounting is invalid", ErrTampered)
	}

	ref, err := ReadReference(root)
	if err != nil || (expectedRunUID != "" && ref.RunUID != expectedRunUID) || ref.BaseSHA != expectedBaseSHA || ref.ResolvedSpecDigest != expectedSpecDigest || ref.Digest != expectedDigest {
		return Ref{}, fmt.Errorf("%w: reference identity is invalid", ErrTampered)
	}
	if err := verifyExactTree(root, expected); err != nil {
		return Ref{}, fmt.Errorf("%w: %v", ErrTampered, err)
	}
	return ref, nil
}

func materialize(root string, pack contextpack.Pack, ref Ref) error {
	staging, err := os.MkdirTemp(root, ".agw-context-")
	if err != nil {
		return fmt.Errorf("%w: create staging directory: %v", ErrOutput, err)
	}
	defer func() {
		_ = makeTreeWritable(staging)
		_ = os.RemoveAll(staging)
	}()
	for _, file := range pack.Files {
		if !safeRelative(file.Path) || file.Path == RefFileName {
			return fmt.Errorf("%w: compiler returned unsafe path %q", ErrOutput, file.Path)
		}
		target := filepath.Join(staging, filepath.FromSlash(file.Path))
		if err := ensureParent(staging, target); err != nil {
			return err
		}
		if err := writeSealedFile(target, file.Content); err != nil {
			return err
		}
	}
	refBody, err := marshalCanonical(ref)
	if err != nil {
		return err
	}
	if err := writeSealedFile(filepath.Join(staging, RefFileName), refBody); err != nil {
		return err
	}
	if err := sealTree(staging); err != nil {
		return err
	}
	if _, err := VerifyForRun(staging, ref.RunUID, pack.Digest, ref.ResolvedSpecDigest, ref.BaseSHA); err != nil {
		return fmt.Errorf("%w: staged output failed verification: %v", ErrOutput, err)
	}
	// The staged children are sealed, but its parent must remain writable long
	// enough to atomically promote those children into the mounted volume.
	if err := os.Chmod(staging, 0755); err != nil {
		return fmt.Errorf("%w: prepare output promotion: %v", ErrOutput, err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("%w: read staging output: %v", ErrOutput, err)
	}
	promoted := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !safeRelative(name) || strings.Contains(name, "/") {
			return fmt.Errorf("%w: staging entry is unsafe", ErrOutput)
		}
		if err := os.Rename(filepath.Join(staging, name), filepath.Join(root, name)); err != nil {
			for _, previous := range promoted {
				_ = os.RemoveAll(filepath.Join(root, previous))
			}
			return fmt.Errorf("%w: promote output: %v", ErrOutput, err)
		}
		promoted = append(promoted, name)
	}
	return nil
}

type repositoryLoader struct {
	root    string
	baseSHA string
}

func (l repositoryLoader) Load(baseSHA, sourcePath string) (policycontract.LoadedScript, error) {
	if baseSHA != l.baseSHA || !safeRelative(sourcePath) {
		return policycontract.LoadedScript{}, fmt.Errorf("%w: policy source is not bound to the pristine base", ErrInvalidInput)
	}
	filePath := filepath.Join(l.root, filepath.FromSlash(sourcePath))
	info, err := os.Lstat(filePath)
	if err != nil {
		return policycontract.LoadedScript{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return policycontract.LoadedScript{BaseSHA: baseSHA, Path: sourcePath, Kind: policycontract.FileKindSymlink}, nil
	}
	if !info.Mode().IsRegular() {
		return policycontract.LoadedScript{BaseSHA: baseSHA, Path: sourcePath, Kind: policycontract.FileKindSpecial}, nil
	}
	if info.Size() < 1 || info.Size() > policycontract.MaxScriptBytes {
		return policycontract.LoadedScript{}, fmt.Errorf("%w: policy script is empty or oversized", ErrInvalidInput)
	}
	body, err := readRequiredText(filePath, policycontract.MaxScriptBytes)
	if err != nil {
		return policycontract.LoadedScript{}, err
	}
	return policycontract.LoadedScript{BaseSHA: baseSHA, Path: sourcePath, Kind: policycontract.FileKindRegular, Bytes: body, Digest: digest(body)}, nil
}

func strategies(spec v1alpha1.ContextStrategySpec) contextpack.ContextStrategies {
	output := contextpack.ContextStrategies{Lexical: "local-ripgrep"}
	if spec.RepoMap != nil {
		output.RepoMap = spec.RepoMap.Kind
	}
	if spec.Symbols != nil {
		output.Symbols = spec.Symbols.Kind
	}
	if spec.History != nil {
		output.History = spec.History.Kind
	}
	return output
}

func producerSettings(spec v1alpha1.ContextStrategySpec) ProducerSettings {
	settings := ProducerSettings{Lexical: true}
	if spec.RepoMap != nil {
		settings.RepoMapBudget = int64(spec.RepoMap.Budget)
	}
	if spec.Symbols != nil {
		settings.SymbolLanguages = append([]string(nil), spec.Symbols.Languages...)
	}
	if spec.History != nil {
		settings.HistoryDepth = int(spec.History.Depth)
	}
	sort.Strings(settings.SymbolLanguages)
	return settings
}

func budgets(spec v1alpha1.ContextStrategySpec) contextpack.Budgets {
	maxBytes := spec.Budget.MaxBytes
	maxInput := maxBytes
	if maxInput > policycontract.MaxInputBytes {
		maxInput = policycontract.MaxInputBytes
	}
	maxFile := maxBytes
	if maxFile > 1<<20 {
		maxFile = 1 << 20
	}
	return contextpack.Budgets{
		MaxInputBytes:  maxInput,
		MaxOutputBytes: maxBytes,
		MaxFileBytes:   maxFile,
		MaxFiles:       1024,
		MaxTokens:      int64(spec.Budget.TotalTokens),
		MaxEntries:     4096,
	}
}

func clonePolicies(input []resolved.PolicySnapshot) []resolved.PolicySnapshot {
	output := make([]resolved.PolicySnapshot, len(input))
	for index, policy := range input {
		output[index] = policy
		output[index].Spec = *policy.Spec.DeepCopy()
		output[index].Spec.Rules = append([]v1alpha1.PolicyRule(nil), policy.Spec.Rules...)
		for ruleIndex := range output[index].Spec.Rules {
			if policy.Spec.Rules[ruleIndex].Check != nil {
				check := *policy.Spec.Rules[ruleIndex].Check
				output[index].Spec.Rules[ruleIndex].Check = &check
			}
		}
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Reference.Name < output[j].Reference.Name })
	return output
}

func validateConfig(config Config) error {
	if len(config.InputJSON) == 0 || len(config.InputJSON) > maxContractBytes || !validRunUID(config.ExpectedRunUID) || !canonical.ValidDigest(config.ExpectedSpecDigest) || !resolved.ValidBaseSHA(config.ExpectedBaseSHA) {
		return fmt.Errorf("%w: contract identity is missing or oversized", ErrInvalidInput)
	}
	for name, value := range map[string]string{"base": config.BaseDir, "skills": config.SkillsDir, "output": config.OutputDir} {
		if !absoluteClean(value) || value == "/" {
			return fmt.Errorf("%w: %s directory is not absolute and canonical", ErrInvalidInput, name)
		}
	}
	if config.BaseDir == config.OutputDir || config.SkillsDir == config.OutputDir {
		return fmt.Errorf("%w: source and output directories must be distinct", ErrInvalidInput)
	}
	return nil
}

func requireDirectory(name string, allowNonEmpty bool) error {
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: directory %q is unavailable", ErrInvalidInput, name)
	}
	if !allowNonEmpty {
		entries, err := os.ReadDir(name)
		if err != nil || len(entries) != 0 {
			return fmt.Errorf("%w: directory %q is not empty", ErrOutput, name)
		}
	}
	return nil
}

func requireEmptyDirectory(name string) error { return requireDirectory(name, false) }

func readOptionalText(name string, max int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: optional file %q is unsafe", ErrInvalidInput, name)
	}
	return readRequiredText(name, max)
}

func readRequiredText(name string, max int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > max {
		return nil, fmt.Errorf("%w: file %q is unavailable or oversized", ErrInvalidInput, name)
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: open file %q: %v", ErrInvalidInput, name, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxReadLimit(max)))
	if err != nil || int64(len(body)) > max {
		return nil, fmt.Errorf("%w: read file %q safely", ErrInvalidInput, name)
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("%w: file %q is not valid text", ErrInvalidInput, name)
	}
	return body, nil
}

func maxReadLimit(max int64) int64 {
	if max < 1 || max > maxReadBytes {
		return maxReadBytes
	}
	return max + 1
}

func ensureParent(root, target string) error {
	parent := filepath.Dir(target)
	if !within(root, parent) {
		return fmt.Errorf("%w: output path escapes root", ErrOutput)
	}
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("%w: create output directory: %v", ErrOutput, err)
	}
	return nil
}

func writeSealedFile(name string, body []byte) error {
	if len(body) > maxContractBytes && name == RefFileName {
		return fmt.Errorf("%w: reference is oversized", ErrOutput)
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return fmt.Errorf("%w: create %q: %v", ErrOutput, name, err)
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return fmt.Errorf("%w: write %q: %v", ErrOutput, name, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("%w: sync %q: %v", ErrOutput, name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%w: close %q: %v", ErrOutput, name, err)
	}
	if err := os.Chmod(name, 0444); err != nil {
		return fmt.Errorf("%w: seal %q: %v", ErrOutput, name, err)
	}
	return nil
}

func sealTree(root string) error {
	return filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: output contains symlink", ErrOutput)
		}
		if info.IsDir() {
			// Directories need write permission on the staging parent while the
			// top-level trees are promoted atomically. They are created by the
			// namespaced root context init and are mounted read-only into the
			// agent, so the agent cannot use this permission.
			return os.Chmod(name, 0755)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
			return fmt.Errorf("%w: output contains writable or special file", ErrOutput)
		}
		return os.Chmod(name, 0444)
	})
}

func verifyExactTree(root string, expected map[string]struct{}) error {
	seen := make(map[string]struct{}, len(expected))
	err := filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if name == root {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe output entry")
		}
		if info.IsDir() {
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative) + "/"
			allowed := false
			for expectedPath := range expected {
				if strings.HasPrefix(expectedPath, relative) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("unexpected output directory %q", strings.TrimSuffix(relative, "/"))
			}
			return nil
		}
		if info.Mode().Perm()&0222 != 0 {
			return fmt.Errorf("unsafe writable output file")
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, ok := expected[relative]; !ok {
			return fmt.Errorf("unexpected output file %q", relative)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("output file set is incomplete")
	}
	return nil
}

func decodeManifest(body []byte) (contextpack.Manifest, error) {
	if strictjson.ValidateObject(body) != nil {
		return contextpack.Manifest{}, ErrTampered
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest contextpack.Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return contextpack.Manifest{}, err
	}
	return manifest, nil
}

func decodeRef(body []byte) (Ref, error) {
	if strictjson.ValidateObject(body) != nil {
		return Ref{}, ErrTampered
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) {
		return Ref{}, ErrTampered
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var ref Ref
	if err := decoder.Decode(&ref); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func marshalCanonical(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal JSON: %v", ErrOutput, err)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize JSON: %v", ErrOutput, err)
	}
	return normalized, nil
}

func makeTreeWritable(root string) error {
	return filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(name, 0755)
		}
		return os.Chmod(name, 0644)
	})
}

func validateText(value string, required bool, max int) error {
	if required && strings.TrimSpace(value) == "" || len(value) > max || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: text field is empty, oversized, or invalid", ErrInvalidInput)
	}
	return nil
}

func safeName(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
			return false
		}
	}
	return true
}

func validRunUID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func safeRelative(value string) bool {
	if value == "" || len(value) > maxPathBytes || strings.ContainsRune(value, 0) || strings.ContainsRune(value, '\\') || path.IsAbs(value) || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") || strings.Contains(value, "/../") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func absoluteClean(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsRune(value, 0)
}

func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func digest(body []byte) string {
	hash := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(hash[:])
}
