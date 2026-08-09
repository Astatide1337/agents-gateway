package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runnerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runtimeproto"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"github.com/Astatide1337/agents-gateway/v2/proto"
)

// TestRunnerMTLSDispatchE2E exercises the complete in-process runner boundary:
// ephemeral CA-issued mTLS, the real runnerdispatch client, the HTTP dispatch
// handler, the persistent task manager, the real Daemon, and a fake sandbox.
// It intentionally does not require Docker, containerd, Podman, or an external
// control plane.
func TestRunnerMTLSDispatchE2E(t *testing.T) {
	caPEM, caCert, caKey := e2eCertificate(t, nil, nil, true, "agw-e2e-ca")
	serverPEM, serverKeyPEM := e2eLeafCertificate(t, caCert, caKey, "runner.test", []string{"runner.test"}, []net.IP{net.IPv4(127, 0, 0, 1)}, x509.ExtKeyUsageServerAuth)
	clientPEM, clientKeyPEM := e2eLeafCertificate(t, caCert, caKey, "control-plane", nil, nil, x509.ExtKeyUsageClientAuth)

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to build server client-CA pool")
	}
	serverCert, err := tls.X509KeyPair(serverPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("load server certificate: %v", err)
	}

	artifact := agentworkflow.ArtifactRef{
		ID:        "artifact-e2e",
		URI:       "s3://agw-e2e/artifact-e2e.json",
		Digest:    "sha256:" + strings.Repeat("b", 64),
		SizeBytes: 2,
		MediaType: "application/json",
	}
	handle := &e2eSandboxHandle{stdout: e2eRuntimeOutput(artifact)}
	backend := &e2eSandboxBackend{handle: handle}
	dispatch, err := NewPersistentDispatch(t.TempDir(), backend)
	if err != nil {
		t.Fatalf("create persistent dispatch: %v", err)
	}
	handler := &HTTPServer{Service: dispatch, Report: runner.ReadinessReport{Ready: true}}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{serverCert},
	}
	server.StartTLS()
	defer server.Close()
	if server.TLS.MinVersion != tls.VersionTLS13 || server.TLS.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("runner TLS policy was not configured as required: %#v", server.TLS)
	}

	secretDir := t.TempDir()
	caFile := e2eWriteSecret(t, secretDir, "ca.pem", caPEM, 0600)
	clientCertFile := e2eWriteSecret(t, secretDir, "client.crt", clientPEM, 0600)
	clientKeyFile := e2eWriteSecret(t, secretDir, "client.key", clientKeyPEM, 0600)
	client, err := runnerdispatch.New(runnerdispatch.Config{
		Endpoint:       server.URL,
		ServerName:     "runner.test",
		CAFile:         caFile,
		ClientCertFile: clientCertFile,
		ClientKeyFile:  clientKeyFile,
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("create real runnerdispatch client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	input := agentworkflow.ScheduleRunnerTaskInput{
		OrganizationID: "org-e2e",
		ProjectID:      "project-e2e",
		RunID:          "run-e2e",
		WorkflowName:   "workflow-e2e",
		StepID:         "step-e2e",
		AgentRef:       "agent/e2e",
		InputRef:       "definition://sha256:" + strings.Repeat("c", 64) + "/input",
		IdempotencyKey: "run-e2e/workflow-e2e/step-e2e",
		Execution:      e2eRuntimeSpec(),
		Contract:       e2eExecutionContract(),
	}
	scheduled, err := client.ScheduleRunnerTask(ctx, input)
	if err != nil {
		t.Fatalf("schedule over mTLS: %v", err)
	}
	if scheduled.Status != "scheduled" || scheduled.TaskID == "" {
		t.Fatalf("unexpected schedule response: %#v", scheduled)
	}

	var status agentworkflow.StatusRunnerTaskResult
	for {
		status, err = client.StatusRunnerTask(ctx, agentworkflow.StatusRunnerTaskInput{
			OrganizationID: input.OrganizationID,
			ProjectID:      input.ProjectID,
			RunID:          input.RunID,
			TaskID:         scheduled.TaskID,
		})
		if err != nil {
			t.Fatalf("poll status over mTLS: %v", err)
		}
		if status.Status == "succeeded" {
			break
		}
		if status.Status == "failed" || status.Status == "lost" || status.Status == "cancelled" {
			t.Fatalf("task reached terminal failure: %#v", status)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for success; last status=%#v", status)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if status.Output != artifact {
		t.Fatalf("status returned non-immutable or unexpected artifact: got=%#v want=%#v", status.Output, artifact)
	}
	if err := status.Output.Validate(); err != nil {
		t.Fatalf("status artifact is invalid: %v", err)
	}

	commandBytes := handle.commandsSnapshot()
	command, err := runtimeproto.ParseLine(commandBytes)
	if err != nil {
		t.Fatalf("parse delivered run.start contract: %v", err)
	}
	if command.Kind != proto.KindRequest || command.Type != proto.RequestRunStart || command.RunID != input.RunID || command.Seq != 1 {
		t.Fatalf("unexpected initial runtime command: %#v", command)
	}
	var delivered RunContract
	if err := json.Unmarshal(command.Data, &delivered); err != nil {
		t.Fatalf("decode delivered run contract: %v", err)
	}
	if delivered.OrganizationID != input.OrganizationID || delivered.ProjectID != input.ProjectID || delivered.AgentRef != input.AgentRef || delivered.Execution.InstructionsRef != input.Contract.InstructionsRef {
		t.Fatalf("initial run.start contract was not delivered intact: %#v", delivered)
	}
	if !reflect.DeepEqual(backend.startedSpecSnapshot(), input.Execution) {
		t.Fatalf("sandbox received unexpected execution spec: got=%#v want=%#v", backend.startedSpec, input.Execution)
	}
}

func e2eCertificate(t *testing.T, parent *x509.Certificate, signer ed25519.PrivateKey, ca bool, commonName string) ([]byte, *x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatalf("generate certificate serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  ca,
	}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
		parent = template
		signer = privateKey
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, privateKey
}

func e2eLeafCertificate(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, commonName string, dnsNames []string, ips []net.IP, usage x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatalf("generate leaf serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	for _, ip := range ips {
		template.IPAddresses = append(template.IPAddresses, ip)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, publicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func e2eWriteSecret(t *testing.T, dir, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

type e2eSandboxBackend struct {
	mu          sync.Mutex
	handle      *e2eSandboxHandle
	startedSpec runner.SandboxSpec
}

func (b *e2eSandboxBackend) Name() runner.BackendKind { return runner.BackendPodman }

func (b *e2eSandboxBackend) Capabilities() runner.BackendCapabilities {
	return runner.RootlessPodmanCapabilities()
}

func (b *e2eSandboxBackend) Start(ctx context.Context, spec runner.SandboxSpec) (runner.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.startedSpec = spec
	b.mu.Unlock()
	return b.handle, nil
}

func (b *e2eSandboxBackend) startedSpecSnapshot() runner.SandboxSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startedSpec
}

type e2eSandboxHandle struct {
	stdout   []byte
	mu       sync.Mutex
	commands bytes.Buffer
	stopped  bool
}

func (h *e2eSandboxHandle) ID() string { return "sandbox-e2e" }

func (h *e2eSandboxHandle) Stdout() io.Reader { return bytes.NewReader(h.stdout) }

func (h *e2eSandboxHandle) Wait(ctx context.Context) (runner.ExitStatus, error) {
	if err := ctx.Err(); err != nil {
		return runner.ExitStatus{}, err
	}
	now := time.Now()
	return runner.ExitStatus{Code: 0, StartedAt: now, FinishedAt: now}, nil
}

func (h *e2eSandboxHandle) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	return nil
}

func (h *e2eSandboxHandle) Send(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.commands.Write(payload)
	return err
}

func (h *e2eSandboxHandle) commandsSnapshot() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.commands.Bytes()...)
}

func e2eRuntimeSpec() runner.SandboxSpec {
	return runner.SandboxSpec{
		Backend: runner.BackendPodman, IsolationGrade: runner.IsolationStandard,
		Image: "example/agent", ImageDigest: "sha256:" + strings.Repeat("a", 64), RunAsUser: "65532",
		ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, Network: runner.NetworkNone,
		Resources: runner.ResourceLimits{CPUs: 1, MemoryBytes: 256 << 20, DiskBytes: 1 << 30, PIDs: 128, Timeout: time.Minute},
		Mounts:    []runner.Mount{{Kind: "workspace", Destination: "/workspace"}},
	}
}

func e2eExecutionContract() agentworkflow.ExecutionContract {
	return agentworkflow.ExecutionContract{
		Agent:           agentworkflow.RevisionRef{Kind: "Agent", Name: "e2e", Digest: "sha256:" + strings.Repeat("c", 64)},
		SandboxProfile:  agentworkflow.RevisionRef{Kind: "SandboxProfile", Name: "e2e", Digest: "sha256:" + strings.Repeat("d", 64)},
		InstructionsRef: "definition://sha256:" + strings.Repeat("c", 64) + "/instructions",
		Instructions:    "Run the bounded in-process E2E task.",
	}
}

func e2eRuntimeOutput(artifact agentworkflow.ArtifactRef) []byte {
	started := proto.Envelope{
		Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventRunStarted,
		RunID: "run-e2e", Seq: 1, Data: json.RawMessage(`{"agent_id":"agent-e2e","sandbox_id":"sandbox-e2e"}`),
	}
	completedData, _ := json.Marshal(map[string]any{"result": "ok", "output": artifact})
	completed := proto.Envelope{
		Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventRunCompleted,
		RunID: "run-e2e", Seq: 2, Terminal: true, Data: completedData,
	}
	first, _ := runtimeproto.EncodeLine(started)
	second, _ := runtimeproto.EncodeLine(completed)
	return append(first, second...)
}
