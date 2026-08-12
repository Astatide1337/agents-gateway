// Package contextpack compiles resolved agent inputs into a bounded, standard
// file tree suitable for a coding-harness sandbox.
//
// The compiler is deliberately pure: it does not read the repository, invoke
// Git, contact Kubernetes, or fetch skills. Callers provide already-resolved
// and already-verified material. The returned Pack is an in-memory snapshot;
// a later initContainer can materialize it read-only and record the pack
// digest in AgentRun status.
package contextpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// SchemaVersion identifies the wire contract for compiled packs.
	SchemaVersion = "agents.astatide.com/contextpack/v1"

	manifestPath = ".agw/context/manifest.json"
	agentsPath   = "AGENTS.md"

	// PolicyContractPath is the sealed, credential-free contract compiled once
	// by policycontract. It is kept in this package so the ContextPack tree has
	// one stable path without making contextpack depend on policycontract.
	PolicyContractPath = ".agw/context/policy-contract.json"

	maxNameBytes      = 63
	maxTextBytes      = 1 << 20
	maxEntryTextBytes = 256 << 10
	maxLineNumber     = 1_000_000_000
	// These are fixed safety ceilings in addition to caller-provided budgets.
	// They prevent a malformed or overly permissive caller budget from turning
	// admission into an unbounded in-memory operation.
	maxCollectionItems  = 4096
	maxPolicySkillItems = 512
)

var (
	// ErrInvalidInput identifies malformed or unsafe compiler input.
	ErrInvalidInput = errors.New("contextpack: invalid input")
	// ErrBudgetExceeded identifies a hard budget that cannot be satisfied by
	// deterministic omission of optional context entries.
	ErrBudgetExceeded = errors.New("contextpack: budget exceeded")
)

// Severity is the enforcement level of a policy rule.
type Severity string

const (
	SeverityBlocking Severity = "blocking"
	SeverityAdvisory Severity = "advisory"
)

// Input is the resolved, credential-free material used to build a ContextPack.
// The caller is responsible for sourcing policy scripts from the pristine
// baseSHA and for verifying skill digests before calling Compile.
type Input struct {
	BaseSHA            string       `json:"baseSHA"`
	ResolvedSpecDigest string       `json:"resolvedSpecDigest"`
	Task               string       `json:"task"`
	Instructions       string       `json:"instructions,omitempty"`
	ExistingAgentsMD   string       `json:"existingAgentsMD,omitempty"`
	Boundaries         Boundaries   `json:"boundaries"`
	Policies           []PolicyRule `json:"policies,omitempty"`
	// PolicyContractManifest is the exact canonical manifest emitted by the
	// policycontract compiler. It is copied byte-for-byte into the pack so
	// broker and verifier consumers can decode the same immutable contract.
	PolicyContractManifest []byte            `json:"policyContractManifest,omitempty"`
	Skills                 []Skill           `json:"skills,omitempty"`
	Lexical                []LexicalEntry    `json:"lexical,omitempty"`
	RepoMap                []RepoMapEntry    `json:"repoMap,omitempty"`
	Symbols                []SymbolEntry     `json:"symbols,omitempty"`
	History                []HistoryEntry    `json:"history,omitempty"`
	Strategies             ContextStrategies `json:"strategies"`
	Budgets                Budgets           `json:"budgets"`
}

// Boundaries are rendered into AGENTS.md. They are informational context for
// the harness; independent scope enforcement remains a Gate responsibility.
type Boundaries struct {
	AllowedPaths   []string `json:"allowedPaths,omitempty"`
	ForbiddenPaths []string `json:"forbiddenPaths,omitempty"`
}

// PolicyRule is one rule from the single Policy contract. ScriptPath and
// ScriptBytes are both required for a blocking rule. An advisory rule may omit
// both; when a script is present its bytes are copied exactly.
type PolicyRule struct {
	ID           string   `json:"id"`
	Severity     Severity `json:"severity"`
	Context      string   `json:"context,omitempty"`
	ScriptPath   string   `json:"scriptPath,omitempty"`
	ScriptBytes  []byte   `json:"scriptBytes,omitempty"`
	ScriptDigest string   `json:"scriptDigest,omitempty"`
}

// Skill is an already-verified Agent Skill. Body is the verified SKILL.md
// content. If Body is empty, ArtifactPath is required and a deterministic
// reference SKILL.md is emitted instead.
type Skill struct {
	Name         string `json:"name"`
	Digest       string `json:"digest"`
	Body         string `json:"body,omitempty"`
	ArtifactPath string `json:"artifactPath,omitempty"`
}

// LexicalEntry is bounded, credential-free metadata for one repository file.
// It deliberately contains no source text. Digest binds the metadata to the
// sealed checkout while the exact-search semantics remain local to the
// producer; callers never need to send repository contents to a service.
type LexicalEntry struct {
	Path      string `json:"path"`
	Extension string `json:"extension,omitempty"`
	Language  string `json:"language,omitempty"`
	Bytes     int64  `json:"bytes"`
	Lines     int64  `json:"lines"`
	Digest    string `json:"digest"`
}

// RepoMapEntry is one structural repository-map record.
type RepoMapEntry struct {
	Path      string `json:"path"`
	Kind      string `json:"kind,omitempty"`
	Signature string `json:"signature,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

// SymbolEntry is one language-server or structural symbol record.
type SymbolEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Signature string `json:"signature,omitempty"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
}

// HistoryEntry is a bounded, already-resolved Git history record.
type HistoryEntry struct {
	CommitSHA string   `json:"commitSHA"`
	Subject   string   `json:"subject,omitempty"`
	Paths     []string `json:"paths,omitempty"`
}

// ContextStrategies names the producers that supplied each optional context
// tier. A non-empty name with no entries is retained in the manifest as a
// zero-entry strategy record; this makes configured-but-empty context explicit.
type ContextStrategies struct {
	Lexical string `json:"lexical,omitempty"`
	RepoMap string `json:"repoMap,omitempty"`
	Symbols string `json:"symbols,omitempty"`
	History string `json:"history,omitempty"`
}

// Budgets are hard limits. All limits must be positive. Optional repository
// map, symbol, and history entries are omitted deterministically when the
// output limits are exhausted. Instructions, skills, and policy scripts are
// mandatory and cause compilation to fail if they cannot fit.
type Budgets struct {
	MaxInputBytes  int64 `json:"maxInputBytes"`
	MaxOutputBytes int64 `json:"maxOutputBytes"`
	MaxFileBytes   int64 `json:"maxFileBytes"`
	MaxFiles       int   `json:"maxFiles"`
	MaxTokens      int64 `json:"maxTokens"`
	MaxEntries     int   `json:"maxEntries"`
}

// File is one output file. Content is exact bytes to materialize. Digest is
// sha256:<lowercase hex> over Content.
type File struct {
	Path          string
	Content       []byte
	Digest        string
	SizeBytes     int64
	TokenEstimate int64
	Kind          string
}

// FileDescriptor is the credential-free provenance record embedded in the
// manifest. The manifest intentionally excludes its own descriptor to avoid a
// self-referential hash; Pack.Files includes it and Pack.Digest equals that
// descriptor's digest.
type FileDescriptor struct {
	Path          string `json:"path"`
	Digest        string `json:"digest"`
	SizeBytes     int64  `json:"sizeBytes"`
	TokenEstimate int64  `json:"tokenEstimate"`
	Kind          string `json:"kind"`
}

// PolicyDescriptor records the resolved policy rule and, when present, the
// output script descriptor.
type PolicyDescriptor struct {
	ID           string   `json:"id"`
	Severity     Severity `json:"severity"`
	Context      string   `json:"context,omitempty"`
	SourcePath   string   `json:"sourcePath,omitempty"`
	OutputPath   string   `json:"outputPath,omitempty"`
	ScriptDigest string   `json:"scriptDigest,omitempty"`
}

// SkillDescriptor records a verified skill without embedding credential
// material or artifact contents in the manifest.
type SkillDescriptor struct {
	Name           string `json:"name"`
	DeclaredDigest string `json:"declaredDigest"`
	ContentDigest  string `json:"contentDigest"`
	Source         string `json:"source"`
	ArtifactPath   string `json:"artifactPath,omitempty"`
	OutputPath     string `json:"outputPath"`
}

// StrategyDescriptor states which context strategy was configured and how
// many entries survived deterministic budget selection.
type StrategyDescriptor struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	OutputPath      string `json:"outputPath,omitempty"`
	IncludedEntries int    `json:"includedEntries"`
	OmittedEntries  int    `json:"omittedEntries"`
}

// lexicalSearch documents the local exact-search contract without embedding
// source contents or a tool-specific index. The producer implements this
// contract with deterministic traversal; a harness may use ripgrep semantics
// against the sealed checkout.
type lexicalSearch struct {
	Enumeration          string `json:"enumeration"`
	Exact                bool   `json:"exact"`
	CaseSensitiveDefault bool   `json:"caseSensitiveDefault"`
	RegexSupported       bool   `json:"regexSupported"`
	HiddenFiles          string `json:"hiddenFiles"`
}

// Omission records a whole optional entry omitted because of a hard budget.
type Omission struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Strategy string `json:"strategy"`
	Reason   string `json:"reason"`
}

// BudgetUsage is the final accounting for the returned pack. OutputBytes and
// FileCount include manifest.json.
type BudgetUsage struct {
	InputBytes    int64 `json:"inputBytes"`
	OutputBytes   int64 `json:"outputBytes"`
	FileCount     int   `json:"fileCount"`
	TokenEstimate int64 `json:"tokenEstimate"`
}

// Manifest is the canonical JSON provenance document. Files contains every
// output except the manifest itself; Pack.Files contains the complete set.
type Manifest struct {
	SchemaVersion        string               `json:"schemaVersion"`
	BaseSHA              string               `json:"baseSHA"`
	ResolvedSpecDigest   string               `json:"resolvedSpecDigest"`
	PolicyContractDigest string               `json:"policyContractDigest,omitempty"`
	Files                []FileDescriptor     `json:"files"`
	Policies             []PolicyDescriptor   `json:"policies,omitempty"`
	Skills               []SkillDescriptor    `json:"skills,omitempty"`
	Strategies           []StrategyDescriptor `json:"strategies,omitempty"`
	Omissions            []Omission           `json:"omissions,omitempty"`
	Budgets              Budgets              `json:"budgets"`
	Usage                BudgetUsage          `json:"usage"`
}

// Pack is the complete in-memory result. Digest is the digest of the
// canonical manifest bytes and therefore content-addresses every listed file.
type Pack struct {
	Files    []File
	Manifest Manifest
	Digest   string
}

// File returns a defensive copy of the named output file.
func (p Pack) File(name string) (File, bool) {
	for _, file := range p.Files {
		if file.Path == name {
			file.Content = append([]byte(nil), file.Content...)
			return file, true
		}
	}
	return File{}, false
}

// ManifestBytes returns the exact bytes emitted as .agw/context/manifest.json.
func (p Pack) ManifestBytes() ([]byte, error) {
	file, ok := p.File(manifestPath)
	if !ok {
		return nil, errors.New("contextpack: pack has no manifest")
	}
	return file.Content, nil
}

// Compile validates input and returns one deterministic, bounded ContextPack.
// No partial Pack is returned on failure. Input slice order is not semantic;
// policies, skills, and context records are normalized into stable order.
func Compile(input Input) (Pack, error) {
	resolved, err := normalizeInput(input)
	if err != nil {
		return Pack{}, err
	}
	if resolved.inputBytes > resolved.budgets.MaxInputBytes {
		return Pack{}, budgetError("maxInputBytes", resolved.inputBytes, resolved.budgets.MaxInputBytes)
	}

	selected := selection{
		lexical: append([]LexicalEntry(nil), resolved.lexical...),
		repoMap: append([]RepoMapEntry(nil), resolved.repoMap...),
		symbols: append([]SymbolEntry(nil), resolved.symbols...),
		history: append([]HistoryEntry(nil), resolved.history...),
	}
	omissions := make([]Omission, 0)

	for {
		candidate, issues, err := assemble(resolved, selected, omissions)
		if err != nil {
			return Pack{}, err
		}
		if len(issues) == 0 {
			return candidate, nil
		}

		entry, ok := lastOptionalEntry(selected, issues)
		if !ok {
			return Pack{}, budgetError(issues[0].metric, issues[0].actual, issues[0].limit)
		}
		removeOptionalEntry(&selected, entry)
		omissions = append(omissions, Omission{
			Kind:     entry.kind,
			Key:      entry.key,
			Strategy: entry.strategy,
			Reason:   issues[0].metric,
		})
		sort.Slice(omissions, func(i, j int) bool {
			if omissions[i].Kind != omissions[j].Kind {
				return omissions[i].Kind < omissions[j].Kind
			}
			return omissions[i].Key < omissions[j].Key
		})
	}
}

type resolvedInput struct {
	baseSHA                string
	resolvedSpecDigest     string
	task                   string
	instructions           string
	existingAgentsMD       string
	boundaries             Boundaries
	policies               []resolvedPolicy
	policyContractManifest []byte
	skills                 []resolvedSkill
	lexical                []LexicalEntry
	repoMap                []RepoMapEntry
	symbols                []SymbolEntry
	history                []HistoryEntry
	strategies             ContextStrategies
	budgets                Budgets
	inputBytes             int64
}

type resolvedPolicy struct {
	id           string
	severity     Severity
	context      string
	scriptPath   string
	scriptBytes  []byte
	scriptDigest string
}

type resolvedSkill struct {
	name           string
	declaredDigest string
	body           string
	artifactPath   string
	source         string
}

type selection struct {
	lexical []LexicalEntry
	repoMap []RepoMapEntry
	symbols []SymbolEntry
	history []HistoryEntry
}

type optionalEntry struct {
	kind     string
	key      string
	strategy string
}

type budgetIssue struct {
	metric string
	actual int64
	limit  int64
}

func normalizeInput(input Input) (resolvedInput, error) {
	if !validBaseSHA(input.BaseSHA) {
		return resolvedInput{}, invalid("baseSHA must be 40 or 64 lowercase hexadecimal characters")
	}
	if !validDigest(input.ResolvedSpecDigest) {
		return resolvedInput{}, invalid("resolvedSpecDigest must be sha256:<64 lowercase hex characters>")
	}
	if err := validateText("task", input.Task, true, maxTextBytes); err != nil {
		return resolvedInput{}, err
	}
	if err := validateText("instructions", input.Instructions, false, maxTextBytes); err != nil {
		return resolvedInput{}, err
	}
	if err := validateText("existingAgentsMD", input.ExistingAgentsMD, false, maxTextBytes); err != nil {
		return resolvedInput{}, err
	}
	if len(input.PolicyContractManifest) > 8<<20 {
		return resolvedInput{}, invalid("policy contract manifest exceeds the fixed safety ceiling")
	}
	if len(input.PolicyContractManifest) > 0 {
		if err := validateBytes("policy contract manifest", input.PolicyContractManifest, 8<<20); err != nil {
			return resolvedInput{}, err
		}
	}
	if strings.TrimSpace(input.Task) == "" {
		return resolvedInput{}, invalid("task must not be empty")
	}
	if err := validateBudgets(input.Budgets); err != nil {
		return resolvedInput{}, err
	}
	if err := validateCollectionCounts(input, input.Budgets.MaxEntries); err != nil {
		return resolvedInput{}, err
	}

	boundaries, err := normalizeBoundaries(input.Boundaries)
	if err != nil {
		return resolvedInput{}, err
	}
	policies, err := normalizePolicies(input.Policies)
	if err != nil {
		return resolvedInput{}, err
	}
	skills, err := normalizeSkills(input.Skills)
	if err != nil {
		return resolvedInput{}, err
	}
	lexical, err := normalizeLexical(input.Lexical)
	if err != nil {
		return resolvedInput{}, err
	}
	repoMap, err := normalizeRepoMap(input.RepoMap)
	if err != nil {
		return resolvedInput{}, err
	}
	symbols, err := normalizeSymbols(input.Symbols)
	if err != nil {
		return resolvedInput{}, err
	}
	history, err := normalizeHistory(input.History)
	if err != nil {
		return resolvedInput{}, err
	}
	strategies, err := normalizeStrategies(input.Strategies, len(lexical), len(repoMap), len(symbols), len(history))
	if err != nil {
		return resolvedInput{}, err
	}

	resolved := resolvedInput{
		baseSHA:                input.BaseSHA,
		resolvedSpecDigest:     input.ResolvedSpecDigest,
		task:                   input.Task,
		instructions:           input.Instructions,
		existingAgentsMD:       input.ExistingAgentsMD,
		boundaries:             boundaries,
		policies:               policies,
		policyContractManifest: append([]byte(nil), input.PolicyContractManifest...),
		skills:                 skills,
		lexical:                lexical,
		repoMap:                repoMap,
		symbols:                symbols,
		history:                history,
		strategies:             strategies,
		budgets:                input.Budgets,
	}

	inputBytes, err := measureInput(resolved)
	if err != nil {
		return resolvedInput{}, err
	}
	resolved.inputBytes = inputBytes
	return resolved, nil
}

func validateBudgets(b Budgets) error {
	if b.MaxInputBytes <= 0 || b.MaxOutputBytes <= 0 || b.MaxFileBytes <= 0 || b.MaxTokens <= 0 {
		return invalid("all byte and token budgets must be positive")
	}
	if b.MaxFiles < 2 || b.MaxEntries <= 0 {
		return invalid("maxFiles must be at least 2 and maxEntries must be positive")
	}
	return nil
}

func validateCollectionCounts(input Input, limit int) error {
	counts := []struct {
		name  string
		count int
	}{
		{"policies", len(input.Policies)},
		{"skills", len(input.Skills)},
		{"lexical", len(input.Lexical)},
		{"repoMap", len(input.RepoMap)},
		{"symbols", len(input.Symbols)},
		{"history", len(input.History)},
	}
	for _, item := range counts {
		if item.count > maxCollectionItems {
			return invalid(fmt.Sprintf("%s exceeds maxEntries", item.name))
		}
	}
	optionalTotal := len(input.Lexical) + len(input.RepoMap) + len(input.Symbols) + len(input.History)
	if optionalTotal > limit || optionalTotal > maxCollectionItems {
		return invalid("optional context entries exceed maxEntries")
	}
	if len(input.Policies)+len(input.Skills) > maxPolicySkillItems {
		return invalid("policies and skills exceed fixed safety ceiling")
	}
	if len(input.Policies)+len(input.Skills)+optionalTotal > limit {
		return invalid("all input entries exceed maxEntries")
	}
	return nil
}

func normalizeBoundaries(input Boundaries) (Boundaries, error) {
	allowed, err := normalizePathList("boundaries.allowedPaths", input.AllowedPaths, true)
	if err != nil {
		return Boundaries{}, err
	}
	forbidden, err := normalizePathList("boundaries.forbiddenPaths", input.ForbiddenPaths, true)
	if err != nil {
		return Boundaries{}, err
	}
	return Boundaries{AllowedPaths: allowed, ForbiddenPaths: forbidden}, nil
}

func normalizePolicies(input []PolicyRule) ([]resolvedPolicy, error) {
	output := make([]resolvedPolicy, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, rule := range input {
		id, err := safeName(rule.ID, fmt.Sprintf("policies[%d].id", index))
		if err != nil {
			return nil, err
		}
		if _, exists := seen[id]; exists {
			return nil, invalid(fmt.Sprintf("duplicate normalized policy id %q", id))
		}
		seen[id] = struct{}{}
		if rule.Severity != SeverityBlocking && rule.Severity != SeverityAdvisory {
			return nil, invalid(fmt.Sprintf("policy %q has invalid severity", id))
		}
		if err := validateText("policy context", rule.Context, false, maxEntryTextBytes); err != nil {
			return nil, err
		}
		scriptPath := ""
		if rule.ScriptPath != "" {
			scriptPath, err = safeRelativePath(rule.ScriptPath, false)
			if err != nil {
				return nil, fmt.Errorf("policy %q: %w", id, err)
			}
		}
		if scriptPath == "" && len(rule.ScriptBytes) > 0 {
			return nil, invalid(fmt.Sprintf("policy %q supplies script bytes without scriptPath", id))
		}
		if scriptPath != "" && len(rule.ScriptBytes) == 0 {
			return nil, invalid(fmt.Sprintf("policy %q supplies scriptPath without script bytes", id))
		}
		if rule.Severity == SeverityBlocking && scriptPath == "" {
			return nil, invalid(fmt.Sprintf("blocking policy %q requires a deterministic check script", id))
		}
		var scriptBytes []byte
		scriptDigest := ""
		if len(rule.ScriptBytes) > 0 {
			if err := validateBytes(fmt.Sprintf("policy %q script", id), rule.ScriptBytes, maxEntryTextBytes); err != nil {
				return nil, err
			}
			scriptBytes = append([]byte(nil), rule.ScriptBytes...)
			scriptDigest = digest(scriptBytes)
			if rule.ScriptDigest != "" {
				if !validDigest(rule.ScriptDigest) || rule.ScriptDigest != scriptDigest {
					return nil, invalid(fmt.Sprintf("policy %q scriptDigest does not match script bytes", id))
				}
			}
		} else if rule.ScriptDigest != "" {
			return nil, invalid(fmt.Sprintf("policy %q supplies scriptDigest without script bytes", id))
		}
		output = append(output, resolvedPolicy{id: id, severity: rule.Severity, context: rule.Context, scriptPath: scriptPath, scriptBytes: scriptBytes, scriptDigest: scriptDigest})
	}
	sort.Slice(output, func(i, j int) bool { return output[i].id < output[j].id })
	return output, nil
}

func normalizeSkills(input []Skill) ([]resolvedSkill, error) {
	output := make([]resolvedSkill, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, skill := range input {
		name, err := safeName(skill.Name, fmt.Sprintf("skills[%d].name", index))
		if err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, invalid(fmt.Sprintf("duplicate normalized skill name %q", name))
		}
		seen[name] = struct{}{}
		if !validDigest(skill.Digest) {
			return nil, invalid(fmt.Sprintf("skill %q has malformed digest", name))
		}
		if skill.Body != "" && skill.ArtifactPath != "" {
			return nil, invalid(fmt.Sprintf("skill %q must provide body or artifactPath, not both", name))
		}
		if skill.Body == "" && skill.ArtifactPath == "" {
			return nil, invalid(fmt.Sprintf("skill %q must provide body or artifactPath", name))
		}
		if skill.Body != "" {
			if err := validateText(fmt.Sprintf("skill %q body", name), skill.Body, true, maxEntryTextBytes); err != nil {
				return nil, err
			}
			output = append(output, resolvedSkill{name: name, declaredDigest: skill.Digest, body: skill.Body, source: "body"})
			continue
		}
		artifactPath, err := safeRelativePath(skill.ArtifactPath, false)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}
		output = append(output, resolvedSkill{name: name, declaredDigest: skill.Digest, artifactPath: artifactPath, source: "artifact-reference"})
	}
	sort.Slice(output, func(i, j int) bool { return output[i].name < output[j].name })
	return output, nil
}

func normalizeLexical(input []LexicalEntry) ([]LexicalEntry, error) {
	output := make([]LexicalEntry, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, entry := range input {
		filePath, err := safeRelativePath(entry.Path, false)
		if err != nil {
			return nil, fmt.Errorf("lexical[%d]: %w", index, err)
		}
		if _, exists := seen[filePath]; exists {
			return nil, invalid(fmt.Sprintf("duplicate lexical path %q", filePath))
		}
		seen[filePath] = struct{}{}
		if err := validateText("lexical extension", entry.Extension, false, 32); err != nil {
			return nil, err
		}
		if err := validateText("lexical language", entry.Language, false, 64); err != nil {
			return nil, err
		}
		if entry.Bytes < 0 || entry.Lines < 0 {
			return nil, invalid(fmt.Sprintf("lexical entry %q has negative size or line count", filePath))
		}
		if !validDigest(entry.Digest) {
			return nil, invalid(fmt.Sprintf("lexical entry %q has malformed digest", filePath))
		}
		output = append(output, LexicalEntry{Path: filePath, Extension: entry.Extension, Language: entry.Language, Bytes: entry.Bytes, Lines: entry.Lines, Digest: entry.Digest})
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Path < output[j].Path })
	return output, nil
}

func normalizeRepoMap(input []RepoMapEntry) ([]RepoMapEntry, error) {
	output := make([]RepoMapEntry, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, entry := range input {
		filePath, err := safeRelativePath(entry.Path, false)
		if err != nil {
			return nil, fmt.Errorf("repoMap[%d]: %w", index, err)
		}
		if _, exists := seen[filePath]; exists {
			return nil, invalid(fmt.Sprintf("duplicate repo-map path %q", filePath))
		}
		seen[filePath] = struct{}{}
		if err := validateText("repo-map kind", entry.Kind, false, 256); err != nil {
			return nil, err
		}
		if err := validateText("repo-map signature", entry.Signature, false, maxEntryTextBytes); err != nil {
			return nil, err
		}
		if err := validateText("repo-map summary", entry.Summary, false, maxEntryTextBytes); err != nil {
			return nil, err
		}
		output = append(output, RepoMapEntry{Path: filePath, Kind: entry.Kind, Signature: entry.Signature, Summary: entry.Summary})
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Path < output[j].Path })
	return output, nil
}

func normalizeSymbols(input []SymbolEntry) ([]SymbolEntry, error) {
	output := make([]SymbolEntry, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, entry := range input {
		filePath, err := safeRelativePath(entry.Path, false)
		if err != nil {
			return nil, fmt.Errorf("symbols[%d]: %w", index, err)
		}
		if err := validateText("symbol name", entry.Name, true, 512); err != nil {
			return nil, err
		}
		if err := validateText("symbol kind", entry.Kind, true, 256); err != nil {
			return nil, err
		}
		if err := validateText("symbol signature", entry.Signature, false, maxEntryTextBytes); err != nil {
			return nil, err
		}
		if entry.Line < 0 || entry.Line > maxLineNumber || entry.Column < 0 || entry.Column > maxLineNumber {
			return nil, invalid("symbol line and column are outside bounded range")
		}
		key := fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s", filePath, entry.Line, entry.Column, entry.Kind, entry.Name)
		if _, exists := seen[key]; exists {
			return nil, invalid(fmt.Sprintf("duplicate symbol %q", key))
		}
		seen[key] = struct{}{}
		output = append(output, SymbolEntry{Path: filePath, Name: entry.Name, Kind: entry.Kind, Signature: entry.Signature, Line: entry.Line, Column: entry.Column})
	}
	sort.Slice(output, func(i, j int) bool {
		left, right := output[i], output[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Column != right.Column {
			return left.Column < right.Column
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Name < right.Name
	})
	return output, nil
}

func normalizeHistory(input []HistoryEntry) ([]HistoryEntry, error) {
	output := make([]HistoryEntry, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, entry := range input {
		if !validCommitSHA(entry.CommitSHA) {
			return nil, invalid(fmt.Sprintf("history[%d] has malformed commit SHA", index))
		}
		if err := validateText("history subject", entry.Subject, false, maxEntryTextBytes); err != nil {
			return nil, err
		}
		paths, err := normalizePathList("history.paths", entry.Paths, false)
		if err != nil {
			return nil, err
		}
		key := entry.CommitSHA + "\x00" + entry.Subject
		if _, exists := seen[key]; exists {
			return nil, invalid(fmt.Sprintf("duplicate history commit %q", entry.CommitSHA))
		}
		seen[key] = struct{}{}
		output = append(output, HistoryEntry{CommitSHA: entry.CommitSHA, Subject: entry.Subject, Paths: paths})
	}
	sort.Slice(output, func(i, j int) bool {
		if output[i].CommitSHA != output[j].CommitSHA {
			return output[i].CommitSHA < output[j].CommitSHA
		}
		return output[i].Subject < output[j].Subject
	})
	return output, nil
}

func normalizeStrategies(input ContextStrategies, lexicalCount, repoMapCount, symbolCount, historyCount int) (ContextStrategies, error) {
	values := []struct {
		field string
		value string
		count int
	}{
		{"lexical strategy", input.Lexical, lexicalCount},
		{"repoMap strategy", input.RepoMap, repoMapCount},
		{"symbols strategy", input.Symbols, symbolCount},
		{"history strategy", input.History, historyCount},
	}
	for _, item := range values {
		if item.value != "" {
			if _, err := safeName(item.value, item.field); err != nil {
				return ContextStrategies{}, err
			}
		} else if item.count > 0 {
			return ContextStrategies{}, invalid(fmt.Sprintf("%s is required when entries are supplied", item.field))
		}
	}
	return ContextStrategies{Lexical: normalizeOptionalName(input.Lexical), RepoMap: normalizeOptionalName(input.RepoMap), Symbols: normalizeOptionalName(input.Symbols), History: normalizeOptionalName(input.History)}, nil
}

func normalizeOptionalName(value string) string {
	if value == "" {
		return ""
	}
	name, _ := safeName(value, "strategy")
	return name
}

func normalizePathList(label string, input []string, allowGlob bool) ([]string, error) {
	output := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, value := range input {
		clean, err := safeRelativePath(value, allowGlob)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", label, index, err)
		}
		if _, exists := seen[clean]; exists {
			return nil, invalid(fmt.Sprintf("duplicate normalized path %q in %s", clean, label))
		}
		seen[clean] = struct{}{}
		output = append(output, clean)
	}
	sort.Strings(output)
	return output, nil
}

func validateText(label, value string, required bool, maxBytes int) error {
	if required && value == "" {
		return invalid(label + " is required")
	}
	if !utf8.ValidString(value) {
		return invalid(label + " is not valid UTF-8")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return invalid(label + " contains NUL")
	}
	if len(value) > maxBytes {
		return invalid(fmt.Sprintf("%s exceeds %d bytes", label, maxBytes))
	}
	for _, r := range value {
		if r == '\r' || r == '\n' || r == '\t' || !unicode.IsControl(r) {
			continue
		}
		return invalid(fmt.Sprintf("%s contains control character U+%04X", label, r))
	}
	return nil
}

func validateBytes(label string, value []byte, maxBytes int) error {
	if !utf8.Valid(value) {
		return invalid(label + " is not valid UTF-8")
	}
	if strings.IndexByte(string(value), 0) >= 0 {
		return invalid(label + " contains NUL")
	}
	if len(value) > maxBytes {
		return invalid(fmt.Sprintf("%s exceeds %d bytes", label, maxBytes))
	}
	return nil
}

func safeName(value, label string) (string, error) {
	if err := validateText(label, value, true, maxNameBytes); err != nil {
		return "", err
	}
	if strings.ContainsAny(value, `/\\`) || value == "." || value == ".." {
		return "", invalid(fmt.Sprintf("%s contains a path separator or traversal", label))
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" || len(name) > maxNameBytes {
		return "", invalid(fmt.Sprintf("%s does not normalize to a bounded safe name", label))
	}
	return name, nil
}

func safeRelativePath(value string, allowGlob bool) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", invalid("path must be non-empty valid UTF-8 without NUL")
	}
	if strings.ContainsRune(value, '\\') || path.IsAbs(value) || strings.HasPrefix(value, "./") || value == "." {
		return "", invalid(fmt.Sprintf("unsafe relative path %q", value))
	}
	if strings.Contains(value, "://") || (len(value) >= 2 && value[1] == ':') {
		return "", invalid(fmt.Sprintf("unsafe URI or drive path %q", value))
	}
	if path.Clean(value) != value {
		return "", invalid(fmt.Sprintf("path is not normalized %q", value))
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", invalid(fmt.Sprintf("unsafe path segment in %q", value))
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return "", invalid(fmt.Sprintf("path contains control character in %q", value))
			}
			if !allowGlob && strings.ContainsRune("*?[]{}", r) {
				return "", invalid(fmt.Sprintf("glob metacharacter is not allowed in %q", value))
			}
		}
	}
	return value, nil
}

func measureInput(input resolvedInput) (int64, error) {
	// JSON provides a deterministic, conservative accounting representation. It
	// base64-encodes scripts, so a budget never undercounts supplied bytes.
	type policyWire struct {
		ID           string   `json:"id"`
		Severity     Severity `json:"severity"`
		Context      string   `json:"context,omitempty"`
		ScriptPath   string   `json:"scriptPath,omitempty"`
		ScriptBytes  []byte   `json:"scriptBytes,omitempty"`
		ScriptDigest string   `json:"scriptDigest,omitempty"`
	}
	type skillWire struct {
		Name           string `json:"name"`
		DeclaredDigest string `json:"declaredDigest"`
		Body           string `json:"body,omitempty"`
		ArtifactPath   string `json:"artifactPath,omitempty"`
		Source         string `json:"source"`
	}
	wire := struct {
		BaseSHA                string
		ResolvedSpecDigest     string
		Task                   string
		Instructions           string
		ExistingAgentsMD       string
		Boundaries             Boundaries
		Policies               []policyWire
		PolicyContractManifest []byte
		Skills                 []skillWire
		Lexical                []LexicalEntry
		RepoMap                []RepoMapEntry
		Symbols                []SymbolEntry
		History                []HistoryEntry
		Strategies             ContextStrategies
		Budgets                Budgets
	}{
		BaseSHA:                input.baseSHA,
		ResolvedSpecDigest:     input.resolvedSpecDigest,
		Task:                   input.task,
		Instructions:           input.instructions,
		ExistingAgentsMD:       input.existingAgentsMD,
		Boundaries:             input.boundaries,
		Policies:               make([]policyWire, 0, len(input.policies)),
		PolicyContractManifest: input.policyContractManifest,
		Skills:                 make([]skillWire, 0, len(input.skills)),
		Lexical:                input.lexical,
		RepoMap:                input.repoMap,
		Symbols:                input.symbols,
		History:                input.history,
		Strategies:             input.strategies,
		Budgets:                input.budgets,
	}
	for _, policy := range input.policies {
		wire.Policies = append(wire.Policies, policyWire{ID: policy.id, Severity: policy.severity, Context: policy.context, ScriptPath: policy.scriptPath, ScriptBytes: policy.scriptBytes, ScriptDigest: policy.scriptDigest})
	}
	for _, skill := range input.skills {
		wire.Skills = append(wire.Skills, skillWire{Name: skill.name, DeclaredDigest: skill.declaredDigest, Body: skill.body, ArtifactPath: skill.artifactPath, Source: skill.source})
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return 0, fmt.Errorf("%w: measure input: %v", ErrInvalidInput, err)
	}
	return int64(len(encoded)), nil
}

func assemble(input resolvedInput, selected selection, omissions []Omission) (Pack, []budgetIssue, error) {
	files, policies, skills, strategies, err := renderFiles(input, selected)
	if err != nil {
		return Pack{}, nil, err
	}
	baseManifest := Manifest{
		SchemaVersion:        SchemaVersion,
		BaseSHA:              input.baseSHA,
		ResolvedSpecDigest:   input.resolvedSpecDigest,
		PolicyContractDigest: optionalDigest(input.policyContractManifest),
		Files:                descriptors(files),
		Policies:             policies,
		Skills:               skills,
		Strategies:           strategies,
		Omissions:            append([]Omission(nil), omissions...),
		Budgets:              input.budgets,
	}
	manifestBytes, usage, err := stabilizeManifest(baseManifest, files, input.inputBytes)
	if err != nil {
		return Pack{}, nil, err
	}
	manifestFile := makeFile(manifestPath, manifestBytes, "manifest")
	allFiles := append(append([]File(nil), files...), manifestFile)
	sort.Slice(allFiles, func(i, j int) bool { return allFiles[i].Path < allFiles[j].Path })
	issues := budgetIssues(allFiles, usage, input.budgets)
	if len(issues) > 0 {
		return Pack{}, issues, nil
	}
	manifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return Pack{}, nil, err
	}
	return Pack{Files: cloneFiles(allFiles), Manifest: manifest, Digest: manifestFile.Digest}, nil, nil
}

func renderFiles(input resolvedInput, selected selection) ([]File, []PolicyDescriptor, []SkillDescriptor, []StrategyDescriptor, error) {
	agents, err := renderAgents(input, selected)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	files := []File{makeFile(agentsPath, []byte(agents), "instructions")}
	if len(input.policyContractManifest) > 0 {
		files = append(files, makeFile(PolicyContractPath, input.policyContractManifest, "policy-contract"))
	}
	policyDescriptors := make([]PolicyDescriptor, 0, len(input.policies))
	for _, policy := range input.policies {
		descriptor := PolicyDescriptor{ID: policy.id, Severity: policy.severity, Context: policy.context, SourcePath: policy.scriptPath, ScriptDigest: policy.scriptDigest}
		if policy.scriptPath != "" {
			outputPath := path.Join(".agents/policies", policy.id, path.Base(policy.scriptPath))
			files = append(files, makeFile(outputPath, policy.scriptBytes, "policy-script"))
			descriptor.OutputPath = outputPath
		}
		policyDescriptors = append(policyDescriptors, descriptor)
	}

	skillDescriptors := make([]SkillDescriptor, 0, len(input.skills))
	for _, skill := range input.skills {
		body := skill.body
		if skill.source == "artifact-reference" {
			body = fmt.Sprintf("# Verified skill reference\n\nThis skill is supplied by a verified artifact.\n\n- digest: %s\n- artifact: %s\n", skill.declaredDigest, skill.artifactPath)
		}
		bodyBytes := []byte(body)
		outputPath := path.Join(".agents/skills", skill.name, "SKILL.md")
		files = append(files, makeFile(outputPath, bodyBytes, "skill"))
		skillDescriptors = append(skillDescriptors, SkillDescriptor{Name: skill.name, DeclaredDigest: skill.declaredDigest, ContentDigest: digest(bodyBytes), Source: skill.source, ArtifactPath: skill.artifactPath, OutputPath: outputPath})
	}

	strategyDescriptors := make([]StrategyDescriptor, 0, 4)
	if input.strategies.Lexical != "" {
		if len(selected.lexical) > 0 {
			body, err := marshalJSON(struct {
				SchemaVersion string         `json:"schemaVersion"`
				Strategy      string         `json:"strategy"`
				Search        lexicalSearch  `json:"search"`
				Entries       []LexicalEntry `json:"entries"`
			}{"agents.astatide.com/context/lexical-index/v1", input.strategies.Lexical, lexicalSearch{
				Enumeration:          "local-ripgrep-compatible",
				Exact:                true,
				CaseSensitiveDefault: true,
				RegexSupported:       true,
				HiddenFiles:          "included-when-not-ignored",
			}, selected.lexical})
			if err != nil {
				return nil, nil, nil, nil, err
			}
			files = append(files, makeFile(".agw/context/lexical-index.json", body, "lexical"))
		}
		strategyDescriptors = append(strategyDescriptors, StrategyDescriptor{Kind: "lexical", Name: input.strategies.Lexical, OutputPath: optionalPath(".agw/context/lexical-index.json", len(selected.lexical) > 0), IncludedEntries: len(selected.lexical), OmittedEntries: len(input.lexical) - len(selected.lexical)})
	}
	if input.strategies.RepoMap != "" {
		if len(selected.repoMap) > 0 {
			body, err := marshalJSON(struct {
				SchemaVersion string         `json:"schemaVersion"`
				Strategy      string         `json:"strategy"`
				Entries       []RepoMapEntry `json:"entries"`
			}{"agents.astatide.com/context/repo-map/v1", input.strategies.RepoMap, selected.repoMap})
			if err != nil {
				return nil, nil, nil, nil, err
			}
			files = append(files, makeFile(".agw/context/repo-map.json", body, "repo-map"))
		}
		strategyDescriptors = append(strategyDescriptors, StrategyDescriptor{Kind: "repo-map", Name: input.strategies.RepoMap, OutputPath: optionalPath(".agw/context/repo-map.json", len(selected.repoMap) > 0), IncludedEntries: len(selected.repoMap), OmittedEntries: len(input.repoMap) - len(selected.repoMap)})
	}
	if input.strategies.Symbols != "" {
		if len(selected.symbols) > 0 {
			body, err := marshalJSON(struct {
				SchemaVersion string        `json:"schemaVersion"`
				Strategy      string        `json:"strategy"`
				Entries       []SymbolEntry `json:"entries"`
			}{"agents.astatide.com/context/symbols/v1", input.strategies.Symbols, selected.symbols})
			if err != nil {
				return nil, nil, nil, nil, err
			}
			files = append(files, makeFile(".agw/context/symbols.json", body, "symbols"))
		}
		strategyDescriptors = append(strategyDescriptors, StrategyDescriptor{Kind: "symbols", Name: input.strategies.Symbols, OutputPath: optionalPath(".agw/context/symbols.json", len(selected.symbols) > 0), IncludedEntries: len(selected.symbols), OmittedEntries: len(input.symbols) - len(selected.symbols)})
	}
	if input.strategies.History != "" {
		if len(selected.history) > 0 {
			body, err := marshalJSON(struct {
				SchemaVersion string         `json:"schemaVersion"`
				Strategy      string         `json:"strategy"`
				Entries       []HistoryEntry `json:"entries"`
			}{"agents.astatide.com/context/history/v1", input.strategies.History, selected.history})
			if err != nil {
				return nil, nil, nil, nil, err
			}
			files = append(files, makeFile(".agw/context/history.json", body, "history"))
		}
		strategyDescriptors = append(strategyDescriptors, StrategyDescriptor{Kind: "history", Name: input.strategies.History, OutputPath: optionalPath(".agw/context/history.json", len(selected.history) > 0), IncludedEntries: len(selected.history), OmittedEntries: len(input.history) - len(selected.history)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, policyDescriptors, skillDescriptors, strategyDescriptors, nil
}

func renderAgents(input resolvedInput, selected selection) (string, error) {
	var b strings.Builder
	b.WriteString("# Agents Gateway Context\n\n")
	b.WriteString("This file is generated from an immutable resolved run contract. Follow the task and boundaries below. The independent Gate remains authoritative.\n\n")
	b.WriteString("## Task\n\n")
	b.WriteString(input.task)
	b.WriteString("\n\n")
	if input.instructions != "" {
		b.WriteString("## Instructions\n\n")
		b.WriteString(input.instructions)
		b.WriteString("\n\n")
	}
	if input.existingAgentsMD != "" {
		b.WriteString("## Repository-provided instructions\n\n")
		b.WriteString("<BEGIN REPOSITORY AGENTS.md>\n")
		b.WriteString(input.existingAgentsMD)
		if !strings.HasSuffix(input.existingAgentsMD, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("<END REPOSITORY AGENTS.md>\n\n")
	}
	b.WriteString("## Boundaries\n\n")
	b.WriteString("The allowed and forbidden patterns are also enforced independently by the Gate.\n\n")
	b.WriteString("Allowed paths:\n")
	if len(input.boundaries.AllowedPaths) == 0 {
		b.WriteString("- (not specified)\n")
	} else {
		for _, value := range input.boundaries.AllowedPaths {
			fmt.Fprintf(&b, "- `%s`\n", value)
		}
	}
	b.WriteString("Forbidden paths:\n")
	if len(input.boundaries.ForbiddenPaths) == 0 {
		b.WriteString("- (not specified)\n")
	} else {
		for _, value := range input.boundaries.ForbiddenPaths {
			fmt.Fprintf(&b, "- `%s`\n", value)
		}
	}
	b.WriteString("\n## Policy contract\n\n")
	if digest := optionalDigest(input.policyContractManifest); digest != "" {
		fmt.Fprintf(&b, "Immutable contract digest: `%s` (the broker self-check and independent Gate use this exact contract).\n\n", digest)
	}
	if len(input.policies) == 0 {
		b.WriteString("No policy rules were supplied.\n")
	} else {
		for _, policy := range input.policies {
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", policy.id, policy.severity, nonEmpty(policy.context, "No additional context."))
			if policy.scriptPath != "" {
				fmt.Fprintf(&b, "  - deterministic self-check: `.agents/policies/%s/%s`\n", policy.id, path.Base(policy.scriptPath))
			} else {
				b.WriteString("  - deterministic self-check: none (advisory only)\n")
			}
		}
	}
	b.WriteString("\n## Context files\n\n")
	b.WriteString("- `.agw/context/manifest.json` is the provenance and omission record.\n")
	if len(selected.lexical) > 0 {
		b.WriteString("- `.agw/context/lexical-index.json` contains local exact-search file metadata; source text remains in the sealed checkout.\n")
	}
	if len(selected.repoMap) > 0 {
		b.WriteString("- `.agw/context/repo-map.json` contains the bounded structural repository map.\n")
	}
	if len(selected.symbols) > 0 {
		b.WriteString("- `.agw/context/symbols.json` contains the bounded symbol records.\n")
	}
	if len(selected.history) > 0 {
		b.WriteString("- `.agw/context/history.json` contains the bounded history records.\n")
	}
	b.WriteString("Context entries omitted by hard budgets are recorded in the manifest; they are never silently truncated.\n\n")
	b.WriteString("## Skills\n\n")
	if len(input.skills) == 0 {
		b.WriteString("No skills were supplied.\n")
	} else {
		for _, skill := range input.skills {
			fmt.Fprintf(&b, "- `.agents/skills/%s/SKILL.md` (verified digest `%s`)\n", skill.name, skill.declaredDigest)
		}
	}
	if err := validateBytes("AGENTS.md", []byte(b.String()), maxTextBytes); err != nil {
		return "", err
	}
	return b.String(), nil
}

func stabilizeManifest(base Manifest, files []File, inputBytes int64) ([]byte, BudgetUsage, error) {
	manifest := base
	for iteration := 0; iteration < 16; iteration++ {
		body, err := marshalJSON(manifest)
		if err != nil {
			return nil, BudgetUsage{}, err
		}
		usage := BudgetUsage{InputBytes: inputBytes, OutputBytes: sumFileBytes(files) + int64(len(body)), FileCount: len(files) + 1, TokenEstimate: sumFileTokens(files) + estimateTokens(body)}
		if manifest.Usage == usage {
			return body, usage, nil
		}
		manifest.Usage = usage
	}
	return nil, BudgetUsage{}, invalid("manifest usage did not stabilize")
}

func decodeManifest(body []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: decode manifest: %v", ErrInvalidInput, err)
	}
	return manifest, nil
}

func descriptors(files []File) []FileDescriptor {
	output := make([]FileDescriptor, 0, len(files))
	for _, file := range files {
		output = append(output, FileDescriptor{Path: file.Path, Digest: file.Digest, SizeBytes: file.SizeBytes, TokenEstimate: file.TokenEstimate, Kind: file.Kind})
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Path < output[j].Path })
	return output
}

func makeFile(filePath string, body []byte, kind string) File {
	copyBody := append([]byte(nil), body...)
	return File{Path: filePath, Content: copyBody, Digest: digest(copyBody), SizeBytes: int64(len(copyBody)), TokenEstimate: estimateTokens(copyBody), Kind: kind}
}

func optionalDigest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return digest(body)
}

func cloneFiles(files []File) []File {
	output := make([]File, len(files))
	for index, file := range files {
		output[index] = file
		output[index].Content = append([]byte(nil), file.Content...)
	}
	return output
}

func budgetIssues(files []File, usage BudgetUsage, budgets Budgets) []budgetIssue {
	issues := make([]budgetIssue, 0)
	for _, file := range files {
		if file.SizeBytes > budgets.MaxFileBytes {
			issues = append(issues, budgetIssue{metric: "maxFileBytes:" + file.Path, actual: file.SizeBytes, limit: budgets.MaxFileBytes})
		}
	}
	if usage.FileCount > budgets.MaxFiles {
		issues = append(issues, budgetIssue{metric: "maxFiles", actual: int64(usage.FileCount), limit: int64(budgets.MaxFiles)})
	}
	if usage.OutputBytes > budgets.MaxOutputBytes {
		issues = append(issues, budgetIssue{metric: "maxOutputBytes", actual: usage.OutputBytes, limit: budgets.MaxOutputBytes})
	}
	if usage.TokenEstimate > budgets.MaxTokens {
		issues = append(issues, budgetIssue{metric: "maxTokens", actual: usage.TokenEstimate, limit: budgets.MaxTokens})
	}
	return issues
}

func lastOptionalEntry(selected selection, issues []budgetIssue) (optionalEntry, bool) {
	candidates := optionalEntries(selected)
	if len(candidates) == 0 {
		return optionalEntry{}, false
	}
	// Prefer an entry from an oversized context file. This preserves as much
	// independent context as possible while remaining deterministic.
	for index := len(issues) - 1; index >= 0; index-- {
		metric := issues[index].metric
		if !strings.HasPrefix(metric, "maxFileBytes:") {
			continue
		}
		filePath := strings.TrimPrefix(metric, "maxFileBytes:")
		kind := kindForContextPath(filePath)
		for candidateIndex := len(candidates) - 1; candidateIndex >= 0; candidateIndex-- {
			if candidates[candidateIndex].kind == kind {
				return candidates[candidateIndex], true
			}
		}
	}
	return candidates[len(candidates)-1], true
}

func optionalEntries(selected selection) []optionalEntry {
	entries := make([]optionalEntry, 0, len(selected.lexical)+len(selected.repoMap)+len(selected.symbols)+len(selected.history))
	for _, entry := range selected.lexical {
		entries = append(entries, optionalEntry{kind: "lexical", key: entry.Path, strategy: "lexical"})
	}
	for _, entry := range selected.repoMap {
		entries = append(entries, optionalEntry{kind: "repo-map", key: entry.Path, strategy: "repo-map"})
	}
	for _, entry := range selected.symbols {
		entries = append(entries, optionalEntry{kind: "symbols", key: fmt.Sprintf("%s:%d:%d:%s:%s", entry.Path, entry.Line, entry.Column, entry.Kind, entry.Name), strategy: "symbols"})
	}
	for _, entry := range selected.history {
		entries = append(entries, optionalEntry{kind: "history", key: entry.CommitSHA + ":" + entry.Subject, strategy: "history"})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].kind != entries[j].kind {
			return entries[i].kind < entries[j].kind
		}
		return entries[i].key < entries[j].key
	})
	return entries
}

func removeOptionalEntry(selected *selection, entry optionalEntry) {
	switch entry.kind {
	case "lexical":
		for index, value := range selected.lexical {
			if value.Path == entry.key {
				selected.lexical = append(selected.lexical[:index], selected.lexical[index+1:]...)
				return
			}
		}
	case "repo-map":
		for index, value := range selected.repoMap {
			if value.Path == entry.key {
				selected.repoMap = append(selected.repoMap[:index], selected.repoMap[index+1:]...)
				return
			}
		}
	case "symbols":
		for index, value := range selected.symbols {
			key := fmt.Sprintf("%s:%d:%d:%s:%s", value.Path, value.Line, value.Column, value.Kind, value.Name)
			if key == entry.key {
				selected.symbols = append(selected.symbols[:index], selected.symbols[index+1:]...)
				return
			}
		}
	case "history":
		for index, value := range selected.history {
			if value.CommitSHA+":"+value.Subject == entry.key {
				selected.history = append(selected.history[:index], selected.history[index+1:]...)
				return
			}
		}
	}
}

func kindForContextPath(filePath string) string {
	switch filePath {
	case ".agw/context/lexical-index.json":
		return "lexical"
	case ".agw/context/repo-map.json":
		return "repo-map"
	case ".agw/context/symbols.json":
		return "symbols"
	case ".agw/context/history.json":
		return "history"
	default:
		return ""
	}
}

func optionalPath(value string, include bool) string {
	if include {
		return value
	}
	return ""
}

func sumFileBytes(files []File) int64 {
	var total int64
	for _, file := range files {
		total += file.SizeBytes
	}
	return total
}

func sumFileTokens(files []File) int64 {
	var total int64
	for _, file := range files {
		total += file.TokenEstimate
	}
	return total
}

func estimateTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	return int64((utf8.RuneCount(body) + 3) / 4)
}

func marshalJSON(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal JSON: %v", ErrInvalidInput, err)
	}
	if !utf8.Valid(body) {
		return nil, invalid("generated JSON is not UTF-8")
	}
	return body, nil
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

func validCommitSHA(value string) bool {
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

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, message)
}

func budgetError(metric string, actual, limit int64) error {
	return fmt.Errorf("%w: %s=%d exceeds limit=%d", ErrBudgetExceeded, metric, actual, limit)
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
