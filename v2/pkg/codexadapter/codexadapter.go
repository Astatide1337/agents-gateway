// Package codexadapter runs the Codex CLI behind the agw.runtime.v1 boundary.
//
// The adapter deliberately has no access to a user's Codex home or auth file.
// It creates a private temporary CODEX_HOME for every invocation and gives
// Codex a loopback-only, unauthenticated provider definition. Provider
// credentials, if any, belong to the loopback broker outside this process.
package codexadapter

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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerbridge"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"github.com/Astatide1337/agents-gateway/v2/proto"
)

const (
	defaultMaxRuntime       = 30 * time.Minute
	defaultTerminationGrace = 5 * time.Second
	maxOutputLineBytes      = 1 << 20
	maxModelBytes           = 256
	maxURLBytes             = 2048
)

// Config is the bounded configuration needed by one adapter process.
type Config struct {
	CodexBinary            string
	ResponsesURL           string
	Workspace              string
	Model                  string
	MaxRuntime             time.Duration
	TerminationGrace       time.Duration
	HeartbeatInterval      time.Duration
	BrokerClientConfigPath string
	BrokerSocketPath       string
	MCPURL                 string
	ArtifactURL            string
	ArtifactCreateURL      string
	EnableTools            bool
	RequireArtifact        bool
}

// ConfigFromEnv reads only adapter-owned environment variables. In
// particular, it never reads OPENAI_API_KEY, CODEX_HOME, or any other auth
// variable. The Responses endpoint must be literal loopback.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Config{
		CodexBinary:            getenv("AGW_CODEX_BIN"),
		ResponsesURL:           getenv("AGW_CODEX_RESPONSES_URL"),
		Workspace:              getenv("AGW_CODEX_WORKSPACE"),
		Model:                  getenv("AGW_CODEX_MODEL"),
		MaxRuntime:             defaultMaxRuntime,
		TerminationGrace:       defaultTerminationGrace,
		HeartbeatInterval:      0,
		BrokerClientConfigPath: getenv("AGW_BROKER_CLIENT_CONFIG"),
		BrokerSocketPath:       getenv("AGW_BROKER_SOCKET"),
	}
	if cfg.CodexBinary == "" {
		cfg.CodexBinary = "codex"
	}
	if cfg.Workspace == "" {
		cfg.Workspace, _ = os.Getwd()
	}
	var err error
	if cfg.ResponsesURL != "" {
		if cfg.ResponsesURL, err = validateLoopbackURL(cfg.ResponsesURL); err != nil {
			return Config{}, fmt.Errorf("AGW_CODEX_RESPONSES_URL: %w", err)
		}
	}
	if cfg.Workspace, err = validateWorkspace(cfg.Workspace); err != nil {
		return Config{}, fmt.Errorf("AGW_CODEX_WORKSPACE: %w", err)
	}
	if err := validateBinaryName(cfg.CodexBinary); err != nil {
		return Config{}, fmt.Errorf("AGW_CODEX_BIN: %w", err)
	}
	if cfg.ResponsesURL != "" {
		if err := validateModel(cfg.Model); err != nil {
			return Config{}, fmt.Errorf("AGW_CODEX_MODEL: %w", err)
		}
	} else if cfg.Model != "" {
		if err := validateModel(cfg.Model); err != nil {
			return Config{}, fmt.Errorf("AGW_CODEX_MODEL: %w", err)
		}
	}
	if cfg.BrokerClientConfigPath == "" {
		cfg.BrokerClientConfigPath = brokerbridge.DefaultClientConfigPath
	}
	if cfg.BrokerSocketPath == "" {
		cfg.BrokerSocketPath = brokerbridge.DefaultSocketPath
	}
	if !filepath.IsAbs(cfg.BrokerClientConfigPath) || filepath.Clean(cfg.BrokerClientConfigPath) != cfg.BrokerClientConfigPath {
		return Config{}, errors.New("AGW_BROKER_CLIENT_CONFIG must be a canonical absolute path")
	}
	if !filepath.IsAbs(cfg.BrokerSocketPath) || filepath.Clean(cfg.BrokerSocketPath) != cfg.BrokerSocketPath {
		return Config{}, errors.New("AGW_BROKER_SOCKET must be a canonical absolute path")
	}
	for name, destination := range map[string]*time.Duration{
		"AGW_CODEX_MAX_RUNTIME":        &cfg.MaxRuntime,
		"AGW_CODEX_TERMINATION_GRACE":  &cfg.TerminationGrace,
		"AGW_CODEX_HEARTBEAT_INTERVAL": &cfg.HeartbeatInterval,
	} {
		value := getenv(name)
		if value == "" {
			continue
		}
		parsed, parseErr := time.ParseDuration(value)
		if parseErr != nil || parsed < 0 {
			return Config{}, fmt.Errorf("%s: must be a non-negative duration", name)
		}
		if name == "AGW_CODEX_MAX_RUNTIME" && (parsed < time.Second || parsed > 24*time.Hour) {
			return Config{}, fmt.Errorf("%s: must be between 1s and 24h", name)
		}
		if name == "AGW_CODEX_TERMINATION_GRACE" && (parsed < 100*time.Millisecond || parsed > time.Minute) {
			return Config{}, fmt.Errorf("%s: must be between 100ms and 1m", name)
		}
		if name == "AGW_CODEX_HEARTBEAT_INTERVAL" && parsed > time.Hour {
			return Config{}, fmt.Errorf("%s: must be at most 1h", name)
		}
		*destination = parsed
	}
	return cfg, nil
}

func validateLoopbackURL(raw string) (string, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return "", errors.New("must be a non-empty URL of at most 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be an http(s) URL without credentials, query, or fragment")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("must use http or https")
	}
	if u.EscapedPath() != "/v1" {
		return "", errors.New("path must be exactly /v1")
	}
	host := strings.ToLower(u.Hostname())
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("host must be localhost or a loopback IP")
		}
	}
	if port := u.Port(); port != "" {
		parsed, portErr := strconv.Atoi(port)
		if portErr != nil || parsed < 1 || parsed > 65535 {
			return "", errors.New("port must be between 1 and 65535")
		}
	}
	return strings.TrimRight(raw, "/"), nil
}

func validateWorkspace(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return "", errors.New("must be a valid directory")
	}
	abs, err := filepath.Abs(raw)
	if err != nil || abs == string(filepath.Separator) {
		return "", errors.New("must be an absolute non-root directory")
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("must name an existing directory")
	}
	return abs, nil
}

func validateBinaryName(raw string) error {
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return errors.New("must not be empty or contain NUL")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return errors.New("must not contain newlines")
	}
	if strings.Contains(raw, string(filepath.Separator)) && !filepath.IsAbs(raw) {
		return errors.New("path must be absolute")
	}
	return nil
}

func validateModel(raw string) error {
	if len(raw) == 0 || len(raw) > maxModelBytes || strings.HasPrefix(raw, "-") {
		return errors.New("must be a bounded model name")
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

// BuildArgs returns the explicit, noninteractive Codex invocation. The
// provider table is intentionally passed through --config rather than a
// user-controlled config file.
func BuildArgs(cfg Config) ([]string, error) {
	if _, err := validateLoopbackURL(cfg.ResponsesURL); err != nil {
		return nil, err
	}
	if _, err := validateWorkspace(cfg.Workspace); err != nil {
		return nil, err
	}
	if err := validateModel(cfg.Model); err != nil {
		return nil, err
	}
	provider := fmt.Sprintf(`{name=%s,base_url=%s,wire_api="responses",requires_openai_auth=false,request_max_retries=0,stream_max_retries=0}`,
		strconv.Quote("Agents Gateway loopback"), strconv.Quote(cfg.ResponsesURL))
	args := []string{
		"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--sandbox", "workspace-write", "--skip-git-repo-check", "--color", "never",
		"--cd", cfg.Workspace,
		"--config", `model_provider="agw_loopback"`,
		"--config", "model_providers.agw_loopback=" + provider,
		"-",
	}
	if cfg.EnableTools {
		if _, err := validateLoopbackEndpoint(cfg.MCPURL, brokerbridge.MCPPath); err != nil {
			return nil, fmt.Errorf("MCP URL: %w", err)
		}
		args = append(args[:len(args)-1],
			"--config", "mcp_servers.agw.url="+strconv.Quote(cfg.MCPURL),
			"--config", "mcp_servers.agw.required=true",
			"--config", "mcp_servers.agw.startup_timeout_sec=10",
			"-",
		)
	}
	args = append(args[:len(args)-1], "--model", cfg.Model, "-")
	return args, nil
}

func validateLoopbackEndpoint(raw, exactPath string) (string, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return "", errors.New("must be a non-empty URL of at most 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.EscapedPath() != exactPath {
		return "", errors.New("must be an exact HTTP loopback URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("host must be localhost or a loopback IP")
		}
	}
	return raw, nil
}

type runStart struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	RunID          string `json:"run_id"`
	AgentRef       string `json:"agent_ref"`
	Execution      struct {
		Agent struct {
			Name string `json:"name"`
		} `json:"agent"`
		Instructions string `json:"instructions"`
	} `json:"execution"`
}

type codexEvent struct {
	Type  string          `json:"type"`
	Item  json.RawMessage `json:"item"`
	Error json.RawMessage `json:"error"`
	Usage json.RawMessage `json:"usage"`
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type emitter struct {
	mu       sync.Mutex
	writer   io.Writer
	runID    string
	seq      uint64
	terminal bool
}

func (e *emitter) emit(typ string, terminal bool, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal {
		return errors.New("cannot emit an event after the terminal event")
	}
	e.seq++
	frame := proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: typ, RunID: e.runID, Seq: e.seq, Terminal: terminal, Data: raw}
	line, err := runtimeproto.EncodeLine(frame)
	if err != nil {
		return err
	}
	if terminal {
		e.terminal = true
	}
	return writeAll(e.writer, line)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// Run consumes one run.start request, executes Codex, and emits only strict
// agw.runtime.v1 events. A returned error means the adapter itself failed;
// the corresponding run.failed event is still emitted when a run was started.
func Run(ctx context.Context, input io.Reader, output io.Writer, diagnostics io.Writer, cfg Config) error {
	if input == nil || output == nil {
		return errors.New("input and output are required")
	}
	if cfg.CodexBinary == "" {
		cfg.CodexBinary = "codex"
	}
	if cfg.Workspace == "" {
		cfg.Workspace, _ = os.Getwd()
	}
	if cfg.MaxRuntime <= 0 {
		cfg.MaxRuntime = defaultMaxRuntime
	}
	if cfg.TerminationGrace <= 0 {
		cfg.TerminationGrace = defaultTerminationGrace
	}
	var bridge *brokerbridge.Bridge
	if cfg.ResponsesURL == "" {
		var endpoints brokerbridge.Endpoints
		var bridgeErr error
		bridge, endpoints, bridgeErr = brokerbridge.Start(brokerbridge.Config{
			ClientConfigPath: cfg.BrokerClientConfigPath,
			SocketPath:       cfg.BrokerSocketPath,
		})
		if bridgeErr != nil {
			return fmt.Errorf("start run broker bridge: %w", bridgeErr)
		}
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = bridge.Close(closeCtx)
		}()
		if cfg.Model != "" && cfg.Model != endpoints.AllowedModel {
			return errors.New("configured model does not match the broker capability")
		}
		cfg.Model = endpoints.AllowedModel
		cfg.ResponsesURL = endpoints.ResponsesBaseURL
		cfg.MCPURL = endpoints.MCPURL
		cfg.ArtifactURL = endpoints.ArtifactURL
		cfg.ArtifactCreateURL = endpoints.ArtifactCreateURL
		cfg.EnableTools = endpoints.ToolsEnabled
		cfg.RequireArtifact = endpoints.ArtifactEnabled
	}
	args, err := BuildArgs(cfg)
	if err != nil {
		return fmt.Errorf("invalid adapter configuration: %w", err)
	}
	decoder := runtimeproto.NewDecoder(input)
	first, err := decoder.Next()
	if err != nil {
		return fmt.Errorf("read run.start: %w", err)
	}
	if first.Kind != proto.KindRequest || first.Type != proto.RequestRunStart {
		return errors.New("first frame must be request run.start")
	}
	if first.Seq != 1 {
		return errors.New("run.start must have sequence 1")
	}
	var start runStart
	if err := json.Unmarshal(first.Data, &start); err != nil {
		return fmt.Errorf("decode run.start: %w", err)
	}
	if start.RunID != first.RunID {
		return errors.New("run.start run_id does not match envelope run_id")
	}
	if strings.TrimSpace(start.Execution.Agent.Name) == "" {
		start.Execution.Agent.Name = start.AgentRef
	}
	if err := validateIdentifier(start.Execution.Agent.Name); err != nil {
		return fmt.Errorf("agent id: %w", err)
	}

	e := &emitter{writer: output, runID: first.RunID}
	if err := e.emit(proto.EventRunStarted, false, map[string]string{
		"agent_id":   start.Execution.Agent.Name,
		"sandbox_id": "sandbox-" + shortDigest(first.RunID),
	}); err != nil {
		return fmt.Errorf("emit run.started: %w", err)
	}
	if err := e.emit(proto.EventModelRequested, false, map[string]string{"model": cfg.Model}); err != nil {
		return fmt.Errorf("emit model.requested: %w", err)
	}

	privateHome, err := os.MkdirTemp("", "agw-codex-home-")
	if err != nil {
		return fmt.Errorf("create private Codex home: %w", err)
	}
	defer os.RemoveAll(privateHome)
	cmdPath, err := resolveBinary(cfg.CodexBinary)
	if err != nil {
		return emitRunFailure(e, "codex_start_failed", err, diagnostics)
	}
	childCtx, cancel := context.WithTimeout(ctx, cfg.MaxRuntime)
	defer cancel()
	cmd := exec.Command(cmdPath, args...)
	cmd.Dir = cfg.Workspace
	cmd.Stdin = strings.NewReader(prompt(start.Execution.Instructions))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return emitRunFailure(e, "codex_start_failed", err, diagnostics)
	}
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{
		"PATH=" + safePath(cmdPath),
		"HOME=" + privateHome,
		"CODEX_HOME=" + privateHome,
		"TMPDIR=" + privateHome,
		"NO_COLOR=1",
		"CI=1",
	}
	if err := childCtx.Err(); err != nil {
		return emitRunCancelled(e, err)
	}
	if err := cmd.Start(); err != nil {
		return emitRunFailure(e, "codex_start_failed", err, diagnostics)
	}

	outputDone := make(chan outputResult, 1)
	go func() { outputDone <- consumeCodexOutput(stdout, e) }()

	cancelRequested := make(chan string, 1)
	inputError := make(chan error, 1)
	go readControl(decoder, first.RunID, first.Seq+1, e, cancelRequested, inputError)

	if cfg.HeartbeatInterval > 0 {
		go emitHeartbeats(childCtx, cfg.HeartbeatInterval, e)
	}

	var processErr error
	var reason string
	var stopTimer *time.Timer
	var codexOutput outputResult
	select {
	case codexOutput = <-outputDone:
	case reason = <-cancelRequested:
		stopTimer = requestTermination(cmd.Process, cfg.TerminationGrace)
		codexOutput = <-outputDone
	case err = <-inputError:
		stopTimer = requestTermination(cmd.Process, cfg.TerminationGrace)
		codexOutput = <-outputDone
		reason = "invalid control input: " + err.Error()
	case <-childCtx.Done():
		stopTimer = requestTermination(cmd.Process, cfg.TerminationGrace)
		codexOutput = <-outputDone
	}
	if codexOutput.err != nil && stopTimer == nil {
		stopTimer = requestTermination(cmd.Process, cfg.TerminationGrace)
	}
	defer func() {
		if stopTimer != nil {
			stopTimer.Stop()
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case processErr = <-waitDone:
	case <-childCtx.Done():
		if stopTimer == nil {
			stopTimer = requestTermination(cmd.Process, cfg.TerminationGrace)
		}
		processErr = <-waitDone
	}
	if reason != "" {
		if strings.HasPrefix(reason, "invalid control input:") {
			return emitRunFailure(e, "adapter_invalid_input", errors.New(reason), diagnostics)
		}
		return emitRunCancelled(e, errors.New(reason))
	}
	if err := ctx.Err(); err != nil {
		return emitRunCancelled(e, err)
	}
	if errors.Is(processErr, context.DeadlineExceeded) || errors.Is(childCtx.Err(), context.DeadlineExceeded) {
		return emitRunFailure(e, "adapter_timeout", errors.New("Codex exceeded the configured runtime limit"), diagnostics)
	}
	if codexOutput.err != nil {
		return emitRunFailure(e, "codex_output_invalid", codexOutput.err, diagnostics)
	}
	if processErr != nil {
		return emitRunFailure(e, "codex_failed", processErr, diagnostics)
	}
	if codexOutput.upstreamError != "" {
		return emitRunFailure(e, "codex_failed", errors.New(codexOutput.upstreamError), diagnostics)
	}
	result := map[string]any{"status": "completed"}
	if codexOutput.lastMessage != "" {
		result["message"] = redactText(codexOutput.lastMessage)
	}
	authored, authoredErr := collectAuthoredArtifacts(cfg.Workspace)
	if authoredErr != nil {
		return emitRunFailure(e, "artifact_manifest_invalid", authoredErr, diagnostics)
	}
	if len(authored) > 0 {
		endpoint := cfg.ArtifactCreateURL
		if endpoint == "" && cfg.ArtifactURL != "" {
			endpoint, authoredErr = authoredArtifactEndpoint(cfg.ArtifactURL)
		}
		if authoredErr != nil || endpoint == "" {
			return emitRunFailure(e, "artifact_upload_unavailable", errors.New("authored artifact upload capability is unavailable"), diagnostics)
		}
		for _, generated := range authored {
			reference, uploadErr := uploadAuthoredArtifact(childCtx, endpoint, generated)
			if uploadErr != nil {
				return emitRunFailure(e, "artifact_upload_failed", uploadErr, diagnostics)
			}
			if err := e.emit(proto.EventArtifactCreated, false, map[string]any{"artifact_id": reference.ID, "artifact": reference}); err != nil {
				return fmt.Errorf("emit artifact.created: %w", err)
			}
		}
	}
	var outputArtifact *workflow.ArtifactRef
	if cfg.ArtifactURL != "" {
		artifact, uploadErr := uploadRunOutput(childCtx, cfg.ArtifactURL, result)
		if uploadErr != nil {
			return emitRunFailure(e, "artifact_upload_failed", uploadErr, diagnostics)
		}
		outputArtifact = &artifact
		if err := e.emit(proto.EventArtifactCreated, false, map[string]any{"artifact_id": artifact.ID, "artifact": artifact}); err != nil {
			return fmt.Errorf("emit artifact.created: %w", err)
		}
	} else if cfg.RequireArtifact {
		return emitRunFailure(e, "artifact_upload_unavailable", errors.New("run output upload capability is unavailable"), diagnostics)
	}
	completed := map[string]any{"result": result}
	if outputArtifact != nil {
		completed["output"] = outputArtifact
	}
	if err := e.emit(proto.EventRunCompleted, true, completed); err != nil {
		return fmt.Errorf("emit run.completed: %w", err)
	}
	return nil
}

func uploadRunOutput(ctx context.Context, endpoint string, result map[string]any) (workflow.ArtifactRef, error) {
	if _, err := validateLoopbackEndpoint(endpoint, brokerbridge.ArtifactPath); err != nil {
		return workflow.ArtifactRef{}, err
	}
	payload, err := json.Marshal(struct {
		Schema string         `json:"schema"`
		Result map[string]any `json:"result"`
	}{Schema: "agents-gateway.run-output.v1", Result: result})
	if err != nil {
		return workflow.ArtifactRef{}, errors.New("encode run output")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return workflow.ArtifactRef{}, errors.New("create run output request")
	}
	request.Header.Set("Content-Type", "application/vnd.agw.run-output+json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer transport.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return workflow.ArtifactRef{}, errors.New("upload run output")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return workflow.ArtifactRef{}, fmt.Errorf("run output upload returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var artifact workflow.ArtifactRef
	if err := decoder.Decode(&artifact); err != nil {
		return workflow.ArtifactRef{}, errors.New("decode run output reference")
	}
	if err := artifact.Validate(); err != nil {
		return workflow.ArtifactRef{}, errors.New("invalid run output reference")
	}
	return artifact, nil
}

type outputResult struct {
	lastMessage   string
	upstreamError string
	err           error
}

func consumeCodexOutput(reader io.Reader, e *emitter) outputResult {
	var result outputResult
	scanner := bufio.NewReaderSize(reader, 64<<10)
	for {
		line, err := readBoundedLine(scanner)
		if len(line) != 0 {
			var event codexEvent
			if unmarshalErr := json.Unmarshal(line, &event); unmarshalErr != nil || event.Type == "" {
				if unmarshalErr == nil {
					unmarshalErr = errors.New("event type is missing")
				}
				result.err = fmt.Errorf("Codex emitted invalid JSONL: %w", unmarshalErr)
				return result
			}
			switch event.Type {
			case "item.completed":
				var item codexItem
				if json.Unmarshal(event.Item, &item) == nil && item.Type == "agent_message" && item.Text != "" {
					result.lastMessage = truncate(redactText(item.Text), 64<<10)
					if emitErr := e.emit(proto.EventAssistantMessage, false, map[string]string{"message": result.lastMessage, "role": "assistant"}); emitErr != nil {
						result.err = emitErr
						return result
					}
				}
			case "turn.completed":
				data := map[string]any{"status": "ok"}
				if inputTokens, outputTokens, ok := boundedUsage(event.Usage); ok {
					data["input_tokens"] = inputTokens
					data["output_tokens"] = outputTokens
				}
				if emitErr := e.emit(proto.EventModelCompleted, false, data); emitErr != nil {
					result.err = emitErr
					return result
				}
			case "error", "turn.failed":
				result.upstreamError = extractError(event.Error)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result
			}
			result.err = err
			return result
		}
	}
}

func readBoundedLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > maxOutputLineBytes {
			return nil, errors.New("Codex output line exceeds 1 MiB")
		}
		if err == nil {
			return bytesTrimLine(line), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return bytesTrimLine(line), io.EOF
		}
		return nil, err
	}
}

func bytesTrimLine(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line
}

func readControl(decoder *runtimeproto.Decoder, runID string, expectedSeq uint64, e *emitter, cancelRequested chan<- string, inputError chan<- error) {
	for {
		frame, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			inputError <- err
			return
		}
		if frame.RunID != runID {
			inputError <- errors.New("control frame run_id does not match active run")
			return
		}
		if frame.Seq != expectedSeq {
			inputError <- fmt.Errorf("control frame sequence must be %d", expectedSeq)
			return
		}
		expectedSeq++
		if frame.Kind != proto.KindRequest {
			inputError <- errors.New("control frame must be a request")
			return
		}
		switch frame.Type {
		case proto.RequestPing:
			if err := e.emit(proto.EventHeartbeat, false, map[string]any{}); err != nil {
				inputError <- err
				return
			}
		case proto.RequestCancel:
			var cancel struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(frame.Data, &cancel); err != nil {
				inputError <- err
				return
			}
			if cancel.Reason == "" {
				cancel.Reason = "cancel requested"
			}
			select {
			case cancelRequested <- redactText(cancel.Reason):
			default:
			}
			return
		default:
			inputError <- fmt.Errorf("unsupported control request %q", frame.Type)
			return
		}
	}
}

func emitHeartbeats(ctx context.Context, interval time.Duration, e *emitter) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = e.emit(proto.EventHeartbeat, false, map[string]any{})
		}
	}
}

func requestTermination(process *os.Process, grace time.Duration) *time.Timer {
	if process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGTERM); err != nil {
		_ = process.Signal(syscall.SIGTERM)
	}
	return time.AfterFunc(grace, func() {
		if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil {
			_ = process.Kill()
		}
	})
}

func resolveBinary(binary string) (string, error) {
	if strings.Contains(binary, string(filepath.Separator)) {
		if !filepath.IsAbs(binary) {
			return "", errors.New("Codex binary path must be absolute")
		}
		info, err := os.Stat(binary)
		if err != nil || info.IsDir() {
			return "", errors.New("Codex binary is not executable")
		}
		return binary, nil
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", errors.New("Codex executable was not found")
	}
	return path, nil
}

func safePath(binary string) string {
	dir := filepath.Dir(binary)
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	if dir == "." || strings.Contains(path, dir) {
		return path
	}
	return dir + string(os.PathListSeparator) + path
}

func prompt(instructions string) string {
	return "You are operating inside an isolated Agents Gateway sandbox. Work only in the current workspace. Follow the task instructions and report the result succinctly.\n\n" +
		"When a standalone document, code file, HTML page, SVG, diagram, or interactive component materially helps the result, publish it as an artifact. Write its source inside the workspace, then create one descriptor per artifact in .agw/artifacts/<name>.json with this exact shape: " +
		`{"schema":"agents-gateway.artifact.v1","title":"Human title","description":"Short description","content_kind":"document|code|single_page_html|svg|diagram|interactive_component","media_type":"text/plain|text/markdown|text/html|image/svg+xml|application/json","source":"relative/path","capabilities":[]}` +
		". Use capabilities [\"sandboxed_scripts\"] only for an interactive_component that genuinely requires local JavaScript. Network, external calls, host access, and credentials are unavailable. Do not create an artifact for trivial status text.\n\nTask instructions:\n" + instructions + "\n"
}

func validateIdentifier(value string) error {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
		return errors.New("must be a non-empty bounded identifier")
	}
	return nil
}

func shortDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:16]
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

var secretPatterns = []string{"sk-", "ghp_", "github_pat_", "glpat-", "glsa_", "xoxb-", "xoxp-", "agt_", "ags_"}

func redactText(value string) string {
	value = strings.ReplaceAll(value, "Authorization: Bearer ", "Authorization: Bearer [REDACTED]")
	value = strings.ReplaceAll(value, "authorization: bearer ", "authorization: bearer [REDACTED]")
	for _, prefix := range secretPatterns {
		for {
			index := strings.Index(value, prefix)
			if index < 0 {
				break
			}
			end := index + len(prefix)
			for end < len(value) && !unicode.IsSpace(rune(value[end])) && !strings.ContainsRune("\"'`,;)]}", rune(value[end])) {
				end++
			}
			value = value[:index] + prefix + "[REDACTED]" + value[end:]
			break
		}
	}
	return value
}

// Redact is exposed for the command package so diagnostics never bypass the
// same conservative secret scrubbing used for protocol messages.
func Redact(value string) string { return redactText(value) }

func extractError(raw json.RawMessage) string {
	var value struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &value) == nil && value.Message != "" {
		return truncate(redactText(value.Message), 64<<10)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return truncate(redactText(text), 64<<10)
	}
	return "Codex reported an unspecified error"
}

func boundedUsage(raw json.RawMessage) (int64, int64, bool) {
	if len(raw) == 0 || string(raw) == "null" || len(raw) > 4096 {
		return 0, 0, false
	}
	var usage struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil || usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.OutputTokens < 0 {
		return 0, 0, false
	}
	// Cached input is a subset of input, not additional billable input.
	return usage.InputTokens, usage.OutputTokens, true
}

func emitRunFailure(e *emitter, code string, err error, diagnostics io.Writer) error {
	message := truncate(redactText(err.Error()), 64<<10)
	if diagnostics != nil {
		_, _ = fmt.Fprintf(diagnostics, "codex adapter: %s: %s\n", code, message)
	}
	if emitErr := e.emit(proto.EventRunFailed, true, map[string]proto.ErrorPayload{"error": {Code: code, Message: message}}); emitErr != nil {
		return fmt.Errorf("%s: emit run.failed: %w", code, emitErr)
	}
	return fmt.Errorf("%s: %s", code, message)
}

func emitRunCancelled(e *emitter, reason error) error {
	message := "cancel requested"
	if reason != nil && reason.Error() != "" {
		message = truncate(redactText(reason.Error()), 64<<10)
	}
	if err := e.emit(proto.EventRunCancelled, true, map[string]string{"reason": message}); err != nil {
		return fmt.Errorf("emit run.cancelled: %w", err)
	}
	return nil
}
