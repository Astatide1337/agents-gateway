// Package claudeadapter runs Claude Code behind the agw.runtime.v1 boundary.
//
// Claude Code only ever sees a loopback Anthropic-compatible endpoint and a
// dummy token. The real provider credential remains mounted in the broker
// sidecar, which injects it only on its validated outbound request.
package claudeadapter

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

	"github.com/Astatide1337/agents-gateway/v3/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v3/pkg/codexadapter"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

const (
	defaultMaxRuntime       = 30 * time.Minute
	defaultTerminationGrace = 5 * time.Second
	defaultMaxTurns         = 100
	maxOutputLineBytes      = 1 << 20
	maxModelBytes           = 256
	maxURLBytes             = 2048
	maxTaskBytes            = 64 << 10
	maxInstructionsBytes    = 64 << 10
	maxPromptBytes          = 256 << 10
	maxChildDiagnostics     = 16 << 10
	maxClaudeTurns          = 1000

	mcpConfigFileName = "mcp.json"
)

// Config is the bounded configuration for one Claude Code invocation.
type Config struct {
	ClaudeBinary      string
	AnthropicBaseURL  string
	Workspace         string
	Model             string
	MCPURL            string
	ArtifactURL       string
	ArtifactCreateURL string
	Task              string
	Instructions      string
	MaxRuntime        time.Duration
	TerminationGrace  time.Duration
	MaxTurns          int
	RequireArtifact   bool
	EnableTools       bool
}

// ConfigFromEnv reads only AGW-owned values. It intentionally does not read
// ANTHROPIC_API_KEY, HOME, CLAUDE_CONFIG_DIR, or any inherited auth setting.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Config{
		ClaudeBinary:      getenv("AGW_CLAUDE_BIN"),
		AnthropicBaseURL:  getenv("AGW_CLAUDE_ANTHROPIC_BASE_URL"),
		Workspace:         getenv("AGW_CLAUDE_WORKSPACE"),
		Model:             getenv("AGW_CLAUDE_MODEL"),
		MCPURL:            getenv("AGW_CLAUDE_MCP_URL"),
		ArtifactURL:       getenv("AGW_CLAUDE_ARTIFACT_URL"),
		ArtifactCreateURL: getenv("AGW_CLAUDE_ARTIFACT_CREATE_URL"),
		Task:              getenv("AGW_TASK"),
		Instructions:      getenv("AGW_INSTRUCTIONS"),
		MaxRuntime:        defaultMaxRuntime,
		TerminationGrace:  defaultTerminationGrace,
		MaxTurns:          defaultMaxTurns,
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "claude"
	}
	if cfg.AnthropicBaseURL == "" {
		return Config{}, errors.New("AGW_CLAUDE_ANTHROPIC_BASE_URL is required")
	}
	var err error
	if cfg.AnthropicBaseURL, err = validateAnthropicBaseURL(cfg.AnthropicBaseURL); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_ANTHROPIC_BASE_URL: %w", err)
	}
	if cfg.Workspace, err = validateWorkspace(cfg.Workspace); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_WORKSPACE: %w", err)
	}
	if err := validateBinaryName(cfg.ClaudeBinary); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_BIN: %w", err)
	}
	if err := validateModel(cfg.Model); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_MODEL: %w", err)
	}
	if !validPayload(cfg.Task, maxTaskBytes, false) || !validPayload(cfg.Instructions, maxInstructionsBytes, true) {
		return Config{}, errors.New("AGW_TASK or AGW_INSTRUCTIONS is invalid")
	}
	brokerBase := getenv("AGW_BROKER")
	if brokerBase == "" {
		return Config{}, errors.New("AGW_BROKER is required")
	}
	brokerBase, err = validateLoopbackOrigin(brokerBase)
	if err != nil {
		return Config{}, fmt.Errorf("AGW_BROKER: %w", err)
	}
	if cfg.MCPURL == "" {
		cfg.MCPURL = brokerBase + "/mcp"
	}
	if cfg.MCPURL, err = validateLoopbackEndpoint(cfg.MCPURL, "/mcp"); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_MCP_URL: %w", err)
	}
	if cfg.ArtifactURL == "" {
		cfg.ArtifactURL = brokerBase + "/v1/artifacts/output"
	}
	if cfg.ArtifactURL, err = validateLoopbackEndpoint(cfg.ArtifactURL, "/v1/artifacts/output"); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_ARTIFACT_URL: %w", err)
	}
	if cfg.ArtifactCreateURL == "" {
		cfg.ArtifactCreateURL = brokerBase + "/v1/artifacts/create"
	}
	if cfg.ArtifactCreateURL, err = validateLoopbackEndpoint(cfg.ArtifactCreateURL, "/v1/artifacts/create"); err != nil {
		return Config{}, fmt.Errorf("AGW_CLAUDE_ARTIFACT_CREATE_URL: %w", err)
	}
	cfg.EnableTools = true
	cfg.RequireArtifact = true
	for name, destination := range map[string]*bool{
		"AGW_CLAUDE_ENABLE_TOOLS":     &cfg.EnableTools,
		"AGW_CLAUDE_REQUIRE_ARTIFACT": &cfg.RequireArtifact,
	} {
		if value := getenv(name); value != "" {
			parsed, parseErr := parseBoolean(value)
			if parseErr != nil {
				return Config{}, fmt.Errorf("%s: %w", name, parseErr)
			}
			*destination = parsed
		}
	}
	for name, destination := range map[string]*time.Duration{
		"AGW_CLAUDE_MAX_RUNTIME":       &cfg.MaxRuntime,
		"AGW_CLAUDE_TERMINATION_GRACE": &cfg.TerminationGrace,
	} {
		if value := getenv(name); value != "" {
			parsed, parseErr := time.ParseDuration(value)
			if parseErr != nil || parsed < 0 {
				return Config{}, fmt.Errorf("%s: must be a non-negative duration", name)
			}
			if name == "AGW_CLAUDE_MAX_RUNTIME" && (parsed < time.Second || parsed > 24*time.Hour) {
				return Config{}, fmt.Errorf("%s: must be between 1s and 24h", name)
			}
			if name == "AGW_CLAUDE_TERMINATION_GRACE" && (parsed < 100*time.Millisecond || parsed > time.Minute) {
				return Config{}, fmt.Errorf("%s: must be between 100ms and 1m", name)
			}
			*destination = parsed
		}
	}
	if value := getenv("AGW_CLAUDE_MAX_TURNS"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed < 1 || parsed > maxClaudeTurns {
			return Config{}, errors.New("AGW_CLAUDE_MAX_TURNS must be between 1 and 1000")
		}
		cfg.MaxTurns = parsed
	}
	return cfg, nil
}

// BuildArgs returns a noninteractive Claude Code invocation. The prompt is an
// argument, not shell text, and the MCP configuration is a private file with
// no credentials.
func BuildArgs(cfg Config, prompt, mcpConfigPath string) ([]string, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if !validPayload(prompt, maxPromptBytes, false) {
		return nil, errors.New("prompt is invalid")
	}
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", strconv.Itoa(cfg.MaxTurns),
		"--model", cfg.Model,
		"--dangerously-skip-permissions",
	}
	if cfg.EnableTools {
		if mcpConfigPath == "" || !filepath.IsAbs(mcpConfigPath) {
			return nil, errors.New("private MCP configuration path is required")
		}
		args = append(args, "--mcp-config", mcpConfigPath)
	}
	return args, nil
}

func validateConfig(cfg Config) error {
	if _, err := validateAnthropicBaseURL(cfg.AnthropicBaseURL); err != nil {
		return err
	}
	if _, err := validateWorkspace(cfg.Workspace); err != nil {
		return err
	}
	if err := validateBinaryName(cfg.ClaudeBinary); err != nil {
		return err
	}
	if err := validateModel(cfg.Model); err != nil {
		return err
	}
	if cfg.MaxRuntime < time.Second || cfg.MaxRuntime > 24*time.Hour || cfg.TerminationGrace < 100*time.Millisecond || cfg.TerminationGrace > time.Minute || cfg.MaxTurns < 1 || cfg.MaxTurns > maxClaudeTurns {
		return errors.New("Claude runtime limits are outside the allowed range")
	}
	if cfg.MCPURL != "" {
		if _, err := validateLoopbackEndpoint(cfg.MCPURL, "/mcp"); err != nil {
			return err
		}
	}
	return nil
}

func validateAnthropicBaseURL(raw string) (string, error) {
	if raw == "" || len(raw) > maxURLBytes || strings.TrimSpace(raw) != raw {
		return "", errors.New("must be a loopback HTTP URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/api") {
		return "", errors.New("must be an exact loopback HTTP origin or /api base")
	}
	if !isLoopbackHost(u.Hostname()) {
		return "", errors.New("host must be loopback")
	}
	if port := u.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return "", errors.New("port must be between 1 and 65535")
		}
	}
	return strings.TrimRight(raw, "/"), nil
}

func validateLoopbackOrigin(raw string) (string, error) {
	if raw == "" || len(raw) > maxURLBytes || strings.TrimSpace(raw) != raw {
		return "", errors.New("must be a loopback HTTP origin")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || !isLoopbackHost(u.Hostname()) {
		return "", errors.New("must be an exact loopback HTTP origin")
	}
	return strings.TrimRight(raw, "/"), nil
}

func validateLoopbackEndpoint(raw, exactPath string) (string, error) {
	if raw == "" || len(raw) > maxURLBytes || strings.TrimSpace(raw) != raw {
		return "", errors.New("must be a bounded loopback HTTP URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.EscapedPath() != exactPath || !isLoopbackHost(u.Hostname()) {
		return "", errors.New("must be an exact loopback HTTP endpoint")
	}
	return raw, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateWorkspace(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return "", errors.New("must be an existing directory")
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
	if raw == "" || strings.ContainsRune(raw, '\x00') || strings.ContainsAny(raw, "\r\n") {
		return errors.New("must not be empty or contain control characters")
	}
	if strings.Contains(raw, string(filepath.Separator)) && !filepath.IsAbs(raw) {
		return errors.New("path must be absolute")
	}
	return nil
}

func validateModel(raw string) error {
	if raw == "" || len(raw) > maxModelBytes || strings.HasPrefix(raw, "-") || !validPayload(raw, maxModelBytes, false) {
		return errors.New("must be a bounded model name")
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

func validPayload(value string, max int, allowEmpty bool) bool {
	return len(value) <= max && (allowEmpty || strings.TrimSpace(value) != "") && !strings.ContainsRune(value, '\x00')
}

func parseBoolean(value string) (bool, error) {
	switch value {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, errors.New("must be true, false, 1, or 0")
	}
}

// Run consumes one run.start frame, runs Claude Code, and emits only
// validated agw.runtime.v1 events. It emits a terminal run.failed or
// run.cancelled event before returning an adapter error.
func Run(ctx context.Context, input io.Reader, output io.Writer, diagnostics io.Writer, cfg Config) error {
	if input == nil || output == nil {
		return errors.New("input and output are required")
	}
	if err := validateConfig(cfg); err != nil {
		return fmt.Errorf("invalid adapter configuration: %w", err)
	}
	decoder := runtimeproto.NewDecoder(input)
	first, err := decoder.Next()
	if err != nil {
		return fmt.Errorf("read run.start: %w", err)
	}
	if first.Kind != proto.KindRequest || first.Type != proto.RequestRunStart || first.Seq != 1 {
		return errors.New("first frame must be request run.start with sequence 1")
	}
	var start runStart
	if err := json.Unmarshal(first.Data, &start); err != nil || start.RunID != first.RunID {
		return errors.New("run.start identity is invalid")
	}
	if strings.TrimSpace(start.Execution.Agent.Name) == "" {
		start.Execution.Agent.Name = start.AgentRef
	}
	if !validPayload(start.Execution.Agent.Name, 256, false) {
		return errors.New("run.start agent identity is invalid")
	}
	e := &emitter{writer: output, runID: first.RunID}
	if err := e.emit(proto.EventRunStarted, false, map[string]string{"agent_id": start.Execution.Agent.Name, "sandbox_id": "sandbox-" + shortDigest(first.RunID)}); err != nil {
		return fmt.Errorf("emit run.started: %w", err)
	}
	if err := e.emit(proto.EventModelRequested, false, map[string]string{"model": cfg.Model}); err != nil {
		return fmt.Errorf("emit model.requested: %w", err)
	}

	privateRoot, err := os.MkdirTemp(cfg.Workspace, ".agw-claude-")
	if err != nil {
		return emitFailure(e, diagnostics, "private_runtime_directory", errors.New("create private Claude runtime directory"))
	}
	defer os.RemoveAll(privateRoot)
	if err := os.Chmod(privateRoot, 0700); err != nil {
		return emitFailure(e, diagnostics, "private_runtime_directory", errors.New("secure private Claude runtime directory"))
	}
	configDir := filepath.Join(privateRoot, "config")
	homeDir := filepath.Join(privateRoot, "home")
	tmpDir := filepath.Join(privateRoot, "tmp")
	for _, directory := range []string{configDir, homeDir, tmpDir} {
		if err := os.Mkdir(directory, 0700); err != nil {
			return emitFailure(e, diagnostics, "private_runtime_directory", errors.New("create private Claude runtime directory"))
		}
	}
	mcpPath := ""
	if cfg.EnableTools {
		mcpPath = filepath.Join(configDir, mcpConfigFileName)
		if err := writeMCPConfig(mcpPath, cfg.MCPURL); err != nil {
			return emitFailure(e, diagnostics, "mcp_configuration", err)
		}
	}
	prompt := buildPrompt(cfg.Instructions, cfg.Task)
	args, err := BuildArgs(cfg, prompt, mcpPath)
	if err != nil {
		return emitFailure(e, diagnostics, "adapter_invalid_input", err)
	}
	binary, err := resolveBinary(cfg.ClaudeBinary)
	if err != nil {
		return emitFailure(e, diagnostics, "claude_binary_unavailable", err)
	}

	childCtx, cancel := context.WithTimeout(ctx, cfg.MaxRuntime)
	defer cancel()
	command := exec.Command(binary, args...)
	command.Dir = cfg.Workspace
	command.Env = []string{
		"PATH=" + safePath(binary),
		"HOME=" + homeDir,
		"CLAUDE_CONFIG_DIR=" + configDir,
		"TMPDIR=" + tmpDir,
		"ANTHROPIC_BASE_URL=" + cfg.AnthropicBaseURL,
		"ANTHROPIC_API_KEY=agw-loopback-dummy",
		"ANTHROPIC_AUTH_TOKEN=agw-loopback-dummy",
		"CI=1",
		"NO_COLOR=1",
		"TERM=dumb",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return emitFailure(e, diagnostics, "claude_process_failed", errors.New("create Claude output pipe"))
	}
	var childDiagnostics boundedDiagnostics
	command.Stderr = &childDiagnostics
	if err := command.Start(); err != nil {
		return emitFailure(e, diagnostics, "claude_process_failed", errors.New("start Claude Code"))
	}
	outputDone := make(chan outputResult, 1)
	go func() { outputDone <- consumeClaudeOutput(stdout, e, cfg.Model) }()

	// StdoutPipe's contract requires every read to finish before Wait closes
	// the pipe. Keep Wait out of the read path: the output consumer reports
	// completion first, and only then do we reap the child. On cancellation or
	// malformed output, terminate the process before waiting so a child that is
	// still writing cannot outlive this run.
	var stopTimer *time.Timer
	var parsed outputResult
	select {
	case parsed = <-outputDone:
	case <-childCtx.Done():
		stopTimer = requestTermination(command.Process, cfg.TerminationGrace)
		parsed = <-outputDone
	}
	if parsed.err != nil || parsed.upstreamError != "" {
		if stopTimer == nil {
			stopTimer = requestTermination(command.Process, cfg.TerminationGrace)
		}
	}
	processErr := command.Wait()
	if stopTimer != nil {
		stopTimer.Stop()
	}
	if childCtx.Err() != nil {
		return emitCancelled(e, diagnostics, childCtx.Err())
	}
	if parsed.err != nil {
		return emitFailure(e, diagnostics, "claude_output_invalid", parsed.err)
	}
	if parsed.upstreamError != "" {
		return emitFailure(e, diagnostics, "claude_failed", errors.New(parsed.upstreamError))
	}
	if processErr != nil {
		return emitFailure(e, diagnostics, "claude_failed", classifyProcessFailure(processErr, childDiagnostics.String()))
	}
	if !parsed.sawSuccessResult {
		return emitFailure(e, diagnostics, "claude_output_incomplete", errors.New("Claude did not emit a successful result"))
	}

	result := map[string]any{"status": "completed"}
	if parsed.lastMessage != "" {
		result["message"] = parsed.lastMessage
	}
	authored, err := codexadapter.CollectAuthoredArtifactUploads(cfg.Workspace)
	if err != nil {
		return emitFailure(e, diagnostics, "artifact_manifest_invalid", err)
	}
	for index, upload := range authored {
		reference, uploadErr := codexadapter.UploadAuthoredArtifact(childCtx, cfg.ArtifactCreateURL, upload)
		if uploadErr != nil {
			return emitFailure(e, diagnostics, "artifact_upload_failed", uploadErr)
		}
		role := "supporting"
		if index == 0 {
			role = "primary"
		}
		if err := e.emit(proto.EventArtifactCreated, false, map[string]any{"artifact_id": reference.ID, "output_role": role, "artifact": reference}); err != nil {
			return fmt.Errorf("emit artifact.created: %w", err)
		}
	}
	var outputArtifact *artifactcatalog.ArtifactRef
	if cfg.RequireArtifact {
		artifact, uploadErr := codexadapter.UploadRunOutput(childCtx, cfg.ArtifactURL, result)
		if uploadErr != nil {
			return emitFailure(e, diagnostics, "artifact_upload_failed", uploadErr)
		}
		outputArtifact = &artifact
		if err := e.emit(proto.EventArtifactCreated, false, map[string]any{"artifact_id": artifact.ID, "output_role": "supporting", "artifact": artifact}); err != nil {
			return fmt.Errorf("emit artifact.created: %w", err)
		}
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

type runStart struct {
	RunID     string `json:"run_id"`
	AgentRef  string `json:"agent_ref"`
	Execution struct {
		Agent struct {
			Name string `json:"name"`
		} `json:"agent"`
	} `json:"execution"`
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
		return errors.New("cannot emit after terminal runtime event")
	}
	e.seq++
	frame, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: typ, RunID: e.runID, Seq: e.seq, Terminal: terminal, Data: raw})
	if err != nil {
		return err
	}
	if terminal {
		e.terminal = true
	}
	return writeAll(e.writer, frame)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
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

type outputResult struct {
	lastMessage      string
	upstreamError    string
	err              error
	sawSuccessResult bool
}

func consumeClaudeOutput(reader io.Reader, e *emitter, model string) outputResult {
	var result outputResult
	scanner := bufio.NewReaderSize(reader, 64<<10)
	for {
		line, err := readBoundedLine(scanner)
		if len(line) > 0 {
			var event map[string]json.RawMessage
			if json.Unmarshal(line, &event) != nil {
				result.err = errors.New("Claude emitted invalid JSONL")
				return result
			}
			var eventType string
			if raw := event["type"]; json.Unmarshal(raw, &eventType) != nil || eventType == "" || len(eventType) > 128 {
				result.err = errors.New("Claude emitted an event without a valid type")
				return result
			}
			switch eventType {
			case "assistant":
				if message := eventTextFromAssistant(event["message"]); message != "" {
					result.lastMessage = message
					if emitErr := e.emit(proto.EventAssistantMessage, false, map[string]string{"message": message, "role": "assistant"}); emitErr != nil {
						result.err = emitErr
						return result
					}
				}
			case "stream_event":
				if message := eventTextFromStreamEvent(event["event"]); message != "" {
					result.lastMessage = message
					if emitErr := e.emit(proto.EventAssistantMessage, false, map[string]string{"message": message, "role": "assistant"}); emitErr != nil {
						result.err = emitErr
						return result
					}
				}
			case "result":
				var failed bool
				_ = json.Unmarshal(event["is_error"], &failed)
				var subtype string
				_ = json.Unmarshal(event["subtype"], &subtype)
				if failed || subtype != "success" || len(event["result"]) == 0 || string(event["result"]) == "null" {
					result.upstreamError = resultError(event["result"], event["error"])
					return result
				}
				var finalText string
				if err := json.Unmarshal(event["result"], &finalText); err != nil {
					result.err = errors.New("Claude emitted an invalid successful result payload")
					return result
				}
				if finalText != "" && result.lastMessage == "" {
					result.lastMessage = redactAndTruncate(finalText, 64<<10)
					if emitErr := e.emit(proto.EventAssistantMessage, false, map[string]string{"message": result.lastMessage, "role": "assistant"}); emitErr != nil {
						result.err = emitErr
						return result
					}
				}
				data := map[string]any{"model": model, "status": "ok"}
				if input, output, ok := boundedUsage(event["usage"]); ok {
					data["input_tokens"] = input
					data["output_tokens"] = output
				}
				if emitErr := e.emit(proto.EventModelCompleted, false, data); emitErr != nil {
					result.err = emitErr
					return result
				}
				result.sawSuccessResult = true
			case "error":
				result.upstreamError = resultError(event["error"], event["message"])
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result
			}
			result.err = errors.New("read Claude output")
			return result
		}
	}
}

func eventTextFromAssistant(raw json.RawMessage) string {
	var message struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return ""
	}
	for _, block := range message.Content {
		var value struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(block, &value) == nil && value.Type == "text" && value.Text != "" {
			return redactAndTruncate(value.Text, 64<<10)
		}
	}
	return ""
}

func eventTextFromStreamEvent(raw json.RawMessage) string {
	var event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &event) == nil && event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
		return redactAndTruncate(event.Delta.Text, 64<<10)
	}
	return ""
}

func boundedUsage(raw json.RawMessage) (int64, int64, bool) {
	if len(raw) == 0 || len(raw) > 4096 {
		return 0, 0, false
	}
	var usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return 0, 0, false
	}
	return usage.InputTokens, usage.OutputTokens, true
}

func resultError(primary, secondary json.RawMessage) string {
	for _, raw := range []json.RawMessage{primary, secondary} {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return redactAndTruncate(text, 64<<10)
		}
		var object struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &object) == nil && object.Message != "" {
			return redactAndTruncate(object.Message, 64<<10)
		}
	}
	return "Claude reported an unspecified error"
}

func readBoundedLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > maxOutputLineBytes {
			return nil, errors.New("Claude output line exceeds 1 MiB")
		}
		if err == nil {
			return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'}), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'}), io.EOF
		}
		return nil, err
	}
}

type boundedDiagnostics struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *boundedDiagnostics) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(value)
	remaining := maxChildDiagnostics - b.buf.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buf.Write(value)
	}
	return written, nil
}

func (b *boundedDiagnostics) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func resolveBinary(binary string) (string, error) {
	if strings.Contains(binary, string(filepath.Separator)) {
		if !filepath.IsAbs(binary) {
			return "", errors.New("Claude binary path must be absolute")
		}
		info, err := os.Stat(binary)
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return "", errors.New("Claude binary is not executable")
		}
		return binary, nil
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", errors.New("Claude executable was not found")
	}
	return path, nil
}

func safePath(binary string) string {
	directory := filepath.Dir(binary)
	if directory == "." {
		return "/usr/local/bin:/usr/bin:/bin"
	}
	return directory + ":/usr/local/bin:/usr/bin:/bin"
}

func writeMCPConfig(path, endpoint string) error {
	if _, err := validateLoopbackEndpoint(endpoint, "/mcp"); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		MCPServers map[string]map[string]string `json:"mcpServers"`
	}{MCPServers: map[string]map[string]string{"agw": {"type": "http", "url": endpoint}}})
	if err != nil {
		return errors.New("encode private MCP configuration")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return errors.New("write private MCP configuration")
	}
	return nil
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

func classifyProcessFailure(processErr error, diagnostics string) error {
	lower := strings.ToLower(diagnostics)
	switch {
	case strings.Contains(lower, "mcp"):
		return errors.New("Claude Code MCP initialization failed")
	case strings.Contains(lower, "anthropic"), strings.Contains(lower, "api"), strings.Contains(lower, "connection refused"):
		return errors.New("Claude Code could not reach the local broker")
	case strings.Contains(lower, "permission"), strings.Contains(lower, "read-only"):
		return errors.New("Claude Code encountered a sandbox filesystem restriction")
	default:
		if processErr == nil {
			return errors.New("Claude Code failed")
		}
		return processErr
	}
}

func buildPrompt(instructions, task string) string {
	return "You are operating inside an isolated Agents Gateway sandbox. Work only in the current workspace. Follow the task instructions and report the result succinctly.\n\n" +
		"When a standalone document, code file, HTML page, SVG, diagram, or interactive component materially helps the result, publish it as an artifact. Write its source inside the workspace, then create one descriptor per artifact in .agw/artifacts/<name>.json with this exact shape: " +
		`{"schema":"agents-gateway.artifact.v1","title":"Human title","description":"Short description","content_kind":"document|code|single_page_html|svg|diagram|interactive_component","media_type":"text/plain|text/markdown|text/html|image/svg+xml|application/json","source":"relative/path","capabilities":[]}` +
		". Use capabilities [\"sandboxed_scripts\"] only for an interactive_component that genuinely requires local JavaScript. Network, external calls, host access, and credentials are unavailable. Do not create an artifact for trivial status text.\n\nInstructions:\n" + instructions + "\n\nTask:\n" + task
}

func shortDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:16]
}

func redactAndTruncate(value string, max int) string {
	value = codexadapter.Redact(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

// Redact is used by the command boundary for diagnostics emitted before the
// adapter has started a run.
func Redact(value string) string { return redactAndTruncate(value, 64<<10) }

func emitFailure(e *emitter, diagnostics io.Writer, code string, err error) error {
	message := "runtime failure"
	if err != nil {
		message = redactAndTruncate(err.Error(), 64<<10)
	}
	if diagnostics != nil {
		_, _ = fmt.Fprintf(diagnostics, "claude adapter: %s: %s\n", code, message)
	}
	if emitErr := e.emit(proto.EventRunFailed, true, map[string]proto.ErrorPayload{"error": {Code: code, Message: message}}); emitErr != nil {
		return fmt.Errorf("%s: emit run.failed: %w", code, emitErr)
	}
	return fmt.Errorf("%s: %s", code, message)
}

func emitCancelled(e *emitter, diagnostics io.Writer, reason error) error {
	message := "run cancelled"
	if reason != nil && reason.Error() != "" {
		message = redactAndTruncate(reason.Error(), 64<<10)
	}
	if diagnostics != nil {
		_, _ = fmt.Fprintf(diagnostics, "claude adapter: run.cancelled: %s\n", message)
	}
	if emitErr := e.emit(proto.EventRunCancelled, true, map[string]string{"reason": message}); emitErr != nil {
		return fmt.Errorf("emit run.cancelled: %w", emitErr)
	}
	return errors.New("run cancelled")
}
