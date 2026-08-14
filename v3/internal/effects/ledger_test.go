package effects

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	fail    error
}

type providerNotFound struct{}

func (providerNotFound) Error() string  { return "provider object not found" }
func (providerNotFound) NotFound() bool { return true }

type providerMarkerStore struct {
	objects map[string][]byte
}

func (s *providerMarkerStore) Create(_ context.Context, key string, body []byte, _ string) (bool, error) {
	if _, exists := s.objects[key]; exists {
		return false, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, nil
}

func (s *providerMarkerStore) Get(_ context.Context, key string) ([]byte, error) {
	value, exists := s.objects[key]
	if !exists {
		return nil, fmt.Errorf("get object: %w", providerNotFound{})
	}
	return append([]byte(nil), value...), nil
}

func TestProviderNotFoundMarkerAllowsFirstClaim(t *testing.T) {
	store := &providerMarkerStore{objects: map[string][]byte{}}
	ledger, err := New(store, "effects", func() time.Time { return time.Unix(1, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	claim := Claim{EffectKey: digest("a"), RequestDigest: digest("b"), Operation: "publish-pr", RunUID: "run-uid"}
	decision, err := ledger.Claim(context.Background(), claim)
	if err != nil || !decision.Execute || decision.Unknown {
		t.Fatalf("provider not-found claim = %#v, %v", decision, err)
	}
}

func (s *memoryStore) Create(_ context.Context, key string, body []byte, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	if _, exists := s.objects[key]; exists {
		return false, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.objects[key]
	if !exists {
		return nil, ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func TestClaimCommitAndReplayAreIdempotent(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	ledger, err := New(store, "agw/effects", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	effectKey := digest("a")
	requestDigest := digest("b")
	claim := Claim{EffectKey: effectKey, RequestDigest: requestDigest, Operation: "publish-pr", RunUID: "run-uid"}
	decision, err := ledger.Claim(context.Background(), claim)
	if err != nil || !decision.Execute || decision.Unknown {
		t.Fatalf("first claim = %#v, %v", decision, err)
	}
	outcome := Outcome{EffectKey: effectKey, RequestDigest: requestDigest, State: OutcomeSucceeded, ResultDigest: digest("c"), ResultRef: "github:pr:1"}
	if err := ledger.Commit(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(context.Background(), outcome); err != nil {
		t.Fatalf("identical outcome replay failed: %v", err)
	}
	decision, err = ledger.Claim(context.Background(), claim)
	if err != nil || decision.Execute || decision.Unknown {
		t.Fatalf("completed claim replay = %#v, %v", decision, err)
	}
}

func TestExistingIncompleteClaimBecomesUnknownAndCannotExecuteAgain(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	ledger, _ := New(store, "effects", func() time.Time { return time.Now().UTC() })
	claim := Claim{EffectKey: digest("d"), RequestDigest: digest("e"), Operation: "publish-pr", RunUID: "run-uid"}
	if decision, err := ledger.Claim(context.Background(), claim); err != nil || !decision.Execute {
		t.Fatalf("first claim = %#v, %v", decision, err)
	}
	decision, err := ledger.Claim(context.Background(), claim)
	if !errors.Is(err, ErrUnknown) || decision.Execute || !decision.Unknown {
		t.Fatalf("incomplete replay = %#v, %v", decision, err)
	}
}

func TestConflictsAndAmbiguousCreatesFailClosed(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	ledger, _ := New(store, "effects", func() time.Time { return time.Now().UTC() })
	effectKey := digest("f")
	first := Claim{EffectKey: effectKey, RequestDigest: digest("1"), Operation: "publish-pr", RunUID: "run-uid"}
	if _, err := ledger.Claim(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.RequestDigest = digest("2")
	if _, err := ledger.Claim(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("request conflict = %v", err)
	}
	store.fail = errors.New("timeout with ambiguous write")
	other := Claim{EffectKey: digest("3"), RequestDigest: digest("4"), Operation: "publish-pr", RunUID: "run-uid"}
	if decision, err := ledger.Claim(context.Background(), other); err == nil || decision.Execute {
		t.Fatalf("ambiguous create permitted execution: %#v, %v", decision, err)
	}
}

func TestRetirementTombstonePermanentlyFencesReplay(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	ledger, err := New(store, "effects", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claim := Claim{EffectKey: digest("1"), RequestDigest: digest("2"), Operation: "publish-pr", RunUID: "run-uid"}
	if decision, err := ledger.Claim(context.Background(), claim); err != nil || !decision.Execute {
		t.Fatalf("claim = %#v, %v", decision, err)
	}
	outcome := Outcome{EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, State: OutcomeSucceeded, ResultDigest: digest("3")}
	if err := ledger.Commit(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}
	fence, err := EncodeTombstone(Tombstone{EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, Operation: claim.Operation, RunUID: claim.RunUID, State: OutcomeSucceeded, RetiredAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.Create(context.Background(), ledger.tombstoneKey(claim.EffectKey), fence, "application/json"); err != nil || !created {
		t.Fatalf("tombstone create = %t, %v", created, err)
	}
	if decision, err := ledger.Claim(context.Background(), claim); !errors.Is(err, ErrUnknown) || decision.Execute || !decision.Unknown {
		t.Fatalf("fenced claim = %#v, %v", decision, err)
	}
	if err := ledger.Commit(context.Background(), outcome); !errors.Is(err, ErrUnknown) {
		t.Fatalf("fenced commit = %v, want ErrUnknown", err)
	}
}

func TestReplayCommitRequiresTheOriginalRunBinding(t *testing.T) {
	for _, runUID := range []string{"other-run", ""} {
		t.Run(fmt.Sprintf("runUID=%q", runUID), func(t *testing.T) {
			store := &memoryStore{objects: map[string][]byte{}}
			now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
			ledger, err := New(store, "effects", func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			claim := Claim{EffectKey: digest("a"), RequestDigest: digest("b"), Operation: "publish-pr", RunUID: "run-uid"}
			if decision, err := ledger.Claim(context.Background(), claim); err != nil || !decision.Execute {
				t.Fatalf("claim = %#v, %v", decision, err)
			}
			outcome := Outcome{EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, State: OutcomeSucceeded, ResultDigest: digest("c"), ResultRef: "github:pr:1"}
			if err := ledger.Commit(context.Background(), outcome); err != nil {
				t.Fatal(err)
			}

			stored := outcome
			stored.SchemaVersion = SchemaVersion
			stored.RunUID = runUID
			stored.RecordedAt = now
			body, err := canonicalJSON(stored)
			if err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			store.objects[ledger.outcomeKey(claim.EffectKey)] = body
			store.mu.Unlock()

			if err := ledger.Commit(context.Background(), outcome); !errors.Is(err, ErrConflict) {
				t.Fatalf("replay with RunUID %q = %v, want ErrConflict", runUID, err)
			}
		})
	}
}

func TestConcurrentClaimsGrantExecutionExactlyOnce(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	ledger, err := New(store, "effects", func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatal(err)
	}
	claim := Claim{EffectKey: digest("d"), RequestDigest: digest("e"), Operation: "publish-pr", RunUID: "run-uid"}
	const callers = 32
	start := make(chan struct{})
	decisions := make(chan ClaimDecision, callers)
	errorsCh := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			<-start
			decision, err := ledger.Claim(context.Background(), claim)
			decisions <- decision
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(decisions)
	close(errorsCh)

	executions := 0
	for decision := range decisions {
		if decision.Execute {
			executions++
		}
	}
	for err := range errorsCh {
		if err != nil && !errors.Is(err, ErrUnknown) {
			t.Fatalf("concurrent claim error = %v", err)
		}
	}
	if executions != 1 {
		t.Fatalf("concurrent claims granted execution %d times, want exactly once", executions)
	}
}

func TestUnknownOutcomeReplayRemainsUnknown(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	ledger, err := New(store, "effects", func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatal(err)
	}
	claim := Claim{EffectKey: digest("6"), RequestDigest: digest("7"), Operation: "publish-pr", RunUID: "run-uid"}
	if decision, err := ledger.Claim(context.Background(), claim); err != nil || !decision.Execute {
		t.Fatalf("claim = %#v, %v", decision, err)
	}
	if err := ledger.Commit(context.Background(), Outcome{EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, State: OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	decision, err := ledger.Claim(context.Background(), claim)
	if !errors.Is(err, ErrUnknown) || decision.Execute || !decision.Unknown {
		t.Fatalf("unknown outcome replay = %#v, %v", decision, err)
	}
}

func TestClaimAndCommitRecheckFenceAfterOutcomeRead(t *testing.T) {
	for _, operation := range []string{"claim", "commit"} {
		t.Run(operation, func(t *testing.T) {
			base := &memoryStore{objects: map[string][]byte{}}
			now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
			initial, err := New(base, "effects", func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			claim := Claim{EffectKey: digest("f"), RequestDigest: digest("1"), Operation: "publish-pr", RunUID: "run-uid"}
			if decision, err := initial.Claim(context.Background(), claim); err != nil || !decision.Execute {
				t.Fatalf("claim = %#v, %v", decision, err)
			}
			outcome := Outcome{EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, State: OutcomeSucceeded, ResultDigest: digest("2"), ResultRef: "github:pr:2"}
			if err := initial.Commit(context.Background(), outcome); err != nil {
				t.Fatal(err)
			}
			fence, err := EncodeTombstone(Tombstone{EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, Operation: claim.Operation, RunUID: claim.RunUID, State: OutcomeSucceeded, RetiredAt: now})
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &fenceAfterOutcomeReadStore{base: base, outcomeKey: initial.outcomeKey(claim.EffectKey), tombstoneKey: initial.tombstoneKey(claim.EffectKey), tombstoneBody: fence}
			ledger, err := New(wrapped, "effects", func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			if operation == "claim" {
				decision, err := ledger.Claim(context.Background(), claim)
				if !errors.Is(err, ErrUnknown) || decision.Execute || !decision.Unknown {
					t.Fatalf("fenced replay claim = %#v, %v", decision, err)
				}
			} else if err := ledger.Commit(context.Background(), outcome); !errors.Is(err, ErrUnknown) {
				t.Fatalf("fenced replay commit = %v, want ErrUnknown", err)
			}
		})
	}
}

type fenceAfterOutcomeReadStore struct {
	base          *memoryStore
	outcomeKey    string
	tombstoneKey  string
	tombstoneBody []byte
	once          sync.Once
}

func (s *fenceAfterOutcomeReadStore) Create(ctx context.Context, key string, body []byte, contentType string) (bool, error) {
	return s.base.Create(ctx, key, body, contentType)
}

func (s *fenceAfterOutcomeReadStore) Get(ctx context.Context, key string) ([]byte, error) {
	body, err := s.base.Get(ctx, key)
	if err == nil && key == s.outcomeKey {
		s.once.Do(func() {
			_, _ = s.base.Create(ctx, s.tombstoneKey, s.tombstoneBody, "application/json")
		})
	}
	return body, err
}

func TestLedgerDecodersRejectNonCanonicalBodies(t *testing.T) {
	claim := Claim{SchemaVersion: SchemaVersion, EffectKey: digest("4"), RequestDigest: digest("5"), Operation: "publish-pr", RunUID: "run-uid", CreatedAt: time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)}
	body, err := canonicalJSON(claim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeClaim(append(body, '\n')); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-canonical claim = %v, want ErrCorrupt", err)
	}
	outcome := Outcome{SchemaVersion: SchemaVersion, EffectKey: claim.EffectKey, RequestDigest: claim.RequestDigest, RunUID: claim.RunUID, State: OutcomeSucceeded, RecordedAt: claim.CreatedAt}
	body, err = canonicalJSON(outcome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOutcome(append(body, ' ')); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-canonical outcome = %v, want ErrCorrupt", err)
	}
}

func TestEffectKeyBindsEveryInput(t *testing.T) {
	patch := digest("9")
	first, err := EffectKey("run-uid", "base-sha", patch, "publish/pr")
	if err != nil {
		t.Fatal(err)
	}
	second, err := EffectKey("run-uid", "other-base", patch, "publish/pr")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different immutable inputs produced one effect key")
	}
}

func digest(hexByte string) string {
	value := ""
	for len(value) < 64 {
		value += hexByte
	}
	return "sha256:" + value[:64]
}
