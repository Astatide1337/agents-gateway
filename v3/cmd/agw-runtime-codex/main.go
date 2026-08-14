// Command agw-runtime-codex is the v3 Codex runtime entrypoint.
//
// The command is intentionally a very small process boundary. It validates
// the immutable checkout identity, then delegates the runtime protocol and
// Codex process lifecycle to pkg/codexadapter. No provider credential is ever
// copied into the adapter process's child environment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/broker"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/codexadapter"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

const (
	gitPath = "git"
	// The runtime image and the work Sandbox both provide these paths. The
	// fixed PATH is used only for the short, read-only git identity check; the
	// adapter separately derives a safe PATH for Codex.
	safeGitPath          = "/usr/local/bin:/usr/bin:/bin"
	maxTaskBytes         = 64 << 10
	maxInstructionsBytes = 64 << 10
	maxAgentRefBytes     = 253
	eventPostTimeout     = 15 * time.Second
	brokerReadyTimeout   = 30 * time.Second
)

type runtimeConfig struct {
	adapter         codexadapter.Config
	brokerBaseURL   string
	baseSHA         string
	basePath        string
	specDigest      string
	brokerEventsURL string
	runUID          string
	agentRef        string
	task            string
	instructions    string
}

// configFromEnv is deliberately stricter than the reusable adapter parser:
// a production work Sandbox must explicitly bind the runtime to a pinned
// checkout and to the loopback broker. The adapter still owns validation of
// all URL, model, duration, and boolean fields.
func configFromEnv(getenv func(string) string) (runtimeConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	if harness := getenv("AGW_HARNESS"); strings.TrimSpace(harness) != "codex" {
		return runtimeConfig{}, errors.New("AGW_HARNESS must be explicitly set to codex")
	}
	workspaceRaw := getenv("AGW_CODEX_WORKSPACE")
	if workspaceRaw == "" || strings.TrimSpace(workspaceRaw) != workspaceRaw || !filepath.IsAbs(workspaceRaw) {
		return runtimeConfig{}, errors.New("AGW_CODEX_WORKSPACE must be an absolute path")
	}
	if broker := getenv("AGW_BROKER"); broker == "" || strings.TrimSpace(broker) != broker {
		return runtimeConfig{}, errors.New("AGW_BROKER is required and must not contain surrounding whitespace")
	}
	brokerBase := getenv("AGW_BROKER")
	runUID := getenv("AGW_RUN_UID")
	agentRef := getenv("AGW_AGENT_REF")
	task := getenv("AGW_TASK")
	instructions := getenv("AGW_INSTRUCTIONS")
	specDigest := getenv("AGW_SPEC_DIGEST")
	if !validIdentifier(runUID, 128) || !validIdentifier(agentRef, maxAgentRefBytes) || !validPayloadText(task, maxTaskBytes, false) || !validPayloadText(instructions, maxInstructionsBytes, true) || !canonical.ValidDigest(specDigest) {
		return runtimeConfig{}, errors.New("runtime identity, task, or instructions are invalid")
	}

	baseSHA := getenv("AGW_BASE_SHA")
	if baseSHA == "" || strings.TrimSpace(baseSHA) != baseSHA || !validBaseSHA(baseSHA) {
		return runtimeConfig{}, errors.New("AGW_BASE_SHA must be a lowercase 40- or 64-character commit SHA")
	}

	adapter, err := codexadapter.ConfigFromEnv(getenv)
	if err != nil {
		return runtimeConfig{}, err
	}
	if adapter.Workspace != workspaceRaw {
		return runtimeConfig{}, errors.New("AGW_CODEX_WORKSPACE changed during validation")
	}

	basePath := getenv("AGW_BASE_PATH")
	if basePath != "" {
		if strings.TrimSpace(basePath) != basePath || !filepath.IsAbs(basePath) {
			return runtimeConfig{}, errors.New("AGW_BASE_PATH must be an absolute path")
		}
		info, statErr := os.Stat(basePath)
		if statErr != nil || !info.IsDir() {
			return runtimeConfig{}, errors.New("AGW_BASE_PATH must name an existing directory")
		}
		if filepath.Clean(basePath) == filepath.Clean(adapter.Workspace) {
			return runtimeConfig{}, errors.New("AGW_BASE_PATH must be distinct from AGW_CODEX_WORKSPACE")
		}
	}

	return runtimeConfig{
		adapter: adapter, brokerBaseURL: brokerBase, baseSHA: baseSHA, basePath: basePath, specDigest: specDigest,
		brokerEventsURL: strings.TrimRight(brokerBase, "/") + broker.RuntimeEventsPath,
		runUID:          runUID, agentRef: agentRef, task: task, instructions: instructions,
	}, nil
}

// waitForBroker closes the startup race between the agent container and its
// loopback sidecar. Containers in a pod start concurrently; the agent must
// not emit its first durable runtime frame until the broker is listening.
// codexadapter has already validated AGW_BROKER as a loopback HTTP base URL,
// but this function repeats the narrow address check before opening a socket.
func waitForBroker(ctx context.Context, raw string) error {
	if ctx == nil || raw == "" {
		return errors.New("broker readiness configuration is unavailable")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("broker readiness URL is invalid")
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return errors.New("broker readiness URL is not loopback")
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(host, "80")
	}
	deadline := time.NewTimer(brokerReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	for {
		connection, dialErr := dialer.DialContext(ctx, "tcp", address)
		if dialErr == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("broker readiness wait canceled")
		case <-deadline.C:
			return errors.New("broker did not become ready")
		case <-ticker.C:
		}
	}
}

func validIdentifier(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '.' || character == '_' {
			continue
		}
		return false
	}
	return true

}

func validPayloadText(value string, max int, allowEmpty bool) bool {
	return len(value) <= max && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00') && (allowEmpty || strings.TrimSpace(value) != "")
}

func validBaseSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// verifyBaseSHA checks both the agent checkout and, when present, the
// pristine base checkout prepared by the clone initContainer. This prevents
// a runtime image from silently operating on a different commit than the
// controller resolved and recorded in AgentRun.status.
func verifyBaseSHA(ctx context.Context, config runtimeConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	paths := []struct {
		name string
		path string
	}{
		{name: "workspace", path: config.adapter.Workspace},
	}
	if config.basePath != "" {
		paths = append(paths, struct {
			name string
			path string
		}{name: "base", path: config.basePath})
	}
	for _, candidate := range paths {
		actual, err := gitHead(ctx, candidate.path)
		if err != nil {
			return fmt.Errorf("read %s checkout revision: %w", candidate.name, err)
		}
		if actual != config.baseSHA {
			return fmt.Errorf("%s checkout revision does not match AGW_BASE_SHA", candidate.name)
		}
	}
	return nil
}

func gitHead(ctx context.Context, directory string) (string, error) {
	git, err := exec.LookPath(gitPath)
	if err != nil {
		return "", errors.New("git executable is unavailable")
	}
	// The work PVC can be initialized by a different UID (and a bind-mounted
	// standalone checkout commonly belongs to the host user). Grant Git safe
	// directory status for this exact already-validated path only; never widen
	// Git's trust set through a global config file.
	command := exec.CommandContext(ctx, git, "-c", "safe.directory="+directory, "-C", directory, "rev-parse", "--verify", "HEAD^{commit}")
	// Git is an identity check only. Do not allow process or host environment
	// variables to select a credential helper, alternate config, or remote.
	command.Env = []string{
		"PATH=" + safeGitPath,
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("git could not read the checkout revision")
	}
	actual := strings.TrimSpace(stdout.String())
	if !validBaseSHA(actual) {
		return "", errors.New("git returned an invalid checkout revision")
	}
	return actual, nil
}

// runAdapter gives the adapter a writable, per-invocation temporary root.
// The Kubernetes work pod uses a read-only root filesystem, so relying on the
// image's /tmp would make the private CODEX_HOME creation fail. The workspace
// PVC is the explicitly writable surface supplied by the workload builder.
func runAdapter(ctx context.Context, input io.Reader, output, diagnostics io.Writer, config runtimeConfig) error {
	// The worktree is the writable subPath supplied by the work PVC. Its
	// parent (/workspace) is backed by the image root, which is read-only in a
	// work pod; creating the temporary root beside the worktree therefore fails
	// before Codex can start. Keep the private runtime state inside the
	// already-validated writable worktree and remove it after the invocation.
	tempRoot, err := os.MkdirTemp(config.adapter.Workspace, ".agw-runtime-tmp-")
	if err != nil {
		return fmt.Errorf("create runtime temporary directory: %w", err)
	}
	defer os.RemoveAll(tempRoot)
	if err := os.Chmod(tempRoot, 0700); err != nil {
		return errors.New("secure runtime temporary directory")
	}

	previous, hadPrevious := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", tempRoot); err != nil {
		return errors.New("configure runtime temporary directory")
	}
	defer func() {
		if hadPrevious {
			_ = os.Setenv("TMPDIR", previous)
		} else {
			_ = os.Unsetenv("TMPDIR")
		}
	}()

	return codexadapter.Run(ctx, input, output, diagnostics, config.adapter)
}

func startFrame(config runtimeConfig) ([]byte, error) {
	prompt := config.task
	if config.instructions != "" {
		prompt = config.instructions + "\n\nTask:\n" + config.task
	}
	data, err := json.Marshal(map[string]any{
		"organization_id": "agw",
		"project_id":      "default",
		"run_id":          config.runUID,
		"workflow_name":   "agentrun",
		"step_id":         "work",
		"agent_ref":       config.agentRef,
		"execution": map[string]any{
			"agent":            map[string]string{"kind": "Agent", "name": config.agentRef, "digest": config.specDigest},
			"sandbox_profile":  map[string]string{"kind": "Sandbox", "name": "work", "digest": config.specDigest},
			"instructions_ref": "agw://" + config.runUID + "/instructions",
			"instructions":     prompt,
		},
	})
	if err != nil {
		return nil, errors.New("encode runtime start request")
	}
	return runtimeproto.EncodeLine(proto.Envelope{
		Protocol: proto.ProtocolVersion, Kind: proto.KindRequest,
		Type: proto.RequestRunStart, RunID: config.runUID, Seq: 1,
		Data: data,
	})
}

type eventPoster struct {
	mu       sync.Mutex
	ctx      context.Context
	endpoint string
	client   *http.Client
}

func newEventPoster(ctx context.Context, endpoint string) *eventPoster {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &eventPoster{ctx: ctx, endpoint: endpoint, client: &http.Client{
		Transport: transport, Timeout: eventPostTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Write is intentionally synchronous: an event is not acknowledged to the
// adapter until the broker has durably accepted it. codexadapter emits one
// complete runtimeproto line per Write call through its writeAll helper.
func (p *eventPoster) Write(body []byte) (int, error) {
	if p == nil {
		return 0, errors.New("runtime event poster is unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil || p.client == nil || len(body) == 0 || len(body) > runtimeproto.MaxFrameBytes+1 || !bytes.HasSuffix(body, []byte{'\n'}) || bytes.Count(body, []byte{'\n'}) != 1 {
		return 0, errors.New("runtime event frame is invalid")
	}
	request, err := http.NewRequestWithContext(p.ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("create runtime event request")
	}
	request.Header.Set("Content-Type", broker.RuntimeEventMediaType)
	request.Header.Set("Cache-Control", "no-store")
	response, err := p.client.Do(request)
	if err != nil {
		return 0, errors.New("runtime event broker is unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return 0, errors.New("runtime event broker rejected the frame")
	}
	return len(body), nil
}

func execute(ctx context.Context, getenv func(string) string, diagnostics io.Writer) int {
	config, err := configFromEnv(getenv)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 2
	}
	if err := waitForBroker(ctx, config.brokerBaseURL); err != nil {
		writeDiagnostic(diagnostics, err)
		return 2
	}
	if err := verifyBaseSHA(ctx, config); err != nil {
		writeDiagnostic(diagnostics, err)
		return 2
	}
	start, err := startFrame(config)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 2
	}
	if err := runAdapter(ctx, bytes.NewReader(start), newEventPoster(ctx, config.brokerEventsURL), diagnostics, config); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1
	}
	return 0
}

func writeDiagnostic(writer io.Writer, err error) {
	if writer == nil || err == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "agw-runtime-codex: %s\n", codexadapter.Redact(err.Error()))
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Getenv, os.Stderr))
}
