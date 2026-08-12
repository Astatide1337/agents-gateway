package contextmaterializer

// This file is the filesystem-aware producer layer for ContextPack. It is
// intentionally small and boring: repository metadata is derived from the
// sealed checkout, the structural fallback is file-oriented, symbols require
// an explicitly injected local adapter, and Git is invoked with fixed argv and
// a network-disabled environment. No producer sends source text anywhere.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
)

const (
	defaultHistoryDepth     = 20
	maxHistoryDepth         = 100
	maxGitStatusBytes       = 64 << 10
	maxGitIdentityBytes     = 256
	maxGitHistoryBytes      = 1 << 20
	maxStructuralSummary    = 768
	maxStructuralSignatures = 8
)

var (
	// ErrProducerUnavailable is returned when a configured producer has no
	// implementation available in the current image. It is deliberately not
	// converted into an empty tier: configured symbols must fail closed.
	ErrProducerUnavailable = errors.New("contextmaterializer: configured producer unavailable")
	ErrDirtyRepository     = errors.New("contextmaterializer: repository is dirty")
	ErrUnsafeRepository    = errors.New("contextmaterializer: repository contains unsafe input")
)

// ProducerSettings are the bounded, credential-free knobs needed by the
// local producers. The API strategy remains the source of truth; these values
// are copied from it by FromSnapshot and are also accepted in test contracts.
type ProducerSettings struct {
	Lexical         bool     `json:"lexical,omitempty"`
	RepoMapBudget   int64    `json:"repoMapBudget,omitempty"`
	SymbolLanguages []string `json:"symbolLanguages,omitempty"`
	HistoryDepth    int      `json:"historyDepth,omitempty"`
}

// StructuralRequest is the narrow extension point for a tree-sitter/AST
// adapter. An adapter may improve signatures, but must return only bounded
// metadata and must operate against the supplied local root.
type StructuralRequest struct {
	Root       string
	Files      []contextpack.LexicalEntry
	MaxEntries int
	MaxTokens  int64
}

// StructuralAdapter is deliberately one method. The default implementation
// is localRepoMap, so no semantic/code graph is required in the base image.
type StructuralAdapter interface {
	Build(context.Context, StructuralRequest) ([]contextpack.RepoMapEntry, error)
}

// StructuralAdapterFunc adapts a function for tests or a local parser image.
type StructuralAdapterFunc func(context.Context, StructuralRequest) ([]contextpack.RepoMapEntry, error)

func (f StructuralAdapterFunc) Build(ctx context.Context, request StructuralRequest) ([]contextpack.RepoMapEntry, error) {
	if f == nil {
		return nil, ErrProducerUnavailable
	}
	return f(ctx, request)
}

// SymbolRequest is the LSP/Serena-compatible boundary. The producer does not
// open a network socket or fetch a language model. The optional local adapter
// starts one explicitly configured stdio language server; callers that do not
// inject it fail closed when symbols are requested.
type SymbolRequest struct {
	Root       string
	Files      []contextpack.LexicalEntry
	Languages  []string
	MaxEntries int
	MaxTokens  int64
}

// SymbolProvider is the only supported symbol integration point. A nil
// provider with symbols configured is an admission/runtime error, never an
// empty successful symbol tier. The shipped LocalSymbolProvider speaks the
// bounded LSP document-symbol subset over a private local stdio process.
type SymbolProvider interface {
	Symbols(context.Context, SymbolRequest) ([]contextpack.SymbolEntry, error)
}

// SymbolProviderFunc adapts a local provider for tests and future sidecars.
type SymbolProviderFunc func(context.Context, SymbolRequest) ([]contextpack.SymbolEntry, error)

func (f SymbolProviderFunc) Symbols(ctx context.Context, request SymbolRequest) ([]contextpack.SymbolEntry, error) {
	if f == nil {
		return nil, ErrProducerUnavailable
	}
	return f(ctx, request)
}

// ProducerConfig contains only local paths and optional in-process adapters.
// GitBinary is resolved and validated before use; an empty value means the
// image's git executable, never a shell command supplied by a caller.
type ProducerConfig struct {
	Root              string
	BaseSHA           string
	GitBinary         string
	Settings          ProducerSettings
	Strategies        contextpack.ContextStrategies
	Budgets           contextpack.Budgets
	StructuralAdapter StructuralAdapter
	SymbolProvider    SymbolProvider
}

// ProducedContext is the normalized material passed to the pure ContextPack
// compiler. It intentionally contains no file contents.
type ProducedContext struct {
	Lexical    []contextpack.LexicalEntry
	RepoMap    []contextpack.RepoMapEntry
	Symbols    []contextpack.SymbolEntry
	History    []contextpack.HistoryEntry
	Strategies contextpack.ContextStrategies
}

type repositoryFile struct {
	entry   contextpack.LexicalEntry
	content []byte
}

type producerState struct {
	read int64
}

// Produce derives all configured local context tiers. It is deterministic for
// a fixed sealed checkout: directory entries, files, adapter outputs, and Git
// records are all normalized before returning. It never returns partial
// output on failure.
func Produce(ctx context.Context, config ProducerConfig) (ProducedContext, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ProducedContext{}, err
	}
	if err := validateProducerConfig(config); err != nil {
		return ProducedContext{}, err
	}
	settings := normalizeProducerSettings(config)
	strategies := config.Strategies
	if settings.Lexical && strategies.Lexical == "" {
		strategies.Lexical = "local-ripgrep"
	}

	needFiles := settings.Lexical || strategies.RepoMap != "" || strategies.Symbols != ""
	state := producerState{}
	files := make([]repositoryFile, 0)
	if needFiles {
		var err error
		files, err = collectRepository(ctx, config.Root, config.Budgets, &state)
		if err != nil {
			return ProducedContext{}, err
		}
	}

	result := ProducedContext{Strategies: strategies}
	for _, file := range files {
		result.Lexical = append(result.Lexical, file.entry)
	}

	if strategies.RepoMap != "" {
		budget := settings.RepoMapBudget
		if budget == 0 {
			budget = config.Budgets.MaxTokens
		}
		if config.StructuralAdapter == nil {
			entries, err := localRepoMap(files, config.Budgets.MaxEntries, budget)
			if err != nil {
				return ProducedContext{}, err
			}
			result.RepoMap = entries
		} else {
			request := StructuralRequest{Root: config.Root, Files: cloneLexical(result.Lexical), MaxEntries: config.Budgets.MaxEntries, MaxTokens: budget}
			entries, err := config.StructuralAdapter.Build(ctx, request)
			if err != nil {
				return ProducedContext{}, fmt.Errorf("%w: structural adapter: %v", ErrProducerUnavailable, err)
			}
			if err := validateRepoMapAdapter(entries, result.Lexical, config.Budgets.MaxEntries, budget); err != nil {
				return ProducedContext{}, err
			}
			result.RepoMap = append([]contextpack.RepoMapEntry(nil), entries...)
		}
		sortRepoMap(result.RepoMap)
	}

	if strategies.Symbols != "" {
		if config.SymbolProvider == nil {
			return ProducedContext{}, fmt.Errorf("%w: symbols strategy %q requires a local LSP/Serena adapter", ErrProducerUnavailable, strategies.Symbols)
		}
		languages := normalizeLanguages(settings.SymbolLanguages)
		if len(languages) == 0 {
			return ProducedContext{}, fmt.Errorf("%w: symbols strategy has no configured languages", ErrProducerUnavailable)
		}
		entries, err := config.SymbolProvider.Symbols(ctx, SymbolRequest{Root: config.Root, Files: cloneLexical(result.Lexical), Languages: languages, MaxEntries: config.Budgets.MaxEntries, MaxTokens: config.Budgets.MaxTokens})
		if err != nil {
			return ProducedContext{}, fmt.Errorf("%w: symbol adapter: %w", ErrProducerUnavailable, err)
		}
		if err := validateSymbols(entries, result.Lexical, languages, config.Budgets.MaxEntries, config.Budgets.MaxTokens); err != nil {
			return ProducedContext{}, err
		}
		result.Symbols = append([]contextpack.SymbolEntry(nil), entries...)
		sortSymbols(result.Symbols)
	}

	if strategies.History != "" {
		depth := settings.HistoryDepth
		if depth == 0 {
			depth = defaultHistoryDepth
		}
		history, err := produceHistory(ctx, config, depth, &state)
		if err != nil {
			return ProducedContext{}, err
		}
		result.History = history
	}

	return result, nil
}

func validateProducerConfig(config ProducerConfig) error {
	if !absoluteClean(config.Root) || config.Root == "/" {
		return fmt.Errorf("%w: repository root is not absolute and canonical", ErrUnsafeRepository)
	}
	info, err := os.Lstat(config.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: repository root is unavailable", ErrUnsafeRepository)
	}
	b := config.Budgets
	if b.MaxInputBytes <= 0 || b.MaxOutputBytes <= 0 || b.MaxFileBytes <= 0 || b.MaxTokens <= 0 || b.MaxEntries <= 0 {
		return fmt.Errorf("%w: producer budgets must be positive", contextpack.ErrInvalidInput)
	}
	if config.BaseSHA != "" && !validCommit(config.BaseSHA) {
		return fmt.Errorf("%w: base SHA is malformed", contextpack.ErrInvalidInput)
	}
	if config.Strategies.RepoMap != "" && config.Strategies.RepoMap != "tree-sitter" {
		return fmt.Errorf("%w: unsupported repository map strategy %q", contextpack.ErrInvalidInput, config.Strategies.RepoMap)
	}
	if config.Strategies.Symbols != "" && config.Strategies.Symbols != "lsp-serena" {
		return fmt.Errorf("%w: unsupported symbols strategy %q", contextpack.ErrInvalidInput, config.Strategies.Symbols)
	}
	if config.Strategies.History != "" && config.Strategies.History != "git-blame-touched" {
		return fmt.Errorf("%w: unsupported history strategy %q", contextpack.ErrInvalidInput, config.Strategies.History)
	}
	if config.Strategies.History != "" && !validCommit(config.BaseSHA) {
		return fmt.Errorf("%w: history requires a pinned base SHA", contextpack.ErrInvalidInput)
	}
	return nil
}

func validateProducerSettings(contract Contract) error {
	if contract.Strategies.Lexical != "" && !contract.Producers.Lexical {
		return fmt.Errorf("%w: lexical strategy is configured but its producer is disabled", ErrInvalidInput)
	}
	if contract.Producers.RepoMapBudget < 0 || contract.Producers.RepoMapBudget > 100000 {
		return fmt.Errorf("%w: repository map budget is outside its bound", ErrInvalidInput)
	}
	if len(contract.Producers.SymbolLanguages) > 8 {
		return fmt.Errorf("%w: symbol language count is bounded", ErrInvalidInput)
	}
	for _, language := range contract.Producers.SymbolLanguages {
		if language == "" || strings.TrimSpace(language) != language {
			return fmt.Errorf("%w: symbol language is malformed", ErrInvalidInput)
		}
	}
	if contract.Strategies.Symbols != "" && len(contract.Producers.SymbolLanguages) == 0 {
		return fmt.Errorf("%w: configured symbols require explicit languages", ErrInvalidInput)
	}
	if contract.Producers.HistoryDepth < 0 || contract.Producers.HistoryDepth > maxHistoryDepth {
		return fmt.Errorf("%w: history depth is outside its bound", ErrInvalidInput)
	}
	return nil
}

func normalizeProducerSettings(config ProducerConfig) ProducerSettings {
	settings := config.Settings
	if settings.RepoMapBudget < 0 {
		settings.RepoMapBudget = 0
	}
	if settings.HistoryDepth < 0 {
		settings.HistoryDepth = 0
	}
	if settings.HistoryDepth > maxHistoryDepth {
		settings.HistoryDepth = maxHistoryDepth
	}
	settings.SymbolLanguages = normalizeLanguages(settings.SymbolLanguages)
	return settings
}

func collectRepository(ctx context.Context, root string, budgets contextpack.Budgets, state *producerState) ([]repositoryFile, error) {
	files := make([]repositoryFile, 0)
	var visit func(string, string) error
	visit = func(directory, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return fmt.Errorf("%w: read repository directory: %v", ErrUnsafeRepository, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			childRelative := name
			if relative != "" {
				childRelative = relative + "/" + name
			}
			if !safeRelative(childRelative) {
				return fmt.Errorf("%w: unsafe repository path %q", ErrUnsafeRepository, childRelative)
			}
			full := filepath.Join(directory, name)
			info, err := os.Lstat(full)
			if err != nil {
				return fmt.Errorf("%w: inspect %q: %v", ErrUnsafeRepository, childRelative, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: symlink %q", ErrUnsafeRepository, childRelative)
			}
			if info.IsDir() {
				if ignoredDirectory(name) {
					continue
				}
				if err := visit(full, childRelative); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%w: special file %q", ErrUnsafeRepository, childRelative)
			}
			if ignoredFile(childRelative) {
				continue
			}
			if info.Size() < 0 || info.Size() > budgets.MaxFileBytes {
				return fmt.Errorf("%w: file %q exceeds maxFileBytes", contextpack.ErrBudgetExceeded, childRelative)
			}
			body, err := readRepositoryFile(full, budgets.MaxFileBytes)
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrUnsafeRepository, childRelative, err)
			}
			if err := state.add(int64(len(body)), budgets.MaxInputBytes); err != nil {
				return err
			}
			if credentialPath(childRelative) || credentialContent(body) {
				return fmt.Errorf("%w: credential-like file %q", ErrUnsafeRepository, childRelative)
			}
			files = append(files, repositoryFile{entry: lexicalEntry(childRelative, body), content: body})
			if len(files) > budgets.MaxEntries {
				return fmt.Errorf("%w: repository file count exceeds maxEntries", contextpack.ErrBudgetExceeded)
			}
		}
		return nil
	}
	if err := visit(root, ""); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].entry.Path < files[j].entry.Path })
	return files, nil
}

func (s *producerState) add(bytesRead, limit int64) error {
	if bytesRead < 0 || s.read > limit-bytesRead {
		return fmt.Errorf("%w: repository input bytes exceed maxInputBytes", contextpack.ErrBudgetExceeded)
	}
	s.read += bytesRead
	return nil
}

func readRepositoryFile(name string, max int64) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > max {
		return nil, errors.New("file changed or is not regular")
	}
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(body)) > max {
		return nil, errors.New("file could not be read within its bound")
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return nil, errors.New("file is not valid UTF-8 text")
	}
	return body, nil
}

func lexicalEntry(name string, body []byte) contextpack.LexicalEntry {
	extension := filepath.Ext(name)
	if extension != "" {
		extension = strings.ToLower(extension[1:])
	}
	lines := int64(0)
	if len(body) > 0 {
		lines = int64(bytes.Count(body, []byte{'\n'}) + 1)
	}
	return contextpack.LexicalEntry{Path: filepath.ToSlash(name), Extension: extension, Language: languageForExtension(extension), Bytes: int64(len(body)), Lines: lines, Digest: digest(body)}
}

func localRepoMap(files []repositoryFile, maxEntries int, maxTokens int64) ([]contextpack.RepoMapEntry, error) {
	if len(files) > maxEntries {
		return nil, fmt.Errorf("%w: local repository map entries exceed maxEntries", contextpack.ErrBudgetExceeded)
	}
	entries := make([]contextpack.RepoMapEntry, 0, len(files))
	for _, file := range files {
		signature := structuralSignature(file.content, file.entry.Language)
		summary := fmt.Sprintf("%s file; %d lines; %d bytes; %s", nonEmpty(file.entry.Language, "unknown"), file.entry.Lines, file.entry.Bytes, file.entry.Digest)
		entries = append(entries, contextpack.RepoMapEntry{Path: file.entry.Path, Kind: "file", Signature: signature, Summary: boundedText(summary, maxStructuralSummary)})
	}
	if estimateContextTokens(entries) > maxTokens {
		return nil, fmt.Errorf("%w: local repository map token budget exceeded", contextpack.ErrBudgetExceeded)
	}
	return entries, nil
}

func validateRepoMapAdapter(entries []contextpack.RepoMapEntry, files []contextpack.LexicalEntry, maxEntries int, maxTokens int64) error {
	if len(entries) > maxEntries {
		return fmt.Errorf("%w: structural adapter returned too many entries", contextpack.ErrBudgetExceeded)
	}
	known := make(map[string]struct{}, len(files))
	seen := make(map[string]struct{}, len(entries))
	for _, file := range files {
		known[file.Path] = struct{}{}
	}
	for _, entry := range entries {
		if !safeRelative(entry.Path) || !validMetadataText(entry.Kind, 256) || !validMetadataText(entry.Signature, maxStructuralSummary) || !validMetadataText(entry.Summary, maxStructuralSummary) {
			return fmt.Errorf("%w: structural adapter returned malformed metadata", ErrUnsafeRepository)
		}
		if _, ok := known[entry.Path]; !ok {
			return fmt.Errorf("%w: structural adapter returned an unknown path %q", ErrUnsafeRepository, entry.Path)
		}
		if _, ok := seen[entry.Path]; ok {
			return fmt.Errorf("%w: structural adapter returned duplicate path %q", ErrUnsafeRepository, entry.Path)
		}
		seen[entry.Path] = struct{}{}
		if credentialPath(entry.Path) || credentialContent([]byte(entry.Signature+"\n"+entry.Summary)) {
			return fmt.Errorf("%w: structural adapter returned credential-like metadata", ErrUnsafeRepository)
		}
	}
	if estimateContextTokens(entries) > maxTokens {
		return fmt.Errorf("%w: structural adapter exceeded its token budget", contextpack.ErrBudgetExceeded)
	}
	return nil
}

func validateSymbols(entries []contextpack.SymbolEntry, files []contextpack.LexicalEntry, languages []string, maxEntries int, maxTokens int64) error {
	if len(entries) > maxEntries {
		return fmt.Errorf("%w: symbol adapter returned too many entries", contextpack.ErrBudgetExceeded)
	}
	known := make(map[string]string, len(files))
	for _, file := range files {
		known[file.Path] = file.Language
	}
	allowed := make(map[string]struct{}, len(languages))
	seen := make(map[string]struct{}, len(entries))
	for _, language := range languages {
		allowed[language] = struct{}{}
	}
	for _, entry := range entries {
		if !safeRelative(entry.Path) || !validMetadataText(entry.Name, 512) || !validMetadataText(entry.Kind, 256) || !validMetadataText(entry.Signature, maxStructuralSummary) || entry.Name == "" || entry.Kind == "" || entry.Line < 0 || entry.Column < 0 {
			return fmt.Errorf("%w: symbol adapter returned malformed metadata", ErrUnsafeRepository)
		}
		language, ok := known[entry.Path]
		if !ok {
			return fmt.Errorf("%w: symbol adapter returned unknown path %q", ErrUnsafeRepository, entry.Path)
		}
		if language != "" {
			if _, ok := allowed[language]; !ok {
				return fmt.Errorf("%w: symbol adapter returned a language outside its request", ErrUnsafeRepository)
			}
		}
		if credentialContent([]byte(entry.Name + "\n" + entry.Kind + "\n" + entry.Signature)) {
			return fmt.Errorf("%w: symbol adapter returned credential-like metadata", ErrUnsafeRepository)
		}
		key := fmt.Sprintf("%s:%d:%d:%s:%s", entry.Path, entry.Line, entry.Column, entry.Kind, entry.Name)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%w: symbol adapter returned duplicate symbol", ErrUnsafeRepository)
		}
		seen[key] = struct{}{}
	}
	if estimateContextTokens(entries) > maxTokens {
		return fmt.Errorf("%w: symbol adapter exceeded its token budget", contextpack.ErrBudgetExceeded)
	}
	return nil
}

func produceHistory(ctx context.Context, config ProducerConfig, depth int, state *producerState) ([]contextpack.HistoryEntry, error) {
	if depth < 1 || depth > maxHistoryDepth {
		return nil, fmt.Errorf("%w: history depth is outside 1..%d", contextpack.ErrInvalidInput, maxHistoryDepth)
	}
	gitBinary, err := resolveGitBinary(config.GitBinary)
	if err != nil {
		return nil, fmt.Errorf("%w: Git is unavailable: %v", ErrProducerUnavailable, err)
	}
	if err := requireGitDirectory(config.Root); err != nil {
		return nil, err
	}
	status, err := runGit(ctx, gitBinary, config.Root, []string{"status", "--porcelain=v1", "--untracked-files=all", "--no-renames"}, maxGitStatusBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: Git status failed", ErrUnsafeRepository)
	}
	if len(bytes.TrimSpace(status)) != 0 {
		return nil, ErrDirtyRepository
	}
	head, err := runGit(ctx, gitBinary, config.Root, []string{"rev-parse", "HEAD"}, maxGitIdentityBytes)
	if err != nil || !validCommit(string(bytes.TrimSpace(head))) || string(bytes.TrimSpace(head)) != config.BaseSHA {
		return nil, fmt.Errorf("%w: Git HEAD does not match the pinned base SHA", ErrUnsafeRepository)
	}
	remaining := config.Budgets.MaxInputBytes - state.read
	if remaining <= 0 {
		return nil, fmt.Errorf("%w: no input budget remains for Git history", contextpack.ErrBudgetExceeded)
	}
	if remaining > maxGitHistoryBytes {
		remaining = maxGitHistoryBytes
	}
	logBody, err := runGit(ctx, gitBinary, config.Root, []string{"--no-pager", "log", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--format=%x1e%H%x00%s%x00", "--name-only", "-z", "-n", strconv.Itoa(depth)}, remaining)
	if err != nil {
		return nil, fmt.Errorf("%w: Git history failed", ErrUnsafeRepository)
	}
	if err := state.add(int64(len(logBody)), config.Budgets.MaxInputBytes); err != nil {
		return nil, err
	}
	entries, err := parseGitHistory(logBody)
	if err != nil {
		return nil, err
	}
	if len(entries) > config.Budgets.MaxEntries {
		return nil, fmt.Errorf("%w: Git history entries exceed maxEntries", contextpack.ErrBudgetExceeded)
	}
	return entries, nil
}

func parseGitHistory(body []byte) ([]contextpack.HistoryEntry, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("%w: Git history is not UTF-8", ErrUnsafeRepository)
	}
	entries := make([]contextpack.HistoryEntry, 0)
	for _, record := range bytes.Split(body, []byte{0x1e}) {
		if len(bytes.TrimSpace(record)) == 0 {
			continue
		}
		fields := bytes.Split(record, []byte{0})
		if len(fields) < 2 {
			return nil, fmt.Errorf("%w: malformed Git history record", ErrUnsafeRepository)
		}
		commit := strings.TrimSpace(string(fields[0]))
		subject := strings.TrimSpace(string(fields[1]))
		if !validCommit(commit) || subject == "" || strings.ContainsAny(subject, "\x00\r\n") || credentialContent([]byte(subject)) {
			return nil, fmt.Errorf("%w: malformed or credential-like Git history metadata", ErrUnsafeRepository)
		}
		paths := make([]string, 0, len(fields)-2)
		seen := make(map[string]struct{}, len(fields)-2)
		for _, raw := range fields[2:] {
			name := strings.TrimLeft(string(raw), "\r\n")
			if name == "" {
				continue
			}
			if !safeRelative(name) || credentialPath(name) || strings.ContainsAny(name, "\x00\r\n") {
				return nil, fmt.Errorf("%w: unsafe Git history path", ErrUnsafeRepository)
			}
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				paths = append(paths, filepath.ToSlash(name))
			}
		}
		sort.Strings(paths)
		entries = append(entries, contextpack.HistoryEntry{CommitSHA: commit, Subject: subject, Paths: paths})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CommitSHA != entries[j].CommitSHA {
			return entries[i].CommitSHA < entries[j].CommitSHA
		}
		return entries[i].Subject < entries[j].Subject
	})
	return entries, nil
}

func resolveGitBinary(configured string) (string, error) {
	if configured == "" {
		configured = "git"
	}
	if strings.ContainsRune(configured, 0) || (!filepath.IsAbs(configured) && configured != "git") {
		return "", errors.New("Git binary must be an absolute path or git")
	}
	resolved := configured
	if configured == "git" {
		var err error
		resolved, err = exec.LookPath("git")
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved || filepath.Base(resolved) != "git" {
		return "", errors.New("resolved Git binary is not an explicit git executable")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", errors.New("Git binary is not a regular executable")
	}
	return resolved, nil
}

func requireGitDirectory(root string) error {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: pristine checkout has no regular .git directory", ErrProducerUnavailable)
	}
	return nil
}

func runGit(ctx context.Context, binary, root string, args []string, maxOutput int64) ([]byte, error) {
	if maxOutput < 1 || !absoluteClean(root) || root == "/" || !filepath.IsAbs(binary) {
		return nil, errors.New("invalid Git invocation")
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := make([]string, 0, len(args)+2)
	argv = append(argv, "-C", root)
	argv = append(argv, args...)
	cmd := exec.CommandContext(commandCtx, binary, argv...)
	cmd.Env = []string{
		"HOME=/nonexistent",
		"PATH=/usr/bin:/bin",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_ALLOW_PROTOCOL=none",
		"LC_ALL=C",
	}
	cmd.Stderr = io.Discard
	var output boundedBuffer
	output.limit = maxOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, err
	}
	if output.tooLarge {
		return nil, errors.New("Git output exceeded its bound")
	}
	return output.bytes(), nil
}

type boundedBuffer struct {
	body     bytes.Buffer
	limit    int64
	tooLarge bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.tooLarge {
		return len(p), nil
	}
	if int64(b.body.Len())+int64(len(p)) > b.limit {
		b.tooLarge = true
		return len(p), nil
	}
	return b.body.Write(p)
}

func (b *boundedBuffer) bytes() []byte { return append([]byte(nil), b.body.Bytes()...) }

func ignoredDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".agw", ".agents", ".cache", ".gradle", ".next", ".nuxt", ".pytest_cache", ".tox", ".turbo", ".venv", "__pycache__", "build", "coverage", "dist", "node_modules", "out", "target", "tmp":
		return true
	default:
		return false
	}
}

func ignoredFile(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	ext := strings.ToLower(filepath.Ext(base))
	switch ext {
	case ".a", ".class", ".dll", ".dylib", ".exe", ".gif", ".ico", ".jar", ".jpeg", ".jpg", ".o", ".png", ".so", ".wasm", ".webp", ".woff", ".woff2", ".zip":
		return true
	}
	return strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") || strings.HasSuffix(base, ".map")
}

func credentialPath(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	if base == ".env" || base == ".npmrc" || base == ".netrc" || base == "credentials" || base == "credentials.json" || base == "secret" || base == "secrets" || base == "id_rsa" || base == "id_ed25519" || base == "terraform.tfstate" {
		return true
	}
	if strings.HasPrefix(base, ".env.") && base != ".env.example" && base != ".env.sample" && base != ".env.template" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(base))
	return ext == ".pem" || ext == ".p12" || ext == ".pfx" || ext == ".jks" || ext == ".key"
}

func credentialContent(body []byte) bool {
	for _, marker := range [][]byte{
		[]byte("-----BEGIN "), []byte("PRIVATE KEY"), []byte("AWS_SECRET_ACCESS_KEY="), []byte("AWS_ACCESS_KEY_ID="),
		[]byte("github_pat_"), []byte("ghp_"), []byte("xoxb-"), []byte("sk_live_"), []byte("sk-proj-"),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func structuralSignature(body []byte, language string) string {
	lines := strings.Split(string(body), "\n")
	values := make([]string, 0, maxStructuralSignatures)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		candidate := trimmed
		for _, prefix := range []string{"export ", "public ", "private ", "async ", "const "} {
			candidate = strings.TrimPrefix(candidate, prefix)
		}
		if !structuralLine(candidate, language) {
			continue
		}
		candidate = strings.Join(strings.Fields(candidate), " ")
		if len(candidate) > 192 {
			candidate = candidate[:192]
		}
		values = append(values, candidate)
		if len(values) == maxStructuralSignatures {
			break
		}
	}
	return boundedText(strings.Join(values, " | "), maxStructuralSummary)
}

func structuralLine(line, language string) bool {
	words := strings.Fields(line)
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "class", "enum", "fn", "func", "function", "interface", "module", "namespace", "struct", "trait", "type":
		return true
	case "def":
		return language == "python" || language == "ruby"
	default:
		return false
	}
}

func languageForExtension(extension string) string {
	switch strings.ToLower(extension) {
	case "go":
		return "go"
	case "js", "jsx", "mjs", "cjs":
		return "javascript"
	case "ts", "tsx", "mts", "cts":
		return "typescript"
	case "py":
		return "python"
	case "rs":
		return "rust"
	case "java":
		return "java"
	case "cs":
		return "dotnet"
	case "c", "h", "cc", "cpp", "cxx", "hpp":
		return "c-cpp"
	default:
		return ""
	}
}

func normalizeLanguages(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	output := make([]string, 0, len(input))
	for _, value := range input {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		output = append(output, value)
	}
	sort.Strings(output)
	return output
}

func cloneLexical(input []contextpack.LexicalEntry) []contextpack.LexicalEntry {
	return append([]contextpack.LexicalEntry(nil), input...)
}

func sortRepoMap(entries []contextpack.RepoMapEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		if entries[i].Signature != entries[j].Signature {
			return entries[i].Signature < entries[j].Signature
		}
		return entries[i].Summary < entries[j].Summary
	})
}

func sortSymbols(entries []contextpack.SymbolEntry) {
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
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
}

func validMetadataText(value string, max int) bool {
	if len(value) > max || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, char := range value {
		if char != '\n' && char != '\r' && char != '\t' && char < 0x20 {
			return false
		}
	}
	return true
}

func estimateContextTokens(value any) int64 {
	body, err := jsonMarshal(value)
	if err != nil {
		return 1<<63 - 1
	}
	return int64((utf8.RuneCount(body) + 3) / 4)
}

func jsonMarshal(value any) ([]byte, error) {
	// Kept as a local wrapper to make the producer's budget calculation
	// explicit and independent from the pack compiler's private renderer.
	return json.Marshal(value)
}

func boundedText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
