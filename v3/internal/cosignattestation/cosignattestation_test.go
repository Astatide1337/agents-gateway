package cosignattestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

type fakeRunner struct {
	calls       int
	result      CommandResult
	writeBundle bool
	bundleBytes []byte
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string, _, _ int64) CommandResult {
	f.calls++
	if f.writeBundle {
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "--bundle" {
				bundle := f.bundleBytes
				if len(bundle) == 0 {
					bundle = validBundleBytes()
				}
				_ = os.WriteFile(args[index+1], bundle, 0600)
			}
		}
	}
	return f.result
}

func TestPlansRequireExplicitKeyAndRedactIt(t *testing.T) {
	runner := &fakeRunner{}
	adapter, err := New(testConfig(runner))
	if err != nil {
		t.Fatal(err)
	}
	attest, err := adapter.PlanAttest("statement.json", "bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	assertArgPair(t, attest.Args, "--key", "awskms://arn:example/key")
	assertArgPair(t, attest.Args, "--type", evidenceattestation.PredicateType)
	assertArgPair(t, attest.Args, "--use-signing-config=false", "")
	assertArgPair(t, attest.Args, "--tlog-upload=false", "")
	if strings.Contains(strings.Join(attest.RedactedArgv(), " "), "awskms://arn:example/key") {
		t.Fatal("redacted plan exposed the key reference")
	}
	verify, err := adapter.PlanVerify("bundle.json", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	assertArgPair(t, verify.Args, "--digest", strings.Repeat("a", 64))
	assertArgPair(t, verify.Args, "--digestAlg", "sha256")
	assertArgPair(t, verify.Args, "--check-claims=true", "")
	assertArgPair(t, verify.Args, "--insecure-ignore-tlog", "")
}

func TestConfigurationRejectsKeylessAndWrongBindings(t *testing.T) {
	base := testConfig(nil)
	for name, mutate := range map[string]func(*Config){
		"missing key": func(config *Config) { config.KeyRef = "" },
		"file signing key without separate verification key": func(config *Config) {
			config.KeyRef = "/keys/cosign.key"
			config.VerifyKeyRef = ""
		},
		"same absolute signing and verification key": func(config *Config) {
			config.KeyRef = "/keys/cosign.key"
			config.VerifyKeyRef = "/keys/cosign.key"
		},
		"environment key":   func(config *Config) { config.KeyRef = "env://COSIGN_KEY" },
		"relative file key": func(config *Config) { config.KeyRef = "keys/cosign.key" },
		"wrong predicate":   func(config *Config) { config.PredicateType = "https://example.invalid/wrong" },
		"wrong media":       func(config *Config) { config.MediaType = "application/json" },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error=%v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestAttestAuthenticatesGateBeforeRunnerAndDryRunIsSideEffectFree(t *testing.T) {
	signed, publicKey := testSignedReport(t)
	runner := &fakeRunner{}
	config := testConfig(runner)
	config.DryRun = true
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	result, err := adapter.Attest(context.Background(), Request{
		SignedReport:         signed,
		TrustedGatePublicKey: publicKey,
		BundlePath:           filepath.Join(temporary, "bundle.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || runner.calls != 0 || len(result.Statement) == 0 {
		t.Fatalf("dry-run result=%#v calls=%d", result, runner.calls)
	}
	if _, err := os.Stat(filepath.Join(temporary, "bundle.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created bundle, stat error=%v", err)
	}
	tampered := append([]byte(nil), signed...)
	tampered[len(tampered)-2] = 'x'
	if _, err := adapter.Attest(context.Background(), Request{
		SignedReport:         tampered,
		TrustedGatePublicKey: publicKey,
		BundlePath:           filepath.Join(temporary, "tampered.json"),
	}); !errors.Is(err, evidenceattestation.ErrUntrustedReport) {
		t.Fatalf("tampered report error=%v, want ErrUntrustedReport", err)
	}
	if runner.calls != 0 {
		t.Fatal("runner was called for an unauthenticated report")
	}
}

func TestAttestUsesAtomicBoundedBundleAndStatementOutputs(t *testing.T) {
	signed, publicKey := testSignedReport(t)
	runner := &fakeRunner{writeBundle: true}
	adapter, err := New(testConfig(runner))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	bundlePath := filepath.Join(directory, "attestation.json")
	statementPath := filepath.Join(directory, "statement.json")
	result, err := adapter.Attest(context.Background(), Request{
		SignedReport:         signed,
		TrustedGatePublicKey: publicKey,
		BundlePath:           bundlePath,
		StatementOutputPath:  statementPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || result.DryRun || result.SubjectDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("result=%#v calls=%d", result, runner.calls)
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatal(err)
	}
	statement, err := os.ReadFile(statementPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidenceattestation.VerifyArtifact(evidenceattestation.MediaType, statement, mustVerified(signed, publicKey), evidenceattestation.Options{}); err != nil {
		t.Fatalf("statement output failed independent verification: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".agw-cosign-") || strings.Contains(entry.Name(), ".agw-statement-") {
			t.Fatalf("temporary output was not cleaned: %s", entry.Name())
		}
	}
}

func TestBundleValidationRequiresSigstoreDSSEShape(t *testing.T) {
	signed, publicKey := testSignedReport(t)
	runner := &fakeRunner{writeBundle: true, bundleBytes: []byte(`{"bundle":"test"}`)}
	adapter, err := New(testConfig(runner))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Attest(context.Background(), Request{
		SignedReport:         signed,
		TrustedGatePublicKey: publicKey,
		BundlePath:           filepath.Join(t.TempDir(), "bundle.json"),
	})
	if !errors.Is(err, ErrBundleInvalid) {
		t.Fatalf("malformed bundle error=%v, want ErrBundleInvalid", err)
	}
}

func TestCommandFailuresAndOversizedOutputFailClosed(t *testing.T) {
	signed, publicKey := testSignedReport(t)
	for name, result := range map[string]CommandResult{
		"nonzero":      {ExitCode: 2, Err: errors.New("exit")},
		"unknown exit": {ExitCode: -1},
		"oversized":    {ExitCode: 0, Stdout: []byte("x"), StdoutTruncated: true},
		"ambiguous":    {ExitCode: 0, Stdout: []byte("signature material")},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{result: result}
			adapter, err := New(testConfig(runner))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Attest(context.Background(), Request{
				SignedReport:         signed,
				TrustedGatePublicKey: publicKey,
				BundlePath:           filepath.Join(t.TempDir(), "bundle.json"),
			})
			if err == nil {
				t.Fatal("ambiguous or failed command was accepted")
			}
		})
	}
}

func TestVerifyBindsDigestAndPredicateBeforeCosign(t *testing.T) {
	signed, publicKey := testSignedReport(t)
	runner := &fakeRunner{result: CommandResult{ExitCode: 0}}
	adapter, err := New(testConfig(runner))
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(bundlePath, validBundleBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Verify(context.Background(), Request{
		SignedReport:         signed,
		TrustedGatePublicKey: publicKey,
		BundlePath:           bundlePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || result.SubjectDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("result=%#v calls=%d", result, runner.calls)
	}
	assertArgPair(t, result.Plan.Args, "--type", evidenceattestation.PredicateType)
	assertArgPair(t, result.Plan.Args, "--digest", strings.Repeat("d", 64))
}

func TestCommandDeadlineIsRetryableButExitFailureIsNot(t *testing.T) {
	for name, result := range map[string]CommandResult{
		"deadline": {Err: context.DeadlineExceeded},
		"exit":     {ExitCode: 2},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{result: result}
			adapter, err := New(testConfig(runner))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := adapter.PlanAttest(filepath.Join(t.TempDir(), "statement.json"), filepath.Join(t.TempDir(), "bundle.json"))
			if err != nil {
				t.Fatal(err)
			}
			err = adapter.run(context.Background(), plan)
			if err == nil {
				t.Fatal("failed command was accepted")
			}
			if name == "deadline" && !IsRetryable(err) {
				t.Fatalf("deadline error=%v, want retryable", err)
			}
			if name == "exit" && IsRetryable(err) {
				t.Fatalf("exit error=%v, did not expect retryable", err)
			}
		})
	}
}

func TestSeparateVerificationKeyNeverEntersAttestPlan(t *testing.T) {
	config := testConfig(&fakeRunner{})
	config.KeyRef = "/keys/cosign.key"
	config.VerifyKeyRef = "/keys/cosign.pub"
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	attest, err := adapter.PlanAttest("/tmp/statement.json", "/tmp/bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	verify, err := adapter.PlanVerify("/tmp/bundle.json", "sha256:"+strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	assertArgPair(t, attest.Args, "--key", "/keys/cosign.key")
	assertArgPair(t, verify.Args, "--key", "/keys/cosign.pub")
}

func testConfig(runner Runner) Config {
	return Config{
		CosignPath:    "/usr/local/bin/cosign-v3",
		KeyRef:        "awskms://arn:example/key",
		PredicateType: evidenceattestation.PredicateType,
		MediaType:     evidenceattestation.MediaType,
		Runner:        runner,
	}
}

func assertArgPair(t *testing.T, args []string, flagName, expected string) {
	t.Helper()
	for index, value := range args {
		if value == flagName {
			if expected == "" {
				return
			}
			if index+1 < len(args) && args[index+1] == expected {
				return
			}
		}
	}
	t.Fatalf("argv does not contain %s %q: %#v", flagName, expected, args)
}

func testSignedReport(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	files := int64(1)
	hasBinary := false
	duration := int64(10)
	decision := gate.Evaluate(gate.Input{
		Scope:        v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
		Requirements: v1alpha1.GateRequirements{ScopeRespected: true, NoBinaryFiles: true, TestStrength: v1alpha1.TestStrengthNone},
		Commands:     []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}},
		Observations: gate.Observations{
			ChangedPaths: []string{"src/main.go"}, FilesChanged: &files, HasBinaryFiles: &hasBinary,
			Commands: []gate.CommandObservation{{Index: 0, ExitCode: int32Ptr(0), EvidenceDigest: "sha256:" + strings.Repeat("f", 64), DurationMillis: &duration}},
		},
	})
	if !decision.Accepted() {
		t.Fatalf("fixture decision rejected: %#v", decision.Checks)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	report, err := gate.BuildReport(gate.ReportContext{
		RunUID:              "run-uid-1",
		SpecDigest:          "sha256:" + strings.Repeat("b", 64),
		BaseSHA:             strings.Repeat("c", 40),
		PatchDigest:         "sha256:" + strings.Repeat("d", 64),
		GateUID:             "gate-uid-1",
		GateGeneration:      1,
		RuntimeImageDigest:  "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64),
		VerifierImageDigest: "ghcr.io/example/verifier@sha256:" + strings.Repeat("e", 64),
		HelperImageDigests: []gate.ImageEvidence{
			{Name: "clone", Digest: "ghcr.io/example/clone@sha256:" + strings.Repeat("1", 64)},
		},
		SkillDigests: []string{"sha256:" + strings.Repeat("2", 64)},
	}, decision)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := gate.SignReport(report, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := gate.SignedReportBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, publicKey
}

func mustVerified(signed []byte, publicKey ed25519.PublicKey) evidenceattestation.VerifiedReport {
	verified, err := evidenceattestation.VerifySignedReportBytes(signed, publicKey)
	if err != nil {
		panic(err)
	}
	return verified
}

func int32Ptr(value int32) *int32 { return &value }

func validBundleBytes() []byte {
	return []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{},"dsseEnvelope":{"payloadType":"application/vnd.in-toto+json","payload":"eA==","signatures":[{}]}}`)
}
