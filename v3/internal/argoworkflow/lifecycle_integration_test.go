package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/phase0/argo-sandbox/bridge"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestProviderFreeLifecycleBindsSandboxEvidenceThroughArgo exercises the
// complete provider-neutral boundary that production Argo uses:
// AgentRun -> immutable Workflow -> operator-owned Sandbox -> runtime/capture
// evidence -> reserved Workflow output -> status observation. It deliberately
// uses the real package boundaries instead of a fake orchestration driver.
func TestProviderFreeLifecycleBindsSandboxEvidenceThroughArgo(t *testing.T) {
	ctx := context.Background()
	input := fixture(t)
	store := &producerMemoryStore{objects: map[string][]byte{}, uri: "s3://agw-artifacts"}
	canonicalSnapshot, err := canonical.CanonicalizeResolvedSpec(input.Snapshot)
	if err != nil {
		t.Fatalf("canonical snapshot: %v", err)
	}
	writer, err := artifacts.NewWriter(store)
	if err != nil {
		t.Fatalf("artifact writer: %v", err)
	}
	if _, err := writer.SaveResolvedSpec(ctx, string(input.Run.UID), input.ResolvedRef.Digest, canonicalSnapshot); err != nil {
		t.Fatalf("save resolved snapshot: %v", err)
	}

	work := integrationWorkSandbox(t, input.Run, input.ResolvedRef.Digest)
	template := workflowTemplateFixture(input.Run.Namespace, input.Config.WorkflowTemplateName, input.Config.WorkflowTemplateUID)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(input.Run, template, work).Build()
	kube := &integrationCreateClient{Client: baseClient}
	backend, err := NewBackend(kube, input.Config)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	binding, err := backend.Ensure(ctx, input.Run, input.Snapshot)
	if err != nil {
		t.Fatalf("Ensure AgentRun Workflow: %v", err)
	}
	if binding.Reference.UID == "" || binding.Reference.Generation != 1 {
		t.Fatalf("Workflow binding=%#v, want API identity", binding)
	}

	setWorkflowStatus(t, kube, binding, map[string]any{"phase": "Running", "message": "workflow is executing"})
	running, err := backend.Observe(ctx, binding)
	if err != nil {
		t.Fatalf("Observe running Workflow: %v", err)
	}
	if running.Phase != BackendRunning || running.Reference != binding.Reference {
		t.Fatalf("running observation=%#v", running)
	}

	producer, err := NewProducer(kube, store, store, input.Config.WorkflowTemplateName, input.Config.WorkflowTemplateUID, input.Config.WorkflowTemplateDigest)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	request := requestFromInput(input)
	prepared, err := producer.Prepare(ctx, request)
	if err != nil {
		t.Fatalf("Prepare operator-owned Sandbox: %v", err)
	}
	request.SandboxUID = string(prepared.Sandbox.UID)
	if prepared.Sandbox.Name != work.Name || prepared.Claim == "" || prepared.Secret == "" {
		t.Fatalf("prepared Sandbox result=%#v", prepared)
	}

	workspace := t.TempDir()
	if err := producer.Stage(ctx, request, workspace); err != nil {
		t.Fatalf("Stage immutable lifecycle inputs: %v", err)
	}
	writeSandboxMarkers(t, workspace, request, work)
	writeCaptureOutput(t, workspace, input.Snapshot, request.BaseSHA)
	writeRuntimeCompletion(t, ctx, store, request)

	handoff, err := producer.Handoff(ctx, request, workspace)
	if err != nil {
		t.Fatalf("Handoff authenticated evidence: %v", err)
	}
	if handoff.Digest == "" || handoff.Contract.WorkflowUID != string(binding.Reference.UID) || handoff.Contract.WorkflowGeneration != binding.Reference.Generation {
		t.Fatalf("handoff=%#v, want bound Workflow identity", handoff)
	}
	if handoff.Contract.Patch.Ref.Digest == "" || handoff.Contract.Runtime.CompletionRef.Digest == "" || handoff.Contract.Verification.InputRef.Digest == "" {
		t.Fatalf("handoff omitted evidence references: %#v", handoff.Contract)
	}

	ready := bridgeSandbox(work, []bridge.Condition{{Type: bridge.ConditionReady, Status: "True", ObservedGeneration: 1, Reason: "PodStarted"}})
	if evaluation, err := bridge.Evaluate(ready, bridge.ConditionReady); err != nil || evaluation.Decision != bridge.Satisfied {
		t.Fatalf("Ready bridge evaluation=%#v err=%v", evaluation, err)
	}
	finished := bridgeSandbox(work, []bridge.Condition{
		{Type: bridge.ConditionReady, Status: "False", ObservedGeneration: 1, Reason: "PodSucceeded"},
		{Type: bridge.ConditionFinished, Status: "True", ObservedGeneration: 1, Reason: bridge.FinishedSucceeded},
	})
	if evaluation, err := bridge.Evaluate(finished, bridge.ConditionFinished); err != nil || evaluation.Decision != bridge.Satisfied {
		t.Fatalf("Finished bridge evaluation=%#v err=%v", evaluation, err)
	}

	setWorkflowStatus(t, kube, binding, lifecycleStatusWithValue("Succeeded", string(handoff.Canonical)))
	succeeded, err := backend.Observe(ctx, binding)
	if err != nil {
		t.Fatalf("Observe successful Workflow handoff: %v", err)
	}
	if succeeded.Phase != BackendSucceeded || succeeded.Output == nil || succeeded.Output.Digest != handoff.Digest {
		t.Fatalf("successful observation=%#v, want canonical handoff digest", succeeded)
	}

	// A backend phase is never enough to advance the run. Removing the sole
	// reserved output from an otherwise successful Workflow remains terminally
	// invalid, which is the fail-closed boundary the controller relies on.
	missingOutput := workflowObject()
	if err := kube.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Reference.Name}, missingOutput); err != nil {
		t.Fatalf("get Workflow for negative assertion: %v", err)
	}
	missingOutput.Object["status"] = map[string]any{"phase": "Succeeded"}
	if _, err := Observe(missingOutput, binding); !errors.Is(err, ErrLifecycleOutputMissing) {
		t.Fatalf("successful Workflow without handoff error=%v, want ErrLifecycleOutputMissing", err)
	}
}

type integrationCreateClient struct {
	client.Client
}

func (c *integrationCreateClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	// The fixture creates exactly one object after seeding: the translated
	// Workflow. Assign the API-server identity that a real create/read-back
	// cycle would provide.
	object.SetUID(types.UID("workflow-uid-integration"))
	object.SetGeneration(1)
	return c.Client.Create(ctx, object, options...)
}

func integrationWorkSandbox(t *testing.T, run *v1alpha1.AgentRun, specDigest string) *sandboxv1beta1.Sandbox {
	t.Helper()
	name, err := sandbox.ChildName(run.UID, sandbox.RoleWork)
	if err != nil {
		t.Fatalf("work Sandbox name: %v", err)
	}
	controller, blockOwnerDeletion := true, true
	policy := sandboxv1beta1.ShutdownPolicyRetain
	return &sandboxv1beta1.Sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: sandboxv1beta1.SandboxKind},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  run.Namespace,
			Name:       name,
			UID:        types.UID("sandbox-uid-integration"),
			Generation: 1,
			Labels: map[string]string{
				sandbox.RunUIDLabelKey:     string(run.UID),
				sandbox.RoleLabelKey:       string(sandbox.RoleWork),
				sandbox.SpecDigestLabelKey: cleanupDigestLabel(specDigest),
			},
			Annotations: map[string]string{
				sandbox.SpecDigestAnnotationKey:             specDigest,
				sandbox.SandboxSpecFingerprintAnnotationKey: "sha256:" + strings.Repeat("b", 64),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version,
				Kind:       "AgentRun",
				Name:       run.Name,
				UID:        run.UID,
				Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{Lifecycle: sandboxv1beta1.Lifecycle{ShutdownPolicy: &policy}},
	}
}

func bridgeSandbox(work *sandboxv1beta1.Sandbox, conditions []bridge.Condition) bridge.Sandbox {
	var result bridge.Sandbox
	result.APIVersion = sandboxv1beta1.GroupVersion.String()
	result.Kind = sandboxv1beta1.SandboxKind
	result.Metadata.Name = work.Name
	result.Metadata.Namespace = work.Namespace
	result.Metadata.UID = string(work.UID)
	result.Metadata.Generation = work.Generation
	result.Status.Conditions = conditions
	return result
}

func writeSandboxMarkers(t *testing.T, workspace string, request Request, work *sandboxv1beta1.Sandbox) {
	t.Helper()
	root := filepath.Join(workspace, ".agw")
	observed := "2026-08-11T12:00:00Z"
	for _, marker := range []struct {
		name      string
		condition string
	}{
		{name: "ready.json", condition: bridge.ConditionReady},
		{name: "finished.json", condition: bridge.ConditionFinished},
	} {
		body, err := json.Marshal(bridge.Marker{Version: bridge.MarkerVersion, Namespace: request.Namespace, Name: work.Name, UID: string(work.UID), Condition: marker.condition, Observed: observed})
		if err != nil {
			t.Fatalf("marshal %s marker: %v", marker.condition, err)
		}
		if err := os.WriteFile(filepath.Join(root, marker.name), body, 0600); err != nil {
			t.Fatalf("write %s marker: %v", marker.condition, err)
		}
	}
}

func writeCaptureOutput(t *testing.T, workspace string, snapshot resolved.Snapshot, baseSHA string) {
	t.Helper()
	patch := []byte("diff --git a/src/main.go b/src/main.go\nindex 1111111..2222222 100644\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	files := []publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte("new\n")}}
	manifest, manifestDigest, err := publish.MarshalPatchManifest(files)
	if err != nil {
		t.Fatalf("marshal patch manifest: %v", err)
	}
	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	result, err := capture.MarshalResult(capture.ResultEnvelope{
		SchemaVersion:  capture.ResultSchemaVersion,
		RunUID:         snapshot.Run.UID,
		SpecDigest:     specDigest,
		BaseSHA:        baseSHA,
		PatchDigest:    publish.DigestForPatchBytes(patch),
		PatchBytes:     int64(len(patch)),
		ManifestDigest: manifestDigest,
		ManifestBytes:  int64(len(manifest)),
		FilesChanged:   1,
		LinesChanged:   2,
		ChangedPaths:   []string{"src/main.go"},
	})
	if err != nil {
		t.Fatalf("marshal capture result: %v", err)
	}
	root := filepath.Join(workspace, ".agw", "capture")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("create capture directory: %v", err)
	}
	for name, body := range map[string][]byte{"result.json": result, "patch.diff": patch, "patch-manifest.json": manifest} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatalf("write capture %s: %v", name, err)
		}
	}
}

func writeRuntimeCompletion(t *testing.T, ctx context.Context, store *producerMemoryStore, request Request) {
	t.Helper()
	collector, err := runtimeevents.NewCollector(request.RunUID, request.SpecDigest, request.BaseSHA, store, runtimeevents.Options{})
	if err != nil {
		t.Fatalf("runtime collector: %v", err)
	}
	started, err := runtimeproto.EncodeLine(proto.Envelope{
		Protocol: proto.ProtocolVersion,
		Kind:     proto.KindEvent,
		Type:     proto.EventRunStarted,
		RunID:    request.RunUID,
		Seq:      1,
		Data:     []byte(`{"agent_id":"agent","sandbox_id":"sandbox"}`),
	})
	if err != nil {
		t.Fatalf("encode runtime start: %v", err)
	}
	if err := collector.Accept(ctx, started); err != nil {
		t.Fatalf("accept runtime start: %v", err)
	}
	line, err := runtimeproto.EncodeLine(proto.Envelope{
		Protocol: proto.ProtocolVersion,
		Kind:     proto.KindEvent,
		Type:     proto.EventRunCompleted,
		RunID:    request.RunUID,
		Seq:      2,
		Terminal: true,
		Data:     []byte(`{"result":"ok","output":{"id":"out","uri":"s3://bucket/out","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size_bytes":1}}`),
	})
	if err != nil {
		t.Fatalf("encode runtime completion: %v", err)
	}
	if err := collector.Accept(ctx, line); err != nil {
		t.Fatalf("accept runtime completion: %v", err)
	}
	if _, err := collector.Finish(ctx, 0); err != nil {
		t.Fatalf("finish runtime completion: %v", err)
	}
}

func setWorkflowStatus(t *testing.T, kube client.Client, binding Binding, status map[string]any) {
	t.Helper()
	workflow := workflowObject()
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: binding.Namespace, Name: binding.Reference.Name}, workflow); err != nil {
		t.Fatalf("get Workflow for status update: %v", err)
	}
	workflow.Object["status"] = status
	if err := kube.Update(context.Background(), workflow); err != nil {
		t.Fatalf("update Workflow status: %v", err)
	}
}
