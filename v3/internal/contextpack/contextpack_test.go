package contextpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func testInput() Input {
	script := []byte("#!/bin/sh\nset -eu\nexit 0\n")
	return Input{
		BaseSHA:            strings.Repeat("a", 40),
		ResolvedSpecDigest: "sha256:" + strings.Repeat("b", 64),
		Task:               "Fix the bounded example task.",
		Instructions:       "Use the existing conventions and run the deterministic checks.",
		ExistingAgentsMD:   "Repository instructions.\n",
		Boundaries: Boundaries{
			AllowedPaths:   []string{"src/**", "tests/**"},
			ForbiddenPaths: []string{".github/**", "**/*.yml"},
		},
		Policies: []PolicyRule{
			{ID: "No Raw SQL", Severity: SeverityBlocking, Context: "Database access uses the repository interface.", ScriptPath: "policies/no-raw-sql.sh", ScriptBytes: script, ScriptDigest: digest(script)},
			{ID: "Exported Docs", Severity: SeverityAdvisory, Context: "Document exported functions."},
		},
		Skills: []Skill{
			{Name: "Codebase Design", Digest: "sha256:" + strings.Repeat("c", 64), Body: "# Codebase Design\n\nUse small cohesive changes.\n"},
		},
		RepoMap: []RepoMapEntry{
			{Path: "tests/example_test.go", Kind: "file", Summary: "Example tests."},
			{Path: "src/main.go", Kind: "file", Signature: "package main", Summary: "Main entry point."},
		},
		Symbols: []SymbolEntry{
			{Path: "src/main.go", Name: "main", Kind: "function", Line: 10, Signature: "func main()"},
			{Path: "src/store.go", Name: "Store", Kind: "type", Line: 3, Signature: "type Store struct{}"},
		},
		History: []HistoryEntry{
			{CommitSHA: strings.Repeat("d", 40), Subject: "add example task", Paths: []string{"src/main.go"}},
		},
		Strategies: ContextStrategies{RepoMap: "tree-sitter", Symbols: "lsp-serena", History: "git-blame-touched"},
		Budgets: Budgets{
			MaxInputBytes:  1 << 20,
			MaxOutputBytes: 1 << 20,
			MaxFileBytes:   1 << 16,
			MaxFiles:       32,
			MaxTokens:      1 << 18,
			MaxEntries:     128,
		},
	}
}

func TestCompileIsDeterministicAcrossSemanticInputPermutation(t *testing.T) {
	first, err := Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	permuted := testInput()
	reversePolicies(permuted.Policies)
	reverseSkills(permuted.Skills)
	reverseRepoMap(permuted.RepoMap)
	reverseSymbols(permuted.Symbols)
	reverseHistory(permuted.History)
	permuted.Boundaries.AllowedPaths = []string{"tests/**", "src/**"}
	permuted.Boundaries.ForbiddenPaths = []string{"**/*.yml", ".github/**"}
	second, err := Compile(permuted)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("pack digest changed after permutation: %s != %s", first.Digest, second.Digest)
	}
	if !reflect.DeepEqual(first.Manifest, second.Manifest) {
		t.Fatalf("manifest changed after permutation:\n%#v\n%#v", first.Manifest, second.Manifest)
	}
	if len(first.Files) != len(second.Files) {
		t.Fatalf("file count changed: %d != %d", len(first.Files), len(second.Files))
	}
	for index := range first.Files {
		if first.Files[index].Path != second.Files[index].Path || !bytes.Equal(first.Files[index].Content, second.Files[index].Content) || first.Files[index].Digest != second.Files[index].Digest {
			t.Fatalf("file %d changed after permutation: %#v vs %#v", index, first.Files[index], second.Files[index])
		}
	}
}

func TestCompileRejectsUnsafePathsAndDuplicateNormalizedNames(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"policy traversal", func(input *Input) { input.Policies[0].ScriptPath = "../check.sh" }},
		{"skill traversal", func(input *Input) { input.Skills[0].ArtifactPath = "../skill"; input.Skills[0].Body = "" }},
		{"repo traversal", func(input *Input) { input.RepoMap[0].Path = "../main.go" }},
		{"symbol absolute", func(input *Input) { input.Symbols[0].Path = "/etc/passwd" }},
		{"history traversal", func(input *Input) { input.History[0].Paths = []string{"src/../secret"} }},
		{"boundary traversal", func(input *Input) { input.Boundaries.AllowedPaths = []string{"../secret/**"} }},
		{"duplicate policy", func(input *Input) {
			input.Policies = append(input.Policies, PolicyRule{ID: "no-raw-sql", Severity: SeverityAdvisory})
		}},
		{"duplicate skill", func(input *Input) {
			input.Skills = append(input.Skills, Skill{Name: "codebase-design", Digest: "sha256:" + strings.Repeat("e", 64), Body: "# duplicate\n"})
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.edit(&input)
			pack, err := Compile(input)
			if err == nil || !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Compile() pack=%#v err=%v, want invalid input", pack, err)
			}
			if pack.Files != nil || pack.Digest != "" {
				t.Fatalf("invalid input returned partial pack: %#v", pack)
			}
		})
	}
}

func TestCompileRejectsBlockingPolicyWithoutScript(t *testing.T) {
	input := testInput()
	input.Policies = []PolicyRule{{ID: "must-check", Severity: SeverityBlocking, Context: "This must be deterministic."}}
	pack, err := Compile(input)
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pack=%#v err=%v, want blocking-script validation error", pack, err)
	}
}

func TestCompilePreservesScriptBytesAndChangesDigestForContentChanges(t *testing.T) {
	input := testInput()
	first, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	scriptFile, ok := first.File(".agents/policies/no-raw-sql/no-raw-sql.sh")
	if !ok {
		t.Fatal("compiled policy script was not emitted")
	}
	wantScript := input.Policies[0].ScriptBytes
	if !bytes.Equal(scriptFile.Content, wantScript) {
		t.Fatalf("script bytes changed: got %q want %q", scriptFile.Content, wantScript)
	}
	if scriptFile.Digest != digest(wantScript) {
		t.Fatalf("script digest=%q want=%q", scriptFile.Digest, digest(wantScript))
	}

	input.Policies[0].ScriptBytes = []byte("#!/bin/sh\nset -eu\nexit 1\n")
	input.Policies[0].ScriptDigest = digest(input.Policies[0].ScriptBytes)
	second, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("changed script content retained pack digest")
	}

	input = testInput()
	first, err = Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Skills[0].Body = "# Codebase Design\n\nA changed rule.\n"
	second, err = Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("changed skill content retained pack digest")
	}
}

func TestCompileEmitsDeterministicArtifactReference(t *testing.T) {
	input := testInput()
	input.Skills[0] = Skill{Name: "Reference Skill", Digest: "sha256:" + strings.Repeat("f", 64), ArtifactPath: ".agw/artifacts/skills/reference.tar.gz"}
	pack, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := pack.File(".agents/skills/reference-skill/SKILL.md")
	if !ok {
		t.Fatal("artifact reference SKILL.md was not emitted")
	}
	if got, want := string(file.Content), "# Verified skill reference\n\nThis skill is supplied by a verified artifact.\n\n- digest: sha256:"+strings.Repeat("f", 64)+"\n- artifact: .agw/artifacts/skills/reference.tar.gz\n"; got != want {
		t.Fatalf("reference body=%q want=%q", got, want)
	}
	if pack.Manifest.Skills[0].Source != "artifact-reference" || pack.Manifest.Skills[0].ArtifactPath == "" {
		t.Fatalf("artifact provenance missing: %#v", pack.Manifest.Skills[0])
	}
}

func TestCompileEmitsCanonicalLexicalIndex(t *testing.T) {
	input := testInput()
	input.RepoMap = nil
	input.Symbols = nil
	input.History = nil
	input.Strategies = ContextStrategies{Lexical: "local-ripgrep"}
	input.Lexical = []LexicalEntry{
		{Path: "z.txt", Extension: "txt", Bytes: 4, Lines: 1, Digest: digest([]byte("z\n"))},
		{Path: "a.go", Extension: "go", Language: "go", Bytes: 12, Lines: 2, Digest: digest([]byte("package p\n"))},
	}
	first, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	permuted := input
	permuted.Lexical = []LexicalEntry{input.Lexical[1], input.Lexical[0]}
	second, err := Compile(permuted)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("lexical pack digest changed after permutation: %s != %s", first.Digest, second.Digest)
	}
	file, ok := first.File(".agw/context/lexical-index.json")
	if !ok || !bytes.Contains(file.Content, []byte(`"exact":true`)) || !bytes.Contains(file.Content, []byte(`"enumeration":"local-ripgrep-compatible"`)) {
		t.Fatalf("lexical index is missing exact-search metadata: %q", file.Content)
	}
}

func TestCompileOmitsWholeContextEntriesAndAccountsForThem(t *testing.T) {
	input := testInput()
	input.RepoMap = []RepoMapEntry{
		{Path: "a.go", Kind: "file", Summary: strings.Repeat("a", 3000)},
		{Path: "b.go", Kind: "file", Summary: strings.Repeat("b", 3000)},
		{Path: "c.go", Kind: "file", Summary: strings.Repeat("c", 3000)},
	}
	input.Symbols = nil
	input.History = nil
	input.Strategies = ContextStrategies{RepoMap: "tree-sitter"}
	input.Budgets.MaxFileBytes = 1 << 16
	input.Budgets.MaxOutputBytes = 1 << 20
	full, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Budgets.MaxOutputBytes = full.Manifest.Usage.OutputBytes - 4500
	input.Budgets.MaxTokens = full.Manifest.Usage.TokenEstimate - 1
	limited, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Manifest.Omissions) == 0 {
		t.Fatalf("expected omissions, manifest=%#v", limited.Manifest)
	}
	if len(limited.Manifest.Omissions) >= len(input.RepoMap) {
		t.Fatalf("all context entries were omitted unexpectedly: %#v", limited.Manifest.Omissions)
	}
	if limited.Manifest.Strategies[0].OmittedEntries != len(limited.Manifest.Omissions) {
		t.Fatalf("omission accounting mismatch: strategy=%#v omissions=%#v", limited.Manifest.Strategies[0], limited.Manifest.Omissions)
	}
	if limited.Manifest.Usage.OutputBytes > input.Budgets.MaxOutputBytes || limited.Manifest.Usage.TokenEstimate > input.Budgets.MaxTokens {
		t.Fatalf("returned pack exceeds budget: usage=%#v budgets=%#v", limited.Manifest.Usage, input.Budgets)
	}
	for _, omission := range limited.Manifest.Omissions {
		if omission.Kind != "repo-map" || omission.Reason == "" {
			t.Fatalf("invalid omission record: %#v", omission)
		}
	}
}

func TestCompileFailsClosedForMandatoryBudgetAndDoesNotReturnPartialOutput(t *testing.T) {
	input := testInput()
	input.Budgets.MaxFileBytes = 32
	pack, err := Compile(input)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("pack=%#v err=%v, want budget error", pack, err)
	}
	if pack.Files != nil || pack.Manifest.Files != nil || pack.Digest != "" {
		t.Fatalf("budget failure returned partial output: %#v", pack)
	}
}

func TestCompileRejectsMalformedDigestsAndInvalidUTF8(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"base sha", func(input *Input) { input.BaseSHA = "ABC" }},
		{"spec digest", func(input *Input) { input.ResolvedSpecDigest = "sha256:" + strings.Repeat("A", 64) }},
		{"skill digest", func(input *Input) { input.Skills[0].Digest = "not-a-digest" }},
		{"script digest", func(input *Input) { input.Policies[0].ScriptDigest = "sha256:" + strings.Repeat("0", 64) }},
		{"invalid task utf8", func(input *Input) { input.Task = string([]byte{0xff}) }},
		{"invalid script utf8", func(input *Input) { input.Policies[0].ScriptBytes = []byte{0xff} }},
		{"invalid skill utf8", func(input *Input) { input.Skills[0].Body = string([]byte{0xff}) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.edit(&input)
			pack, err := Compile(input)
			if err == nil || !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("pack=%#v err=%v, want invalid input", pack, err)
			}
		})
	}
}

func TestCompileAcceptsGitSHA256BaseSHA(t *testing.T) {
	input := testInput()
	input.BaseSHA = strings.Repeat("e", 64)
	if pack, err := Compile(input); err != nil || pack.Digest == "" {
		t.Fatalf("Compile() pack=%#v err=%v, want SHA-256 base accepted", pack, err)
	}
}

func TestManifestIsSelfConsistentAndEveryOutputHasDigest(t *testing.T) {
	pack, err := Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := pack.ManifestBytes()
	if err != nil {
		t.Fatal(err)
	}
	if pack.Digest != digest(manifestBytes) {
		t.Fatalf("pack digest=%q want manifest digest=%q", pack.Digest, digest(manifestBytes))
	}
	var decoded Manifest
	if err := json.Unmarshal(manifestBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, pack.Manifest) {
		t.Fatalf("decoded manifest differs from returned manifest:\n%#v\n%#v", decoded, pack.Manifest)
	}
	if len(pack.Files) != len(decoded.Files)+1 {
		t.Fatalf("pack has %d files but manifest has %d non-manifest descriptors", len(pack.Files), len(decoded.Files))
	}
	for _, file := range pack.Files {
		if file.Path == "" || !validDigest(file.Digest) || file.Digest != digest(file.Content) || file.SizeBytes != int64(len(file.Content)) || file.TokenEstimate != estimateTokens(file.Content) {
			t.Fatalf("invalid output descriptor: %#v", file)
		}
	}
	for _, descriptor := range decoded.Files {
		file, ok := pack.File(descriptor.Path)
		if !ok {
			t.Fatalf("manifest references missing file %q", descriptor.Path)
		}
		if descriptor.Digest != file.Digest || descriptor.SizeBytes != file.SizeBytes || descriptor.TokenEstimate != file.TokenEstimate || descriptor.Kind != file.Kind {
			t.Fatalf("manifest descriptor mismatch: %#v vs %#v", descriptor, file)
		}
	}
	for _, file := range pack.Files {
		if file.Path == manifestPath {
			continue
		}
		if _, ok := findDescriptor(decoded.Files, file.Path); !ok {
			t.Fatalf("non-manifest output %q is not covered by manifest", file.Path)
		}
	}
	if _, ok := findDescriptor(decoded.Files, manifestPath); ok {
		t.Fatal("manifest must not self-reference its own digest")
	}
	if strings.Contains(string(manifestBytes), "authorization") || strings.Contains(string(manifestBytes), "secret-value") {
		t.Fatalf("manifest contains credential-like material: %s", manifestBytes)
	}
}

func TestCompileRequiresExplicitPositiveBudgets(t *testing.T) {
	input := testInput()
	input.Budgets.MaxTokens = 0
	pack, err := Compile(input)
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pack=%#v err=%v, want invalid budget", pack, err)
	}
}

func findDescriptor(descriptors []FileDescriptor, name string) (FileDescriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Path == name {
			return descriptor, true
		}
	}
	return FileDescriptor{}, false
}

func reversePolicies(values []PolicyRule) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseSkills(values []Skill) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseRepoMap(values []RepoMapEntry) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseSymbols(values []SymbolEntry) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseHistory(values []HistoryEntry) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func ExampleCompile() {
	input := Input{
		BaseSHA:            strings.Repeat("a", 40),
		ResolvedSpecDigest: "sha256:" + strings.Repeat("b", 64),
		Task:               "Run a bounded task.",
		Budgets: Budgets{
			MaxInputBytes:  64 << 10,
			MaxOutputBytes: 64 << 10,
			MaxFileBytes:   16 << 10,
			MaxFiles:       8,
			MaxTokens:      16 << 10,
			MaxEntries:     16,
		},
	}
	pack, err := Compile(input)
	if err != nil {
		panic(err)
	}
	f, _ := pack.File(agentsPath)
	fmt.Println(f.Path, pack.Digest != "")
	// Output: AGENTS.md true
}
