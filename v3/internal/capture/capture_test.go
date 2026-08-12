package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

const (
	testBaseSHA = "0123456789abcdef0123456789abcdef01234567"
	testImage   = "ghcr.io/astatide/agw-capture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestBuildCreatesOnlyNetworklessWorkPVCCapture(t *testing.T) {
	snapshot := testSnapshot()
	options := Options{Image: testImage, WorkspaceClaimName: "agw-workspace-abc", Timeout: 4 * time.Minute}
	first, err := Build(snapshot, testBaseSHA, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(snapshot, testBaseSHA, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical capture inputs produced different plans")
	}
	if first.Job == nil || first.NetworkPolicy == nil {
		t.Fatal("plan omitted Job or deny-all NetworkPolicy")
	}
	job := first.Job
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || job.Namespace != snapshot.Run.Namespace || job.Name != first.JobName {
		t.Fatalf("unexpected Job identity: %#v", job.ObjectMeta)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "AgentRun" || string(job.OwnerReferences[0].UID) != snapshot.Run.UID {
		t.Fatalf("Job owner reference=%#v", job.OwnerReferences)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.Completions == nil || *job.Spec.Completions != 1 {
		t.Fatalf("Job retry/completion policy=%#v", job.Spec)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((4*time.Minute)/time.Second) || job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("Job deadline/retention policy=%#v", job.Spec)
	}
	if job.Spec.ManualSelector == nil || !*job.Spec.ManualSelector || job.Spec.Selector == nil {
		t.Fatal("Job does not use an explicit deterministic selector")
	}

	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 || pod.Containers[0].Name != Role || len(pod.InitContainers) != 0 || len(pod.EphemeralContainers) != 0 {
		t.Fatalf("capture topology containers=%v init=%v ephemeral=%v", containerNames(pod.Containers), containerNames(pod.InitContainers), len(pod.EphemeralContainers))
	}
	if pod.HostUsers == nil || *pod.HostUsers || pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Fatalf("host isolation=%#v", pod)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != "" {
		t.Fatalf("service-account access was not disabled: automount=%v name=%q", pod.AutomountServiceAccountToken, pod.ServiceAccountName)
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("pod lifecycle/network defaults are not fail-closed")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context=%#v", pod.SecurityContext)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].Name != WorkspaceVolumeName || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != options.WorkspaceClaimName {
		t.Fatalf("capture volumes=%#v", pod.Volumes)
	}
	container := pod.Containers[0]
	if !reflect.DeepEqual(container.Command, []string{CaptureEntrypoint}) || container.WorkingDir != RepoPath || len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != WorkspaceVolumeName || container.VolumeMounts[0].ReadOnly {
		t.Fatalf("capture container contract=%#v", container)
	}
	if container.EnvFrom != nil || container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capture container security/env=%#v", container)
	}
	for _, env := range container.Env {
		if env.ValueFrom != nil {
			t.Fatalf("capture env has a projected value: %#v", env)
		}
	}
	if got := envValue(container, "AGW_CAPTURE_BASE_SHA"); got != testBaseSHA {
		t.Fatalf("base SHA env=%q", got)
	}
	if got := envValue(container, "AGW_CAPTURE_PROTOCOL"); got != OutputProtocol {
		t.Fatalf("output protocol env=%q", got)
	}
	if got := envValue(container, "AGW_CAPTURE_RUN_UID"); got != snapshot.Run.UID {
		t.Fatalf("run UID env=%q, want %q", got, snapshot.Run.UID)
	}
	if got := envValue(container, "AGW_CAPTURE_SPEC_JSON"); got == "" || !strings.Contains(got, `"baseSHA":"`+testBaseSHA+`"`) {
		t.Fatalf("capture contract does not bind base SHA: %q", got)
	}

	policy := first.NetworkPolicy
	if policy.APIVersion != "networking.k8s.io/v1" || policy.Kind != "NetworkPolicy" || len(policy.OwnerReferences) != 1 || policy.OwnerReferences[0].Kind != "AgentRun" {
		t.Fatalf("NetworkPolicy identity/owner=%#v", policy)
	}
	if !reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) || len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 {
		t.Fatalf("NetworkPolicy is not explicit deny-all: %#v", policy.Spec)
	}
	if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, job.Spec.Selector.MatchLabels) {
		t.Fatalf("NetworkPolicy selector=%v Job selector=%v", policy.Spec.PodSelector.MatchLabels, job.Spec.Selector.MatchLabels)
	}
	if first.Output.ManifestPath != ManifestPath || first.Output.MaxManifestBytes != DefaultMaxManifestBytes || first.Output.MaxFrameBytes != MaxEncodedFrameBytes(first.Output.MaxResultBytes, first.Output.MaxPatchBytes, first.Output.MaxManifestBytes) || first.Output.MaxFrameBytes <= 0 {
		t.Fatalf("invalid output contract=%#v", first.Output)
	}
}

func TestBuildRejectsUnsafeOrUnboundedInputs(t *testing.T) {
	base := testSnapshot()
	cases := []struct {
		name   string
		mutate func(*resolved.Snapshot, *Options)
		base   string
	}{
		{name: "unpinned image", mutate: func(_ *resolved.Snapshot, options *Options) { options.Image = "ghcr.io/astatide/agw-capture:latest" }},
		{name: "invalid PVC", mutate: func(_ *resolved.Snapshot, options *Options) { options.WorkspaceClaimName = "../workspace" }},
		{name: "capture timeout exceeds run", mutate: func(_ *resolved.Snapshot, options *Options) { options.Timeout = time.Hour }},
		{name: "invalid base SHA", base: strings.Repeat("A", 40)},
		{name: "run patch limit exceeds hard ceiling", mutate: func(snapshot *resolved.Snapshot, _ *Options) {
			snapshot.Spec.Limits.MaxPatchBytes = HardMaxPatchBytes + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := base
			options := Options{Image: testImage, WorkspaceClaimName: "agw-workspace-abc"}
			if tc.mutate != nil {
				tc.mutate(&snapshot, &options)
			}
			baseSHA := testBaseSHA
			if tc.base != "" {
				baseSHA = tc.base
			}
			if _, err := Build(snapshot, baseSHA, options); err == nil {
				t.Fatal("Build unexpectedly accepted unsafe input")
			}
		})
	}
}

func TestOutputFrameRoundTripAndTamperDetection(t *testing.T) {
	manifest, manifestDigest, err := publish.MarshalPatchManifest([]publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte("new\n")}})
	if err != nil {
		t.Fatal(err)
	}
	resultJSON := []byte(`{"schemaVersion":"agents.astatide.com/capture/v1alpha2","runUID":"run","specDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","baseSHA":"` + testBaseSHA + `","patchDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","patchBytes":42,"manifestDigest":"` + manifestDigest + `","manifestBytes":` + fmt.Sprint(len(manifest)) + `,"filesChanged":1,"linesChanged":0,"hasBinaryFiles":false,"changedPaths":["src/main.go"]}`)
	patch := []byte("diff --git a/src/main.go b/src/main.go\n")
	frame, err := EncodeOutputFrame(resultJSON, patch, manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOutputFrame(frame, int64(len(resultJSON)), int64(len(patch)), int64(len(manifest)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.ResultJSON, resultJSON) || !bytes.Equal(decoded.Patch, patch) || !bytes.Equal(decoded.Manifest, manifest) {
		t.Fatal("frame round trip changed payload")
	}
	mutated := append([]byte(nil), frame...)
	mutated[len(mutated)-2] ^= 1
	if _, err := DecodeOutputFrame(mutated, int64(len(resultJSON)), int64(len(patch)), int64(len(manifest))); !errors.Is(err, ErrDigestMismatch) && !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("tampered frame error=%v", err)
	}
	if _, err := DecodeOutputFrame(append(frame, []byte("extra")...), int64(len(resultJSON)), int64(len(patch)), int64(len(manifest))); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("trailing frame error=%v", err)
	}
	if MaxEncodedFrameBytes(0, 1, 1) != 0 || MaxEncodedFrameBytes(DefaultMaxResultBytes, HardMaxPatchBytes+1, DefaultMaxManifestBytes) != 0 {
		t.Fatal("invalid frame limits returned a non-zero bound")
	}
}

func TestValidateComputesPatchEvidenceAndEnforcesGateRules(t *testing.T) {
	snapshot := testSnapshot()
	patch := []byte("diff --git a/src/main.go b/src/main.go\nindex 1111111..2222222 100644\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1,2 @@\n-old\n+new\n+added\n")
	output := makeOutput(t, snapshot, testBaseSHA, patch, []string{"src/main.go"}, 3, false)
	validated, err := Validate(snapshot, testBaseSHA, output, ValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if validated.Envelope.FilesChanged != 1 || validated.Evidence.LinesChanged != 3 || validated.Evidence.HasBinaryFiles || !reflect.DeepEqual(validated.Evidence.ChangedPaths, []string{"src/main.go"}) {
		t.Fatalf("validated evidence=%#v", validated.Evidence)
	}
	if !bytes.Equal(validated.PatchBytes(), patch) {
		t.Fatal("validated patch was not preserved")
	}

	tests := []struct {
		name   string
		mutate func(*resolved.Snapshot, *ResultEnvelope, []byte)
		want   error
	}{
		{name: "scope violation", mutate: func(_ *resolved.Snapshot, envelope *ResultEnvelope, _ []byte) {
			envelope.ChangedPaths = []string{"internal/main.go"}
		}, want: ErrDigestMismatch},
		{name: "diff line bound", mutate: func(snapshot *resolved.Snapshot, _ *ResultEnvelope, _ []byte) { snapshot.Gate.Require.MaxDiffLines = 2 }, want: ErrDiffLimit},
		{name: "binary policy", mutate: func(snapshot *resolved.Snapshot, envelope *ResultEnvelope, _ []byte) {
			snapshot.Gate.Require.NoBinaryFiles = true
			envelope.HasBinaryFiles = true
		}, want: ErrDigestMismatch},
		{name: "file bound", mutate: func(snapshot *resolved.Snapshot, _ *ResultEnvelope, _ []byte) {
			snapshot.Gate.Require.MaxFilesChanged = 0
			snapshot.Spec.Scope.Paths = []string{"src/**"}
			snapshot.Gate.Require.ScopeRespected = true
			snapshot.Gate.Require.MaxDiffLines = 100
		}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidateSnapshot := snapshot
			candidate := output
			var envelope ResultEnvelope
			if err := jsonUnmarshal(output.ResultJSON, &envelope); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(&candidateSnapshot, &envelope, patch)
			}
			candidateDigest, err := canonical.ResolvedSpecDigest(candidateSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			if candidateDigest != canonicalDigest(snapshot) {
				envelope.SpecDigest = candidateDigest
			}
			candidate.ResultJSON, _ = MarshalResult(envelope)
			_, validationErr := Validate(candidateSnapshot, testBaseSHA, candidate, ValidationLimits{})
			if tc.want == nil {
				if validationErr != nil {
					t.Fatalf("Validate error=%v", validationErr)
				}
				return
			}
			if !errors.Is(validationErr, tc.want) {
				t.Fatalf("Validate error=%v, want %v", validationErr, tc.want)
			}
		})
	}

	binaryPatch := []byte("diff --git a/image.bin b/image.bin\nGIT binary patch\nliteral 1\nA0\n")
	binaryOutput := makeOutput(t, snapshot, testBaseSHA, binaryPatch, []string{"image.bin"}, 0, true)
	if _, err := Validate(snapshot, testBaseSHA, binaryOutput, ValidationLimits{}); !errors.Is(err, ErrBinaryPatch) {
		t.Fatalf("binary patch error=%v", err)
	}
	snapshot.Spec.Scope.Paths = []string{"**"}
	binaryOutput = makeOutput(t, snapshot, testBaseSHA, binaryPatch, []string{"image.bin"}, 0, true)
	if _, err := Validate(snapshot, testBaseSHA, binaryOutput, ValidationLimits{}); !errors.Is(err, ErrBinaryPatch) {
		t.Fatalf("binary patch error=%v", err)
	}
}

func TestValidateRejectsMalformedEvidenceAndCanonicalViolations(t *testing.T) {
	snapshot := testSnapshot()
	patch := []byte("diff --git a/src/main.go b/src/main.go\n")
	output := makeOutput(t, snapshot, testBaseSHA, patch, []string{"src/main.go"}, 0, false)
	var envelope ResultEnvelope
	if err := jsonUnmarshal(output.ResultJSON, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.ChangedPaths = []string{"../escape"}
	if _, err := MarshalResult(envelope); !errors.Is(err, ErrPathEvidence) {
		t.Fatalf("unsafe path marshal error=%v", err)
	}
	output.ResultJSON = bytes.Replace(output.ResultJSON, []byte(`{"`), []byte("{ \""), 1)
	if _, err := Validate(snapshot, testBaseSHA, output, ValidationLimits{}); !errors.Is(err, ErrNonCanonicalResult) {
		t.Fatalf("noncanonical result error=%v", err)
	}
	output = makeOutput(t, snapshot, testBaseSHA, []byte("not a git diff\n"), []string{"src/main.go"}, 0, false)
	if _, err := Validate(snapshot, testBaseSHA, output, ValidationLimits{}); !errors.Is(err, ErrInvalidPatch) {
		t.Fatalf("malformed patch error=%v", err)
	}
	output = makeOutput(t, snapshot, testBaseSHA, patch, []string{"src/main.go"}, 0, false)
	if _, err := Validate(snapshot, strings.Repeat("f", 40), output, ValidationLimits{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("base mismatch error=%v", err)
	}
}

func TestValidateRejectsManifestTamperMismatchBinaryAndOversize(t *testing.T) {
	snapshot := testSnapshot()
	patch := []byte("diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	output := makeOutput(t, snapshot, testBaseSHA, patch, []string{"src/main.go"}, 2, false)

	t.Run("tampered bytes", func(t *testing.T) {
		candidate := output
		candidate.Manifest = append([]byte(nil), output.Manifest...)
		candidate.Manifest[len(candidate.Manifest)-1] ^= 1
		if _, err := Validate(snapshot, testBaseSHA, candidate, ValidationLimits{}); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("tampered manifest error=%v", err)
		}
	})

	t.Run("paths disagree with patch", func(t *testing.T) {
		manifest, digest, err := publish.MarshalPatchManifest([]publish.FileChange{{Path: "src/other.go", Mode: "100644", Content: []byte("new\n")}})
		if err != nil {
			t.Fatal(err)
		}
		candidate := output
		candidate.Manifest = manifest
		var envelope ResultEnvelope
		if err := jsonUnmarshal(output.ResultJSON, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.ManifestDigest = digest
		envelope.ManifestBytes = int64(len(manifest))
		candidate.ResultJSON, err = MarshalResult(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Validate(snapshot, testBaseSHA, candidate, ValidationLimits{}); !errors.Is(err, ErrPathEvidence) {
			t.Fatalf("manifest path mismatch error=%v", err)
		}
	})

	t.Run("binary final content", func(t *testing.T) {
		manifest, digest, err := publish.MarshalPatchManifest([]publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte{0, 1, 2}}})
		if err != nil {
			t.Fatal(err)
		}
		candidate := output
		candidate.Manifest = manifest
		var envelope ResultEnvelope
		if err := jsonUnmarshal(output.ResultJSON, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.ManifestDigest = digest
		envelope.ManifestBytes = int64(len(manifest))
		candidate.ResultJSON, err = MarshalResult(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Validate(snapshot, testBaseSHA, candidate, ValidationLimits{}); !errors.Is(err, ErrBinaryPatch) {
			t.Fatalf("binary manifest error=%v", err)
		}
	})

	t.Run("manifest bytes exceed limit", func(t *testing.T) {
		candidate := output
		candidate.Manifest = make([]byte, HardMaxManifestBytes+1)
		if _, err := Validate(snapshot, testBaseSHA, candidate, ValidationLimits{}); !errors.Is(err, ErrDiffLimit) {
			t.Fatalf("oversized manifest error=%v", err)
		}
	})
}

func TestReadAndPersistUseNarrowControllerSeams(t *testing.T) {
	snapshot := testSnapshot()
	patch := []byte("diff --git a/src/main.go b/src/main.go\nindex 1111111..2222222 100644\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	output := makeOutput(t, snapshot, testBaseSHA, patch, []string{"src/main.go"}, 2, false)
	frame, err := EncodeOutputFrame(output.ResultJSON, output.Patch, output.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{frame: frame}
	validated, err := ReadAndValidate(context.Background(), reader, snapshot, testBaseSHA, snapshot.Run.Namespace, "capture-pod", ValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if reader.maxBytes != MaxEncodedFrameBytes(DefaultMaxResultBytes, DefaultMaxPatchBytes, DefaultMaxManifestBytes) || reader.namespace != snapshot.Run.Namespace || reader.pod != "capture-pod" {
		t.Fatalf("reader request=%#v", reader)
	}

	sink := &fakeSink{objects: map[string][]byte{}}
	refs, err := Persist(context.Background(), sink, validated)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Patch.Kind != "patch" || refs.Patch.Name != "patch.diff" || refs.Patch.MediaType != PatchMediaType || refs.Patch.Digest != validated.Envelope.PatchDigest || refs.Patch.SizeBytes != int64(len(patch)) {
		t.Fatalf("patch artifact ref=%#v", refs.Patch)
	}
	if refs.Manifest.Kind != "patch-manifest" || refs.Manifest.Name != "patch-manifest.json" || refs.Manifest.MediaType != ManifestMediaType || refs.Manifest.Digest != validated.Envelope.ManifestDigest || refs.Manifest.SizeBytes != int64(len(output.Manifest)) {
		t.Fatalf("manifest artifact ref=%#v", refs.Manifest)
	}
	if len(sink.puts) != 2 || !bytes.Equal(sink.objects[sink.puts[0]], patch) || !bytes.Equal(sink.objects[sink.puts[1]], output.Manifest) || !strings.Contains(sink.puts[0], "/patches/") || !strings.HasSuffix(sink.puts[1], ".manifest.json") {
		t.Fatalf("sink writes=%#v", sink.puts)
	}
	if _, err := Persist(context.Background(), sink, validated); err != nil {
		t.Fatalf("idempotent persist: %v", err)
	}
	sink.objects[sink.puts[0]] = []byte("different")
	if _, err := Persist(context.Background(), sink, validated); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("conflicting persist error=%v", err)
	}
	if _, err := Persist(context.Background(), sink, ValidatedOutput{}); !errors.Is(err, ErrInvalidArtifactInput) {
		t.Fatalf("unvalidated persist error=%v", err)
	}
}

func TestQuotedGitPathsAreNormalized(t *testing.T) {
	patch := []byte("diff --git \"a/src/foo bar.go\" \"b/src/foo bar.go\"\n--- \"a/src/foo bar.go\"\n+++ \"b/src/foo bar.go\"\n@@ -1 +1 @@\n-old\n+new\n")
	evidence, err := analyzePatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence.ChangedPaths, []string{"src/foo bar.go"}) || evidence.LinesChanged != 2 {
		t.Fatalf("quoted path evidence=%#v", evidence)
	}
}

func testSnapshot() resolved.Snapshot {
	inlineTask := "fix the issue"
	inlineInstructions := "keep the change focused"
	digest := strings.Repeat("a", 64)
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "repair-427", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15", Generation: 3},
		BaseSHA:       testBaseSHA,
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "issue-fixer", GateRef: "go-default",
			Source:    v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1},
			Task:      v1alpha1.TaskSpec{Inline: &inlineTask},
			Scope:     v1alpha1.ScopeSpec{Paths: []string{"src/**"}, Forbidden: []string{"src/generated/**"}},
			Workspace: v1alpha1.WorkspaceSpec{Size: "8Gi"},
			Publish:   v1alpha1.PublishSpec{Mode: v1alpha1.PublishNone},
			Limits:    v1alpha1.LimitsSpec{Timeout: "45m", MaxToolCalls: 60, MaxCostUSD: "2.00"},
		},
		Task:         inlineTask,
		Instructions: inlineInstructions,
		Agent: v1alpha1.AgentSpec{
			Runtime:       v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@sha256:" + digest},
			Instructions:  v1alpha1.InstructionsSpec{Inline: &inlineInstructions},
			ToolSetRef:    "github-readonly",
			ModelRouteRef: "default-codex",
		},
		Gate: v1alpha1.GateSpec{
			Verify:  v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + digest, FromCleanCheckout: true, Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}}, Timeout: "20m"},
			Require: v1alpha1.GateRequirements{ScopeRespected: true, MaxFilesChanged: 3, MaxDiffLines: 10, NoBinaryFiles: true},
		},
	}
}

func makeOutput(t *testing.T, snapshot resolved.Snapshot, baseSHA string, patch []byte, paths []string, lines int64, binary bool) CapturedOutput {
	t.Helper()
	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]publish.FileChange, len(paths))
	for index, path := range paths {
		files[index] = publish.FileChange{Path: path, Mode: "100644", Content: []byte("final\n")}
	}
	manifest, manifestDigest, err := publish.MarshalPatchManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	envelope := ResultEnvelope{SchemaVersion: ResultSchemaVersion, RunUID: snapshot.Run.UID, SpecDigest: digest, BaseSHA: baseSHA, PatchDigest: digestBytes(patch), PatchBytes: int64(len(patch)), ManifestDigest: manifestDigest, ManifestBytes: int64(len(manifest)), FilesChanged: int64(len(paths)), LinesChanged: lines, HasBinaryFiles: binary, ChangedPaths: paths}
	body, err := MarshalResult(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return CapturedOutput{ResultJSON: body, Patch: patch, Manifest: manifest}
}

func canonicalDigest(snapshot resolved.Snapshot) string {
	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		panic(err)
	}
	return digest
}

func jsonUnmarshal(body []byte, value any) error {
	return json.Unmarshal(body, value)
}

type fakeReader struct {
	frame     []byte
	namespace string
	pod       string
	maxBytes  int64
}

func (f *fakeReader) ReadCaptureOutput(_ context.Context, namespace, pod string, maxBytes int64) ([]byte, error) {
	f.namespace, f.pod, f.maxBytes = namespace, pod, maxBytes
	return append([]byte(nil), f.frame...), nil
}

type fakeSink struct {
	objects     map[string][]byte
	puts        []string
	contentType string
}

func (f *fakeSink) Put(_ context.Context, key string, body []byte, contentType string) (bool, string, error) {
	f.puts = append(f.puts, key)
	f.contentType = contentType
	if _, ok := f.objects[key]; ok {
		return false, "s3://bucket/" + key, nil
	} else {
		f.objects[key] = append([]byte(nil), body...)
		return true, "s3://bucket/" + key, nil
	}
}

func (f *fakeSink) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := f.objects[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), body...), nil
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for index, container := range containers {
		names[index] = container.Name
	}
	return names
}

func envValue(container corev1.Container, name string) string {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}
