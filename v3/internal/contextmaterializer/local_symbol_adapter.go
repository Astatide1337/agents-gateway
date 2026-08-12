package contextmaterializer

// This file is the bounded local LSP client used by the optional symbol tier.
// It deliberately implements only the LSP document-symbol exchange needed by
// ContextPack. It does not install or claim a Serena runtime; the adapter does
// not speak MCP, start a network listener, build an index, or send repository
// bytes to a service.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	defaultSymbolAdapterTimeout     = 45 * time.Second
	minSymbolAdapterTimeout         = time.Second
	maxSymbolAdapterTimeout         = 5 * time.Minute
	defaultSymbolAdapterOutputBytes = 4 << 20
	maxSymbolAdapterOutputBytes     = 16 << 20
	defaultSymbolAdapterMessages    = 4096
	maxSymbolAdapterMessages        = 10000
	defaultSymbolAdapterItems       = 4096
	maxSymbolAdapterItems           = 10000
	maxSymbolAdapterArgs            = 32
	maxSymbolAdapterArgBytes        = 256
	maxSymbolAdapterArgsJSONBytes   = 16 << 10
	maxSymbolAdapterBinaryBytes     = 512 << 20
	maxLSPHeaderBytes               = 4 << 10
	maxLSPMessageBytes              = 1 << 20
	maxLSPStringBytes               = 256 << 10
	maxSymbolDepth                  = 64
	maxLSPPosition                  = 1_000_000_000
)

var (
	// ErrSymbolAdapterConfig identifies an explicit but unsafe adapter
	// configuration. ErrProducerUnavailable identifies an executable or
	// protocol that cannot be used in the current image. Both are fail-closed
	// conditions when the symbols strategy is configured.
	ErrSymbolAdapterConfig = errors.New("contextmaterializer: invalid local symbol adapter configuration")
	ErrSymbolAdapterOutput = errors.New("contextmaterializer: local symbol adapter output exceeded its bound")
)

// LocalSymbolAdapterConfig is intentionally operational configuration rather
// than part of the AgentRun API. The CRD selects the lsp-serena symbol tier;
// this immutable, image-local setting selects the already-installed LSP
// executable. Args are fixed at process start and never contain task or
// repository data; the repository is passed only as the child working
// directory and standard LSP file URIs.
type LocalSymbolAdapterConfig struct {
	Command string
	// Digest is the exact sha256 digest of the executable bytes in the
	// context image. The image digest alone is not enough: a derived image
	// may contain a different provider at the same path.
	Digest         string
	Args           []string
	Timeout        time.Duration
	MaxOutputBytes int64
	MaxMessages    int
	MaxItems       int
}

// LocalSymbolAdapterConfigFromEnv parses the optional context-image
// configuration. The boolean is false only when no adapter variable is set;
// an incomplete or malformed explicit configuration returns an error.
func LocalSymbolAdapterConfigFromEnv(getenv func(string) string) (LocalSymbolAdapterConfig, bool, error) {
	if getenv == nil {
		return LocalSymbolAdapterConfig{}, false, fmt.Errorf("%w: environment reader is required", ErrSymbolAdapterConfig)
	}
	command := getenv("AGW_CONTEXT_SYMBOL_ADAPTER")
	digest := getenv("AGW_CONTEXT_SYMBOL_ADAPTER_SHA256")
	argsJSON := getenv("AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON")
	timeoutText := getenv("AGW_CONTEXT_SYMBOL_ADAPTER_TIMEOUT")
	outputText := getenv("AGW_CONTEXT_SYMBOL_ADAPTER_MAX_OUTPUT_BYTES")
	messagesText := getenv("AGW_CONTEXT_SYMBOL_ADAPTER_MAX_MESSAGES")
	itemsText := getenv("AGW_CONTEXT_SYMBOL_ADAPTER_MAX_ITEMS")
	configured := command != "" || digest != "" || argsJSON != "" || timeoutText != "" || outputText != "" || messagesText != "" || itemsText != ""
	if !configured {
		return LocalSymbolAdapterConfig{}, false, nil
	}
	if command == "" {
		return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: AGW_CONTEXT_SYMBOL_ADAPTER is required when symbol adapter configuration is present", ErrSymbolAdapterConfig)
	}
	if !canonical.ValidDigest(digest) {
		return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: AGW_CONTEXT_SYMBOL_ADAPTER_SHA256 must be sha256:<64 lowercase hex characters>", ErrSymbolAdapterConfig)
	}

	config := LocalSymbolAdapterConfig{
		Command:        command,
		Digest:         digest,
		Timeout:        defaultSymbolAdapterTimeout,
		MaxOutputBytes: defaultSymbolAdapterOutputBytes,
		MaxMessages:    defaultSymbolAdapterMessages,
		MaxItems:       defaultSymbolAdapterItems,
	}
	if argsJSON != "" {
		if len(argsJSON) > maxSymbolAdapterArgsJSONBytes {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter argv JSON is oversized", ErrSymbolAdapterConfig)
		}
		normalized, err := strictjson.Normalize([]byte(argsJSON))
		if err != nil || !bytes.Equal(normalized, []byte(argsJSON)) {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter argv JSON must be strict canonical JSON", ErrSymbolAdapterConfig)
		}
		decoder := json.NewDecoder(bytes.NewReader(normalized))
		decoder.DisallowUnknownFields()
		if bytes.Equal(normalized, []byte("null")) {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter argv JSON must be an array of strings", ErrSymbolAdapterConfig)
		}
		if err := decoder.Decode(&config.Args); err != nil {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter argv JSON is not an array of strings", ErrSymbolAdapterConfig)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter argv JSON has trailing data", ErrSymbolAdapterConfig)
		}
	}
	var err error
	if timeoutText != "" {
		config.Timeout, err = time.ParseDuration(timeoutText)
		if err != nil {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter timeout is invalid", ErrSymbolAdapterConfig)
		}
	}
	if outputText != "" {
		config.MaxOutputBytes, err = parsePositiveBound(outputText, maxSymbolAdapterOutputBytes)
		if err != nil {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter output limit is invalid", ErrSymbolAdapterConfig)
		}
	}
	if messagesText != "" {
		value, parseErr := parsePositiveBound(messagesText, maxSymbolAdapterMessages)
		if parseErr != nil {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter message limit is invalid", ErrSymbolAdapterConfig)
		}
		config.MaxMessages = int(value)
	}
	if itemsText != "" {
		value, parseErr := parsePositiveBound(itemsText, maxSymbolAdapterItems)
		if parseErr != nil {
			return LocalSymbolAdapterConfig{}, true, fmt.Errorf("%w: adapter item limit is invalid", ErrSymbolAdapterConfig)
		}
		config.MaxItems = int(value)
	}
	return config, true, nil
}

func parsePositiveBound(value string, maximum int64) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return 0, errors.New("not a decimal bound")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 || parsed > maximum {
		return 0, errors.New("decimal bound is outside the allowed range")
	}
	return parsed, nil
}

// NewLocalSymbolProvider validates and returns the production adapter. The
// child is placed in a private network namespace; if the runtime cannot create
// that namespace the adapter fails closed instead of silently allowing an LSP
// implementation to access the network.
func NewLocalSymbolProvider(config LocalSymbolAdapterConfig) (SymbolProvider, error) {
	return newLocalSymbolProvider(config, true)
}

func newLocalSymbolProvider(config LocalSymbolAdapterConfig, isolateNetwork bool) (SymbolProvider, error) {
	validated, err := validateLocalSymbolAdapterConfig(config)
	if err != nil {
		return nil, err
	}
	return &localSymbolProvider{config: validated, isolateNetwork: isolateNetwork}, nil
}

// localSymbolProvider carries the isolation decision separately from the
// public config. This small indirection keeps the production constructor's
// security default visible at the call site.
type localSymbolProvider struct {
	config         LocalSymbolAdapterConfig
	isolateNetwork bool
}

func validateLocalSymbolAdapterConfig(config LocalSymbolAdapterConfig) (LocalSymbolAdapterConfig, error) {
	if !absoluteClean(config.Command) || config.Command == "/" || strings.ContainsAny(config.Command, "\x00\r\n") {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: command must be an absolute canonical path", ErrSymbolAdapterConfig)
	}
	if !canonical.ValidDigest(config.Digest) {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: executable sha256 digest is required", ErrSymbolAdapterConfig)
	}
	info, err := os.Lstat(config.Command)
	if err != nil {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: configured LSP executable is unavailable", ErrProducerUnavailable)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: configured LSP executable must be a regular non-symlink executable", ErrSymbolAdapterConfig)
	}
	resolved, err := filepath.EvalSymlinks(config.Command)
	if err != nil || resolved != config.Command {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: configured LSP executable path contains a symlink", ErrSymbolAdapterConfig)
	}
	actualDigest, err := digestExecutable(config.Command)
	if err != nil {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: configured LSP executable cannot be hashed", ErrProducerUnavailable)
	}
	if actualDigest != config.Digest {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: configured LSP executable digest does not match AGW_CONTEXT_SYMBOL_ADAPTER_SHA256", ErrSymbolAdapterConfig)
	}
	if len(config.Args) > maxSymbolAdapterArgs {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: adapter argv has too many arguments", ErrSymbolAdapterConfig)
	}
	args := append([]string(nil), config.Args...)
	for _, arg := range args {
		if len(arg) > maxSymbolAdapterArgBytes || strings.ContainsAny(arg, "\x00\r\n") || strings.Contains(arg, "${") || strings.Contains(arg, "{{") {
			return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: adapter argv contains an unsafe or templated argument", ErrSymbolAdapterConfig)
		}
	}
	if config.Timeout == 0 {
		config.Timeout = defaultSymbolAdapterTimeout
	}
	if config.Timeout < minSymbolAdapterTimeout || config.Timeout > maxSymbolAdapterTimeout {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: adapter timeout is outside its bound", ErrSymbolAdapterConfig)
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultSymbolAdapterOutputBytes
	}
	if config.MaxOutputBytes < 1 || config.MaxOutputBytes > maxSymbolAdapterOutputBytes {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: adapter output limit is outside its bound", ErrSymbolAdapterConfig)
	}
	if config.MaxMessages == 0 {
		config.MaxMessages = defaultSymbolAdapterMessages
	}
	if config.MaxMessages < 1 || config.MaxMessages > maxSymbolAdapterMessages {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: adapter message limit is outside its bound", ErrSymbolAdapterConfig)
	}
	if config.MaxItems == 0 {
		config.MaxItems = defaultSymbolAdapterItems
	}
	if config.MaxItems < 1 || config.MaxItems > maxSymbolAdapterItems {
		return LocalSymbolAdapterConfig{}, fmt.Errorf("%w: adapter item limit is outside its bound", ErrSymbolAdapterConfig)
	}
	config.Args = args
	return config, nil
}

func digestExecutable(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	count, err := io.CopyN(hash, file, maxSymbolAdapterBinaryBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if count < 1 || count > maxSymbolAdapterBinaryBytes {
		return "", fmt.Errorf("executable exceeds the %d-byte bound", maxSymbolAdapterBinaryBytes)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func (p *localSymbolProvider) Symbols(ctx context.Context, request SymbolRequest) ([]contextpack.SymbolEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.MaxEntries < 1 || request.MaxTokens < 1 {
		return nil, fmt.Errorf("%w: symbol request budgets are invalid", ErrProducerUnavailable)
	}
	root, err := validateLocalRepositoryRoot(request.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: local repository root is unsafe", ErrProducerUnavailable)
	}
	files, known, err := validateSymbolRequestFiles(root, request)
	if err != nil {
		return nil, fmt.Errorf("%w: symbol request is unsafe", ErrProducerUnavailable)
	}
	if len(files) == 0 {
		return []contextpack.SymbolEntry{}, nil
	}
	itemLimit := request.MaxEntries
	if itemLimit < 1 {
		return nil, fmt.Errorf("%w: symbol item limit is invalid", ErrProducerUnavailable)
	}
	if itemLimit > p.config.MaxItems {
		itemLimit = p.config.MaxItems
	}
	languages := normalizeLanguages(request.Languages)
	if len(languages) == 0 {
		return nil, fmt.Errorf("%w: symbol language set is empty", ErrProducerUnavailable)
	}
	allowedLanguages := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		allowedLanguages[language] = struct{}{}
	}
	filtered := files[:0]
	for _, file := range files {
		if _, ok := allowedLanguages[file.Language]; ok {
			filtered = append(filtered, file)
		}
	}
	files = filtered
	if len(files) == 0 {
		return []contextpack.SymbolEntry{}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	process, transport, _, asyncError, cleanup, err := p.start(runCtx, root)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	requestID := int64(1)
	if err := transport.sendRequest(requestID, "initialize", initializeParams(root)); err != nil {
		return nil, protocolUnavailable(err)
	}
	if _, err := transport.awaitResponse(runCtx, requestID); err != nil {
		return nil, protocolUnavailable(err)
	}
	if err := transport.sendNotification("initialized", map[string]any{}); err != nil {
		return nil, protocolUnavailable(err)
	}

	entries := make([]contextpack.SymbolEntry, 0)
	for _, file := range files {
		if err := runCtx.Err(); err != nil {
			if async := asyncError(); async != nil {
				return nil, async
			}
			return nil, err
		}
		requestID++
		uri, err := localFileURI(root, file.Path)
		if err != nil {
			return nil, protocolUnavailable(err)
		}
		params := documentSymbolParams(uri)
		if err := transport.sendRequest(requestID, "textDocument/documentSymbol", params); err != nil {
			return nil, protocolUnavailable(err)
		}
		response, err := transport.awaitResponse(runCtx, requestID)
		if err != nil {
			return nil, protocolUnavailable(err)
		}
		remaining := itemLimit - len(entries)
		if remaining < 1 {
			return nil, fmt.Errorf("%w: symbol adapter returned too many items", contextpack.ErrBudgetExceeded)
		}
		decoded, err := decodeDocumentSymbols(response, file.Path, root, known, remaining)
		if err != nil {
			return nil, protocolUnavailable(err)
		}
		entries = append(entries, decoded...)
		if len(entries) > itemLimit {
			return nil, fmt.Errorf("%w: symbol adapter returned too many items", contextpack.ErrBudgetExceeded)
		}
	}

	requestID++
	if err := transport.sendRequest(requestID, "shutdown", nil); err != nil {
		return nil, protocolUnavailable(err)
	}
	if _, err := transport.awaitResponse(runCtx, requestID); err != nil {
		return nil, protocolUnavailable(err)
	}
	if err := transport.sendNotification("exit", nil); err != nil {
		return nil, protocolUnavailable(err)
	}
	if err := process.closeInput(); err != nil {
		return nil, protocolUnavailable(err)
	}
	if err := process.wait(); err != nil {
		return nil, protocolUnavailable(err)
	}
	if async := asyncError(); async != nil {
		return nil, async
	}
	sortSymbols(entries)
	if estimateContextTokens(entries) > request.MaxTokens {
		return nil, fmt.Errorf("%w: local LSP symbols exceeded their token budget", contextpack.ErrBudgetExceeded)
	}
	return entries, nil
}

func protocolUnavailable(err error) error {
	if err == nil {
		return fmt.Errorf("%w: local LSP protocol failed", ErrProducerUnavailable)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, contextpack.ErrBudgetExceeded) {
		return err
	}
	if errors.Is(err, ErrSymbolAdapterOutput) {
		return err
	}
	return fmt.Errorf("%w: local LSP protocol failed", ErrProducerUnavailable)
}

type localSymbolProcess struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	closeOutputs sync.Once
	waitCh       chan struct{}
	mu           sync.Mutex
	waitE        error
	waitOK       bool
}

func (p *localSymbolProvider) start(ctx context.Context, root string) (*localSymbolProcess, *lspTransport, <-chan error, func() error, func(), error) {
	tmp, err := os.MkdirTemp("", "agw-context-lsp-")
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("%w: local LSP temp directory unavailable", ErrProducerUnavailable)
	}
	cleanupTemp := func() { _ = os.RemoveAll(tmp) }
	cmd := exec.CommandContext(ctx, p.config.Command, p.config.Args...)
	cmd.Dir = root
	cmd.Env = localSymbolEnvironment(root, tmp)
	cmd.SysProcAttr = localSymbolSysProcAttr(p.isolateNetwork)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanupTemp()
		return nil, nil, nil, nil, nil, fmt.Errorf("%w: local LSP stdin unavailable", ErrProducerUnavailable)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		cleanupTemp()
		return nil, nil, nil, nil, nil, fmt.Errorf("%w: local LSP stdout unavailable", ErrProducerUnavailable)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		cleanupTemp()
		return nil, nil, nil, nil, nil, fmt.Errorf("%w: local LSP stderr unavailable", ErrProducerUnavailable)
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		cleanupTemp()
		return nil, nil, nil, nil, nil, fmt.Errorf("%w: local LSP process could not start", ErrProducerUnavailable)
	}
	// The child inherited the writer ends. Keep only the parent-owned readers;
	// EOF now comes from the child's actual exit rather than cmd.Wait closing a
	// StdoutPipe/ StderrPipe underneath the reader goroutines.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

	budget := &outputBudget{limit: p.config.MaxOutputBytes}
	stderrDone := make(chan error, 1)
	var asyncMu sync.Mutex
	var asyncErr error
	setAsyncError := func(value error) {
		if value == nil {
			return
		}
		asyncMu.Lock()
		if asyncErr == nil {
			asyncErr = value
		}
		asyncMu.Unlock()
	}
	getAsyncError := func() error {
		asyncMu.Lock()
		defer asyncMu.Unlock()
		return asyncErr
	}
	go func() {
		_, readErr := io.Copy(io.Discard, &budgetReader{reader: stderrReader, budget: budget})
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			setAsyncError(readErr)
		}
		stderrDone <- readErr
	}()

	process := &localSymbolProcess{cmd: cmd, stdin: stdin, stdout: stdoutReader, stderr: stderrReader, waitCh: make(chan struct{})}
	go func() {
		waitErr := cmd.Wait()
		process.mu.Lock()
		process.waitE = waitErr
		process.waitOK = true
		close(process.waitCh)
		process.mu.Unlock()
	}()
	contextDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			process.closeOutputPipes()
		case <-contextDone:
		}
	}()
	transport := &lspTransport{
		reader:       bufio.NewReaderSize(&budgetReader{reader: stdoutReader, budget: budget}, 64<<10),
		writer:       bufio.NewWriterSize(stdin, 64<<10),
		maxMessages:  p.config.MaxMessages,
		maxBodyBytes: minInt64(p.config.MaxOutputBytes, maxLSPMessageBytes),
	}
	cleanup := func() {
		close(contextDone)
		_ = process.terminate()
		_ = <-stderrDone
		process.closeOutputPipes()
		cleanupTemp()
	}
	return process, transport, stderrDone, getAsyncError, cleanup, nil
}

func localSymbolSysProcAttr(isolateNetwork bool) *syscall.SysProcAttr {
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if !isolateNetwork {
		return attributes
	}
	// The context init container deliberately drops all capabilities. Creating
	// both namespaces together lets an unprivileged process own the child
	// network namespace; the one-entry maps preserve the caller's ability to
	// read the sealed checkout without granting host identity to the child.
	attributes.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	attributes.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	attributes.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	attributes.GidMappingsEnableSetgroups = false
	return attributes
}

func localSymbolEnvironment(root, temp string) []string {
	return []string{
		"HOME=" + temp,
		"TMPDIR=" + temp,
		"XDG_CACHE_HOME=" + filepath.Join(temp, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(temp, "config"),
		"XDG_DATA_HOME=" + filepath.Join(temp, "data"),
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"AGW_LSP_ROOT=" + root,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GOPROXY=off",
		"GOSUMDB=off",
		"NPM_CONFIG_OFFLINE=true",
		"PIP_NO_INDEX=1",
		"NO_PROXY=",
		"no_proxy=",
		"HTTP_PROXY=http://127.0.0.1:9",
		"HTTPS_PROXY=http://127.0.0.1:9",
		"ALL_PROXY=http://127.0.0.1:9",
		"http_proxy=http://127.0.0.1:9",
		"https_proxy=http://127.0.0.1:9",
		"all_proxy=http://127.0.0.1:9",
	}
}

func (p *localSymbolProcess) closeInput() error {
	if p.stdin == nil {
		return nil
	}
	err := p.stdin.Close()
	p.stdin = nil
	return err
}

func (p *localSymbolProcess) closeOutputPipes() {
	p.closeOutputs.Do(func() {
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
		if p.stderr != nil {
			_ = p.stderr.Close()
		}
	})
}

func (p *localSymbolProcess) wait() error {
	<-p.waitCh
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitE
}

func (p *localSymbolProcess) terminate() error {
	_ = p.closeInput()
	if p.cmd == nil || p.cmd.Process == nil {
		return p.wait()
	}
	p.mu.Lock()
	finished := p.waitOK
	p.mu.Unlock()
	if finished {
		return p.wait()
	}
	if p.cmd.SysProcAttr != nil && p.cmd.SysProcAttr.Setpgid {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	} else {
		_ = p.cmd.Process.Kill()
	}
	return p.wait()
}

type outputBudget struct {
	mu    sync.Mutex
	used  int64
	limit int64
}

func (b *outputBudget) consume(size int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if size < 0 || b.used > b.limit-int64(size) {
		return ErrSymbolAdapterOutput
	}
	b.used += int64(size)
	return nil
}

type budgetReader struct {
	reader io.Reader
	budget *outputBudget
}

func (r *budgetReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	n, err := r.reader.Read(buffer)
	if n > 0 {
		if budgetErr := r.budget.consume(n); budgetErr != nil {
			return 0, budgetErr
		}
	}
	return n, err
}

type lspTransport struct {
	reader       *bufio.Reader
	writer       *bufio.Writer
	maxMessages  int
	maxBodyBytes int64
	messages     int
}

func (t *lspTransport) sendRequest(id int64, method string, params any) error {
	return t.send(lspOutbound{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
}

func (t *lspTransport) sendNotification(method string, params any) error {
	return t.send(lspOutbound{JSONRPC: "2.0", Method: method, Params: params})
}

type lspOutbound struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
}

func (t *lspTransport) send(message lspOutbound) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	body, err = strictjson.Normalize(body)
	if err != nil {
		return err
	}
	if len(body) > maxLSPMessageBytes {
		return ErrSymbolAdapterOutput
	}
	if _, err := fmt.Fprintf(t.writer, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := t.writer.Write(body); err != nil {
		return err
	}
	return t.writer.Flush()
}

type lspMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspError       `json:"error,omitempty"`
}

type lspError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (t *lspTransport) awaitResponse(ctx context.Context, expectedID int64) ([]byte, error) {
	expected, _ := json.Marshal(expectedID)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t.messages++
		if t.messages > t.maxMessages {
			return nil, fmt.Errorf("%w: LSP message limit exceeded", contextpack.ErrBudgetExceeded)
		}
		body, err := readLSPFrame(t.reader, t.maxBodyBytes)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, err
		}
		var message lspMessage
		if err := strictDecode(body, &message); err != nil {
			return nil, err
		}
		if message.JSONRPC != "2.0" {
			return nil, errors.New("LSP message has an invalid JSON-RPC version")
		}
		if message.Method != "" {
			if len(message.ID) > 0 {
				if err := t.sendError(message.ID, -32601, "method not supported"); err != nil {
					return nil, err
				}
			}
			continue
		}
		if len(message.ID) == 0 || !strictjson.Equal(message.ID, expected) {
			return nil, errors.New("LSP response id did not match the outstanding request")
		}
		if message.Error != nil {
			return nil, errors.New("LSP server returned an error")
		}
		if len(message.Result) == 0 {
			return nil, errors.New("LSP response omitted result")
		}
		if err := strictjson.Validate(message.Result); err != nil {
			return nil, err
		}
		return append([]byte(nil), message.Result...), nil
	}
}

func (t *lspTransport) sendError(id json.RawMessage, code int, message string) error {
	return t.sendRaw(lspOutboundResponse{JSONRPC: "2.0", ID: id, Error: &lspError{Code: code, Message: message}})
}

type lspOutboundResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *lspError       `json:"error,omitempty"`
}

func (t *lspTransport) sendRaw(message lspOutboundResponse) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	body, err = strictjson.Normalize(body)
	if err != nil {
		return err
	}
	if len(body) > maxLSPMessageBytes {
		return ErrSymbolAdapterOutput
	}
	if _, err := fmt.Fprintf(t.writer, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := t.writer.Write(body); err != nil {
		return err
	}
	return t.writer.Flush()
}

func readLSPFrame(reader *bufio.Reader, maxBodyBytes int64) ([]byte, error) {
	contentLength := int64(-1)
	contentType := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) > maxLSPHeaderBytes {
			return nil, ErrSymbolAdapterOutput
		}
		if line == "\r\n" {
			break
		}
		if !strings.HasSuffix(line, "\r\n") {
			return nil, errors.New("LSP header is not CRLF terminated")
		}
		line = strings.TrimSuffix(line, "\r\n")
		separator := strings.IndexByte(line, ':')
		if separator <= 0 || strings.ContainsAny(line[:separator], " \t") {
			return nil, errors.New("LSP header is malformed")
		}
		name := line[:separator]
		value := strings.TrimSpace(line[separator+1:])
		switch strings.ToLower(name) {
		case "content-length":
			if contentLength >= 0 || value == "" {
				return nil, errors.New("LSP content length is duplicated or empty")
			}
			parsed, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || parsed < 1 || parsed > maxBodyBytes {
				return nil, ErrSymbolAdapterOutput
			}
			contentLength = parsed
		case "content-type":
			if contentType != "" || value == "" {
				return nil, errors.New("LSP content type is duplicated or empty")
			}
			contentType = value
		default:
			return nil, errors.New("LSP response contains an unknown header")
		}
	}
	if contentLength < 1 {
		return nil, errors.New("LSP response omitted content length")
	}
	if contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/vscode-jsonrpc") {
		return nil, errors.New("LSP response has an unsupported content type")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	if err := strictjson.Validate(body); err != nil {
		return nil, err
	}
	return body, nil
}

func strictDecode(body []byte, target any) error {
	normalized, err := strictjson.Normalize(body)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON has trailing data")
	}
	return nil
}

type initializeRequestParams struct {
	ProcessID        *int              `json:"processId"`
	ClientInfo       clientInfo        `json:"clientInfo"`
	RootPath         string            `json:"rootPath"`
	RootURI          string            `json:"rootUri"`
	Capabilities     map[string]any    `json:"capabilities"`
	Trace            string            `json:"trace"`
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

func initializeParams(root string) initializeRequestParams {
	uri, _ := localFileURI(root, "")
	return initializeRequestParams{
		ProcessID:        nil,
		ClientInfo:       clientInfo{Name: "agw-context", Version: "v3"},
		RootPath:         root,
		RootURI:          uri,
		Capabilities:     map[string]any{"workspace": map[string]any{"workspaceFolders": true}, "textDocument": map[string]any{"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true}}},
		Trace:            "off",
		WorkspaceFolders: []workspaceFolder{{URI: uri, Name: filepath.Base(root)}},
	}
}

type documentSymbolRequestParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

func documentSymbolParams(uri string) documentSymbolRequestParams {
	return documentSymbolRequestParams{TextDocument: textDocumentIdentifier{URI: uri}}
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Detail         string              `json:"detail,omitempty"`
	Kind           int                 `json:"kind"`
	Tags           []int               `json:"tags,omitempty"`
	Deprecated     bool                `json:"deprecated,omitempty"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children,omitempty"`
}

type lspSymbolInformation struct {
	Name          string      `json:"name"`
	Kind          int         `json:"kind"`
	Tags          []int       `json:"tags,omitempty"`
	Deprecated    bool        `json:"deprecated,omitempty"`
	Location      lspLocation `json:"location"`
	ContainerName string      `json:"containerName,omitempty"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

func decodeDocumentSymbols(result []byte, requestedPath, root string, known map[string]struct{}, maxItems int) ([]contextpack.SymbolEntry, error) {
	if bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		return []contextpack.SymbolEntry{}, nil
	}
	var hierarchical []lspDocumentSymbol
	if err := strictDecode(result, &hierarchical); err == nil {
		entries := make([]contextpack.SymbolEntry, 0)
		for _, symbol := range hierarchical {
			if err := flattenDocumentSymbol(symbol, requestedPath, 0, &entries, maxItems); err != nil {
				return nil, err
			}
		}
		return entries, nil
	}
	var flat []lspSymbolInformation
	if err := strictDecode(result, &flat); err != nil {
		return nil, err
	}
	entries := make([]contextpack.SymbolEntry, 0, len(flat))
	for _, symbol := range flat {
		if len(entries) >= maxItems {
			return nil, fmt.Errorf("%w: symbol item limit exceeded", contextpack.ErrBudgetExceeded)
		}
		path, err := localPathFromURI(root, symbol.Location.URI, known)
		if err != nil || path != requestedPath {
			return nil, errors.New("LSP symbol location escaped the requested repository file")
		}
		entry, err := symbolEntry(symbol.Name, symbol.Kind, "", symbol.Location.Range)
		if err != nil {
			return nil, err
		}
		entry.Path = path
		entries = append(entries, entry)
	}
	return entries, nil
}

func flattenDocumentSymbol(symbol lspDocumentSymbol, path string, depth int, entries *[]contextpack.SymbolEntry, maxItems int) error {
	if depth > maxSymbolDepth {
		return fmt.Errorf("%w: nested LSP symbol depth exceeded", contextpack.ErrBudgetExceeded)
	}
	if len(*entries) >= maxItems {
		return fmt.Errorf("%w: symbol item limit exceeded", contextpack.ErrBudgetExceeded)
	}
	entry, err := symbolEntry(symbol.Name, symbol.Kind, symbol.Detail, symbol.Range)
	if err != nil {
		return err
	}
	entry.Path = path
	*entries = append(*entries, entry)
	for _, child := range symbol.Children {
		if err := flattenDocumentSymbol(child, path, depth+1, entries, maxItems); err != nil {
			return err
		}
	}
	return nil
}

func symbolEntry(name string, kind int, detail string, symbolRange lspRange) (contextpack.SymbolEntry, error) {
	if !validMetadataText(name, maxLSPStringBytes) || name == "" || !validMetadataText(detail, maxLSPStringBytes) || credentialContent([]byte(name+"\n"+detail)) {
		return contextpack.SymbolEntry{}, errors.New("LSP symbol metadata is malformed")
	}
	if err := validateLSPRange(symbolRange); err != nil {
		return contextpack.SymbolEntry{}, err
	}
	kindName, ok := lspSymbolKindName(kind)
	if !ok {
		return contextpack.SymbolEntry{}, errors.New("LSP symbol kind is unknown")
	}
	return contextpack.SymbolEntry{Name: name, Kind: kindName, Signature: detail, Line: symbolRange.Start.Line + 1, Column: symbolRange.Start.Character + 1}, nil
}

func validateLSPRange(value lspRange) error {
	for _, position := range []lspPosition{value.Start, value.End} {
		if position.Line < 0 || position.Line > maxLSPPosition || position.Character < 0 || position.Character > maxLSPPosition {
			return errors.New("LSP symbol range is outside its bound")
		}
	}
	if value.End.Line < value.Start.Line || (value.End.Line == value.Start.Line && value.End.Character < value.Start.Character) {
		return errors.New("LSP symbol range is inverted")
	}
	return nil
}

func lspSymbolKindName(kind int) (string, bool) {
	names := [...]string{"", "file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface", "function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enumMember", "struct", "event", "operator", "typeParameter"}
	if kind <= 0 || kind >= len(names) {
		return "", false
	}
	return names[kind], true
}

func validateLocalRepositoryRoot(root string) (string, error) {
	if !absoluteClean(root) || root == "/" {
		return "", errors.New("repository root is not canonical")
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("repository root is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return "", errors.New("repository root contains a symlink")
	}
	return root, nil
}

func validateSymbolRequestFiles(root string, request SymbolRequest) ([]contextpack.LexicalEntry, map[string]struct{}, error) {
	files := append([]contextpack.LexicalEntry(nil), request.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	known := make(map[string]struct{}, len(files))
	for _, file := range files {
		if !safeRelative(file.Path) || file.Path == "" {
			return nil, nil, errors.New("symbol request contains an unsafe path")
		}
		if _, exists := known[file.Path]; exists {
			return nil, nil, errors.New("symbol request contains duplicate paths")
		}
		if err := validateLocalFile(root, file.Path); err != nil {
			return nil, nil, err
		}
		known[file.Path] = struct{}{}
	}
	return files, known, nil
}

func validateLocalFile(root, relative string) error {
	current := root
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("local symbol path is unsafe")
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("local symbol path contains an unavailable or symlink component")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return errors.New("local symbol path contains a non-directory component")
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return errors.New("local symbol path is not a regular file")
		}
	}
	return nil
}

func localFileURI(root, relative string) (string, error) {
	full := root
	if relative != "" {
		if !safeRelative(relative) {
			return "", errors.New("local symbol URI path is unsafe")
		}
		if err := validateLocalFile(root, relative); err != nil {
			return "", err
		}
		full = filepath.Join(root, filepath.FromSlash(relative))
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(full)}).String(), nil
}

func localPathFromURI(root, raw string, known map[string]struct{}) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" {
		return "", errors.New("LSP URI is not a local file URI")
	}
	full := filepath.Clean(filepath.FromSlash(parsed.Path))
	relative, err := filepath.Rel(root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || !safeRelative(filepath.ToSlash(relative)) {
		return "", errors.New("LSP URI escaped the repository")
	}
	relative = filepath.ToSlash(relative)
	if _, ok := known[relative]; !ok {
		return "", errors.New("LSP URI referenced an unknown repository file")
	}
	if err := validateLocalFile(root, relative); err != nil {
		return "", err
	}
	return relative, nil
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

var _ SymbolProvider = (*localSymbolProvider)(nil)
