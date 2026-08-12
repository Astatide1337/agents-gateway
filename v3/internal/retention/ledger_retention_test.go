package retention

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
)

func TestLedgerRetentionPlansOnlyAProvenTerminalPair(t *testing.T) {
	policy := DefaultPolicy()
	policy.DryRun = false
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.LedgerPairs) != 1 || len(plan.Actions) != 2 {
		t.Fatalf("plan pairs/actions = %d/%d, want 1/2: %#v", len(plan.LedgerPairs), len(plan.Actions), plan)
	}
	if plan.LedgerPairs[0].EffectKey != run.EffectKey || plan.Actions[0].PairKey != run.EffectKey || plan.Actions[1].PairKey != run.EffectKey {
		t.Fatalf("pair identity was not propagated: %#v", plan)
	}

	unknown := cloneLedgerObjects(objects)
	unknown[1].Body = canonicalLedgerBody(t, effects.Outcome{SchemaVersion: effects.SchemaVersion, EffectKey: run.EffectKey, RequestDigest: run.PatchDigest, RunUID: run.UID, State: effects.OutcomeUnknown, RecordedAt: retentionNow.Add(-31 * 24 * time.Hour)})
	plan, err = policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: unknown})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerUnknown) {
		t.Fatalf("unknown outcome plan = %#v, error = %v", plan, err)
	}

	withArtifact := cloneLedgerObjects(objects)
	plan, err = policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Artifacts: []Artifact{{RunUID: run.UID, Key: runPrefix(policy.ObjectPrefix, run.UID) + "events/manifest.json"}}, Ledger: withArtifact})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerArtifacts) {
		t.Fatalf("retained artifact plan = %#v, error = %v", plan, err)
	}

	missing := []LedgerObject{objects[0]}
	plan, err = policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: missing})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerIncomplete) {
		t.Fatalf("incomplete pair plan = %#v, error = %v", plan, err)
	}
}

func TestLedgerRetentionNeverRetiresBeforeThirtyDays(t *testing.T) {
	policy := DefaultPolicy()
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	run.CompletedAt = retentionNow.Add(-29 * 24 * time.Hour)
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerYoung) {
		t.Fatalf("29-day ledger plan = %#v, error = %v, want SkipLedgerYoung", plan, err)
	}
}

func TestLedgerRetentionFailsClosedOnMalformedDuplicateAndMismatchedInventory(t *testing.T) {
	policy := DefaultPolicy()
	run, objects := provenLedgerRun(t, policy, effects.OutcomeFailed)

	malformed := append([]LedgerObject(nil), objects...)
	malformed[0].Body = append([]byte(nil), malformed[0].Body...)
	malformed[0].Body = append(malformed[0].Body, '\n')
	if _, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: malformed}); !errors.Is(err, ErrLedgerInventory) {
		t.Fatalf("non-canonical inventory error = %v, want ErrLedgerInventory", err)
	}

	duplicate := append(append([]LedgerObject(nil), objects...), objects[0])
	if _, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: duplicate}); !errors.Is(err, ErrLedgerInventory) {
		t.Fatalf("duplicate inventory error = %v, want ErrLedgerInventory", err)
	}

	mismatch := append([]LedgerObject(nil), objects...)
	mismatch[1].Body = canonicalLedgerBody(t, effects.Outcome{SchemaVersion: effects.SchemaVersion, EffectKey: run.EffectKey, RequestDigest: run.PatchDigest, RunUID: "other-run", State: effects.OutcomeSucceeded, RecordedAt: retentionNow.Add(-31 * 24 * time.Hour)})
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: mismatch})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerIdentityMismatch) {
		t.Fatalf("mismatched identity plan = %#v, error = %v", plan, err)
	}

	oversized := append([]LedgerObject(nil), objects...)
	oversized[0].Body = make([]byte, effects.MaxLedgerBodyBytes+1)
	if _, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: oversized}); !errors.Is(err, ErrLedgerInventory) {
		t.Fatalf("oversized inventory error = %v, want ErrLedgerInventory", err)
	}

	missingETag := cloneLedgerObjects(objects)
	missingETag[0].ETag = ""
	plan, err = policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: missingETag})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerETagMissing) {
		t.Fatalf("missing ETag plan = %#v, error = %v", plan, err)
	}

	conflictingTombstone := cloneLedgerObjects(objects)
	tombstone := effects.Tombstone{SchemaVersion: effects.SchemaVersion, EffectKey: run.EffectKey, RequestDigest: run.PatchDigest, Operation: "publish/pr", RunUID: "different-run", State: effects.OutcomeFailed, RetiredAt: retentionNow.Add(-31 * 24 * time.Hour)}
	conflictingTombstone = append(conflictingTombstone, LedgerObject{Key: policy.ObjectPrefix + "/" + policy.LedgerPrefix + "/tombstones/" + run.EffectKey[len(canonical.DigestPrefix):] + ".json", ETag: "\"tombstone-etag\"", Body: canonicalLedgerBody(t, tombstone)})
	plan, err = policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: conflictingTombstone})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerTombstone) {
		t.Fatalf("conflicting tombstone plan = %#v, error = %v", plan, err)
	}
}

func TestTombstoneOnlyInventoryIsAStableNoop(t *testing.T) {
	policy := DefaultPolicy()
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	claim, err := effects.DecodeClaim(objects[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := effects.DecodeOutcome(objects[1].Body)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := effects.Tombstone{SchemaVersion: effects.SchemaVersion, EffectKey: run.EffectKey, RequestDigest: claim.RequestDigest, Operation: claim.Operation, RunUID: run.UID, State: outcome.State, RetiredAt: retentionNow.Add(-31 * 24 * time.Hour)}
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: []LedgerObject{{Key: policy.ObjectPrefix + "/" + policy.LedgerPrefix + "/tombstones/" + run.EffectKey[len(canonical.DigestPrefix):] + ".json", ETag: "\"tombstone-etag\"", Body: canonicalLedgerBody(t, tombstone)}}})
	if err != nil || len(plan.Actions) != 0 || len(plan.LedgerPairs) != 0 {
		t.Fatalf("tombstone-only inventory = %#v, error = %v, want stable no-op", plan, err)
	}
}

func TestLedgerRetentionRejectsForeignEvidenceReferences(t *testing.T) {
	policy := DefaultPolicy()
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	run.RequiredObjectKeys = []string{policy.ObjectPrefix + "/shared/evidence.json"}
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipArtifactKeyMismatch) {
		t.Fatalf("foreign evidence reference plan = %#v, error = %v", plan, err)
	}
}

func TestLedgerRetentionRequiresWorkspaceToBeGone(t *testing.T) {
	policy := DefaultPolicy()
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	resources := []Resource{{Kind: PersistentVolumeKind, Namespace: run.Namespace, Name: "workspace", UID: "pvc-uid", Labels: map[string]string{RunUIDLabelKey: run.UID}}}
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Resources: resources, Ledger: objects})
	if err != nil || len(plan.Actions) != 0 || !hasLedgerSkip(plan, SkipLedgerWorkspace) {
		t.Fatalf("retained workspace plan = %#v, error = %v", plan, err)
	}
}

func TestApplyLedgerPairFencesThenDeletesOutcomeBeforeClaim(t *testing.T) {
	policy := DefaultPolicy()
	policy.DryRun = false
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil {
		t.Fatal(err)
	}
	store := &ledgerRetentionStore{bodies: make(map[string][]byte)}
	for _, object := range objects {
		store.bodies[object.Key] = append([]byte(nil), object.Body...)
	}
	report, err := Apply(context.Background(), plan, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: true, LifecycleAttested: true})
	if err != nil || report.Deleted != 2 || len(store.deletes) != 2 {
		t.Fatalf("pair apply = %#v, error = %v, deletes = %#v", report, err, store.deletes)
	}
	wantOrder := []string{plan.LedgerPairs[0].OutcomeKey, plan.LedgerPairs[0].ClaimKey}
	if !reflect.DeepEqual(store.deletes, wantOrder) {
		t.Fatalf("delete order = %#v, want outcome then claim %#v", store.deletes, wantOrder)
	}
	if !reflect.DeepEqual(store.etags, []string{plan.LedgerPairs[0].OutcomeETag, plan.LedgerPairs[0].ClaimETag}) {
		t.Fatalf("delete ETags = %#v, want outcome then claim ETags", store.etags)
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].TombstoneKey]; !ok {
		t.Fatal("permanent tombstone was not retained")
	}
}

func TestApplyLedgerPairNeverDeletesBeforeAmbiguousTombstoneCreate(t *testing.T) {
	policy := DefaultPolicy()
	policy.DryRun = false
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil {
		t.Fatal(err)
	}
	store := &ledgerRetentionStore{bodies: make(map[string][]byte), createErr: errors.New("ambiguous tombstone write")}
	for _, object := range objects {
		store.bodies[object.Key] = append([]byte(nil), object.Body...)
	}
	report, err := Apply(context.Background(), plan, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: true, LifecycleAttested: true})
	if err == nil || report.Failed != 2 || len(store.deletes) != 0 {
		t.Fatalf("ambiguous tombstone apply = %#v, error = %v, deletes = %#v", report, err, store.deletes)
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].ClaimKey]; !ok {
		t.Fatal("claim was deleted after ambiguous tombstone create")
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].OutcomeKey]; !ok {
		t.Fatal("outcome was deleted after ambiguous tombstone create")
	}
}

func TestApplyLedgerPairIsRetrySafeAcrossClaimDeleteFailure(t *testing.T) {
	policy := DefaultPolicy()
	policy.DryRun = false
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil {
		t.Fatal(err)
	}
	store := &ledgerRetentionStore{bodies: make(map[string][]byte), failKey: plan.LedgerPairs[0].ClaimKey}
	for _, object := range objects {
		store.bodies[object.Key] = append([]byte(nil), object.Body...)
	}
	first, err := Apply(context.Background(), plan, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: true, LifecycleAttested: true})
	if err == nil || first.Failed == 0 {
		t.Fatalf("first crash-boundary apply = %#v, error = %v, want claim failure", first, err)
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].TombstoneKey]; !ok {
		t.Fatal("tombstone disappeared after claim-delete failure")
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].ClaimKey]; !ok {
		t.Fatal("claim unexpectedly disappeared after injected failure")
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].OutcomeKey]; ok {
		t.Fatal("outcome remained after the first retirement step")
	}

	store.failKey = ""
	second, err := Apply(context.Background(), plan, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: true, LifecycleAttested: true})
	if err != nil || second.Deleted != 2 {
		t.Fatalf("retry apply = %#v, error = %v, want idempotent completion", second, err)
	}
	if _, ok := store.bodies[plan.LedgerPairs[0].ClaimKey]; ok {
		t.Fatal("claim remained after retry")
	}
}

func TestApplyRejectsUnpairedAndDuplicateLedgerPlansAndActionOverflow(t *testing.T) {
	policy := DefaultPolicy()
	policy.DryRun = false
	run, objects := provenLedgerRun(t, policy, effects.OutcomeSucceeded)
	plan, err := policy.Plan(retentionNow, Inventory{Runs: []Run{run}, Ledger: objects})
	if err != nil {
		t.Fatal(err)
	}
	store := &ledgerRetentionStore{bodies: make(map[string][]byte)}
	for _, object := range objects {
		store.bodies[object.Key] = append([]byte(nil), object.Body...)
	}

	unpaired := plan
	unpaired.Actions = append(append([]Action(nil), plan.Actions...), Action{Mode: ActionDelete, PairKey: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Role: "ledger-claim"})
	if _, err := Apply(context.Background(), unpaired, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: true, LifecycleAttested: true}); !errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("unpaired ledger action error = %v, want ErrUnsupportedAction", err)
	}

	duplicate := plan
	duplicate.LedgerPairs = append(append([]LedgerPairPlan(nil), plan.LedgerPairs...), plan.LedgerPairs[0])
	if _, err := Apply(context.Background(), duplicate, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: true, LifecycleAttested: true}); !errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("duplicate ledger pair error = %v, want ErrUnsupportedAction", err)
	}

	overflow := Plan{Actions: make([]Action, DefaultMaxActionsPerPlan+1)}
	if _, err := Apply(context.Background(), overflow, &retentionKubeDeleteFake{}, store, ApplierConfig{ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, Enforce: false, DryRun: true}); !errors.Is(err, ErrActionLimit) {
		t.Fatalf("overflow apply error = %v, want ErrActionLimit", err)
	}
}

type ledgerRetentionStore struct {
	bodies    map[string][]byte
	deletes   []string
	etags     []string
	failKey   string
	createErr error
}

func (s *ledgerRetentionStore) List(context.Context, string, int) ([]ObjectInfo, bool, error) {
	return nil, true, nil
}

func (s *ledgerRetentionStore) Delete(_ context.Context, key, etag string) error {
	s.deletes = append(s.deletes, key)
	s.etags = append(s.etags, etag)
	if key == s.failKey {
		s.failKey = ""
		return errors.New("injected crash after outcome retirement")
	}
	if _, ok := s.bodies[key]; !ok {
		return ErrObjectNotFound
	}
	delete(s.bodies, key)
	return nil
}

func (s *ledgerRetentionStore) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s.bodies[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), body...), nil
}

func (s *ledgerRetentionStore) Create(_ context.Context, key string, body []byte, _ string) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	if _, ok := s.bodies[key]; ok {
		return false, nil
	}
	s.bodies[key] = append([]byte(nil), body...)
	return true, nil
}

func provenLedgerRun(t *testing.T, policy Policy, state effects.OutcomeState) (Run, []LedgerObject) {
	t.Helper()
	run := completedRun(v1alpha1.PhaseSucceeded, retentionNow.Add(-31*24*time.Hour))
	run.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run.PatchDigest = retentionDigest
	effectKey, err := effects.EffectKey(run.UID, run.BaseSHA, run.PatchDigest, "publish/pr")
	if err != nil {
		t.Fatal(err)
	}
	run.EffectKey = effectKey
	created := retentionNow.Add(-31 * 24 * time.Hour)
	claim := effects.Claim{SchemaVersion: effects.SchemaVersion, EffectKey: effectKey, RequestDigest: run.PatchDigest, Operation: "publish/pr", RunUID: run.UID, CreatedAt: created}
	outcome := effects.Outcome{SchemaVersion: effects.SchemaVersion, EffectKey: effectKey, RequestDigest: run.PatchDigest, RunUID: run.UID, State: state, RecordedAt: created}
	hex := effectKey[len(canonical.DigestPrefix):]
	root := policy.ObjectPrefix + "/" + policy.LedgerPrefix
	return run, []LedgerObject{
		{Key: root + "/claims/" + hex + ".json", ETag: "\"claim-etag\"", Body: canonicalLedgerBody(t, claim)},
		{Key: root + "/outcomes/" + hex + ".json", ETag: "\"outcome-etag\"", Body: canonicalLedgerBody(t, outcome)},
	}
}

func canonicalLedgerBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := canonical.CanonicalizeResolvedSpec(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func cloneLedgerObjects(objects []LedgerObject) []LedgerObject {
	result := make([]LedgerObject, len(objects))
	for index, object := range objects {
		result[index] = object
		result[index].Body = append([]byte(nil), object.Body...)
	}
	return result
}

func hasLedgerSkip(plan Plan, reason SkipReason) bool {
	for _, skipped := range plan.Skipped {
		if skipped.Reason == reason {
			return true
		}
	}
	return false
}
