package contextmaterializer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
)

func TestProduceLocalTiersAreDeterministicAndBounded(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	files := map[string]string{
		"zeta.txt":       "last\n",
		"src/main.go":    "package main\n\nfunc main() {}\n",
		"src/store.go":   "package main\n\ntype Store struct{}\n",
		"build/output":   "must not enter the index\n",
		"node_modules/x": "must not enter the index\n",
	}
	writeRepositoryFiles(t, firstRoot, files, []string{"zeta.txt", "src/store.go", "build/output", "src/main.go", "node_modules/x"})
	writeRepositoryFiles(t, secondRoot, files, []string{"node_modules/x", "src/main.go", "build/output", "zeta.txt", "src/store.go"})

	config := func(root string) ProducerConfig {
		return ProducerConfig{
			Root: root,
			Settings: ProducerSettings{
				Lexical:       true,
				RepoMapBudget: 1 << 16,
			},
			Strategies: contextpack.ContextStrategies{Lexical: "local-ripgrep", RepoMap: "tree-sitter"},
			Budgets:    producerTestBudgets(),
		}
	}
	first, err := Produce(context.Background(), config(firstRoot))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Produce(context.Background(), config(secondRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("producer result changed with traversal order:\nfirst=%#v\nsecond=%#v", first, second)
	}
	for _, entry := range first.Lexical {
		if strings.HasPrefix(entry.Path, "build/") || strings.HasPrefix(entry.Path, "node_modules/") {
			t.Fatalf("generated/dependency output entered lexical index: %#v", entry)
		}
	}
	if len(first.Lexical) != 3 || len(first.RepoMap) != 3 {
		t.Fatalf("unexpected local tier counts: lexical=%d repoMap=%d", len(first.Lexical), len(first.RepoMap))
	}

	packInput := contextpack.Input{
		BaseSHA:            strings.Repeat("a", 40),
		ResolvedSpecDigest: "sha256:" + strings.Repeat("b", 64),
		Task:               "inspect",
		Instructions:       "keep it bounded",
		Lexical:            first.Lexical,
		RepoMap:            first.RepoMap,
		Strategies:         first.Strategies,
		Budgets:            producerTestBudgets(),
	}
	firstPack, err := contextpack.Compile(packInput)
	if err != nil {
		t.Fatal(err)
	}
	packInput.Lexical = second.Lexical
	packInput.RepoMap = second.RepoMap
	secondPack, err := contextpack.Compile(packInput)
	if err != nil {
		t.Fatal(err)
	}
	if firstPack.Digest != secondPack.Digest {
		t.Fatalf("compiled digest changed with traversal order: %s != %s", firstPack.Digest, secondPack.Digest)
	}
}

func TestProduceRejectsUnsafeRepositoryInputs(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("safe\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed utf8",
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "bad.txt"), []byte{0xff, 0xfe}, 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "credential path",
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=hidden\n"), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "credential content",
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "special file",
			setup: func(t *testing.T, root string) {
				if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			_, err := Produce(context.Background(), ProducerConfig{
				Root: root, Settings: ProducerSettings{Lexical: true},
				Strategies: contextpack.ContextStrategies{Lexical: "local-ripgrep"}, Budgets: producerTestBudgets(),
			})
			if err == nil || !errors.Is(err, ErrUnsafeRepository) {
				t.Fatalf("Produce() error=%v, want unsafe repository", err)
			}
		})
	}
}

func TestProduceSymbolsFailClosedWithoutLocalAdapter(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Produce(context.Background(), ProducerConfig{
		Root:       root,
		Settings:   ProducerSettings{Lexical: true, SymbolLanguages: []string{"go"}},
		Strategies: contextpack.ContextStrategies{Lexical: "local-ripgrep", Symbols: "lsp-serena"},
		Budgets:    producerTestBudgets(),
	})
	if err == nil || !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("Produce() error=%v, want unavailable symbol adapter", err)
	}
}

func TestProduceSymbolsUsesNarrowLocalAdapter(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Produce(context.Background(), ProducerConfig{
		Root:       root,
		Settings:   ProducerSettings{Lexical: true, SymbolLanguages: []string{"go", "go"}},
		Strategies: contextpack.ContextStrategies{Lexical: "local-ripgrep", Symbols: "lsp-serena"},
		Budgets:    producerTestBudgets(),
		SymbolProvider: SymbolProviderFunc(func(_ context.Context, request SymbolRequest) ([]contextpack.SymbolEntry, error) {
			if request.Root != root || !reflect.DeepEqual(request.Languages, []string{"go"}) || len(request.Files) != 1 {
				t.Fatalf("unexpected symbol request: %#v", request)
			}
			return []contextpack.SymbolEntry{{Path: "main.go", Name: "main", Kind: "function", Line: 3, Signature: "func main()"}}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Symbols) != 1 || result.Symbols[0].Name != "main" {
		t.Fatalf("unexpected symbols: %#v", result.Symbols)
	}
}

func TestProduceHistoryRequiresCleanPinnedGitCheckout(t *testing.T) {
	root := t.TempDir()
	gitTestCommand(t, root, "init", "-q")
	gitTestCommand(t, root, "config", "user.email", "agw-test@example.invalid")
	gitTestCommand(t, root, "config", "user.name", "agw-test")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, root, "add", "main.go")
	gitTestCommand(t, root, "commit", "-qm", "initial")
	head := strings.TrimSpace(string(gitTestCommand(t, root, "rev-parse", "HEAD")))

	config := ProducerConfig{
		Root: root, BaseSHA: head,
		Settings:   ProducerSettings{Lexical: true, HistoryDepth: 5},
		Strategies: contextpack.ContextStrategies{Lexical: "local-ripgrep", History: "git-blame-touched"},
		Budgets:    producerTestBudgets(),
	}
	result, err := Produce(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.History) != 1 || result.History[0].CommitSHA != head || !reflect.DeepEqual(result.History[0].Paths, []string{"main.go"}) {
		t.Fatalf("unexpected history: %#v", result.History)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\n// dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Produce(context.Background(), config); !errors.Is(err, ErrDirtyRepository) {
		t.Fatalf("dirty checkout error=%v, want ErrDirtyRepository", err)
	}
}

func TestParseGitHistoryRejectsUnsafeMetadata(t *testing.T) {
	bad := []byte{0x1e}
	bad = append(bad, []byte(strings.Repeat("a", 40)+"\x00subject\x00../escape\x00")...)
	if _, err := parseGitHistory(bad); err == nil || !errors.Is(err, ErrUnsafeRepository) {
		t.Fatalf("parseGitHistory() error=%v, want unsafe history", err)
	}
}

func producerTestBudgets() contextpack.Budgets {
	return contextpack.Budgets{MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxFileBytes: 1 << 16, MaxFiles: 64, MaxTokens: 1 << 16, MaxEntries: 64}
}

func writeRepositoryFiles(t *testing.T, root string, files map[string]string, order []string) {
	t.Helper()
	for _, name := range order {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(files[name]), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitTestCommand(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v (%s)", args, err, bytes.TrimSpace(body))
	}
	return body
}
