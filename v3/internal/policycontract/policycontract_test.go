package policycontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	testBaseSHA    = "0123456789012345678901234567890123456789"
	testSpecDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestCompileIsDeterministicAcrossPermutations(t *testing.T) {
	alphaScript := []byte("#!/bin/sh\nprintf '%s\\n' alpha\n")
	betaScript := []byte("#!/bin/sh\nprintf '%s\\n' beta\n")
	first := []resolved.PolicySnapshot{
		policySnapshot("z-quality", "z-uid", 2, v1alpha1.PolicyRule{
			ID: "beta", Severity: v1alpha1.PolicySeverityBlocking, Context: "beta context",
			Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/beta.sh", Expect: "exit0"},
		}),
		policySnapshot("a-quality", "a-uid", 1,
			v1alpha1.PolicyRule{ID: "advisory", Severity: v1alpha1.PolicySeverityAdvisory, Context: "advisory"},
			v1alpha1.PolicyRule{ID: "alpha", Severity: v1alpha1.PolicySeverityBlocking, Context: "alpha context",
				Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/alpha.sh", Expect: "exit0"}},
		),
	}
	second := []resolved.PolicySnapshot{
		policySnapshot("a-quality", "a-uid", 1,
			v1alpha1.PolicyRule{ID: "alpha", Severity: v1alpha1.PolicySeverityBlocking, Context: "alpha context",
				Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/alpha.sh", Expect: "exit0"}},
			v1alpha1.PolicyRule{ID: "advisory", Severity: v1alpha1.PolicySeverityAdvisory, Context: "advisory"},
		),
		policySnapshot("z-quality", "z-uid", 2, v1alpha1.PolicyRule{
			ID: "beta", Severity: v1alpha1.PolicySeverityBlocking, Context: "beta context",
			Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/beta.sh", Expect: "exit0"},
		}),
	}
	loader := mapLoader(testBaseSHA, map[string][]byte{
		"policies/alpha.sh": alphaScript,
		"policies/beta.sh":  betaScript,
	})
	left, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: first}, loader)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: second}, loader)
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest() != right.Digest() || !bytes.Equal(left.ManifestBytes(), right.ManifestBytes()) {
		t.Fatalf("permuted policy input changed manifest: %s != %s", left.Digest(), right.Digest())
	}
	if got, want := ruleIDs(left.Rules()), []string{"advisory", "alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rule order = %v, want %v", got, want)
	}
}

func TestCompileRejectsCrossPolicyDuplicateRuleID(t *testing.T) {
	input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{
		policySnapshot("first", "uid-first", 1, advisoryRule("same")),
		policySnapshot("second", "uid-second", 1, advisoryRule("same")),
	}}
	_, err := Compile(input, nil)
	if !errors.Is(err, ErrDuplicateRule) {
		t.Fatalf("error = %v, want ErrDuplicateRule", err)
	}
}

func TestCompileRejectsDuplicateScriptedRulesBeforeLoading(t *testing.T) {
	input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{
		policySnapshot("first", "uid-first", 1, blockingRule("same", "policies/first.sh")),
		policySnapshot("second", "uid-second", 1, blockingRule("same", "policies/second.sh")),
	}}
	loads := 0
	loader := ScriptLoaderFunc(func(baseSHA, sourcePath string) (LoadedScript, error) {
		loads++
		body := []byte("exit 0\n")
		return LoadedScript{BaseSHA: baseSHA, Path: sourcePath, Kind: FileKindRegular, Bytes: body, Digest: scriptDigest(body)}, nil
	})
	_, err := Compile(input, loader)
	if !errors.Is(err, ErrDuplicateRule) {
		t.Fatalf("error = %v, want ErrDuplicateRule", err)
	}
	if loads != 0 {
		t.Fatalf("ScriptLoader called %d times before duplicate validation; want 0", loads)
	}
}

func TestCompileRequiresValidResolvedSpecDigest(t *testing.T) {
	policy := policySnapshot("quality", "uid-quality", 1, advisoryRule("rule"))
	for _, digest := range []string{"", "sha256:short", "sha256:" + strings.Repeat("A", 64)} {
		t.Run(digest, func(t *testing.T) {
			_, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: digest, Policies: []resolved.PolicySnapshot{policy}}, nil)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestCompileRejectsUnsafeScriptPaths(t *testing.T) {
	paths := []string{"../check.sh", "/check.sh", "policies/../check.sh", "policies//check.sh", `policies\\check.sh`, "https://example/check.sh", "policies/.check.sh"}
	for _, scriptPath := range paths {
		t.Run(scriptPath, func(t *testing.T) {
			input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{
				policySnapshot("quality", "uid-quality", 1, blockingRule("unsafe", scriptPath)),
			}}
			_, err := Compile(input, mapLoader(testBaseSHA, nil))
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestCompileRejectsMalformedSourceProvenance(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*resolved.PolicySnapshot)
	}{
		{"missing name", func(p *resolved.PolicySnapshot) { p.Reference.Name = "" }},
		{"invalid name", func(p *resolved.PolicySnapshot) { p.Reference.Name = "not a dns name" }},
		{"missing uid", func(p *resolved.PolicySnapshot) { p.Reference.UID = "" }},
		{"missing resource version", func(p *resolved.PolicySnapshot) { p.Reference.ResourceVersion = "" }},
		{"missing generation", func(p *resolved.PolicySnapshot) { p.Reference.Generation = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := policySnapshot("quality", "uid-quality", 1, advisoryRule("rule"))
			tc.mutate(&policy)
			_, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policy}}, nil)
			if !errors.Is(err, ErrMalformedProvenance) {
				t.Fatalf("error = %v, want ErrMalformedProvenance", err)
			}
		})
	}
}

func TestCompileRejectsMissingAndTamperedScripts(t *testing.T) {
	rule := blockingRule("check", "policies/check.sh")
	input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid-quality", 1, rule)}}

	if _, err := Compile(input, nil); !errors.Is(err, ErrScriptMissing) {
		t.Fatalf("nil loader error = %v, want ErrScriptMissing", err)
	}
	if _, err := Compile(input, ScriptLoaderFunc(func(string, string) (LoadedScript, error) {
		return LoadedScript{}, nil
	})); !errors.Is(err, ErrScriptMissing) {
		t.Fatalf("empty loader result = %v, want ErrScriptMissing", err)
	}

	good := []byte("#!/bin/sh\nexit 0\n")
	badDigest := ScriptLoaderFunc(func(baseSHA, sourcePath string) (LoadedScript, error) {
		return LoadedScript{BaseSHA: baseSHA, Path: sourcePath, Kind: FileKindRegular, Bytes: []byte("tampered"), Digest: scriptDigest(good)}, nil
	})
	if _, err := Compile(input, badDigest); !errors.Is(err, ErrScriptMismatch) {
		t.Fatalf("tampered script error = %v, want ErrScriptMismatch", err)
	}
	wrongBase := mapLoader("ffffffffffffffffffffffffffffffffffffffff", map[string][]byte{"policies/check.sh": good})
	if _, err := Compile(input, wrongBase); !errors.Is(err, ErrScriptMismatch) {
		t.Fatalf("wrong base error = %v, want ErrScriptMismatch", err)
	}
	wrongPath := ScriptLoaderFunc(func(baseSHA, sourcePath string) (LoadedScript, error) {
		return LoadedScript{BaseSHA: baseSHA, Path: "policies/other.sh", Kind: FileKindRegular, Bytes: good, Digest: scriptDigest(good)}, nil
	})
	if _, err := Compile(input, wrongPath); !errors.Is(err, ErrScriptMismatch) {
		t.Fatalf("wrong path error = %v, want ErrScriptMismatch", err)
	}
}

func TestCompileRejectsSymlinkAndSpecialScripts(t *testing.T) {
	for _, kind := range []FileKind{FileKindSymlink, FileKindSpecial} {
		t.Run(string(kind), func(t *testing.T) {
			input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{
				policySnapshot("quality", "uid-quality", 1, blockingRule("check", "policies/check.sh")),
			}}
			loader := ScriptLoaderFunc(func(baseSHA, sourcePath string) (LoadedScript, error) {
				body := []byte("exit 0\n")
				return LoadedScript{BaseSHA: baseSHA, Path: sourcePath, Kind: kind, Bytes: body, Digest: scriptDigest(body)}, nil
			})
			_, err := Compile(input, loader)
			if !errors.Is(err, ErrUnsafeScript) {
				t.Fatalf("error = %v, want ErrUnsafeScript", err)
			}
		})
	}
}

func TestAdvisoryRulesAreDemoted(t *testing.T) {
	input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid-quality", 1,
		advisoryRule("without-script"),
		v1alpha1.PolicyRule{ID: "with-script", Severity: v1alpha1.PolicySeverityAdvisory, Context: "informational", Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/advisory.sh", Expect: "exit0"}},
	)}}
	body := []byte("exit 1\n")
	compiled, err := Compile(input, mapLoader(testBaseSHA, map[string][]byte{"policies/advisory.sh": body}))
	if err != nil {
		t.Fatal(err)
	}
	self, gate := compiled.SelfChecks(), compiled.GateChecks()
	if len(self) != 2 || len(gate) != 2 {
		t.Fatalf("descriptor counts = %d/%d, want 2/2", len(self), len(gate))
	}
	for index := range self {
		if self[index].Blocking || gate[index].RejectsOnFailure {
			t.Fatalf("advisory rule %q was blocking: self=%+v gate=%+v", self[index].RuleID, self[index], gate[index])
		}
		if self[index].FailureMode != FailureAdvisory || gate[index].FailureMode != FailureAdvisory {
			t.Fatalf("advisory rule %q failure modes = %q/%q", self[index].RuleID, self[index].FailureMode, gate[index].FailureMode)
		}
	}
	byID := make(map[string]SelfCheckDescriptor, len(self))
	gateByID := make(map[string]GateCheckDescriptor, len(gate))
	for index := range self {
		byID[self[index].RuleID] = self[index]
		gateByID[gate[index].RuleID] = gate[index]
	}
	if byID["without-script"].HasScript || len(byID["without-script"].Argv) != 0 {
		t.Fatalf("no-script advisory descriptor = %+v", byID["without-script"])
	}
	if !byID["with-script"].HasScript || gateByID["with-script"].RejectsOnFailure {
		t.Fatalf("script advisory descriptor = self:%+v gate:%+v", byID["with-script"], gateByID["with-script"])
	}
}

func TestContextPackOutputPathAndBytesHaveParity(t *testing.T) {
	body := []byte("#!/bin/sh\nexit 0\n")
	input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{
		policySnapshot("quality", "uid-quality", 1, blockingRule("rule", "policies/check.sh")),
	}}
	compiled, err := Compile(input, mapLoader(testBaseSHA, map[string][]byte{"policies/check.sh": body}))
	if err != nil {
		t.Fatal(err)
	}
	rule := compiled.Rules()[0]
	self := compiled.SelfChecks()[0]
	gate := compiled.GateChecks()[0]
	if self.ContextPackOutputPath != gate.ContextPackOutputPath || self.ContextPackOutputPath != rule.ContextPackPath {
		t.Fatalf("output paths differ: rule=%q self=%q gate=%q", rule.ContextPackPath, self.ContextPackOutputPath, gate.ContextPackOutputPath)
	}
	if self.ScriptDigest != gate.ScriptDigest || self.ScriptDigest != rule.ScriptDigest {
		t.Fatalf("script digests differ")
	}
	if !reflect.DeepEqual(self.Argv, []string{ScriptRunner, self.ContextPackOutputPath}) || !reflect.DeepEqual(gate.Argv, self.Argv) {
		t.Fatalf("argv mismatch: self=%v gate=%v", self.Argv, gate.Argv)
	}
	cpRules := compiled.ContextPackRules()
	if !bytes.Equal(cpRules[0].ScriptBytes, body) || cpRules[0].Context != rule.Context || cpRules[0].ScriptDigest != rule.ScriptDigest {
		t.Fatalf("ContextPack projection changed canonical rule")
	}
	pack, err := contextpack.Compile(contextpack.Input{
		BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Task: "test task", Policies: cpRules,
		Budgets: contextpack.Budgets{MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTokens: 1 << 20, MaxEntries: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pack.Manifest.Policies[0].OutputPath; got != self.ContextPackOutputPath {
		t.Fatalf("ContextPack output path = %q, compiler path = %q", got, self.ContextPackOutputPath)
	}
}

func TestManifestIsCanonicalAndCredentialFree(t *testing.T) {
	uniqueScript := []byte("#!/bin/sh\nprintf 'UNIQUE_POLICY_SCRIPT_BYTES\\n'\n")
	compiled, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{
		policySnapshot("quality", "uid-quality", 3, v1alpha1.PolicyRule{ID: "unique", Severity: v1alpha1.PolicySeverityBlocking, Context: "do not leak credentials", Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/unique.sh", Expect: "exit0"}}),
	}}, mapLoader(testBaseSHA, map[string][]byte{"policies/unique.sh": uniqueScript}))
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes := compiled.ManifestBytes()
	manifest := compiled.Manifest()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	canonicalEncoded, err := strictjson.Normalize(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonicalEncoded, manifestBytes) {
		t.Fatalf("manifest bytes are not canonical JSON")
	}
	if !json.Valid(manifestBytes) || bytes.Contains(manifestBytes, uniqueScript) {
		t.Fatalf("manifest is invalid or contains script bytes: %s", manifestBytes)
	}
	if got := compiled.Digest(); got != scriptDigest(manifestBytes) {
		t.Fatalf("manifest digest = %q, want %q", got, scriptDigest(manifestBytes))
	}
	if manifest.ResolvedSpecDigest != testSpecDigest || !bytes.Contains(manifestBytes, []byte(`"resolvedSpecDigest":"`+testSpecDigest+`"`)) {
		t.Fatalf("manifest is not bound to resolvedSpecDigest: %s", manifestBytes)
	}
	entry := manifest.Rules[0]
	if entry.Source.Name != "quality" || entry.Source.UID != "uid-quality" || entry.Source.ResourceVersion != "1" || entry.Source.Generation != 3 || entry.Check.ScriptDigest != scriptDigest(uniqueScript) {
		t.Fatalf("manifest lost provenance or script digest: %+v", entry)
	}
	if entry.Check.ScriptSourcePath != "policies/unique.sh" || entry.Check.ContextPackOutputPath != ".agents/policies/unique/unique.sh" || entry.Check.ExpectedExitCode != 0 {
		t.Fatalf("manifest check binding incomplete: %+v", entry.Check)
	}
}

func TestCompiledAndLoaderDataAreDefensivelyCopied(t *testing.T) {
	body := []byte("#!/bin/sh\nexit 0\n")
	loaded := LoadedScript{BaseSHA: testBaseSHA, Path: "policies/check.sh", Kind: FileKindRegular, Bytes: body, Digest: scriptDigest(body)}
	input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid-quality", 1, blockingRule("rule", "policies/check.sh"))}}
	compiled, err := Compile(input, ScriptLoaderFunc(func(string, string) (LoadedScript, error) { return loaded, nil }))
	if err != nil {
		t.Fatal(err)
	}
	body[0] = 'X'
	loaded.Bytes[1] = 'Y'
	if got := string(compiled.Rules()[0].ScriptBytes()); got != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("compiled script was aliased to loader bytes: %q", got)
	}
	rules := compiled.Rules()
	rules[0].Context = "mutated"
	rules[0].ScriptBytes()[0] = 'Z'
	manifestBytes := compiled.ManifestBytes()
	manifestBytes[0] = 'Z'
	cpRules := compiled.ContextPackRules()
	cpRules[0].ScriptBytes[0] = 'Z'
	self := compiled.SelfChecks()
	self[0].Argv[1] = "mutated"
	gate := compiled.GateChecks()
	gate[0].Argv[1] = "mutated"
	if compiled.Rules()[0].Context == "mutated" || compiled.ManifestBytes()[0] == 'Z' || compiled.ContextPackRules()[0].ScriptBytes[0] == 'Z' || compiled.SelfChecks()[0].Argv[1] == "mutated" || compiled.GateChecks()[0].Argv[1] == "mutated" {
		t.Fatal("compiled result exposed mutable internal state")
	}
	if !reflect.DeepEqual(input.Policies[0].Spec.Rules[0].Check, blockingRule("rule", "policies/check.sh").Check) {
		t.Fatal("input snapshot was mutated")
	}
}

func TestCompileEnforcesBoundsAndUnknownValues(t *testing.T) {
	t.Run("too many policies", func(t *testing.T) {
		policies := make([]resolved.PolicySnapshot, MaxPolicies+1)
		for index := range policies {
			policies[index] = policySnapshot("policy-"+string(rune('a'+index%26))+string(rune('a'+index/26)), "uid", 1, advisoryRule("rule"))
		}
		if _, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: policies}, nil); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("error = %v, want ErrTooLarge", err)
		}
	})
	t.Run("too many rules in policy", func(t *testing.T) {
		rules := make([]v1alpha1.PolicyRule, MaxRulesPerPolicy+1)
		for index := range rules {
			rules[index] = advisoryRule("rule-" + strings.Repeat("a", 1) + string(rune('a'+index%26)))
		}
		if _, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid", 1, rules...)}}, nil); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("error = %v, want ErrTooLarge", err)
		}
	})
	t.Run("oversized context", func(t *testing.T) {
		rule := advisoryRule("rule")
		rule.Context = strings.Repeat("x", MaxPolicyContextBytes+1)
		_, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid", 1, rule)}}, nil)
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("error = %v, want ErrInvalidInput", err)
		}
	})
	t.Run("oversized script", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), MaxScriptBytes+1)
		input := Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid", 1, blockingRule("rule", "policies/check.sh"))}}
		_, err := Compile(input, mapLoader(testBaseSHA, map[string][]byte{"policies/check.sh": body}))
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("error = %v, want ErrTooLarge", err)
		}
	})
	t.Run("unknown values", func(t *testing.T) {
		cases := []v1alpha1.PolicyRule{
			{ID: "bad", Severity: "unknown"},
			{ID: "bad", Severity: v1alpha1.PolicySeverityAdvisory, Check: &v1alpha1.PolicyCheck{Kind: "command", Script: "check.sh", Expect: "exit0"}},
			{ID: "bad", Severity: v1alpha1.PolicySeverityAdvisory, Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "check.sh", Expect: "exit1"}},
		}
		for _, rule := range cases {
			if _, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid", 1, rule)}}, nil); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("rule %+v error = %v, want ErrInvalidInput", rule, err)
			}
		}
	})
}

func TestCompileDoesNotRequireLoaderForAdvisoryWithoutScript(t *testing.T) {
	compiled, err := Compile(Input{BaseSHA: testBaseSHA, ResolvedSpecDigest: testSpecDigest, Policies: []resolved.PolicySnapshot{policySnapshot("quality", "uid-quality", 1, advisoryRule("advisory"))}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Rules()) != 1 || compiled.Rules()[0].HasScript() {
		t.Fatalf("unexpected no-script advisory result: %+v", compiled.Rules())
	}
}

func policySnapshot(name, uid string, generation int64, rules ...v1alpha1.PolicyRule) resolved.PolicySnapshot {
	return resolved.PolicySnapshot{
		Reference: resolved.ObjectVersion{Name: name, UID: uid, ResourceVersion: "1", Generation: generation},
		Spec:      v1alpha1.PolicySpec{Rules: rules},
	}
}

func advisoryRule(id string) v1alpha1.PolicyRule {
	return v1alpha1.PolicyRule{ID: id, Severity: v1alpha1.PolicySeverityAdvisory, Context: "advisory context"}
}

func blockingRule(id, scriptPath string) v1alpha1.PolicyRule {
	return v1alpha1.PolicyRule{ID: id, Severity: v1alpha1.PolicySeverityBlocking, Context: "blocking context", Check: &v1alpha1.PolicyCheck{Kind: "script", Script: scriptPath, Expect: "exit0"}}
}

func mapLoader(baseSHA string, scripts map[string][]byte) ScriptLoader {
	return ScriptLoaderFunc(func(gotBaseSHA, sourcePath string) (LoadedScript, error) {
		if gotBaseSHA != baseSHA {
			return LoadedScript{}, ErrScriptMismatch
		}
		body, ok := scripts[sourcePath]
		if !ok {
			return LoadedScript{}, ErrScriptMissing
		}
		return LoadedScript{BaseSHA: gotBaseSHA, Path: sourcePath, Kind: FileKindRegular, Bytes: body, Digest: scriptDigest(body)}, nil
	})
}

func scriptDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ruleIDs(rules []Rule) []string {
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		ids = append(ids, rule.ID)
	}
	return ids
}
