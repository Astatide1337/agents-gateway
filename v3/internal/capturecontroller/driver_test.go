package capturecontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

const (
	testBaseSHA = "0123456789abcdef0123456789abcdef01234567"
	testRunUID  = "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15"
	testJobUID  = "capture-job-uid"
	testImage   = "ghcr.io/astatide/agw-capture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestEnsureIsDeterministicAndValidatesCreateRaces(t *testing.T) {
	plan := testPlan(t)
	ctx := context.Background()

	base := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	driver := newTestDriver(t, base, nil, nil)
	first, err := driver.Ensure(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if first.Job.UID == "" {
		// The fake client does not allocate UIDs. Give the object the same
		// identity a real API server would provide for the observation tests.
		first.Job.UID = testJobUID
		if err := base.Update(ctx, first.Job); err != nil {
			t.Fatal(err)
		}
	}
	second, err := driver.Ensure(ctx, plan)
	if err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	if second.Job.Name != plan.Job.Name || second.NetworkPolicy.Name != plan.NetworkPolicy.Name {
		t.Fatalf("unexpected ensure result: %#v", second)
	}

	t.Run("AlreadyExists-validates-success", func(t *testing.T) {
		underlying := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		race := &createRaceClient{Client: underlying, mutate: func(object client.Object) {}}
		driver := newTestDriver(t, race, nil, nil)
		if _, err := driver.Ensure(ctx, plan); err != nil {
			t.Fatalf("validated AlreadyExists race: %v", err)
		}
		if !race.jobCreateAttempted {
			t.Fatal("test did not exercise the Job create race")
		}
	})

	t.Run("AlreadyExists-foreign-is-rejected", func(t *testing.T) {
		underlying := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		race := &createRaceClient{Client: underlying, mutate: func(object client.Object) {
			job := object.(*batchv1.Job)
			job.OwnerReferences[0].UID = types.UID("other-run")
		}}
		driver := newTestDriver(t, race, nil, nil)
		if _, err := driver.Ensure(ctx, plan); !errors.Is(err, ErrForeign) {
			t.Fatalf("foreign AlreadyExists race error=%v, want ErrForeign", err)
		}
	})

	t.Run("NetworkPolicy-AlreadyExists-is-revalidated", func(t *testing.T) {
		underlying := fake.NewClientBuilder().WithScheme(testScheme()).Build()
		race := &policyRaceClient{Client: underlying}
		driver := newTestDriver(t, race, nil, nil)
		if _, err := driver.Ensure(ctx, plan); err != nil {
			t.Fatalf("validated NetworkPolicy AlreadyExists race: %v", err)
		}
		if !race.policyCreateAttempted {
			t.Fatal("test did not exercise the NetworkPolicy create race")
		}
	})
}

func TestEnsureRejectsForeignConflictAndSecrets(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(t)

	t.Run("foreign-owner", func(t *testing.T) {
		foreign := plan.Job.DeepCopy()
		foreign.OwnerReferences[0].UID = types.UID("different-run")
		kube := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(foreign).Build()
		driver := newTestDriver(t, kube, nil, nil)
		if _, err := driver.Ensure(ctx, plan); !errors.Is(err, ErrForeign) {
			t.Fatalf("error=%v, want ErrForeign", err)
		}
	})

	t.Run("spec-input-conflict", func(t *testing.T) {
		conflict := plan.Job.DeepCopy()
		conflict.Annotations[InputDigest] = "sha256:" + strings.Repeat("b", 64)
		kube := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(conflict).Build()
		driver := newTestDriver(t, kube, nil, nil)
		if _, err := driver.Ensure(ctx, plan); !errors.Is(err, ErrConflict) {
			t.Fatalf("error=%v, want ErrConflict", err)
		}
	})

	t.Run("secret-volume", func(t *testing.T) {
		unsafe := plan
		unsafe.Job = plan.Job.DeepCopy()
		unsafe.Job.Spec.Template.Spec.Volumes = append(unsafe.Job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "artifact"}},
		})
		driver := newTestDriver(t, fake.NewClientBuilder().WithScheme(testScheme()).Build(), nil, nil)
		if _, err := driver.Ensure(ctx, unsafe); !errors.Is(err, ErrConflict) {
			t.Fatalf("error=%v, want ErrConflict", err)
		}
	})
}

func TestObserveClassifiesAllLifecycleAndOwnershipStates(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(t)
	kube := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	driver := newTestDriver(t, kube, nil, nil)
	ensured, err := driver.Ensure(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	ensured.Job.UID = testJobUID
	if err := kube.Update(ctx, ensured.Job); err != nil {
		t.Fatal(err)
	}
	addOwnedPod(t, kube, ensured.Job)

	assertState := func(name string, condition *batchv1.JobCondition, want ErrorClass, sentinel error) {
		t.Run(name, func(t *testing.T) {
			var current batchv1.Job
			if err := kube.Get(ctx, client.ObjectKeyFromObject(ensured.Job), &current); err != nil {
				t.Fatal(err)
			}
			current.Status.Conditions = nil
			if condition != nil {
				current.Status.Conditions = []batchv1.JobCondition{*condition}
			}
			if err := kube.Status().Update(ctx, &current); err != nil {
				t.Fatal(err)
			}
			observation, err := driver.Observe(ctx, plan)
			if !errors.Is(err, sentinel) || observation.State != want || observation.Pod == nil {
				t.Fatalf("observation=%#v error=%v, want state=%s error=%v", observation, err, want, sentinel)
			}
			if got, ok := ClassOf(err); !ok || got != want {
				t.Fatalf("ClassOf(%v)=(%q,%t), want %q", err, got, ok, want)
			}
		})
	}

	assertState("running", nil, ClassRunning, ErrRunning)
	assertState("succeeded", &batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}, ClassSucceeded, ErrSucceeded)
	assertState("failed", &batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}, ClassFailed, ErrFailed)

	t.Run("missing-job", func(t *testing.T) {
		if err := kube.Delete(ctx, ensured.Job); err != nil {
			t.Fatal(err)
		}
		observation, err := driver.Observe(ctx, plan)
		if !errors.Is(err, ErrMissing) || observation.State != ClassMissing {
			t.Fatalf("observation=%#v error=%v, want missing", observation, err)
		}
	})
}

func TestObserveRequiresExactlyOneOwnedPodAndFreshIdentity(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(t)
	kube := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	driver := newTestDriver(t, kube, nil, nil)
	ensured, err := driver.Ensure(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	ensured.Job.UID = testJobUID
	if err := kube.Update(ctx, ensured.Job); err != nil {
		t.Fatal(err)
	}

	t.Run("no-pod-is-missing", func(t *testing.T) {
		if _, err := driver.Observe(ctx, plan); !errors.Is(err, ErrMissing) {
			t.Fatalf("error=%v, want ErrMissing", err)
		}
	})

	t.Run("foreign-pod", func(t *testing.T) {
		foreign := podForJob(ensured.Job, "foreign-pod")
		foreign.OwnerReferences[0].UID = types.UID("another-job")
		if err := kube.Create(ctx, foreign); err != nil {
			t.Fatal(err)
		}
		if _, err := driver.Observe(ctx, plan); !errors.Is(err, ErrForeign) {
			t.Fatalf("error=%v, want ErrForeign", err)
		}
		if err := kube.Delete(ctx, foreign); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("two-owned-pods-is-conflict", func(t *testing.T) {
		addOwnedPod(t, kube, ensured.Job)
		second := podForJob(ensured.Job, "second-pod")
		if err := kube.Create(ctx, second); err != nil {
			t.Fatal(err)
		}
		if _, err := driver.Observe(ctx, plan); !errors.Is(err, ErrConflict) {
			t.Fatalf("error=%v, want ErrConflict", err)
		}
	})
}

func TestCaptureUsesFixedContainerBoundAndPersistsValidatedPatchAndManifest(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(t)
	kube := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	sink := &memorySink{objects: make(map[string][]byte)}
	reader := &recordingReader{}
	driver := newTestDriver(t, kube, reader, sink)
	ensured, err := driver.Ensure(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	ensured.Job.UID = testJobUID
	ensured.Job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := kube.Status().Update(ctx, ensured.Job); err != nil {
		t.Fatal(err)
	}
	pod := addOwnedPod(t, kube, ensured.Job)

	patch := []byte("diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	specDigest, err := canonical.ResolvedSpecDigest(planSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	patchSum := sha256.Sum256(patch)
	manifest, manifestDigest, err := publish.MarshalPatchManifest([]publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte("new\n")}})
	if err != nil {
		t.Fatal(err)
	}
	resultJSON, err := capture.MarshalResult(capture.ResultEnvelope{
		SchemaVersion: capture.ResultSchemaVersion, RunUID: testRunUID, SpecDigest: specDigest, BaseSHA: testBaseSHA,
		PatchDigest: fmt.Sprintf("sha256:%x", patchSum[:]), PatchBytes: int64(len(patch)), FilesChanged: 1, LinesChanged: 2,
		ManifestDigest: manifestDigest, ManifestBytes: int64(len(manifest)),
		ChangedPaths: []string{"src/main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.frame, err = capture.EncodeOutputFrame(resultJSON, patch, manifest)
	if err != nil {
		t.Fatal(err)
	}

	result, err := driver.Capture(ctx, plan, planSnapshot(t), testBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	patchKey := strings.TrimPrefix(result.Artifact.URI, "s3://bucket/")
	manifestKey := strings.TrimPrefix(result.ManifestArtifact.URI, "s3://bucket/")
	if result.Artifact.Kind != "patch" || result.ManifestArtifact.Kind != "patch-manifest" || !bytes.Equal(sink.objects[patchKey], patch) || !bytes.Equal(sink.objects[manifestKey], manifest) || result.Validated.ManifestArtifact() == nil || len(result.Validated.Files()) != 1 {
		t.Fatalf("capture result=%#v sink=%#v", result, sink)
	}
	if reader.container != CaptureContainer || reader.maxBytes != plan.Output.MaxFrameBytes || reader.namespace != pod.Namespace || reader.pod != pod.Name {
		t.Fatalf("log request=%#v", reader)
	}
}

func TestReadAndValidateRejectsOutputBeyondHardSeamCap(t *testing.T) {
	reader := fixedOutputReader{logs: LogReaderFunc(func(context.Context, string, string, string, int64) ([]byte, error) {
		return []byte("12345"), nil
	}), maxBytes: 4}
	if _, err := reader.ReadCaptureOutput(context.Background(), "agw-runs", "capture", 4); !errors.Is(err, ErrLogOutput) {
		t.Fatalf("error=%v, want ErrLogOutput", err)
	}
}

func TestCleanupIsIdempotentAndRefusesForeignObjects(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(t)
	kube := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	driver := newTestDriver(t, kube, nil, nil)
	if _, err := driver.Ensure(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := driver.Cleanup(ctx, plan); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := driver.Cleanup(ctx, plan); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}

	foreign := plan.NetworkPolicy.DeepCopy()
	foreign.OwnerReferences[0].UID = types.UID("foreign")
	if err := kube.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := driver.Cleanup(ctx, plan); !errors.Is(err, ErrForeign) {
		t.Fatalf("foreign cleanup error=%v, want ErrForeign", err)
	}
	var retained networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKeyFromObject(foreign), &retained); err != nil {
		t.Fatalf("foreign NetworkPolicy was deleted: %v", err)
	}
}

func TestClassOfRecognizesExportedSentinels(t *testing.T) {
	tests := []struct {
		err   error
		class ErrorClass
	}{
		{ErrMissing, ClassMissing}, {ErrRunning, ClassRunning}, {ErrSucceeded, ClassSucceeded},
		{ErrFailed, ClassFailed}, {ErrForeign, ClassForeign}, {ErrConflict, ClassConflict},
	}
	for _, test := range tests {
		got, ok := ClassOf(test.err)
		if !ok || got != test.class {
			t.Errorf("ClassOf(%v)=(%q,%t), want %q", test.err, got, ok, test.class)
		}
	}
}

func newTestDriver(t *testing.T, kube client.Client, reader LogReader, sink capture.ArtifactSink) *Driver {
	t.Helper()
	if reader == nil {
		reader = LogReaderFunc(func(context.Context, string, string, string, int64) ([]byte, error) {
			return nil, errors.New("reader not configured")
		})
	}
	driver, err := New(kube, Options{Logs: reader, Artifacts: sink})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func testPlan(t *testing.T) capture.Plan {
	t.Helper()
	snapshot := planSnapshot(t)
	plan, err := capture.Build(snapshot, testBaseSHA, capture.Options{
		Image: testImage, WorkspaceClaimName: "workspace-repair-427", Timeout: 4 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Job.UID = testJobUID
	plan.NetworkPolicy.UID = "capture-policy-uid"
	return plan
}

func planSnapshot(t *testing.T) resolved.Snapshot {
	t.Helper()
	inlineTask := "fix the issue"
	inlineInstructions := "keep the change focused"
	digest := strings.Repeat("a", 64)
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "repair-427", UID: testRunUID, Generation: 3},
		BaseSHA:       testBaseSHA,
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "issue-fixer", GateRef: "go-default",
			Source:    v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1},
			Task:      v1alpha1.TaskSpec{Inline: &inlineTask},
			Scope:     v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
			Workspace: v1alpha1.WorkspaceSpec{Size: "8Gi"},
			Publish:   v1alpha1.PublishSpec{Mode: v1alpha1.PublishNone},
			Limits:    v1alpha1.LimitsSpec{Timeout: "5m", MaxToolCalls: 60, MaxCostUSD: "2.00", MaxPatchBytes: 4096},
		},
		Task: inlineTask,
		Agent: v1alpha1.AgentSpec{
			Runtime:       v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@sha256:" + digest},
			Instructions:  v1alpha1.InstructionsSpec{Inline: &inlineInstructions},
			ToolSetRef:    "github-readonly",
			ModelRouteRef: "default-codex",
		},
		Instructions: inlineInstructions,
		Gate: v1alpha1.GateSpec{
			Verify:  v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + digest, FromCleanCheckout: true, Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}}, Timeout: "20m"},
			Require: v1alpha1.GateRequirements{ScopeRespected: true, MaxFilesChanged: 3, MaxDiffLines: 20, NoBinaryFiles: true},
		},
	}
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	return scheme
}

func addOwnedPod(t *testing.T, kube client.Client, job *batchv1.Job) *corev1.Pod {
	t.Helper()
	pod := podForJob(job, "capture-pod")
	if err := kube.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func podForJob(job *batchv1.Job, name string) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: name, Labels: copyStringMap(job.Spec.Selector.MatchLabels),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: CaptureContainer, Image: job.Spec.Template.Spec.Containers[0].Image}}, RestartPolicy: corev1.RestartPolicyNever},
	}
}

type recordingReader struct {
	frame     []byte
	namespace string
	pod       string
	container string
	maxBytes  int64
}

func (r *recordingReader) ReadCaptureLogs(_ context.Context, namespace, podName, containerName string, maxBytes int64) ([]byte, error) {
	r.namespace, r.pod, r.container, r.maxBytes = namespace, podName, containerName, maxBytes
	return append([]byte(nil), r.frame...), nil
}

type memorySink struct {
	objects map[string][]byte
	keys    []string
}

func (s *memorySink) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	s.keys = append(s.keys, key)
	if existing, ok := s.objects[key]; ok {
		if !bytes.Equal(existing, body) {
			return false, "", errors.New("conflict")
		}
		return false, "s3://bucket/" + key, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, "s3://bucket/" + key, nil
}

func (s *memorySink) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), body...), nil
}

type createRaceClient struct {
	client.Client
	mutate             func(client.Object)
	jobCreateAttempted bool
}

func (c *createRaceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	job, ok := object.(*batchv1.Job)
	if !ok || c.jobCreateAttempted {
		return c.Client.Create(ctx, object, options...)
	}
	c.jobCreateAttempted = true
	copy := job.DeepCopy()
	c.mutate(copy)
	if err := c.Client.Create(ctx, copy, options...); err != nil {
		return err
	}
	return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, job.Name)
}

type policyRaceClient struct {
	client.Client
	policyCreateAttempted bool
}

func (c *policyRaceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	policy, ok := object.(*networkingv1.NetworkPolicy)
	if !ok || c.policyCreateAttempted {
		return c.Client.Create(ctx, object, options...)
	}
	c.policyCreateAttempted = true
	if err := c.Client.Create(ctx, policy.DeepCopy(), options...); err != nil {
		return err
	}
	return apierrors.NewAlreadyExists(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, policy.Name)
}
