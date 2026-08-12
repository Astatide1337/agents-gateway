// Package fullworkload contains a provider-free integration contract for the
// direct fallback backend. It intentionally exercises the same builders and
// phase drivers that the operator uses; only Kubernetes and object storage are
// replaced with disposable in-memory seams.
package fullworkload

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/capturecontroller"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestProviderFreeProductionBuildersThroughDirectBackend proves the complete
// direct path without a model, MCP, GitHub, or object-store credential:
//
//	work builder -> direct Job/PVC backend -> capture builder/controller
//	-> immutable patch + manifest -> independent verify builder/controller
//	-> finished evidence -> signed Gate report -> bounded cleanup
//
// The test does not claim that a real container image executed. The remaining
// image/provider boundary is documented in README.md beside this test.
func TestProviderFreeProductionBuildersThroughDirectBackend(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	snapshot := providerFreeSnapshot()
	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		t.Fatalf("resolve spec digest: %v", err)
	}
	owner := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Namespace: snapshot.Run.Namespace,
		Name:      snapshot.Run.Name,
		UID:       types.UID(snapshot.Run.UID),
	}}
	kube := newKubernetesClient(t)
	backend, err := sandbox.NewJobBackend(kube, sandbox.BackendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new direct backend: %v", err)
	}

	workManifest, err := workload.Build(snapshot, workloadOptions(snapshot, now))
	if err != nil {
		t.Fatalf("build production work Sandbox: %v", err)
	}
	workPlan := sandbox.SandboxPlan{Owner: owner, Role: sandbox.RoleWork, SpecDigest: specDigest, Sandbox: workManifest}
	workRef, err := backend.Ensure(ctx, workPlan)
	if err != nil {
		t.Fatalf("ensure production work through direct backend: %v", err)
	}
	replayedWorkRef, err := backend.Ensure(ctx, workPlan)
	if err != nil {
		t.Fatalf("replay production work through direct backend: %v", err)
	}
	if workRef != replayedWorkRef {
		t.Fatalf("direct backend replay changed work identity: first=%#v second=%#v", workRef, replayedWorkRef)
	}
	workJob := getJob(t, kube, workRef)
	if got := containerNames(workJob.Spec.Template.Spec.Containers); got != "agent,broker" {
		t.Fatalf("direct work Job containers=%q, want agent,broker", got)
	}
	workClaimName := "workspace-" + workRef.Name
	if claim := getPVC(t, kube, workClaimName); claim.OwnerReferences[0].UID != owner.UID {
		t.Fatalf("work PVC owner=%q, want AgentRun UID %q", claim.OwnerReferences[0].UID, owner.UID)
	}

	setJobActive(t, kube, workJob)
	active, err := backend.Observe(ctx, workRef)
	if err != nil {
		t.Fatalf("observe active work: %v", err)
	}
	if !active.Exists || !active.Ready || active.Finished {
		t.Fatalf("active work observation=%#v, want ready but unfinished", active)
	}
	setJobComplete(t, kube, workJob)
	finished, err := backend.Observe(ctx, workRef)
	if err != nil {
		t.Fatalf("observe finished work: %v", err)
	}
	if !finished.Ready || !finished.Finished {
		t.Fatalf("finished work observation=%#v, want ready and finished", finished)
	}

	patch, manifest, resultJSON := captureFixture(t, snapshot, specDigest)
	capturePlan, err := capture.Build(snapshot, snapshot.BaseSHA, capture.Options{
		Image:              pinned("capture"),
		WorkspaceClaimName: workClaimName,
		Timeout:            4 * time.Minute,
		MaxPatchBytes:      8 << 20,
		MaxResultBytes:     64 << 10,
		MaxManifestBytes:   64 << 10,
	})
	if err != nil {
		t.Fatalf("build production capture Job: %v", err)
	}
	store := newMemoryStore()
	reader := &captureReader{}
	captureDriver, err := capturecontroller.New(kube, capturecontroller.Options{Logs: reader, Artifacts: store})
	if err != nil {
		t.Fatalf("new capture driver: %v", err)
	}
	ensuredCapture, err := captureDriver.Ensure(ctx, capturePlan)
	if err != nil {
		t.Fatalf("ensure production capture Job: %v", err)
	}
	if volumes := ensuredCapture.Job.Spec.Template.Spec.Volumes; len(volumes) != 1 || volumes[0].PersistentVolumeClaim == nil || volumes[0].PersistentVolumeClaim.ClaimName != workClaimName {
		t.Fatalf("capture Job workspace claim=%#v, want the completed work PVC %q", volumes, workClaimName)
	}
	setCaptureJobComplete(t, kube, ensuredCapture.Job)
	addCapturePod(t, kube, currentJob(t, kube, ensuredCapture.Job))
	reader.frame, err = capture.EncodeOutputFrame(resultJSON, patch, manifest)
	if err != nil {
		t.Fatalf("encode capture frame: %v", err)
	}
	captured, err := captureDriver.Capture(ctx, capturePlan, snapshot, snapshot.BaseSHA)
	if err != nil {
		t.Fatalf("capture and persist production evidence: %v", err)
	}
	if captured.Artifact.Digest == "" || captured.ManifestArtifact.Digest == "" || !bytes.Equal(store.objects[artifactKey(captured.Artifact)], patch) || !bytes.Equal(store.objects[artifactKey(captured.ManifestArtifact)], manifest) {
		t.Fatalf("capture did not persist exact patch/manifest: %#v", captured)
	}
	if err := captureDriver.Cleanup(ctx, capturePlan); err != nil {
		t.Fatalf("cleanup capture child: %v", err)
	}
	assertNotFound(t, kube, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: snapshot.Run.Namespace, Name: capturePlan.JobName}})
	assertNotFound(t, kube, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: snapshot.Run.Namespace, Name: capturePlan.JobName + "-deny"}})

	verifyInput := verifycontroller.Inputs{
		Snapshot: snapshot, SpecDigest: specDigest, BaseSHA: snapshot.BaseSHA, Patch: captured.Artifact,
		Credentials: verifycontroller.CredentialProjection{
			SecretName: workload.VerifySecretName(snapshot.Run.UID), CloneSecretKey: "clone-token",
		},
		ArtifactStoreRegion: "us-east-1", ArtifactStoreBucket: "provider-free",
		ArtifactCredentialTTL: 45 * time.Minute, MaxPatchBytes: 8 << 20, MaxOutputBytes: 64 << 10,
		FetchImage: pinned("fetch"), ApplyImage: pinned("apply"), LockdownImage: pinned("lockdown"),
		Now: now, ShutdownTime: now.Add(20 * time.Minute), MaxShutdownDuration: time.Hour,
	}
	verifyPlan, err := verifyworkload.Build(snapshot, captured.Artifact, snapshot.BaseSHA, verifyworkload.Options{
		FetchImage: pinned("fetch"), ApplyImage: pinned("apply"), LockdownImage: pinned("lockdown"),
		SecretName: workload.VerifySecretName(snapshot.Run.UID), CloneSecretKey: "clone-token",
		ArtifactStoreRegion: "us-east-1", ArtifactStoreBucket: "provider-free", ArtifactCredentialTTL: 45 * time.Minute,
		MaxPatchBytes: 8 << 20, MaxOutputBytes: 64 << 10,
		Now: now, ShutdownTime: now.Add(20 * time.Minute), MaxShutdownDuration: time.Hour,
	})
	if err != nil {
		t.Fatalf("build production verify Sandbox: %v", err)
	}
	if verifyPlan.Role != sandbox.RoleVerify || len(verifyPlan.Sandbox.Spec.VolumeClaimTemplates) != 0 {
		t.Fatalf("verify builder plan=%#v, want independent no-PVC plan", verifyPlan)
	}
	evidence := &verificationReader{}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	verifyDriver, err := verifycontroller.New(verifycontroller.Options{
		Backend: backend, Evidence: evidence, ReportStore: store,
		ReportSigner: verifycontroller.SignerFunc(func(report gate.VerificationReport) (gate.SignedReport, error) {
			return gate.SignReport(report, privateKey)
		}),
	})
	if err != nil {
		t.Fatalf("new verify driver: %v", err)
	}
	pending, err := verifyDriver.Reconcile(ctx, verifyInput)
	if err != nil {
		t.Fatalf("initial verify reconcile: %v", err)
	}
	if pending.Complete || pending.SandboxFinished || pending.VerifySandboxRef.Name == "" {
		t.Fatalf("initial verify decision=%#v, want pending unfinished child", pending)
	}
	verifyJob := getJob(t, kube, pending.SandboxObservation.Ref)
	if got := containerNames(verifyJob.Spec.Template.Spec.Containers); got != "verify" {
		t.Fatalf("direct verify Job containers=%q, want verify only", got)
	}
	verifyContainer := verifyJob.Spec.Template.Spec.Containers[0]
	if len(verifyContainer.EnvFrom) != 0 {
		t.Fatal("verify container received an EnvFrom credential projection")
	}
	for _, env := range verifyContainer.Env {
		if env.ValueFrom != nil {
			t.Fatalf("verify container received a valueFrom credential projection: %#v", env)
		}
	}
	for _, mount := range verifyContainer.VolumeMounts {
		if mount.Name == verifyworkload.FetchSecretVolumeName {
			t.Fatal("verify container received the fetch Secret volume")
		}
	}
	for _, volume := range verifyJob.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			t.Fatalf("verify Job mounted PVC %q; verification must be independent", volume.PersistentVolumeClaim.ClaimName)
		}
	}
	setJobComplete(t, kube, verifyJob)
	evidence.frame = verificationFixture(t, snapshot, specDigest, captured.Artifact.Digest)
	completed, err := verifyDriver.Reconcile(ctx, verifyInput)
	if err != nil {
		t.Fatalf("completed verify reconcile: %v", err)
	}
	if !completed.Complete || completed.Gate == nil || completed.Gate.Verdict != string(gate.Accepted) || completed.Gate.ReportRef == nil {
		t.Fatalf("completed verify decision=%#v, want signed Accepted report", completed)
	}
	reportBytes := store.objects[artifactKey(*completed.Gate.ReportRef)]
	signed, err := gate.ParseSignedReport(reportBytes)
	if err != nil {
		t.Fatalf("parse persisted signed report: %v", err)
	}
	if err := gate.VerifySignedReport(signed, privateKey.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("verify persisted signed report: %v", err)
	}
	replayed, err := verifyDriver.Reconcile(ctx, verifyInput)
	if err != nil {
		t.Fatalf("replayed verify reconcile: %v", err)
	}
	if !replayed.Complete || replayed.Gate == nil || replayed.Gate.ReportRef.Digest != completed.Gate.ReportRef.Digest {
		t.Fatalf("verify replay changed report identity: first=%#v second=%#v", completed, replayed)
	}
	if err := backend.Delete(ctx, pending.SandboxObservation.Ref); err != nil {
		t.Fatalf("cleanup verify child: %v", err)
	}
	assertNotFound(t, kube, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: snapshot.Run.Namespace, Name: pending.SandboxObservation.Ref.Name}})
	if err := backend.Delete(ctx, workRef); err != nil {
		t.Fatalf("cleanup work child: %v", err)
	}
	assertNotFound(t, kube, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: snapshot.Run.Namespace, Name: workRef.Name}})
	// The direct backend intentionally leaves the AgentRun-owned workspace PVC
	// for retention. The test proves both halves of that contract.
	_ = getPVC(t, kube, workClaimName)
}

func providerFreeSnapshot() resolved.Snapshot {
	digest := strings.Repeat("a", 64)
	inlineTask := "exercise the provider-free lifecycle"
	inlineInstructions := "make no external changes"
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		BaseSHA:       strings.Repeat("d", 40),
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "provider-free-run", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15", Generation: 1},
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "fixture-agent", GateRef: "fixture-gate",
			Source: v1alpha1.SourceSpec{Repo: "github.com/example/provider-free", BaseRef: "main", Depth: 1},
			Task:   v1alpha1.TaskSpec{Inline: &inlineTask}, Scope: v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
			Workspace: v1alpha1.WorkspaceSpec{Size: "8Gi"}, Publish: v1alpha1.PublishSpec{Mode: v1alpha1.PublishNone},
			Limits: v1alpha1.LimitsSpec{Timeout: "45m", MaxToolCalls: 1, MaxCostUSD: "0", MaxPatchBytes: 8 << 20},
		},
		Task: inlineTask, Instructions: inlineInstructions,
		Agent: v1alpha1.AgentSpec{
			Runtime:      v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime-codex@sha256:" + digest},
			Instructions: v1alpha1.InstructionsSpec{Inline: &inlineInstructions}, ContextStrategyRef: "fixture-context",
			ToolSetRef: "fixture-tools", ModelRouteRef: "fixture-model",
		},
		Gate: v1alpha1.GateSpec{
			Verify:  v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + digest, FromCleanCheckout: true, Commands: []v1alpha1.VerifyCommand{{Argv: []string{"true"}}}, Timeout: "20m"},
			Require: v1alpha1.GateRequirements{ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone, MaxFilesChanged: 3, MaxDiffLines: 20, NoBinaryFiles: true},
			OnFail:  "Rejected", Mode: v1alpha1.GateShadow,
		},
		ToolSet: v1alpha1.ToolSetSpec{
			Servers:          []v1alpha1.ToolServer{{Name: "fixture", Ref: "https://mcp.example.invalid/mcp", Tools: []v1alpha1.ToolDefinition{{Name: "read", Effect: v1alpha1.EffectRead}}}},
			Profiles:         []v1alpha1.ToolProfile{{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "fixture", Tool: "read"}}}, {Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "fixture", Tool: "read"}}}, {Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "fixture", Tool: "read"}}}},
			MaxToolsPerPhase: 1,
		},
		ModelRoute:      v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "fixture", Kind: "openai-responses", Model: "provider-free", Family: "codex", Priority: 1}}, Budget: v1alpha1.ModelBudget{MaxCostUSD: "0"}},
		ContextStrategy: v1alpha1.ContextStrategySpec{RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 128}, Budget: v1alpha1.ContextBudgetSpec{TotalTokens: 1024, MaxBytes: 1 << 20}},
		References:      resolved.References{Gate: resolved.ObjectVersion{Name: "fixture-gate", UID: "fixture-gate-uid", Generation: 1}},
	}
}

func workloadOptions(snapshot resolved.Snapshot, now time.Time) workload.Options {
	return workload.Options{
		CloneImage: pinned("clone"), ContextImage: pinned("context"), LockdownImage: pinned("lockdown"), BrokerImage: pinned("broker"),
		SecretName: workload.WorkSecretName(snapshot.Run.UID), CloneSecretKey: "clone-token",
		Now: now, ShutdownTime: now.Add(45 * time.Minute), MaxShutdownDuration: time.Hour,
	}
}

func captureFixture(t *testing.T, snapshot resolved.Snapshot, specDigest string) ([]byte, []byte, []byte) {
	t.Helper()
	patch := []byte("diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	manifest, manifestDigest, err := publish.MarshalPatchManifest([]publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte("new\n")}})
	if err != nil {
		t.Fatalf("marshal capture manifest: %v", err)
	}
	result, err := capture.MarshalResult(capture.ResultEnvelope{
		SchemaVersion: capture.ResultSchemaVersion, RunUID: snapshot.Run.UID, SpecDigest: specDigest, BaseSHA: snapshot.BaseSHA,
		PatchDigest: publish.DigestForPatchBytes(patch), PatchBytes: int64(len(patch)), ManifestDigest: manifestDigest, ManifestBytes: int64(len(manifest)),
		FilesChanged: 1, LinesChanged: 2, ChangedPaths: []string{"src/main.go"},
	})
	if err != nil {
		t.Fatalf("marshal capture result: %v", err)
	}
	return patch, manifest, result
}

func verificationFixture(t *testing.T, snapshot resolved.Snapshot, specDigest, patchDigest string) []byte {
	t.Helper()
	exitCode, filesChanged, linesChanged, duration := int32(0), int64(1), int64(2), int64(1)
	binary := false
	body, err := json.Marshal(verifycontroller.MachineEvidence{
		SchemaVersion: verifycontroller.EvidenceSchemaVersion, RunUID: snapshot.Run.UID, SpecDigest: specDigest, BaseSHA: snapshot.BaseSHA, PatchDigest: patchDigest,
		ChangedPaths: []string{"src/main.go"}, FilesChanged: &filesChanged, LinesChanged: &linesChanged, HasBinaryFiles: &binary,
		Commands: []verifycontroller.CommandEvidence{{Index: 0, ExitCode: &exitCode, EvidenceDigest: digest("command output"), DurationMillis: &duration}},
	})
	if err != nil {
		t.Fatalf("marshal verification evidence: %v", err)
	}
	frame, err := verifycontroller.EncodeEvidenceFrame(body)
	if err != nil {
		t.Fatalf("encode verification evidence: %v", err)
	}
	return frame
}

func newKubernetesClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme, sandboxv1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&batchv1.Job{}).Build()
}

func getJob(t *testing.T, kube client.Client, ref sandbox.SandboxRef) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, job); err != nil {
		t.Fatalf("get Job %s/%s: %v", ref.Namespace, ref.Name, err)
	}
	return job
}

func getPVC(t *testing.T, kube client.Client, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "agw-runs", Name: name}, pvc); err != nil {
		t.Fatalf("get PVC %q: %v", name, err)
	}
	return pvc
}

func setJobActive(t *testing.T, kube client.Client, job *batchv1.Job) {
	t.Helper()
	job = currentJob(t, kube, job)
	job.Status.Active = 1
	if err := kube.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("set Job active: %v", err)
	}
}

func setJobComplete(t *testing.T, kube client.Client, job *batchv1.Job) {
	t.Helper()
	job = currentJob(t, kube, job)
	job.Status.Active = 0
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed"}}
	if err := kube.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("set Job complete: %v", err)
	}
}

func setCaptureJobComplete(t *testing.T, kube client.Client, job *batchv1.Job) {
	t.Helper()
	job = currentJob(t, kube, job)
	job.UID = types.UID("provider-free-capture-job")
	if err := kube.Update(context.Background(), job); err != nil {
		t.Fatalf("assign capture Job identity: %v", err)
	}
	job = currentJob(t, kube, job)
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed"}}
	if err := kube.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("set capture Job complete: %v", err)
	}
}

func currentJob(t *testing.T, kube client.Client, job *batchv1.Job) *batchv1.Job {
	t.Helper()
	current := &batchv1.Job{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(job), current); err != nil {
		t.Fatalf("refresh Job %s/%s: %v", job.Namespace, job.Name, err)
	}
	return current
}

func addCapturePod(t *testing.T, kube client.Client, job *batchv1.Job) {
	t.Helper()
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: job.Namespace, Name: "provider-free-capture-pod", Labels: copyMap(job.Spec.Selector.MatchLabels),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}},
	}, Spec: job.Spec.Template.Spec}
	if err := kube.Create(context.Background(), pod); err != nil {
		t.Fatalf("create capture Pod: %v", err)
	}
}

func assertNotFound(t *testing.T, kube client.Client, object client.Object) {
	t.Helper()
	err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object)
	if err == nil {
		t.Fatalf("%T %s/%s still exists", object, object.GetNamespace(), object.GetName())
	}
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get %T %s/%s: %v", object, object.GetNamespace(), object.GetName(), err)
	}
}

func pinned(name string) string {
	return "ghcr.io/astatide/agw-" + name + "@sha256:" + strings.Repeat("b", 64)
}

func digest(value string) string { return publish.DigestForPatchBytes([]byte(value)) }

func artifactKey(ref v1alpha1.ArtifactRef) string {
	return strings.TrimPrefix(ref.URI, "s3://provider-free/")
}

func containerNames(containers []corev1.Container) string {
	names := make([]string, len(containers))
	for index := range containers {
		names[index] = containers[index].Name
	}
	return strings.Join(names, ",")
}

func copyMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

type captureReader struct{ frame []byte }

func (r *captureReader) ReadCaptureLogs(_ context.Context, _, _, _ string, _ int64) ([]byte, error) {
	return append([]byte(nil), r.frame...), nil
}

type verificationReader struct{ frame []byte }

func (r *verificationReader) ReadFrame(_ context.Context, _ sandbox.SandboxRef, _ int64) ([]byte, error) {
	if len(r.frame) == 0 {
		return nil, verifycontroller.ErrEvidenceNotReady
	}
	return append([]byte(nil), r.frame...), nil
}

type memoryStore struct{ objects map[string][]byte }

func newMemoryStore() *memoryStore { return &memoryStore{objects: make(map[string][]byte)} }

func (s *memoryStore) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	if existing, ok := s.objects[key]; ok {
		if !bytes.Equal(existing, body) {
			return false, "", fmt.Errorf("immutable object conflict for %q", key)
		}
		return false, "s3://provider-free/" + key, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, "s3://provider-free/" + key, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q is missing", key)
	}
	return append([]byte(nil), body...), nil
}
