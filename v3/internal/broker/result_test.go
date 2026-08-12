package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCallToolShapesOversizedSuccessfulResultAndStoresImmutableReference(t *testing.T) {
	store := newTestObjectStore()
	transport := &testTransport{mcpResult: `{"content":[{"type":"text","text":"` + strings.Repeat("result-", 7000) + `"}],"isError":false}`}
	broker := newTestBroker(t, testBrokerOptions{transport: transport, artifacts: store})

	result, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.Reference == nil || result.Reference.Digest == "" || result.Reference.URI == "" || result.Reference.Path == "" {
		t.Fatalf("missing immutable result reference: %#v", result.Reference)
	}
	if len(result.Content) >= 45<<10 || bytesContains(result.Content, []byte("result-result-result")) {
		t.Fatalf("oversized raw result was returned inline: %d bytes", len(result.Content))
	}
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Structured struct {
			Schema    string              `json:"schema"`
			Status    string              `json:"status"`
			Reference ToolResultReference `json:"reference"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result.Content, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.IsError || envelope.Structured.Schema != toolResultSchema || envelope.Structured.Status != "truncated" || len(envelope.Content) != 1 || !strings.Contains(envelope.Content[0].Text, "immutable reference") {
		t.Fatalf("unexpected shaped response: %#v", envelope)
	}
	if envelope.Structured.Reference != *result.Reference {
		t.Fatalf("MCP reference=%#v, direct reference=%#v", envelope.Structured.Reference, *result.Reference)
	}
	stored, err := store.Get(context.Background(), result.Reference.Path)
	if err != nil || !strings.Contains(string(stored), "result-result") || digestBytes(stored) != result.Reference.Digest {
		t.Fatalf("stored result bytes=%d error=%v digest=%s ref=%s", len(stored), err, digestBytes(stored), result.Reference.Digest)
	}
}

func TestCallToolStructuresCompilerDiagnosticsAndRedactsCredential(t *testing.T) {
	store := newTestObjectStore()
	transport := &testTransport{mcpResult: `{"content":[{"type":"text","text":"go test failed\n/workspace/repo/pkg/main.go:12:3: token=mcp-token\n../../outside.go:99:1: should be rejected"}],"isError":true}`}
	config := newTestConfig(testBrokerOptions{transport: transport, artifacts: store, withToolCredential: true})
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.Reference == nil || bytesContains(result.Content, []byte("mcp-token")) {
		t.Fatalf("credential or raw error leaked: %s", result.Content)
	}
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Structured struct {
			Status      string              `json:"status"`
			Diagnostics []ToolDiagnostic    `json:"diagnostics"`
			Reference   ToolResultReference `json:"reference"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result.Content, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.IsError || envelope.Structured.Status != "error" || len(envelope.Structured.Diagnostics) != 1 {
		t.Fatalf("unexpected diagnostic response: %#v", envelope)
	}
	diagnostic := envelope.Structured.Diagnostics[0]
	if diagnostic.File != "repo/pkg/main.go" || diagnostic.Line != 12 || diagnostic.Column != 3 || !strings.Contains(diagnostic.Message, "[REDACTED]") || strings.Contains(diagnostic.Message, "outside") {
		t.Fatalf("diagnostic=%#v", diagnostic)
	}
	if envelope.Structured.Reference != *result.Reference {
		t.Fatalf("MCP reference=%#v, direct reference=%#v", envelope.Structured.Reference, *result.Reference)
	}
	stored, err := store.Get(context.Background(), result.Reference.Path)
	if err != nil || bytesContains(stored, []byte("mcp-token")) || !bytesContains(stored, []byte("[REDACTED]")) {
		t.Fatalf("stored error leaked credential: error=%v body=%s", err, stored)
	}
}

func TestParseToolDiagnosticsUsesCanonicalOrder(t *testing.T) {
	texts := []string{
		"/workspace/repo/z.go:9:2: zed\n/workspace/repo/a.go:3:1: alpha",
		"/workspace/repo/a.go:2:4: earlier",
	}
	got := parseToolDiagnostics(texts, "/workspace", nil, maxDiagnosticsPerResult)
	if len(got) != 3 {
		t.Fatalf("diagnostics=%#v", got)
	}
	want := []struct {
		file string
		line int
	}{{"repo/a.go", 2}, {"repo/a.go", 3}, {"repo/z.go", 9}}
	for i := range want {
		if got[i].File != want[i].file || got[i].Line != want[i].line {
			t.Fatalf("diagnostic[%d]=%#v, want %s:%d", i, got[i], want[i].file, want[i].line)
		}
	}
}

func TestCallToolRedactsCredentialInSmallSuccessfulResult(t *testing.T) {
	transport := &testTransport{mcpResult: `{"content":[{"type":"text","text":"token=mcp-token"}],"isError":false}`}
	broker := newTestBroker(t, testBrokerOptions{transport: transport, withToolCredential: true})
	result, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if err != nil || result.Reference != nil || bytesContains(result.Content, []byte("mcp-token")) || !bytesContains(result.Content, []byte("[REDACTED]")) {
		t.Fatalf("small result=%s ref=%#v error=%v", result.Content, result.Reference, err)
	}
}

func TestCallToolRedactsCredentialShapesWithoutConfiguredCredential(t *testing.T) {
	transport := &testTransport{mcpResult: `{"content":[{"type":"text","text":"authorization: Bearer external-secret token=another-secret"}],"structuredContent":{"api_key":"api-secret","password":"pass-secret"},"isError":false}`}
	result := newTestBroker(t, testBrokerOptions{transport: transport}).mustCallTool(t, "read_issue")
	for _, secret := range []string{"external-secret", "another-secret", "api-secret", "pass-secret"} {
		if bytesContains(result.Content, []byte(secret)) {
			t.Fatalf("unconfigured credential leaked in result: %q: %s", secret, result.Content)
		}
	}
	if !bytesContains(result.Content, []byte("[REDACTED]")) {
		t.Fatalf("generic credential redaction marker missing: %s", result.Content)
	}
}

func TestCallToolRejectsDuplicateKeysInUpstreamResult(t *testing.T) {
	transport := &testTransport{mcpResult: `{"content":[],"content":[],"isError":false}`}
	broker := newTestBroker(t, testBrokerOptions{transport: transport})
	_, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if !errors.Is(err, ErrUpstreamInvalid) {
		t.Fatalf("duplicate upstream result error=%v", err)
	}
}

func TestToolResultStorageFailureFailsClosedAndDoesNotReplayWrites(t *testing.T) {
	store := &failingResultStore{err: errors.New("storage failed: credential=mcp-token")}
	largeResult := `{"content":[{"type":"text","text":"` + strings.Repeat("x", 50<<10) + `"}],"isError":false}`
	transport := &testTransport{mcpResult: largeResult}
	config := newTestConfig(testBrokerOptions{transport: transport, withToolCredential: true})
	config.ResultStore = store
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if !errors.Is(err, ErrToolResultUnavailable) || strings.Contains(err.Error(), "mcp-token") {
		t.Fatalf("read storage error=%v", err)
	}

	if err := broker.TransitionToEdit(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "write_issue", Approved: true, Arguments: []byte(`{"force":true,"issue":"427"}`)})
	if !errors.Is(err, ErrUnknownEffect) || strings.Contains(err.Error(), "mcp-token") {
		t.Fatalf("write storage error=%v", err)
	}
	_, outcomes := config.Effects.(*testLedger).snapshot()
	if len(outcomes) != 1 || outcomes[0].State != "unknown" {
		t.Fatalf("write storage outcomes=%#v", outcomes)
	}
}

func TestFilesystemResultStoreRejectsHazardousKeysAndFiles(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemResultStore(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	key := "runs/run/broker-results/abc.json"
	created, uri, err := store.Put(context.Background(), key, []byte("{}"), toolResultMediaType)
	if err != nil || !created || uri != "artifact://agw/abc" {
		t.Fatalf("first Put() = created=%v uri=%q error=%v", created, uri, err)
	}
	created, _, err = store.Put(context.Background(), key, []byte("{}"), toolResultMediaType)
	if err != nil || created {
		t.Fatalf("idempotent Put() = created=%v error=%v", created, err)
	}
	if _, _, err := store.Put(context.Background(), key, []byte("different"), toolResultMediaType); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("duplicate-key overwrite error=%v", err)
	}
	if _, err := store.Get(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	for _, unsafeKey := range []string{"../escape", "runs/../escape", "/absolute", "runs/run/broker-results/../x", "runs\\run\\x"} {
		if _, _, err := store.Put(context.Background(), unsafeKey, []byte("x"), toolResultMediaType); err == nil {
			t.Fatalf("unsafe key accepted: %q", unsafeKey)
		}
	}
	if _, _, err := store.Put(context.Background(), "runs/run/broker-results/large.json", []byte(strings.Repeat("x", 1025)), toolResultMediaType); err == nil {
		t.Fatal("unbounded result accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Put(canceled, "runs/run/broker-results/canceled.json", []byte("x"), toolResultMediaType); err == nil {
		t.Fatal("canceled write accepted")
	}

	outside := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystemResultStore(linkRoot, 1024); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("symlink root error=%v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "runs", "symlink-run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "runs", "symlink-run", "broker-results")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put(context.Background(), "runs/symlink-run/broker-results/x.json", []byte("x"), toolResultMediaType); err == nil {
		t.Fatal("child symlink accepted")
	}
	if err := os.MkdirAll(filepath.Join(root, "runs", "fifo-run"), 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "runs", "fifo-run", "broker-results")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put(context.Background(), "runs/fifo-run/broker-results/x.json", []byte("x"), toolResultMediaType); err == nil {
		t.Fatal("special-file path accepted")
	}
}

func TestFilesystemResultStoreConcurrentImmutablePut(t *testing.T) {
	store, err := NewFilesystemResultStore(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	start := make(chan struct{})
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _, putErr := store.Put(context.Background(), "runs/run/broker-results/concurrent.json", []byte("same"), toolResultMediaType)
			results <- putErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent immutable Put() error=%v", err)
		}
	}
}

func TestMCPRejectsCompressedToolResult(t *testing.T) {
	broker := newTestBroker(t, testBrokerOptions{transport: &testTransport{mcpEncoding: "gzip"}})
	_, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if !errors.Is(err, ErrUpstreamInvalid) {
		t.Fatalf("compressed result error=%v", err)
	}
}

type failingResultStore struct {
	err error
}

func (s *failingResultStore) Put(context.Context, string, []byte, string) (bool, string, error) {
	return false, "", s.err
}

func (s *failingResultStore) Get(context.Context, string) ([]byte, error) {
	return nil, s.err
}

func bytesContains(body, value []byte) bool {
	return strings.Contains(string(body), string(value))
}

func (b *Broker) mustCallTool(t *testing.T, tool string) ToolCallResult {
	t.Helper()
	result, err := b.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: tool, Arguments: []byte(`{"issue":"427"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
