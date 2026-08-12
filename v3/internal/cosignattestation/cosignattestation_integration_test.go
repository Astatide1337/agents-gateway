package cosignattestation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

const pinnedCosignVersion = "v3.1.3"

var pinnedCosignVersionPattern = regexp.MustCompile(`(?:^|[^0-9A-Za-z])v3\.1\.3(?:$|[^0-9A-Za-z])`)

// TestRealCosignLocalKeyDSSEAttestationRoundTrip is deliberately kept out of
// the normal fake-runner tests. It runs only when the exact cosign version
// pinned by the v3 image contract is already installed on the machine. The
// test never downloads a binary, contacts a registry/Rekor/Fulcio/TUF
// service, or changes files outside t.TempDir.
func TestRealCosignLocalKeyDSSEAttestationRoundTrip(t *testing.T) {
	cosignPath, ok := findPinnedCosign()
	if !ok {
		t.Skipf("cosign %s is not installed; set AGW_COSIGN_PATH to an installed compatible binary to run the offline integration test", pinnedCosignVersion)
	}

	// ExecRunner inherits the test process environment. These values make the
	// adapter's real cosign children noninteractive and keep all transparency
	// and remote identity lookups disabled for this test.
	t.Setenv("COSIGN_PASSWORD", "")
	t.Setenv("COSIGN_TLOG_UPLOAD", "false")
	t.Setenv("COSIGN_YES", "true")
	t.Setenv("SIGSTORE_NO_PUBLIC_KEY_DOWNLOAD", "true")

	root := t.TempDir()
	privateKeyPath := filepath.Join(root, "cosign.key")
	publicKeyPath := filepath.Join(root, "cosign.pub")
	generateLocalKeyPair(t, cosignPath, privateKeyPath, publicKeyPath)
	assertRegularFileMode(t, privateKeyPath, 0600)
	assertRegularFileMode(t, publicKeyPath, 0644)

	signedReport, gatePublicKey := testSignedReport(t)
	bundlePath := filepath.Join(root, "verification.bundle.json")
	statementPath := filepath.Join(root, "verification.statement.json")
	adapter, err := New(Config{
		CosignPath:     cosignPath,
		KeyRef:         privateKeyPath,
		VerifyKeyRef:   publicKeyPath,
		PredicateType:  evidenceattestation.PredicateType,
		MediaType:      evidenceattestation.MediaType,
		TempDir:        root,
		Timeout:        30 * time.Second,
		MaxBundleBytes: DefaultMaxBundleBytes,
	})
	if err != nil {
		t.Fatal(err)
	}

	attested, err := adapter.Attest(context.Background(), Request{
		SignedReport:         signedReport,
		TrustedGatePublicKey: gatePublicKey,
		BundlePath:           bundlePath,
		StatementOutputPath:  statementPath,
	})
	if err != nil {
		t.Fatalf("real cosign attest-blob failed: %v", err)
	}
	if attested.DryRun || attested.SubjectDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("unexpected attestation result: %#v", attested)
	}

	verified, err := adapter.Verify(context.Background(), Request{
		SignedReport:         signedReport,
		TrustedGatePublicKey: gatePublicKey,
		BundlePath:           bundlePath,
	})
	if err != nil {
		t.Fatalf("real cosign verify-blob-attestation failed: %v", err)
	}
	if verified.DryRun || verified.SubjectDigest != attested.SubjectDigest || verified.PredicateType != evidenceattestation.PredicateType {
		t.Fatalf("unexpected verification result: %#v", verified)
	}
	if !hasArg(adapterPlanArgs(t, attested.Plan), "--tlog-upload=false") {
		t.Fatal("attestation plan did not disable transparency-log upload")
	}
	if !hasArg(adapterPlanArgs(t, verified.Plan), "--insecure-ignore-tlog") {
		t.Fatal("verification plan did not disable transparency-log lookup")
	}

	statementBytes, err := os.ReadFile(statementPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(statementBytes, attested.Statement) {
		t.Fatal("statement output differs from the adapter's canonical statement")
	}
	verifiedReport := mustVerified(signedReport, gatePublicKey)
	if err := evidenceattestation.VerifyArtifact(evidenceattestation.MediaType, statementBytes, verifiedReport, evidenceattestation.Options{}); err != nil {
		t.Fatalf("canonical statement failed independent Gate binding verification: %v", err)
	}

	payload := readRealDSSEPayload(t, bundlePath)
	statement, err := evidenceattestation.ParseStatement(payload)
	if err != nil {
		t.Fatalf("cosign bundle payload is not a canonical AGW statement: %v", err)
	}
	if err := evidenceattestation.VerifyArtifact(evidenceattestation.MediaType, payload, verifiedReport, evidenceattestation.Options{}); err != nil {
		t.Fatalf("cosign bundle payload failed independent Gate binding verification: %v", err)
	}
	if got := statement.Predicate.PatchDigest; got != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("bundle predicate patch digest=%q", got)
	}

	tests := []struct {
		name   string
		mutate func(*evidenceattestation.Statement)
	}{
		{
			name: "subject",
			mutate: func(statement *evidenceattestation.Statement) {
				statement.Subject[0].Digest["sha256"] = strings.Repeat("0", 64)
			},
		},
		{
			name: "predicate-type",
			mutate: func(statement *evidenceattestation.Statement) {
				statement.PredicateType = "https://agents.astatide.com/verification/tampered"
			},
		},
		{
			name: "verdict",
			mutate: func(statement *evidenceattestation.Statement) {
				statement.Predicate.Verdict = gate.Rejected
			},
		},
	}
	for _, test := range tests {
		t.Run("tampered-"+test.name, func(t *testing.T) {
			tamperedBundle := filepath.Join(root, "tampered-"+test.name+".bundle.json")
			rewriteBundlePayload(t, bundlePath, tamperedBundle, test.mutate)
			_, err := adapter.Verify(context.Background(), Request{
				SignedReport:         signedReport,
				TrustedGatePublicKey: gatePublicKey,
				BundlePath:           tamperedBundle,
			})
			if err == nil {
				t.Fatalf("tampered %s payload was accepted", test.name)
			}
			if !errors.Is(err, ErrCommandFailed) && !errors.Is(err, ErrBundleInvalid) {
				t.Fatalf("tampered %s payload returned unexpected error: %v", test.name, err)
			}
		})
	}
}

func findPinnedCosign() (string, bool) {
	candidates := make([]string, 0, 3)
	if configured := strings.TrimSpace(os.Getenv("AGW_COSIGN_PATH")); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates, "cosign-v3", "cosign")
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, path, "version")
		command.Env = offlineCosignEnvironment()
		command.Stdin = nil
		output, runErr := command.CombinedOutput()
		cancel()
		if runErr == nil && pinnedCosignVersionPattern.Match(output) {
			return path, true
		}
	}
	return "", false
}

func generateLocalKeyPair(t *testing.T, cosignPath, privateKeyPath, publicKeyPath string) {
	t.Helper()
	prefix := strings.TrimSuffix(privateKeyPath, ".key")
	if prefix == privateKeyPath || publicKeyPath != prefix+".pub" {
		t.Fatalf("cosign test key paths do not share a .key/.pub prefix")
	}
	output := runCosign(t, cosignPath, []string{
		"generate-key-pair",
		"--output-key-prefix", prefix,
	})
	if _, err := os.Stat(privateKeyPath); err != nil {
		t.Fatalf("cosign did not create private test key: %v\n%s", err, output)
	}
	if _, err := os.Stat(publicKeyPath); err != nil {
		t.Fatalf("cosign did not create public test key: %v\n%s", err, output)
	}
}

func runCosign(t *testing.T, cosignPath string, args []string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, cosignPath, args...)
	command.Env = offlineCosignEnvironment()
	command.Stdin = nil
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	output := append(stdout.Bytes(), stderr.Bytes()...)
	if err != nil {
		t.Fatalf("cosign %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func offlineCosignEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		key := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			key = entry[:separator]
		}
		switch key {
		case "COSIGN_PASSWORD", "COSIGN_TLOG_UPLOAD", "COSIGN_YES", "SIGSTORE_NO_PUBLIC_KEY_DOWNLOAD":
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"COSIGN_PASSWORD=",
		"COSIGN_TLOG_UPLOAD=false",
		"COSIGN_YES=true",
		"SIGSTORE_NO_PUBLIC_KEY_DOWNLOAD=true",
	)
}

type realBundle struct {
	MediaType            string          `json:"mediaType"`
	VerificationMaterial json.RawMessage `json:"verificationMaterial"`
	DSSEEnvelope         struct {
		PayloadType string            `json:"payloadType"`
		Payload     string            `json:"payload"`
		Signatures  []json.RawMessage `json:"signatures"`
	} `json:"dsseEnvelope"`
}

func readRealDSSEPayload(t *testing.T, bundlePath string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	var bundle realBundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatalf("bundle is not JSON: %v", err)
	}
	if !strings.Contains(bundle.MediaType, "sigstore.bundle") {
		t.Fatalf("bundle media type=%q, want a Sigstore bundle", bundle.MediaType)
	}
	if len(bundle.VerificationMaterial) == 0 || len(bundle.DSSEEnvelope.Signatures) == 0 {
		t.Fatalf("bundle is missing verification material or DSSE signatures: %#v", bundle)
	}
	if bundle.DSSEEnvelope.PayloadType != evidenceattestation.DSSEPayloadType {
		t.Fatalf("DSSE payload type=%q, want %q", bundle.DSSEEnvelope.PayloadType, evidenceattestation.DSSEPayloadType)
	}
	payload, err := decodeBundlePayload(bundle.DSSEEnvelope.Payload)
	if err != nil {
		t.Fatalf("decode DSSE payload: %v", err)
	}
	return payload
}

func decodeBundlePayload(encoded string) ([]byte, error) {
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err == nil {
		return payload, nil
	}
	return base64.RawStdEncoding.DecodeString(encoded)
}

func rewriteBundlePayload(t *testing.T, sourcePath, destinationPath string, mutate func(*evidenceattestation.Statement)) {
	t.Helper()
	encoded, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(bundle["dsseEnvelope"], &envelope); err != nil {
		t.Fatalf("decode DSSE envelope: %v", err)
	}
	var encodedPayload string
	if err := json.Unmarshal(envelope["payload"], &encodedPayload); err != nil {
		t.Fatalf("decode DSSE payload field: %v", err)
	}
	payload, err := decodeBundlePayload(encodedPayload)
	if err != nil {
		t.Fatalf("decode DSSE payload: %v", err)
	}
	var statement evidenceattestation.Statement
	if err := json.Unmarshal(payload, &statement); err != nil {
		t.Fatalf("decode attestation statement: %v", err)
	}
	mutate(&statement)
	payload, err = json.Marshal(statement)
	if err != nil {
		t.Fatalf("marshal tampered statement: %v", err)
	}
	newPayload, err := json.Marshal(base64.StdEncoding.EncodeToString(payload))
	if err != nil {
		t.Fatal(err)
	}
	envelope["payload"] = newPayload
	newEnvelope, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	bundle["dsseEnvelope"] = newEnvelope
	newBundle, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destinationPath, newBundle, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertRegularFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file", path)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%#o, want %#o", path, got, want)
	}
}

func adapterPlanArgs(t *testing.T, plan Plan) []string {
	t.Helper()
	if plan.Executable == "" || len(plan.Args) == 0 {
		t.Fatalf("empty cosign plan: %#v", plan)
	}
	return plan.Args
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
