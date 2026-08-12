package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
)

type recordedCall struct {
	argv  []string
	stdin []byte
}

type fakeResponse struct {
	stdout string
	stderr string
	err    error
}

type fakeRunner struct {
	responses []fakeResponse
	calls     []recordedCall
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin []byte, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, recordedCall{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	response := fakeResponse{}
	if len(f.responses) >= len(f.calls) {
		response = f.responses[len(f.calls)-1]
	}
	if response.stdout != "" {
		_, _ = io.WriteString(stdout, response.stdout)
	}
	if response.stderr != "" {
		_, _ = io.WriteString(stderr, response.stderr)
	}
	return response.err
}

func TestRunUsesCreateWithExactArgvAndPreservesConnectionFlags(t *testing.T) {
	manifest := validManifest("repair-427", "")
	path := writeManifest(t, manifest)
	runner := &fakeRunner{responses: []fakeResponse{{stdout: "agentrun.agents.astatide.com/repair-427\n"}}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})

	if err := cli.Run(context.Background(), []string{
		"--context", "dev; touch /tmp/should-not-exist",
		"--kubeconfig", "/tmp/kube config",
		"run", "-f", path, "-n", "preview",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantArgv := []string{
		"kubectl", "--context", "dev; touch /tmp/should-not-exist", "--kubeconfig", "/tmp/kube config",
		"create", "--filename", "-", "--namespace", "preview",
	}
	if got := runner.calls[0].argv; !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgv)
	}
	if got := string(runner.calls[0].stdin); got != manifest {
		t.Fatalf("stdin changed manifest: got %q, want %q", got, manifest)
	}
	if strings.Contains(strings.Join(runner.calls[0].argv, " "), "apply") {
		t.Fatal("run delegated apply instead of create")
	}
}

func TestCredentialBearingKubectlFlagsAreNotAccepted(t *testing.T) {
	for _, flag := range []string{"--token", "--client-key", "--client-certificate", "--insecure-skip-tls-verify"} {
		runner := &fakeRunner{}
		cli := NewCLI(Config{Runner: runner})
		if err := cli.Run(context.Background(), []string{flag, "secret-value", "cancel", "repair-427"}); err == nil {
			t.Fatalf("credential-bearing global flag %s was accepted", flag)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("kubectl was invoked for rejected flag %s", flag)
		}
	}
}

func TestRunRejectsMalformedOrUnsafeInputBeforeCallingKubectl(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		file     bool
		args     []string
		want     string
	}{
		{name: "multiple documents", contents: validManifest("one", "") + "---\n" + validManifest("two", ""), file: true, args: nil, want: "exactly one"},
		{name: "wrong kind", contents: strings.Replace(validManifest("one", ""), "kind: AgentRun", "kind: ConfigMap", 1), file: true, args: nil, want: "must be"},
		{name: "server status", contents: validManifest("one", "") + "status: {}\n", file: true, args: nil, want: "status"},
		{name: "namespace mismatch", contents: validManifest("one", "other"), file: true, args: []string{"-n", "preview"}, want: "does not match"},
		{name: "injection name", contents: validManifest("bad;touch /tmp/pwned", ""), file: true, args: nil, want: "invalid AgentRun name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.yaml")
			if !tc.file {
				t.Fatal("test fixture must be a file")
			}
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"run", "-f", path}
			args = append(args, tc.args...)
			runner := &fakeRunner{}
			cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
			err := cli.Run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("kubectl called for invalid manifest: %#v", runner.calls)
			}
		})
	}

	t.Run("directory", func(t *testing.T) {
		runner := &fakeRunner{}
		cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
		err := cli.Run(context.Background(), []string{"run", "-f", t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("error = %v, want regular-file error", err)
		}
		if len(runner.calls) != 0 {
			t.Fatal("kubectl called for directory")
		}
	})
}

func TestCancelUsesOnlyReplaySafeMergePatch(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{stdout: "agentruns.agents.astatide.com/repair-427\n"}, {stdout: "agentruns.agents.astatide.com/repair-427\n"}}}
	cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
	args := []string{"cancel", "repair-427", "-n", "preview", "--context", "dev"}
	for range 2 {
		if err := cli.Run(context.Background(), args); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}
	want := []string{"kubectl", "--context", "dev", "patch", agentRunResource, "repair-427", "--namespace", "preview", "--type", "merge", "--patch", `{"spec":{"cancelRequested":true}}`, "--output", "name"}
	for i, call := range runner.calls {
		if !reflect.DeepEqual(call.argv, want) {
			t.Errorf("call %d argv = %#v, want %#v", i, call.argv, want)
		}
		var patch map[string]map[string]bool
		if err := json.Unmarshal([]byte(call.argv[11]), &patch); err != nil {
			t.Fatalf("patch JSON: %v", err)
		}
		if !reflect.DeepEqual(patch, map[string]map[string]bool{"spec": {"cancelRequested": true}}) {
			t.Fatalf("patch = %#v", patch)
		}
		if len(call.stdin) != 0 {
			t.Errorf("cancel unexpectedly sent stdin")
		}
	}
}

func TestLogsResolvesOwnedSandboxAndPodBeforeStreaming(t *testing.T) {
	runUID := "11111111-1111-4111-8111-111111111111"
	sandboxName := deterministicSandboxName(runUID, "work")
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: agentRunJSON("repair-427", "preview", runUID, sandboxName, "sandbox-uid", "work")},
		{stdout: sandboxJSON(sandboxName, "preview", "sandbox-uid", "repair-427", runUID, "work", "pod-427", "sha256:"+strings.Repeat("a", 64))},
		{stdout: podJSON("pod-427", "preview", "pod-uid", sandboxName, "sandbox-uid", []string{"agent", "broker"})},
		{stdout: "broker log line\n"},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"logs", "repair-427", "--container", "broker", "--follow", "-n", "preview"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if got := stdout.String(); got != "broker log line\n" {
		t.Fatalf("logs output = %q", got)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("got %d kubectl calls, want 4", len(runner.calls))
	}
	wantLogs := []string{"kubectl", "get", agentRunResource, "repair-427", "--namespace", "preview", "--output", "json"}
	if !reflect.DeepEqual(runner.calls[0].argv, wantLogs) {
		t.Fatalf("AgentRun argv = %#v, want %#v", runner.calls[0].argv, wantLogs)
	}
	wantFinal := []string{"kubectl", "logs", "pod-427", "--namespace", "preview", "--container", "broker", "--follow"}
	if !reflect.DeepEqual(runner.calls[3].argv, wantFinal) {
		t.Fatalf("logs argv = %#v, want %#v", runner.calls[3].argv, wantFinal)
	}
}

func TestLogsFallsBackToValidatedSandboxLabels(t *testing.T) {
	runUID := "22222222-2222-4222-8222-222222222222"
	sandboxName := deterministicSandboxName(runUID, "verify")
	digest := "sha256:" + strings.Repeat("b", 64)
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: agentRunWithoutSandboxRefJSON("repair-428", "preview", runUID, digest)},
		{stdout: sandboxListJSON(sandboxName, "preview", "sandbox-uid-2", "repair-428", runUID, "verify", "pod-428", digest)},
		{stdout: sandboxJSON(sandboxName, "preview", "sandbox-uid-2", "repair-428", runUID, "verify", "pod-428", digest)},
		{stdout: podJSON("pod-428", "preview", "pod-uid-2", sandboxName, "sandbox-uid-2", []string{"verify"})},
		{stdout: "verify log\n"},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"logs", "repair-428", "--container=verify", "-n", "preview"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(runner.calls) != 5 || !strings.Contains(strings.Join(runner.calls[1].argv, " "), runUIDLabelKey+"="+runUID) {
		t.Fatalf("label fallback argv/calls = %#v", runner.calls)
	}
}

func TestLogsResolvesOwnedJobAndExactlyOnePodBeforeStreaming(t *testing.T) {
	runUID := "55555555-5555-4555-8555-555555555555"
	jobName := deterministicSandboxName(runUID, "work")
	digest := "sha256:" + strings.Repeat("a", 64)
	planFingerprint := "sha256:" + strings.Repeat("f", 64)
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: agentRunJobJSON("repair-job", "preview", runUID, jobName, "job-uid", "work", digest, planFingerprint)},
		{stdout: jobJSON(jobName, "preview", "job-uid", "repair-job", runUID, "work", digest, planFingerprint)},
		{stdout: jobPodListJSON("job-pod", "preview", "pod-uid", jobName, "job-uid", runUID, "work")},
		{stdout: "job log line\n"},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"logs", "repair-job", "--container", "broker", "-n", "preview"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if stdout.String() != "job log line\n" {
		t.Fatalf("logs output=%q", stdout.String())
	}
	if len(runner.calls) != 4 || !strings.Contains(strings.Join(runner.calls[2].argv, " "), "batch.kubernetes.io/controller-uid=job-uid") {
		t.Fatalf("Job pod resolution calls=%#v", runner.calls)
	}
}

func TestLogsRejectsAmbiguousJobPods(t *testing.T) {
	runUID := "66666666-6666-4666-8666-666666666666"
	jobName := deterministicSandboxName(runUID, "verify")
	digest := "sha256:" + strings.Repeat("a", 64)
	planFingerprint := "sha256:" + strings.Repeat("f", 64)
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: agentRunJobJSON("repair-ambiguous", "preview", runUID, jobName, "job-uid", "verify", digest, planFingerprint)},
		{stdout: jobJSON(jobName, "preview", "job-uid", "repair-ambiguous", runUID, "verify", digest, planFingerprint)},
		{stdout: jobPodListJSON("one", "preview", "pod-one", jobName, "job-uid", runUID, "verify", "two", "pod-two")},
	}}
	cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
	err := cli.Run(context.Background(), []string{"logs", "repair-ambiguous", "--container", "verify", "-n", "preview"})
	if err == nil || !strings.Contains(err.Error(), "exactly one Pod") {
		t.Fatalf("error=%v, want exactly-one-pod rejection", err)
	}
}

func TestLogsRejectsForeignOwnershipAndInjectionNames(t *testing.T) {
	runUID := "33333333-3333-4333-8333-333333333333"
	sandboxName := deterministicSandboxName(runUID, "work")
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: agentRunJSON("repair-429", "preview", runUID, sandboxName, "sandbox-uid-3", "work")},
		{stdout: sandboxJSON(sandboxName, "preview", "sandbox-uid-3", "other-run", runUID, "work", "pod-429", "sha256:"+strings.Repeat("a", 64))},
	}}
	cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"logs", "repair-429", "-n", "preview"}); err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("foreign sandbox error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("got %d calls before ownership rejection, want 2", len(runner.calls))
	}
	before := len(runner.calls)
	if err := cli.Run(context.Background(), []string{"logs", "bad;touch /tmp/pwned"}); err == nil || !strings.Contains(err.Error(), "invalid run name") {
		t.Fatalf("injection name error = %v", err)
	}
	if len(runner.calls) != before {
		t.Fatal("kubectl called for injection name")
	}
}

func TestKubectlErrorsAreBounded(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{stderr: strings.Repeat("x", maxCommandErrorBytes*2), err: errors.New("exit status 1")}}}
	cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
	err := cli.Run(context.Background(), []string{"cancel", "repair-427"})
	if err == nil {
		t.Fatal("expected kubectl error")
	}
	if len(err.Error()) > maxCommandErrorBytes+256 {
		t.Fatalf("error is not bounded: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "output truncated") {
		t.Fatalf("bounded error did not indicate truncation: %v", err)
	}
}

func TestCaptureRejectsOversizedJSON(t *testing.T) {
	runUID := "44444444-4444-4444-8444-444444444444"
	digest := "sha256:" + strings.Repeat("d", 64)
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: agentRunWithoutSandboxRefJSON("repair-430", "preview", runUID, digest)},
		{stdout: strings.Repeat("x", maxJSONOutputBytes+1)},
	}}
	cli := NewCLI(Config{Runner: runner, Stderr: io.Discard})
	err := cli.Run(context.Background(), []string{"logs", "repair-430", "-n", "preview"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want bounded-output error", err)
	}
}

func validManifest(name, namespace string) string {
	manifest := fmt.Sprintf("apiVersion: %s\nkind: AgentRun\nmetadata:\n  name: %s\n", agentRunAPIVersion, name)
	if namespace != "" {
		manifest += "  namespace: " + namespace + "\n"
	}
	return manifest + "spec: {}\n"
}

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentRunJSON(name, namespace, uid, sandboxName, sandboxUID, role string) string {
	digest := "sha256:" + strings.Repeat("a", 64)
	ref := map[string]string{"name": sandboxName, "kind": "Sandbox", "uid": sandboxUID, "role": role, "specDigest": digest, "planFingerprint": "sha256:" + strings.Repeat("f", 64)}
	status := map[string]any{"specDigest": digest}
	if role == "work" {
		status["workSandboxRef"] = ref
	} else {
		status["verifySandboxRef"] = ref
	}
	return mustJSON(map[string]any{"apiVersion": agentRunAPIVersion, "kind": agentRunKind, "metadata": map[string]string{"name": name, "namespace": namespace, "uid": uid}, "status": status})
}

func agentRunJobJSON(name, namespace, uid, jobName, jobUID, role, digest, planFingerprint string) string {
	ref := map[string]string{"name": jobName, "kind": "Job", "uid": jobUID, "role": role, "specDigest": digest, "planFingerprint": planFingerprint}
	status := map[string]any{"specDigest": digest}
	if role == "work" {
		status["workSandboxRef"] = ref
	} else {
		status["verifySandboxRef"] = ref
	}
	return mustJSON(map[string]any{"apiVersion": agentRunAPIVersion, "kind": agentRunKind, "metadata": map[string]string{"name": name, "namespace": namespace, "uid": uid}, "status": status})
}

func agentRunWithoutSandboxRefJSON(name, namespace, uid, digest string) string {
	return mustJSON(map[string]any{"apiVersion": agentRunAPIVersion, "kind": agentRunKind, "metadata": map[string]string{"name": name, "namespace": namespace, "uid": uid}, "status": map[string]string{"specDigest": digest}})
}

func sandboxJSON(name, namespace, uid, runName, runUID, role, podName, digest string) string {
	return mustJSON(map[string]any{"apiVersion": sandboxAPIVersion, "kind": sandboxKind, "metadata": map[string]any{
		"name": name, "namespace": namespace, "uid": uid,
		"labels":          map[string]string{runUIDLabelKey: runUID, runRoleLabelKey: role, specDigestLabel: strings.TrimPrefix(digest, "sha256:")},
		"annotations":     map[string]string{sandboxPodAnn: podName, specDigestAnn: digest, sandbox.SandboxSpecFingerprintAnnotationKey: "sha256:" + strings.Repeat("f", 64)},
		"ownerReferences": []map[string]any{{"apiVersion": agentRunAPIVersion, "kind": agentRunKind, "name": runName, "uid": runUID, "controller": true}},
	}})
}

func sandboxListJSON(name, namespace, uid, runName, runUID, role, podName, digest string) string {
	var item map[string]any
	if err := json.Unmarshal([]byte(sandboxJSON(name, namespace, uid, runName, runUID, role, podName, digest)), &item); err != nil {
		panic(err)
	}
	return mustJSON(map[string]any{"apiVersion": sandboxAPIVersion, "kind": "SandboxList", "items": []any{item}})
}

func jobJSON(name, namespace, uid, runName, runUID, role, digest, planFingerprint string) string {
	return mustJSON(map[string]any{"apiVersion": jobAPIVersion, "kind": jobKind, "metadata": map[string]any{
		"name": name, "namespace": namespace, "uid": uid,
		"labels": map[string]string{
			runUIDLabelKey: runUID, runRoleLabelKey: role, specDigestLabel: strings.TrimPrefix(digest, "sha256:"), sandbox.ChildNameLabelKey: name,
		},
		"annotations":     map[string]string{specDigestAnn: digest, sandbox.JobSpecFingerprintAnnotationKey: planFingerprint},
		"ownerReferences": []map[string]any{{"apiVersion": agentRunAPIVersion, "kind": agentRunKind, "name": runName, "uid": runUID, "controller": true}},
	}})
}

func jobPodListJSON(name, namespace, podUID, jobName, jobUID, runUID, role string, extra ...string) string {
	items := []any{jobPodMap(name, namespace, podUID, jobName, jobUID, runUID, role)}
	for index := 0; index+1 < len(extra); index += 2 {
		items = append(items, jobPodMap(extra[index], namespace, extra[index+1], jobName, jobUID, runUID, role))
	}
	return mustJSON(map[string]any{"apiVersion": podAPIVersion, "kind": "PodList", "items": items})
}

func jobPodMap(name, namespace, podUID, jobName, jobUID, runUID, role string) map[string]any {
	return map[string]any{"apiVersion": podAPIVersion, "kind": podKind, "metadata": map[string]any{
		"name": name, "namespace": namespace, "uid": podUID,
		"labels": map[string]string{
			runUIDLabelKey: runUID, runRoleLabelKey: role, sandbox.ChildNameLabelKey: jobName, jobControllerUID: jobUID, jobNameLabel: jobName,
		},
		"ownerReferences": []map[string]any{{"apiVersion": jobAPIVersion, "kind": jobKind, "name": jobName, "uid": jobUID, "controller": true}},
	}, "spec": map[string]any{"containers": []map[string]string{{"name": "agent"}, {"name": "broker"}, {"name": "verify"}}}}
}

func podJSON(name, namespace, uid, sandboxName, sandboxUID string, containers []string) string {
	podContainers := make([]map[string]string, 0, len(containers))
	for _, container := range containers {
		podContainers = append(podContainers, map[string]string{"name": container})
	}
	return mustJSON(map[string]any{"apiVersion": podAPIVersion, "kind": podKind, "metadata": map[string]any{
		"name": name, "namespace": namespace, "uid": uid,
		"ownerReferences": []map[string]any{{"apiVersion": sandboxAPIVersion, "kind": sandboxKind, "name": sandboxName, "uid": sandboxUID, "controller": true}},
	}, "spec": map[string]any{"containers": podContainers}})
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
