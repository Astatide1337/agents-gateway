package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type retentionObjectFake struct {
	objects  []ObjectInfo
	complete bool
	deletes  []struct{ key, etag string }
	err      error
}

func (f *retentionObjectFake) List(context.Context, string, int) ([]ObjectInfo, bool, error) {
	return append([]ObjectInfo(nil), f.objects...), f.complete, f.err
}

func (f *retentionObjectFake) Delete(_ context.Context, key, etag string) error {
	f.deletes = append(f.deletes, struct{ key, etag string }{key: key, etag: etag})
	return nil
}

type retentionKubeDeleteFake struct {
	objects []client.Object
	options [][]client.DeleteOption
	err     error
}

func (f *retentionKubeDeleteFake) Delete(_ context.Context, object client.Object, options ...client.DeleteOption) error {
	f.objects = append(f.objects, object)
	f.options = append(f.options, options)
	return f.err
}

func TestKubernetesSourceFailsClosedOnPartialObjectInventory(t *testing.T) {
	reader := &retentionListFake{run: apiRunForInventory()}
	objects := &retentionObjectFake{complete: false}
	source := &KubernetesSource{Reader: reader, Metadata: reader, Objects: objects, Namespace: "agw-runs", ObjectPrefix: "agents-gateway/v3", Limits: DefaultInventoryLimits()}
	_, err := source.Collect(context.Background())
	if !errors.Is(err, ErrInventoryIncomplete) {
		t.Fatalf("Collect() error = %v, want ErrInventoryIncomplete", err)
	}
}

func TestKubernetesSourceFailsClosedOnUnboundedOrUnprefixedObjects(t *testing.T) {
	reader := &retentionListFake{}
	objects := &retentionObjectFake{complete: true, objects: []ObjectInfo{
		{Key: "agents-gateway/v3/runs/run-uid/events/a.json", ETag: "\"a\""},
		{Key: "agents-gateway/v3/runs/run-uid/events/b.json", ETag: "\"b\""},
	}}
	source := &KubernetesSource{Reader: reader, Metadata: reader, Objects: objects, Namespace: "agw-runs", ObjectPrefix: "agents-gateway/v3", Limits: InventoryLimits{MaxRuns: 4, MaxResources: 4, MaxObjects: 1, MaxLedgerBodyBytes: effects.MaxLedgerBodyBytes}}
	if _, err := source.Collect(context.Background()); !errors.Is(err, ErrInventoryIncomplete) {
		t.Fatalf("unbounded object inventory error = %v, want ErrInventoryIncomplete", err)
	}

	objects.objects = []ObjectInfo{{Key: "outside/prefix.json", ETag: "\"outside\""}}
	source.Limits.MaxObjects = 4
	if _, err := source.Collect(context.Background()); !errors.Is(err, ErrInventoryIncomplete) {
		t.Fatalf("unprefixed object inventory error = %v, want ErrInventoryIncomplete", err)
	}
}

func TestInventoryLimitsRejectUnboundedConfiguration(t *testing.T) {
	limits := DefaultInventoryLimits()
	limits.MaxObjects = MaxInventoryItems + 1
	if _, err := limits.normalized(); !errors.Is(err, ErrInvalidInventoryConfig) {
		t.Fatalf("unbounded inventory limits error = %v, want ErrInvalidInventoryConfig", err)
	}
}

func TestApplyIsDryRunByDefaultAndLifecycleGuarded(t *testing.T) {
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-time.Hour))
	secret := secretResource(run, runOwner(run), true)
	policy := DefaultPolicy()
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{secret}})
	if err != nil || len(plan.Actions) != 1 {
		t.Fatalf("plan = %#v, error = %v", plan, err)
	}
	kube := &retentionKubeDeleteFake{}
	objects := &retentionObjectFake{complete: true}
	report, err := Apply(context.Background(), plan, kube, objects, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, Enforce: false, DryRun: true})
	if err != nil || report.Deleted != 0 || len(kube.objects) != 0 {
		t.Fatalf("dry-run report = %#v, error = %v, kube deletes=%d", report, err, len(kube.objects))
	}

	policy.DryRun = false
	plan, err = policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{secret}})
	if err != nil || len(plan.Actions) != 1 {
		t.Fatalf("enforcing plan = %#v, error = %v", plan, err)
	}
	report, err = Apply(context.Background(), plan, kube, objects, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, Enforce: true, DryRun: false, LifecycleAttested: false})
	if err != nil || !report.Guarded || report.Deleted != 0 || len(kube.objects) != 0 {
		t.Fatalf("guarded report = %#v, error = %v, kube deletes=%d", report, err, len(kube.objects))
	}
}

func TestApplyUsesUIDAndETagFences(t *testing.T) {
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-30*24*time.Hour))
	secret := secretResource(run, runOwner(run), true)
	artifact := oldArtifact(run, "patch/patch.diff", 12)
	policy := DefaultPolicy()
	policy.DryRun = false
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{secret}, Artifacts: []Artifact{artifact}})
	if err != nil || len(plan.Actions) != 2 {
		t.Fatalf("plan = %#v, error = %v", plan, err)
	}
	kube := &retentionKubeDeleteFake{}
	objects := &retentionObjectFake{complete: true}
	report, err := Apply(context.Background(), plan, kube, objects, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, Enforce: true, DryRun: false, LifecycleAttested: true})
	if err != nil || report.Deleted != 2 || len(kube.objects) != 1 || len(objects.deletes) != 1 {
		t.Fatalf("apply report = %#v, error = %v, kube=%d objects=%d", report, err, len(kube.objects), len(objects.deletes))
	}
	if objects.deletes[0].etag != artifact.ETag {
		t.Fatalf("object delete ETag = %q, want %q", objects.deletes[0].etag, artifact.ETag)
	}
	if len(kube.options[0]) == 0 {
		t.Fatal("Kubernetes delete did not receive UID precondition")
	}
}

func TestUnflushedEvidenceCannotBePlanned(t *testing.T) {
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-30*24*time.Hour))
	run.EvidenceFlushed = false
	artifact := oldArtifact(run, "events/completion.jsonl", 10)
	plan, err := DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{run}, Artifacts: []Artifact{artifact}})
	if err != nil || len(plan.Actions) != 0 {
		t.Fatalf("plan = %#v, error = %v", plan, err)
	}
	assertSkippedArtifact(t, plan, artifact.Key, SkipEvidenceNotFlushed)
}

type retentionListFake struct {
	run           *v1alpha1.AgentRun
	continueToken string
	err           error
}

func (f *retentionListFake) ListMetadata(_ context.Context, resource schema.GroupVersionResource, namespace string, _ int) (*metav1.PartialObjectMetadataList, error) {
	if f.err != nil {
		return nil, f.err
	}
	if resource.Group != "" || resource.Version != "v1" || (resource.Resource != "secrets" && resource.Resource != "persistentvolumeclaims") {
		return nil, errors.New("unexpected metadata resource")
	}
	return &metav1.PartialObjectMetadataList{Items: []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: resource.Resource + "-one"}}}}, nil
}

func (f *retentionListFake) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if f.err != nil {
		return f.err
	}
	switch typed := list.(type) {
	case *v1alpha1.AgentRunList:
		if f.run != nil {
			typed.Items = []v1alpha1.AgentRun{*f.run}
		}
		typed.Continue = f.continueToken
	case *corev1.SecretList, *corev1.PersistentVolumeClaimList:
		return errors.New("typed Secret/PVC lists are forbidden; use metadata API")
	default:
		return errors.New("unexpected list type")
	}
	return nil
}

func apiRunForInventory() *v1alpha1.AgentRun {
	completed := metav1.NewTime(retentionNow.Add(-30 * 24 * time.Hour))
	return &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-name", Namespace: "agw-runs", UID: types.UID("run-uid")}, Status: v1alpha1.AgentRunStatus{
		Phase: v1alpha1.PhaseSucceeded, SpecDigest: retentionDigest, CompletedAt: &completed,
		EventStreamRef: &v1alpha1.ArtifactRef{URI: "s3://bucket/agents-gateway/v3/runs/run-uid/events/manifest.json", Digest: retentionDigest},
		WorkSandboxRef: &v1alpha1.ChildRef{Name: "work", Kind: SandboxKind, UID: "sandbox-uid", Role: WorkRole, SpecDigest: retentionDigest, PlanFingerprint: retentionDigest},
	}}
}
