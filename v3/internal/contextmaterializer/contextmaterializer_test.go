package contextmaterializer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

func TestRunMaterializesAndVerifiesSealedPack(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	skills := filepath.Join(root, "skills")
	output := filepath.Join(root, "output")
	for _, directory := range []string{base, filepath.Join(skills, "codebase-design"), output} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "AGENTS.md"), []byte("Repository instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "policies"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "policies", "no-raw-sql.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "codebase-design", "SKILL.md"), []byte("# Codebase Design\n\nUse small changes.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	contract := testContract()
	input, err := Encode(contract)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{
		InputJSON:          string(input),
		ExpectedRunUID:     contract.RunUID,
		ExpectedSpecDigest: contract.ResolvedSpecDigest,
		ExpectedBaseSHA:    contract.BaseSHA,
		BaseDir:            base,
		SkillsDir:          skills,
		OutputDir:          output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Digest, "sha256:") || result.Ref.Digest != result.Digest {
		t.Fatalf("result=%#v", result)
	}
	if result.Ref.ManifestPath != ".agw/context/manifest.json" {
		t.Fatalf("manifest path=%q", result.Ref.ManifestPath)
	}
	if _, err := VerifyForRun(output, contract.RunUID, result.Digest, contract.ResolvedSpecDigest, contract.BaseSHA); err != nil {
		t.Fatalf("post-materialization verification: %v", err)
	}

	for _, name := range []string{
		"AGENTS.md",
		".agents/policies/no-raw-sql/no-raw-sql.sh",
		".agents/skills/codebase-design/SKILL.md",
		".agw/context/manifest.json",
		RefFileName,
	} {
		info, err := os.Lstat(filepath.Join(output, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("output %q: %v", name, err)
		}
		if info.Mode()&0222 != 0 {
			t.Fatalf("output %q is writable: %o", name, info.Mode().Perm())
		}
	}
	manifestBody, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(".agw/context/manifest.json")))
	if err != nil {
		t.Fatal(err)
	}
	var manifest contextpack.Manifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range manifest.Files {
		if descriptor.Path == RefFileName {
			t.Fatal("reference marker must not be part of the self-hashed manifest")
		}
	}
}

func TestVerifyRejectsTamperingAndUnexpectedFiles(t *testing.T) {
	output, result, contract := materializedFixture(t)

	manifestPath := filepath.Join(output, filepath.FromSlash(".agw/context/manifest.json"))
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifestPath, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(manifest, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyForRun(output, contract.RunUID, result.Digest, contract.ResolvedSpecDigest, contract.BaseSHA); !errors.Is(err, ErrTampered) {
		t.Fatalf("tampered manifest verification error=%v, want ErrTampered", err)
	}

	output, result, contract = materializedFixture(t)
	if err := os.Chmod(output, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "unexpected.txt"), []byte("unexpected"), 0444); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyForRun(output, contract.RunUID, result.Digest, contract.ResolvedSpecDigest, contract.BaseSHA); !errors.Is(err, ErrTampered) {
		t.Fatalf("unexpected-file verification error=%v, want ErrTampered", err)
	}

	output, result, contract = materializedFixture(t)
	if err := os.Mkdir(filepath.Join(output, "unexpected"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyForRun(output, contract.RunUID, result.Digest, contract.ResolvedSpecDigest, contract.BaseSHA); !errors.Is(err, ErrTampered) {
		t.Fatalf("unexpected-directory verification error=%v, want ErrTampered", err)
	}
}

func TestRunFailsClosedOnMissingPolicyAndNonEmptyOutput(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	skills := filepath.Join(root, "skills")
	output := filepath.Join(root, "output")
	for _, directory := range []string{base, skills, output} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	input, err := Encode(testContract())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "existing"), []byte("do not overwrite"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Config{InputJSON: string(input), ExpectedRunUID: testContract().RunUID, ExpectedSpecDigest: testContract().ResolvedSpecDigest, ExpectedBaseSHA: testContract().BaseSHA, BaseDir: base, SkillsDir: skills, OutputDir: output}); !errors.Is(err, ErrOutput) {
		t.Fatalf("non-empty output error=%v, want ErrOutput", err)
	}

	if err := os.Remove(filepath.Join(output, "existing")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Config{InputJSON: string(input), ExpectedRunUID: testContract().RunUID, ExpectedSpecDigest: testContract().ResolvedSpecDigest, ExpectedBaseSHA: testContract().BaseSHA, BaseDir: base, SkillsDir: skills, OutputDir: output}); err == nil {
		t.Fatal("missing policy script was accepted")
	}
}

func TestRunConcurrentOutputsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	skills := filepath.Join(root, "skills")
	if err := os.MkdirAll(filepath.Join(base, "policies"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(skills, "codebase-design"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "policies", "no-raw-sql.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "codebase-design", "SKILL.md"), []byte("# Skill\n"), 0644); err != nil {
		t.Fatal(err)
	}
	contract := testContract()
	body, err := Encode(contract)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 4
	digests := make([]string, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			output := filepath.Join(root, "output-"+string(rune('a'+index)))
			if err := os.Mkdir(output, 0755); err != nil {
				errs[index] = err
				return
			}
			result, err := Run(context.Background(), Config{InputJSON: string(body), ExpectedRunUID: contract.RunUID, ExpectedSpecDigest: contract.ResolvedSpecDigest, ExpectedBaseSHA: contract.BaseSHA, BaseDir: base, SkillsDir: skills, OutputDir: output})
			errs[index], digests[index] = err, result.Digest
		}()
	}
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
		if digests[index] != digests[0] {
			t.Fatalf("worker %d digest=%q, want %q", index, digests[index], digests[0])
		}
	}
}

func TestDecodeRejectsNonCanonicalAndUnknownFields(t *testing.T) {
	body, err := Encode(testContract())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(append([]byte(" "), body...)); err == nil {
		t.Fatal("non-canonical contract accepted")
	}
	unknown := append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"secret":"must-not-be-representable"}`)...)
	if _, err := Decode(unknown); err == nil {
		t.Fatal("unknown contract field accepted")
	}
}

func materializedFixture(t *testing.T) (string, Result, Contract) {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "base")
	skills := filepath.Join(root, "skills", "codebase-design")
	output := filepath.Join(root, "output")
	for _, directory := range []string{base, skills, output} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "policies"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "policies", "no-raw-sql.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("# Skill\n"), 0644); err != nil {
		t.Fatal(err)
	}
	contract := testContract()
	body, err := Encode(contract)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{InputJSON: string(body), ExpectedRunUID: contract.RunUID, ExpectedSpecDigest: contract.ResolvedSpecDigest, ExpectedBaseSHA: contract.BaseSHA, BaseDir: base, SkillsDir: filepath.Dir(skills), OutputDir: output})
	if err != nil {
		t.Fatal(err)
	}
	return output, result, contract
}

func testContract() Contract {
	baseSHA := strings.Repeat("e", 40)
	specDigest := "sha256:" + strings.Repeat("b", 64)
	return Contract{
		SchemaVersion:      SchemaVersion,
		RunUID:             "run-uid",
		BaseSHA:            baseSHA,
		ResolvedSpecDigest: specDigest,
		Task:               "Fix the bounded example.",
		Instructions:       "Keep the change focused.",
		Boundaries:         contextpack.Boundaries{AllowedPaths: []string{"src/**"}, ForbiddenPaths: []string{".github/**"}},
		Policies: []resolved.PolicySnapshot{{
			Reference: resolved.ObjectVersion{Name: "default-quality", UID: "policy-uid", ResourceVersion: "17", Generation: 1},
			Spec: v1alpha1.PolicySpec{Rules: []v1alpha1.PolicyRule{{
				ID: "no-raw-sql", Severity: v1alpha1.PolicySeverityBlocking, Context: "Use the repository abstraction.",
				Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/no-raw-sql.sh", Expect: "exit0"},
			}}},
		}},
		Skills: []SkillInput{{Name: "codebase-design", Digest: "sha256:" + strings.Repeat("c", 64)}},
		// Producer tiers are exercised by the dedicated producer tests below;
		// this fixture remains the minimal policy/skill materialization case.
		Strategies: contextpack.ContextStrategies{},
		Budgets: contextpack.Budgets{
			MaxInputBytes:  8 << 20,
			MaxOutputBytes: 8 << 20,
			MaxFileBytes:   1 << 20,
			MaxFiles:       1024,
			MaxTokens:      40000,
			MaxEntries:     4096,
		},
	}
}
