package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryStoreTenantAndRevisionBoundaries(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	scope := Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	first, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "fixer", Digest: "sha256:first", AppliedBy: "user", Document: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 {
		t.Fatalf("revision=%d", first.Revision)
	}
	same, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "fixer", Digest: "sha256:first", AppliedBy: "user", Document: []byte(`{"v":1}`)})
	if err != nil || same.Revision != 1 {
		t.Fatalf("idempotent apply: %#v %v", same, err)
	}
	second, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "fixer", Digest: "sha256:second", AppliedBy: "user", Document: []byte(`{"v":2}`)})
	if err != nil || second.Revision != 2 {
		t.Fatalf("second revision: %#v %v", second, err)
	}
	_, err = storage.GetResource(ctx, Scope{OrganizationID: "org-b", ProjectID: "project-a"}, "Agent", "fixer")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant lookup=%v", err)
	}
}

func TestMemoryStorageJSONIdempotencyIsSemanticAndFailClosed(t *testing.T) {
	ctx := context.Background()
	scope := Scope{OrganizationID: "org-json", ProjectID: "project-json"}
	storage := NewMemory()
	firstDocument := []byte(`{"nested":{"value":2},"message":"first"}`)
	if _, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "json", Digest: "sha256:json", Document: firstDocument, AppliedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	replay, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "json", Digest: "sha256:json", Document: []byte(`{"message":"first","nested":{"value":2.0}}`), AppliedBy: "test-replay"})
	if err != nil || replay.Revision != 1 {
		t.Fatalf("semantic resource replay=%#v err=%v", replay, err)
	}
	if _, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "json", Digest: "sha256:json", Document: []byte(`{"nested":{"value":9007199254740992},"message":"first"}`), AppliedBy: "test-conflict"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("arbitrary-precision resource mismatch returned %v, want conflict", err)
	}
	if _, err := storage.ApplyResource(ctx, Resource{Scope: scope, Kind: "Agent", Name: "invalid", Digest: "sha256:invalid", Document: []byte(`{"value":1,"value":2}`), AppliedBy: "test"}); err == nil {
		t.Fatal("duplicate-key resource was accepted")
	}

	if _, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-json", Kind: "AgentRun", DefinitionDigest: "sha256:run", RequestedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	version := ArtifactVersion{
		Scope: scope, ArtifactID: "artifact-json", VersionID: "artifact-json-v1", RunID: "run-json", VersionNumber: 1,
		Document: []byte(`{"value":2,"nested":{"message":"first"}}`), ContentObjectKey: "content/json", SourceObjectKey: "source/json",
	}
	if _, err := storage.PutArtifactVersion(ctx, version); err != nil {
		t.Fatal(err)
	}
	version.Document = []byte(`{"nested":{"message":"first"},"value":2.0}`)
	if _, err := storage.PutArtifactVersion(ctx, version); err != nil {
		t.Fatalf("semantic artifact replay: %v", err)
	}
	version.Document = []byte(`{"nested":{"message":"first"},"value":9007199254740993}`)
	if _, err := storage.PutArtifactVersion(ctx, version); !errors.Is(err, ErrConflict) {
		t.Fatalf("arbitrary-precision artifact mismatch returned %v, want conflict", err)
	}
	claimed, err := storage.Claim(ctx, scope.OrganizationID, scope.ProjectID, "run-json", "effect-json", "sha256:request")
	if err != nil || !claimed {
		t.Fatalf("claim JSON effect claimed=%v err=%v", claimed, err)
	}
	if err := storage.Complete(ctx, scope.OrganizationID, scope.ProjectID, "run-json", "effect-json", "unknown", []byte(`{"value":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := storage.Complete(ctx, scope.OrganizationID, scope.ProjectID, "run-json", "effect-json", "unknown", []byte(`{"value":2.0}`)); err != nil {
		t.Fatalf("semantic effect replay: %v", err)
	}
	if err := storage.Complete(ctx, scope.OrganizationID, scope.ProjectID, "run-json", "effect-json", "unknown", []byte(`{"value":9007199254740993}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("arbitrary-precision effect mismatch returned %v, want conflict", err)
	}
}

func TestMemoryArtifactCatalogListIsBounded(t *testing.T) {
	storage := NewMemory()
	scope := Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	if _, err := storage.CreateRun(context.Background(), Run{
		Scope: scope, ID: "run-a", Kind: "AgentRun", DefinitionDigest: "sha256:test", RequestedBy: "user-a",
	}); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= ArtifactCatalogListLimit+1; index++ {
		id := fmt.Sprintf("version-%04d", index)
		if _, err := storage.PutArtifactVersion(context.Background(), ArtifactVersion{
			Scope: scope, ArtifactID: "artifact-a", VersionID: id, RunID: "run-a",
			VersionNumber: int64(index), Document: []byte(`{}`), ContentObjectKey: "content/" + id, SourceObjectKey: "source/" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := storage.ListArtifactVersions(context.Background(), scope, "artifact-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != ArtifactCatalogListLimit {
		t.Fatalf("artifact catalog length=%d want=%d", len(versions), ArtifactCatalogListLimit)
	}
}

func TestMemoryCollectionsAreTenantScopedAndBounded(t *testing.T) {
	storage := NewMemory()
	ctx := context.Background()
	scope := Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	other := Scope{OrganizationID: "org-b", ProjectID: "project-b"}
	for index, kind := range []string{"Agent", "Workflow", "Runner"} {
		for _, current := range []Scope{scope, other} {
			if _, err := storage.ApplyResource(ctx, Resource{
				Scope: current, Kind: kind, Name: fmt.Sprintf("item-%d", index),
				Digest: fmt.Sprintf("sha256:%064d", index+1), AppliedBy: "test", Document: []byte(`{"kind":"` + kind + `"}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	items, more, err := storage.ListResources(ctx, scope, "Agent,Workflow", Page{Limit: 1})
	if err != nil || len(items) != 1 || !more || items[0].Scope != scope || items[0].Kind == "Runner" {
		t.Fatalf("resource page items=%#v more=%v err=%v", items, more, err)
	}
	items, more, err = storage.ListResources(ctx, scope, "Agent,Workflow", Page{Limit: 2, Offset: 2})
	if err != nil || len(items) != 0 || more {
		t.Fatalf("resource terminal page items=%#v more=%v err=%v", items, more, err)
	}
	if _, _, err := storage.ListRuns(ctx, scope, Page{Limit: MaxPageLimit + 1}); err == nil {
		t.Fatal("oversized run page was accepted")
	}
}

func TestMemoryUsageAndAuditCollectionsRemainScoped(t *testing.T) {
	storage := NewMemory()
	ctx := context.Background()
	scope := Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	if _, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-active", Kind: "AgentRun", DefinitionDigest: "sha256:test", RequestedBy: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-done", Kind: "AgentRun", DefinitionDigest: "sha256:test2", RequestedBy: "tester", Status: "Succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendAudit(ctx, AuditEvent{Scope: scope, PrincipalID: "tester", Action: "read", ResourceType: "http", ResourceID: "runs", Decision: "allowed", Metadata: []byte(`{"token":"hidden"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendAudit(ctx, AuditEvent{Scope: Scope{OrganizationID: "org-b", ProjectID: "project-b"}, PrincipalID: "other", Action: "read", ResourceType: "http", ResourceID: "runs", Decision: "allowed"}); err != nil {
		t.Fatal(err)
	}
	usage, err := storage.GetUsage(ctx, scope)
	if err != nil || usage.TotalRuns != 2 || usage.ActiveRuns != 1 {
		t.Fatalf("usage=%#v err=%v", usage, err)
	}
	audits, more, err := storage.ListAudit(ctx, scope, Page{Limit: 10})
	if err != nil || more || len(audits) != 1 || audits[0].PrincipalID != "tester" {
		t.Fatalf("audit page=%#v more=%v err=%v", audits, more, err)
	}
}

func TestRunIdempotencyAndEventScope(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	scope := Scope{OrganizationID: "org", ProjectID: "project"}
	run, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-1", Kind: "AgentRun", DefinitionDigest: "sha256:x", RequestedBy: "user", IdempotencyKey: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "different", Kind: "AgentRun", DefinitionDigest: "sha256:x", RequestedBy: "user", IdempotencyKey: "request-1"})
	if err != nil || replayed.ID != run.ID {
		t.Fatalf("idempotency replay %#v %v", replayed, err)
	}
	event, err := storage.AppendEvent(ctx, Event{Scope: scope, RunID: run.ID, Type: "run.started", Payload: []byte(`{}`)})
	if err != nil || event.Sequence != 1 {
		t.Fatalf("append event %#v %v", event, err)
	}
	if _, err := storage.ListEvents(ctx, Scope{OrganizationID: "other", ProjectID: "project"}, run.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant events=%v", err)
	}
}

func TestMemoryEventsAndAuditUseStrictCanonicalJSON(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	scope := Scope{OrganizationID: "org-events", ProjectID: "project-events"}
	run, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-events", Kind: "AgentRun", DefinitionDigest: "sha256:events", RequestedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defaultEvent, err := storage.AppendEvent(ctx, Event{Scope: scope, RunID: run.ID, Type: "default"})
	if err != nil || string(defaultEvent.Payload) != `{}` {
		t.Fatalf("default event=%#v err=%v", defaultEvent, err)
	}
	canonicalEvent, err := storage.AppendEvent(ctx, Event{Scope: scope, RunID: run.ID, Type: "complex", Payload: []byte(`{"z":2.0,"a":{"b":1e3,"a":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(canonicalEvent.Payload), `{"a":{"a":true,"b":1e3},"z":2}`; got != want {
		t.Fatalf("canonical event payload=%s want=%s", got, want)
	}
	if _, err := storage.AppendEvent(ctx, Event{Scope: scope, RunID: run.ID, Type: "duplicate", Payload: []byte(`{"x":1,"x":2}`)}); err == nil {
		t.Fatal("duplicate-key event payload was accepted")
	}
	events, err := storage.ListEvents(ctx, scope, run.ID, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%#v err=%v", events, err)
	}

	if err := storage.AppendAudit(ctx, AuditEvent{Scope: scope, PrincipalID: "test", Action: "default", ResourceType: "run", ResourceID: run.ID, Decision: "allowed"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendAudit(ctx, AuditEvent{Scope: scope, PrincipalID: "test", Action: "complex", ResourceType: "run", ResourceID: run.ID, Decision: "allowed", Metadata: []byte(`{"z":2.0,"a":{"b":1e3,"a":true}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendAudit(ctx, AuditEvent{Scope: scope, PrincipalID: "test", Action: "duplicate", ResourceType: "run", ResourceID: run.ID, Decision: "allowed", Metadata: []byte(`{"x":1,"x":2}`)}); err == nil {
		t.Fatal("duplicate-key audit metadata was accepted")
	}
	audits, _, err := storage.ListAudit(ctx, scope, Page{Limit: 10})
	if err != nil || len(audits) != 2 {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
	for _, audit := range audits {
		if audit.Action == "default" && string(audit.Metadata) != `{}` {
			t.Fatalf("default audit metadata=%s", audit.Metadata)
		}
		if audit.Action == "complex" && string(audit.Metadata) != `{"a":{"a":true,"b":1e3},"z":2}` {
			t.Fatalf("canonical audit metadata=%s", audit.Metadata)
		}
	}
}

func TestRunStatusTransitionsAreMonotonic(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	scope := Scope{OrganizationID: "org", ProjectID: "project"}
	if _, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-opaque", Kind: "AgentRun", DefinitionDigest: "sha256:x", RequestedBy: "user"}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"Scheduled", "Running", "WaitingApproval", "Running", "Succeeded"} {
		if _, err := storage.SetRunStatus(ctx, scope, "run-opaque", status, ""); err != nil {
			t.Fatalf("transition to %s: %v", status, err)
		}
	}
	if _, err := storage.SetRunStatus(ctx, scope, "run-opaque", "Running", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal run was reopened: %v", err)
	}
	if _, err := storage.SetRunStatus(ctx, scope, "run-opaque", "invented", ""); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestEffectClaimIsAtMostOnce(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	scope := Scope{OrganizationID: "org", ProjectID: "project"}
	if _, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run", Kind: "AgentRun", DefinitionDigest: "sha256:x", RequestedBy: "user"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.Claim(ctx, "org", "project", "run", "effect", "sha256:request")
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	claimed, err = storage.Claim(ctx, "org", "project", "run", "effect", "sha256:request")
	if err != nil || claimed {
		t.Fatalf("duplicate claim=%v err=%v", claimed, err)
	}
	if err := storage.Complete(ctx, "org", "project", "run", "effect", "unknown", []byte(`null`)); err != nil {
		t.Fatal(err)
	}
	if err := storage.Complete(ctx, "org", "project", "run", "effect", "succeeded", []byte(`{"ok":true}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsafe effect transition: %v", err)
	}
}

func TestRunSignalClaimIsConcurrentAndRecoverable(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	scope := Scope{OrganizationID: "org", ProjectID: "project"}
	if _, err := storage.CreateRun(ctx, Run{Scope: scope, ID: "run-signal", Kind: "AgentRun", DefinitionDigest: "sha256:x", RequestedBy: "user"}); err != nil {
		t.Fatal(err)
	}
	keyHash := strings.Repeat("a", 64)
	fingerprint := strings.Repeat("b", 64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	owners := 0
	ownerToken := ""
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claim, err := storage.ClaimRunSignal(ctx, scope, "run-signal", keyHash, fingerprint, fmt.Sprintf("%064x", i+1), time.Minute)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if claim.Owner {
				mu.Lock()
				owners++
				ownerToken = fmt.Sprintf("%064x", i+1)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if owners != 1 {
		t.Fatalf("owners=%d, want exactly one concurrent owner", owners)
	}
	if err := storage.AcceptRunSignal(ctx, scope, "run-signal", keyHash, fingerprint, ownerToken); err != nil {
		t.Fatalf("accept concurrent claim: %v", err)
	}

	// A different fingerprint is rejected before any delivery can occur.
	if _, err := storage.ClaimRunSignal(ctx, scope, "run-signal", keyHash, strings.Repeat("c", 64), strings.Repeat("d", 64), time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("fingerprint conflict=%v", err)
	}

	// Use a fresh key to verify that an expired pending claim can be recovered.
	recoveryKey := strings.Repeat("e", 64)
	first, err := storage.ClaimRunSignal(ctx, scope, "run-signal", recoveryKey, fingerprint, strings.Repeat("f", 64), time.Millisecond)
	if err != nil || !first.Owner {
		t.Fatalf("initial recovery claim=%#v err=%v", first, err)
	}
	time.Sleep(3 * time.Millisecond)
	second, err := storage.ClaimRunSignal(ctx, scope, "run-signal", recoveryKey, fingerprint, strings.Repeat("1", 64), time.Minute)
	if err != nil || !second.Owner {
		t.Fatalf("recovered claim=%#v err=%v", second, err)
	}
	if err := storage.AcceptRunSignal(ctx, scope, "run-signal", recoveryKey, fingerprint, strings.Repeat("1", 64)); err != nil {
		t.Fatalf("accept recovered claim: %v", err)
	}
	replay, err := storage.ClaimRunSignal(ctx, scope, "run-signal", recoveryKey, fingerprint, strings.Repeat("2", 64), time.Minute)
	if err != nil || replay.State != RunSignalAccepted || replay.Owner {
		t.Fatalf("accepted replay=%#v err=%v", replay, err)
	}
}
