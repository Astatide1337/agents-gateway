package contextmaterializer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
)

func TestLocalSymbolAdapterProducesSortedBoundedSymbols(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"z.go", "a.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package p\n\nfunc main() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	provider := testLocalSymbolProvider(t, "TestLSPHelperProcessGood", 16)
	entries, err := provider.Symbols(context.Background(), SymbolRequest{
		Root: root,
		Files: []contextpack.LexicalEntry{
			{Path: "z.go", Language: "go"},
			{Path: "a.go", Language: "go"},
		},
		Languages:  []string{"go", "go"},
		MaxEntries: 16,
		MaxTokens:  4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []contextpack.SymbolEntry{
		{Path: "a.go", Name: "main", Kind: "function", Signature: "func main()", Line: 2, Column: 1},
		{Path: "z.go", Name: "main", Kind: "function", Signature: "func main()", Line: 2, Column: 1},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("symbols=%#v, want %#v", entries, want)
	}
}

func TestLocalSymbolAdapterRejectsUnknownSymbolFields(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}
	provider := testLocalSymbolProvider(t, "TestLSPHelperProcessUnknownField", 16)
	_, err := provider.Symbols(context.Background(), SymbolRequest{
		Root: root, Files: []contextpack.LexicalEntry{{Path: "main.go", Language: "go"}}, Languages: []string{"go"}, MaxEntries: 16, MaxTokens: 4096,
	})
	if err == nil || !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("Symbols() error=%v, want fail-closed protocol error", err)
	}
}

func TestLocalSymbolAdapterRejectsUnsafeLocalInputs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}
	provider := testLocalSymbolProvider(t, "TestLSPHelperProcessGood", 16)
	_, err := provider.Symbols(context.Background(), SymbolRequest{
		Root: root, Files: []contextpack.LexicalEntry{{Path: "../main.go", Language: "go"}}, Languages: []string{"go"}, MaxEntries: 16, MaxTokens: 4096,
	})
	if err == nil || !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("path traversal error=%v, want fail closed", err)
	}
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Symbols(context.Background(), SymbolRequest{
		Root: root, Files: []contextpack.LexicalEntry{{Path: "link.go", Language: "go"}}, Languages: []string{"go"}, MaxEntries: 16, MaxTokens: 4096,
	})
	if err == nil || !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("symlink error=%v, want fail closed", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.go"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Symbols(context.Background(), SymbolRequest{
		Root: root, Files: []contextpack.LexicalEntry{{Path: "pipe.go", Language: "go"}}, Languages: []string{"go"}, MaxEntries: 16, MaxTokens: 4096,
	})
	if err == nil || !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("special-file error=%v, want fail closed", err)
	}
}

func TestLocalSymbolAdapterBoundsItemsAndOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}
	provider := testLocalSymbolProvider(t, "TestLSPHelperProcessTwo", 1)
	_, err := provider.Symbols(context.Background(), SymbolRequest{
		Root: root, Files: []contextpack.LexicalEntry{{Path: "main.go", Language: "go"}}, Languages: []string{"go"}, MaxEntries: 16, MaxTokens: 4096,
	})
	if err == nil || !errors.Is(err, contextpack.ErrBudgetExceeded) {
		t.Fatalf("item limit error=%v, want budget exceeded", err)
	}

	config := testLocalSymbolConfig(t, "TestLSPHelperProcessGood")
	config.MaxOutputBytes = 1
	provider, err = newLocalSymbolProvider(config, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Symbols(context.Background(), SymbolRequest{
		Root: root, Files: []contextpack.LexicalEntry{{Path: "main.go", Language: "go"}}, Languages: []string{"go"}, MaxEntries: 16, MaxTokens: 4096,
	})
	if err == nil || !errors.Is(err, ErrSymbolAdapterOutput) {
		t.Fatalf("output limit error=%v, want output bound", err)
	}
}

func TestLocalSymbolAdapterConfigFromEnvIsStrictAndOptional(t *testing.T) {
	values := map[string]string{}
	config, configured, err := LocalSymbolAdapterConfigFromEnv(func(name string) string { return values[name] })
	if err != nil || configured || config.Command != "" {
		t.Fatalf("unset adapter config=(%#v,%t,%v), want disabled", config, configured, err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	values = map[string]string{
		"AGW_CONTEXT_SYMBOL_ADAPTER":                  executable,
		"AGW_CONTEXT_SYMBOL_ADAPTER_SHA256":           executableDigest(t, executable),
		"AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON":        `["-test.run=TestLSPHelperProcessGood"]`,
		"AGW_CONTEXT_SYMBOL_ADAPTER_TIMEOUT":          "2s",
		"AGW_CONTEXT_SYMBOL_ADAPTER_MAX_OUTPUT_BYTES": "65536",
		"AGW_CONTEXT_SYMBOL_ADAPTER_MAX_MESSAGES":     "32",
		"AGW_CONTEXT_SYMBOL_ADAPTER_MAX_ITEMS":        "16",
	}
	config, configured, err = LocalSymbolAdapterConfigFromEnv(func(name string) string { return values[name] })
	if err != nil || !configured || config.Command != executable || !reflect.DeepEqual(config.Args, []string{"-test.run=TestLSPHelperProcessGood"}) || config.Timeout != 2*time.Second || config.MaxOutputBytes != 65536 || config.MaxMessages != 32 || config.MaxItems != 16 {
		t.Fatalf("valid adapter config=(%#v,%t,%v)", config, configured, err)
	}
	values["AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON"] = `[ "-test.run=TestLSPHelperProcessGood" ]`
	if _, _, err := LocalSymbolAdapterConfigFromEnv(func(name string) string { return values[name] }); err == nil {
		t.Fatal("non-canonical adapter argv was accepted")
	}
	values["AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON"] = `null`
	if _, _, err := LocalSymbolAdapterConfigFromEnv(func(name string) string { return values[name] }); err == nil {
		t.Fatal("null adapter argv was accepted")
	}
	values["AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON"] = `["-test.run=TestLSPHelperProcessGood"]`
	delete(values, "AGW_CONTEXT_SYMBOL_ADAPTER_SHA256")
	if _, _, err := LocalSymbolAdapterConfigFromEnv(func(name string) string { return values[name] }); err == nil {
		t.Fatal("adapter without an executable digest was accepted")
	}
}

func TestLocalSymbolAdapterRejectsSymlinkExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "lsp")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalSymbolProvider(LocalSymbolAdapterConfig{Command: link}); err == nil || !errors.Is(err, ErrSymbolAdapterConfig) {
		t.Fatalf("symlink executable error=%v, want configuration error", err)
	}
}

func TestLocalSymbolAdapterRejectsExecutableDigestMismatch(t *testing.T) {
	config := testLocalSymbolConfig(t, "TestLSPHelperProcessGood")
	config.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := NewLocalSymbolProvider(config); err == nil || !errors.Is(err, ErrSymbolAdapterConfig) {
		t.Fatalf("digest mismatch error=%v, want configuration error", err)
	}
}

func TestNewLocalSymbolProviderRequiresNetworkIsolation(t *testing.T) {
	provider, err := NewLocalSymbolProvider(testLocalSymbolConfig(t, "TestLSPHelperProcessGood"))
	if err != nil {
		t.Fatal(err)
	}
	local, ok := provider.(*localSymbolProvider)
	if !ok || !local.isolateNetwork {
		t.Fatalf("production provider isolation=%v, want enabled", local.isolateNetwork)
	}
}

func TestLocalSymbolAdapterAcceptsFlatLSPSymbolsButRejectsRemoteURI(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}
	known := map[string]struct{}{"main.go": {}}
	body, err := json.Marshal([]map[string]any{{
		"name": "main",
		"kind": 12,
		"location": map[string]any{
			"uri":   (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(root, "main.go"))}).String(),
			"range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 4}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := decodeDocumentSymbols(body, "main.go", root, known, 4)
	if err != nil || len(entries) != 1 || entries[0].Kind != "function" || entries[0].Line != 1 {
		t.Fatalf("flat symbols=(%#v,%v)", entries, err)
	}
	remote, _ := json.Marshal([]map[string]any{{
		"name": "main", "kind": 12,
		"location": map[string]any{"uri": "file:///etc/passwd", "range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}}},
	}})
	if _, err := decodeDocumentSymbols(remote, "main.go", root, known, 4); err == nil {
		t.Fatal("remote/out-of-repository LSP URI was accepted")
	}
}

func TestReadLSPFrameRejectsUnknownHeaders(t *testing.T) {
	frame := bufio.NewReader(strings.NewReader("X-Unknown: value\r\nContent-Length: 2\r\n\r\n{}"))
	if _, err := readLSPFrame(frame, 1024); err == nil {
		t.Fatal("unknown LSP header was accepted")
	}
}

func testLocalSymbolProvider(t *testing.T, helper string, maxItems int) SymbolProvider {
	t.Helper()
	provider, err := newLocalSymbolProvider(testLocalSymbolConfig(t, helper, maxItems), false)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func testLocalSymbolConfig(t *testing.T, helper string, maxItems ...int) LocalSymbolAdapterConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	items := 16
	if len(maxItems) > 0 {
		items = maxItems[0]
	}
	return LocalSymbolAdapterConfig{Command: executable, Digest: executableDigest(t, executable), Args: []string{"-test.run=" + helper}, Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20, MaxMessages: 32, MaxItems: items}
}

func executableDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestLSPHelperProcessGood(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, "\x00"), "-test.run=TestLSPHelperProcessGood") {
		return
	}
	runFakeLSP(t, false)
	os.Exit(0)
}

func TestLSPHelperProcessUnknownField(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, "\x00"), "-test.run=TestLSPHelperProcessUnknownField") {
		return
	}
	runFakeLSP(t, true)
	os.Exit(0)
}

func TestLSPHelperProcessTwo(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, "\x00"), "-test.run=TestLSPHelperProcessTwo") {
		return
	}
	runFakeLSP(t, false)
	os.Exit(0)
}

func runFakeLSP(t *testing.T, unknownField bool) {
	t.Helper()
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readTestLSPFrame(reader)
		if err != nil {
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &message); err != nil {
			return
		}
		switch message.Method {
		case "initialize":
			writeTestLSPFrame(t, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "result": map[string]any{"capabilities": map[string]any{"documentSymbolProvider": true}}})
		case "textDocument/documentSymbol":
			item := map[string]any{"name": "main", "detail": "func main()", "kind": 12, "range": map[string]any{"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 2, "character": 1}}, "selectionRange": map[string]any{"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 1, "character": 4}}}
			if unknownField {
				item["unknown"] = true
			}
			items := []any{item}
			if strings.Contains(strings.Join(os.Args, "\x00"), "-test.run=TestLSPHelperProcessTwo") {
				second := map[string]any{"name": "other", "detail": "func other()", "kind": 12, "range": map[string]any{"start": map[string]any{"line": 3, "character": 0}, "end": map[string]any{"line": 4, "character": 1}}, "selectionRange": map[string]any{"start": map[string]any{"line": 3, "character": 0}, "end": map[string]any{"line": 3, "character": 5}}}
				items = append(items, second)
			}
			writeTestLSPFrame(t, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "result": items})
		case "shutdown":
			writeTestLSPFrame(t, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "result": nil})
		case "exit":
			return
		}
	}
}

func readTestLSPFrame(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "Content-Length:") {
		return nil, errors.New("bad test frame")
	}
	var length int
	if _, err := fmt.Sscanf(line, "Content-Length: %d", &length); err != nil || length < 1 || length > 1<<20 {
		return nil, errors.New("bad test length")
	}
	if _, err := reader.ReadString('\n'); err != nil {
		return nil, err
	}
	body := make([]byte, length)
	_, err = io.ReadFull(reader, body)
	return body, err
}

func writeTestLSPFrame(t *testing.T, message any) {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		return
	}
	_, _ = os.Stdout.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
	_, _ = os.Stdout.Write(body)
}
