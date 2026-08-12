// Command agw-runtime-claude is the v3 Claude Code runtime entrypoint.
// It binds one headless Claude process to a pinned checkout and the loopback
// broker without inheriting provider credentials or a user's Claude home.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	"github.com/Astatide1337/agents-gateway/v3/pkg/claudeadapter"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

const (
	gitPath          = "git"
	safeGitPath      = "/usr/local/bin:/usr/bin:/bin"
	maxTaskBytes     = 64 << 10
	maxAgentRefBytes = 253
	eventPostTimeout = 15 * time.Second
)

type runtimeConfig struct {
	adapter         claudeadapter.Config
	baseSHA         string
	basePath        string
	specDigest      string
	brokerEventsURL string
	runUID          string
	agentRef        string
	task            string
	instructions    string
}

func configFromEnv(getenv func(string) string) (runtimeConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if strings.TrimSpace(getenv("AGW_HARNESS")) != "claude-code" {
		return runtimeConfig{}, errors.New("AGW_HARNESS must be explicitly set to claude-code")
	}
	brokerBase := getenv("AGW_BROKER")
	if brokerBase == "" || strings.TrimSpace(brokerBase) != brokerBase {
		return runtimeConfig{}, errors.New("AGW_BROKER is required and must not contain surrounding whitespace")
	}
	runUID := getenv("AGW_RUN_UID")
	agentRef := getenv("AGW_AGENT_REF")
	task := getenv("AGW_TASK")
	instructions := getenv("AGW_INSTRUCTIONS")
	specDigest := getenv("AGW_SPEC_DIGEST")
	if !validIdentifier(runUID, 128) || !validIdentifier(agentRef, maxAgentRefBytes) || !validPayloadText(task, maxTaskBytes, false) || !validPayloadText(instructions, maxTaskBytes, true) || !canonical.ValidDigest(specDigest) {
		return runtimeConfig{}, errors.New("runtime identity, task, or instructions are invalid")
	}
	baseSHA := getenv("AGW_BASE_SHA")
	if !validBaseSHA(baseSHA) || strings.TrimSpace(baseSHA) != baseSHA {
		return runtimeConfig{}, errors.New("AGW_BASE_SHA must be a lowercase 40- or 64-character commit SHA")
	}
	adapter, err := claudeadapter.ConfigFromEnv(getenv)
	if err != nil {
		return runtimeConfig{}, err
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
			return runtimeConfig{}, errors.New("AGW_BASE_PATH must be distinct from AGW_CLAUDE_WORKSPACE")
		}
	}
	return runtimeConfig{
		adapter: adapter, baseSHA: baseSHA, basePath: basePath, specDigest: specDigest,
		brokerEventsURL: strings.TrimRight(brokerBase, "/") + broker.RuntimeEventsPath,
		runUID:          runUID, agentRef: agentRef, task: task, instructions: instructions,
	}, nil
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

func verifyBaseSHA(ctx context.Context, config runtimeConfig) error {
	paths := []struct{ name, path string }{{name: "workspace", path: config.adapter.Workspace}}
	if config.basePath != "" {
		paths = append(paths, struct{ name, path string }{name: "base", path: config.basePath})
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
	command := exec.CommandContext(ctx, git, "-c", "safe.directory="+directory, "-C", directory, "rev-parse", "--verify", "HEAD^{commit}")
	command.Env = []string{"PATH=" + safeGitPath, "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	command.Stderr = io.Discard
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		return "", errors.New("git could not read the checkout revision")
	}
	actual := strings.TrimSpace(stdout.String())
	if !validBaseSHA(actual) {
		return "", errors.New("git returned an invalid checkout revision")
	}
	return actual, nil
}

func startFrame(config runtimeConfig) ([]byte, error) {
	prompt := config.task
	if config.instructions != "" {
		prompt = config.instructions + "\n\nTask:\n" + config.task
	}
	data, err := json.Marshal(map[string]any{
		"organization_id": "agw", "project_id": "default", "run_id": config.runUID,
		"workflow_name": "agentrun", "step_id": "work", "agent_ref": config.agentRef,
		"execution": map[string]any{
			"agent":            map[string]string{"kind": "Agent", "name": config.agentRef, "digest": config.specDigest},
			"sandbox_profile":  map[string]string{"kind": "Sandbox", "name": "work", "digest": config.specDigest},
			"instructions_ref": "agw://" + config.runUID + "/instructions", "instructions": prompt,
		},
	})
	if err != nil {
		return nil, errors.New("encode runtime start request")
	}
	return runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: config.runUID, Seq: 1, Data: data})
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
	return &eventPoster{ctx: ctx, endpoint: endpoint, client: &http.Client{Transport: transport, Timeout: eventPostTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

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
	if err := verifyBaseSHA(ctx, config); err != nil {
		writeDiagnostic(diagnostics, err)
		return 2
	}
	start, err := startFrame(config)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 2
	}
	if err := claudeadapter.Run(ctx, bytes.NewReader(start), newEventPoster(ctx, config.brokerEventsURL), diagnostics, config.adapter); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1
	}
	return 0
}

func writeDiagnostic(writer io.Writer, err error) {
	if writer == nil || err == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "agw-runtime-claude: %s\n", claudeadapter.Redact(err.Error()))
}

func main() {
	ctx, stop := signalNotifyContext(context.Background())
	defer stop()
	os.Exit(execute(ctx, os.Getenv, os.Stderr))
}

// Kept as a small seam so command tests can replace signal setup without
// making the runtime's security-sensitive execution path depend on globals.
var signalNotifyContext = func(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
