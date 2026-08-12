package retention

import (
	"errors"
	"reflect"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var retentionNow = time.Date(2026, 8, 11, 15, 0, 0, 0, time.UTC)

func TestDefaultPolicyIsConservative(t *testing.T) {
	policy := DefaultPolicy()
	if policy.ArtifactRetentionDays != 14 || policy.WorktreeRetentionDays != 7 || !policy.DryRun {
		t.Fatalf("default policy = %#v", policy)
	}
	if policy.MaxActionsPerPlan != DefaultMaxActionsPerPlan {
		t.Fatalf("default action bound = %d, want %d", policy.MaxActionsPerPlan, DefaultMaxActionsPerPlan)
	}

	if _, err := (Policy{}).Plan(retentionNow, Inventory{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("zero policy error = %v, want ErrInvalidPolicy", err)
	}
	invalid := policy
	invalid.ArtifactRetentionDays = MaxRetentionDays + 1
	if _, err := invalid.Plan(retentionNow, Inventory{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("oversized retention error = %v, want ErrInvalidPolicy", err)
	}
	invalid = policy
	invalid.LedgerRetentionDays = DefaultLedgerRetentionDays - 1
	if _, err := invalid.Plan(retentionNow, Inventory{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("short ledger retention error = %v, want ErrInvalidPolicy", err)
	}
	invalid = policy
	invalid.MaxActionsPerPlan = MaxRetentionActions + 1
	if _, err := invalid.Plan(retentionNow, Inventory{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("unbounded action limit error = %v, want ErrInvalidPolicy", err)
	}
	if _, err := policy.Plan(time.Time{}, Inventory{}); !errors.Is(err, ErrInvalidNow) {
		t.Fatalf("zero clock error = %v, want ErrInvalidNow", err)
	}
}

func TestTerminalSecretCleanupRequiresExactOwnershipAndLabels(t *testing.T) {
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-time.Hour))
	valid := secretResource(run, runOwner(run), true)

	wrongOwner := valid
	wrongOwner.Name = "wrong-owner-secret"
	wrongOwner.OwnerReferences = []OwnerReference{runOwnerWithUID(run, "other-run")}

	missingBlock := valid
	missingBlock.Name = "missing-block-secret"
	owner := runOwner(run)
	owner.BlockOwnerDeletion = nil
	missingBlock.OwnerReferences = []OwnerReference{owner}

	ambiguous := valid
	ambiguous.Name = "ambiguous-secret"
	second := runOwner(run)
	second.Name = "other-run"
	second.UID = types.UID("other-run")
	second.Controller = boolPtr(true)
	ambiguous.OwnerReferences = []OwnerReference{runOwner(run), second}

	wrongLabel := valid
	wrongLabel.Name = "wrong-label-secret"
	wrongLabel.Namespace = "other-ns"

	plan, err := DefaultPolicy().Plan(retentionNow, Inventory{
		Runs:      []Run{run},
		Resources: []Resource{valid, wrongOwner, missingBlock, ambiguous, wrongLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Target.Name != valid.Name || plan.Actions[0].Mode != ActionWouldDelete {
		t.Fatalf("actions = %#v, want one dry-run action for %s", plan.Actions, valid.Name)
	}
	assertSkipped(t, plan, "wrong-owner-secret", SkipOwnerMismatch)
	assertSkipped(t, plan, "missing-block-secret", SkipOwnerMismatch)
	assertSkipped(t, plan, "ambiguous-secret", SkipOwnerAmbiguous)
	assertSkipped(t, plan, "wrong-label-secret", SkipRunLabelMismatch)
}

func TestUnknownEffectPreservesAllResourcesAndEvidence(t *testing.T) {
	run := completedRun(v1alpha1.PhaseUnknownEffect, retentionNow.Add(-30*24*time.Hour))
	child := Child{Kind: SandboxKind, Name: "agw-work", UID: "sandbox-uid", Role: WorkRole, SpecDigest: retentionDigest}
	run.Children = []Child{child}
	secret := secretResource(run, runOwner(run), true)
	pvc := pvcResource(run, child, true)
	artifact := oldArtifact(run, "events/completion.jsonl", 10)

	plan, err := DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{secret, pvc}, Artifacts: []Artifact{artifact}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("unknown-effect actions = %#v, want no actions", plan.Actions)
	}
	assertSkippedArtifact(t, plan, artifact.Key, SkipUnknownEffect)
	assertSkipped(t, plan, pvc.Name, SkipUnknownEffect)
}

func TestWorktreePVCRequiresFreshSandboxUIDFenceAndAge(t *testing.T) {
	child := Child{Kind: SandboxKind, Name: "agw-work", UID: "sandbox-uid", Role: WorkRole, SpecDigest: retentionDigest}
	run := completedRun(v1alpha1.PhaseRejected, retentionNow.Add(-8*24*time.Hour))
	run.Children = []Child{child}
	valid := pvcResource(run, child, true)
	plan, err := DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{valid}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Target.UID != valid.UID || plan.Actions[0].Target.Kind != PersistentVolumeKind {
		t.Fatalf("PVC actions = %#v", plan.Actions)
	}

	jobChild := Child{Kind: JobKind, Name: "agw-work", UID: "job-uid", Role: WorkRole, SpecDigest: retentionDigest}
	jobRun := completedRun(v1alpha1.PhaseRejected, retentionNow.Add(-8*24*time.Hour))
	jobRun.Children = []Child{jobChild}
	jobPVC := pvcResource(jobRun, jobChild, true)
	jobPVC.OwnerReferences = []OwnerReference{runOwner(jobRun)}
	plan, err = DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{jobRun}, Resources: []Resource{jobPVC}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Target.UID != jobPVC.UID {
		t.Fatalf("Job-backed PVC actions = %#v", plan.Actions)
	}

	tooYoung := run
	tooYoung.CompletedAt = retentionNow.Add(-6 * 24 * time.Hour)
	plan, err = DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{tooYoung}, Resources: []Resource{valid}})
	if err != nil {
		t.Fatal(err)
	}
	assertSkipped(t, plan, valid.Name, SkipRetentionWindow)

	foreign := valid
	foreign.Name = "foreign-pvc"
	foreign.OwnerReferences = []OwnerReference{sandboxOwner(child, "foreign-sandbox")}
	plan, err = DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{foreign}})
	if err != nil {
		t.Fatal(err)
	}
	assertSkipped(t, plan, foreign.Name, SkipOwnerMismatch)

	unowned := valid
	unowned.Name = "existing-claim"
	unowned.OwnerReferences = nil
	plan, err = DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{unowned}})
	if err != nil {
		t.Fatal(err)
	}
	assertSkipped(t, plan, unowned.Name, SkipOwnerMismatch)
}

func TestArtifactRetentionUsesExactPerRunPrefix(t *testing.T) {
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-15*24*time.Hour))
	valid := oldArtifact(run, "events/completion.jsonl", 10)
	young := valid
	young.Key = "agents-gateway/v3/runs/run-uid/events/young.jsonl"
	young.CreatedAt = retentionNow.Add(-time.Hour)
	wrongPrefix := valid
	wrongPrefix.Key = "agents-gateway/v3/runs/other-run/events/completion.jsonl"
	traversal := valid
	traversal.Key = "agents-gateway/v3/runs/run-uid/../other.jsonl"
	wrongRun := valid
	wrongRun.Key = "agents-gateway/v3/runs/other-run/events/wrong-run.jsonl"
	missingCreated := valid
	missingCreated.Key = "agents-gateway/v3/runs/run-uid/events/missing-created.jsonl"
	missingCreated.CreatedAt = time.Time{}

	plan, err := DefaultPolicy().Plan(retentionNow, Inventory{Runs: []Run{run}, Artifacts: []Artifact{valid, young, wrongPrefix, traversal, wrongRun, missingCreated}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Target.Kind != ObjectStoreKind || plan.Actions[0].Target.ObjectKey != valid.Key {
		t.Fatalf("artifact actions = %#v, want only %q", plan.Actions, valid.Key)
	}
	assertSkippedArtifact(t, plan, young.Key, SkipArtifactYoung)
	assertSkippedArtifact(t, plan, wrongPrefix.Key, SkipArtifactKeyMismatch)
	assertSkippedArtifact(t, plan, traversal.Key, SkipArtifactKeyInvalid)
	assertSkippedArtifact(t, plan, wrongRun.Key, SkipArtifactKeyMismatch)
	assertSkippedArtifact(t, plan, missingCreated.Key, SkipArtifactCreatedMissing)
}

func TestPlanIsDeterministicAndBounded(t *testing.T) {
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-30*24*time.Hour))
	child := Child{Kind: SandboxKind, Name: "agw-work", UID: "sandbox-uid", Role: WorkRole, SpecDigest: retentionDigest}
	run.Children = []Child{child}
	secret := secretResource(run, runOwner(run), true)
	pvc := pvcResource(run, child, true)
	artifact := oldArtifact(run, "events/completion.jsonl", 10)
	policy := DefaultPolicy()
	policy.DryRun = false
	first, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{pvc, secret}, Artifacts: []Artifact{artifact}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{secret, pvc}, Artifacts: []Artifact{artifact}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("plans differ for reordered inventory:\nfirst=%#v\nsecond=%#v", first, second)
	}
	for _, action := range first.Actions {
		if action.Mode != ActionDelete {
			t.Fatalf("live policy action mode = %q", action.Mode)
		}
	}

	limited := policy
	limited.MaxActionsPerPlan = 1
	if _, err := limited.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: []Resource{secret, pvc}, Artifacts: []Artifact{artifact}}); !errors.Is(err, ErrActionLimit) {
		t.Fatalf("bounded plan error = %v, want ErrActionLimit", err)
	}
}

func TestFromAgentRunCopiesBoundedIdentity(t *testing.T) {
	completed := metav1.NewTime(retentionNow.Add(-time.Hour))
	run := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-name", Namespace: "agw-runs", UID: types.UID("run-uid")}, Status: v1alpha1.AgentRunStatus{
		Phase: v1alpha1.PhaseSucceeded, SpecDigest: retentionDigest, CompletedAt: &completed,
		WorkSandboxRef: &v1alpha1.ChildRef{Name: "work", Kind: SandboxKind, UID: "sandbox-uid", Role: WorkRole, SpecDigest: retentionDigest, PlanFingerprint: retentionDigest},
		ContextPackRef: &v1alpha1.ArtifactRef{URI: "s3://bucket/agents-gateway/v3/runs/run-uid/context/pack.json", Digest: retentionDigest},
	}}
	got, err := FromAgentRun(run)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.WorkSandboxRef.Name = "mutated"
	if got.Name != run.Name || len(got.Children) != 1 || got.Children[0].Name != "work" || len(got.RequiredObjectKeys) != 1 || got.RequiredObjectKeys[0] != "agents-gateway/v3/runs/run-uid/context/pack.json" {
		t.Fatalf("copied run = %#v", got)
	}
}

func completedRun(phase v1alpha1.Phase, completed time.Time) Run {
	return Run{Namespace: "agw-runs", Name: "run-name", UID: "run-uid", SpecDigest: retentionDigest, Phase: phase, CompletedAt: completed, EvidenceFlushed: true, LedgerFlushed: true}
}

func secretResource(run Run, owner OwnerReference, valid bool) Resource {
	labels := map[string]string{RunUIDLabelKey: run.UID, ManagedByLabelKey: RunSecretManagedBy}
	if !valid {
		labels = nil
	}
	return Resource{Kind: SecretKind, Namespace: run.Namespace, Name: "run-secret", UID: "secret-uid", SpecDigest: run.SpecDigest, Labels: labels, Annotations: map[string]string{SpecDigestAnnotation: run.SpecDigest}, OwnerReferences: []OwnerReference{owner}}
}

func pvcResource(run Run, child Child, valid bool) Resource {
	labels := map[string]string{RunUIDLabelKey: run.UID, RoleLabelKey: WorkRole}
	if !valid {
		labels = nil
	}
	return Resource{Kind: PersistentVolumeKind, Namespace: run.Namespace, Name: "workspace-claim", UID: "pvc-uid", SpecDigest: child.SpecDigest, Labels: labels, OwnerReferences: []OwnerReference{sandboxOwner(child, "")}}
}

func oldArtifact(run Run, suffix string, size int64) Artifact {
	return Artifact{RunUID: run.UID, Key: "agents-gateway/v3/runs/" + run.UID + "/" + suffix, ETag: "\"etag\"", CreatedAt: retentionNow.Add(-20 * 24 * time.Hour), SizeBytes: size}
}

const retentionDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func runOwner(run Run) OwnerReference {
	return runOwnerWithUID(run, run.UID)
}

func runOwnerWithUID(run Run, uid string) OwnerReference {
	return OwnerReference{APIVersion: AgentRunAPIVersion, Kind: AgentRunKind, Name: run.Name, UID: types.UID(uid), Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}
}

func sandboxOwner(child Child, uid string) OwnerReference {
	if uid == "" {
		uid = child.UID
	}
	return OwnerReference{APIVersion: SandboxAPIVersion, Kind: SandboxKind, Name: child.Name, UID: types.UID(uid), Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}
}

func boolPtr(value bool) *bool { return &value }

func assertSkipped(t *testing.T, plan Plan, name string, reason SkipReason) {
	t.Helper()
	for _, skip := range plan.Skipped {
		if skip.Name == name && skip.Reason == reason {
			return
		}
	}
	t.Fatalf("no skip for %s with reason %q in %#v", name, reason, plan.Skipped)
}

func assertSkippedArtifact(t *testing.T, plan Plan, key string, reason SkipReason) {
	t.Helper()
	for _, skip := range plan.Skipped {
		if skip.ObjectKey == key && skip.Reason == reason {
			return
		}
	}
	t.Fatalf("no artifact skip for %s with reason %q in %#v", key, reason, plan.Skipped)
}
