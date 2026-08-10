package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgreSQLLive exercises the real database/sql implementation and RLS.
// CI supplies a disposable migrated database and a non-BYPASSRLS application
// role; ordinary unit test runs skip it.
func TestPostgreSQLLive(t *testing.T) {
	databaseURL := os.Getenv("AGW_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGW_TEST_DATABASE_URL is not configured")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	storage, err := NewPostgreSQL(database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	projectOne := Scope{OrganizationID: "00000000-0000-0000-0000-000000000001", ProjectID: "00000000-0000-0000-0000-000000000011"}
	projectTwo := Scope{OrganizationID: projectOne.OrganizationID, ProjectID: "00000000-0000-0000-0000-000000000012"}
	digest := "sha256:" + strings.Repeat("a", 64)
	resourceDocument := []byte(`{
  "kind": "Agent",
  "message": "line one\nline two with \\path, \"quotes\", and Unicode: ☃",
  "nested": {"value": 2, "array": [{"value": "first"}, {"value": "second"}]}
}`)
	applied, err := storage.ApplyResource(ctx, Resource{Scope: projectOne, Kind: "Agent", Name: "live", Digest: digest, Document: resourceDocument, AppliedBy: "test"})
	if err != nil || applied.Revision != 1 {
		t.Fatalf("apply=%#v err=%v", applied, err)
	}
	boundaryDocument := []byte(`{"positive":1e4096,"negative":1e-4096}`)
	if _, err := storage.ApplyResource(ctx, Resource{Scope: projectOne, Kind: "Agent", Name: "number-boundary", Digest: "sha256:" + strings.Repeat("b", 64), Document: boundaryDocument, AppliedBy: "test"}); err != nil {
		t.Fatalf("PostgreSQL-compatible number boundary was rejected: %v", err)
	}
	for _, document := range [][]byte{[]byte(`{"positive":1e4097}`), []byte(`{"negative":1e-4097}`)} {
		if _, err := storage.ApplyResource(ctx, Resource{Scope: projectOne, Kind: "Agent", Name: "number-rejected", Digest: "sha256:" + strings.Repeat("c", 64), Document: document, AppliedBy: "test"}); err == nil {
			t.Fatalf("out-of-budget PostgreSQL number was accepted: %s", document)
		}
	}
	if !jsonEquivalent(applied.Document, resourceDocument) {
		t.Fatalf("apply document changed: got=%s want=%s", applied.Document, resourceDocument)
	}
	replayed, err := storage.ApplyResource(ctx, Resource{Scope: projectOne, Kind: "Agent", Name: "live", Digest: digest, Document: []byte(`{"nested":{"array":[{"value":"first"},{"value":"second"}],"value":2.0},"message":"line one\nline two with \\path, \"quotes\", and Unicode: ☃","kind":"Agent"}`), AppliedBy: "test-replay"})
	if err != nil || replayed.Revision != 1 || !jsonEquivalent(replayed.Document, resourceDocument) {
		t.Fatalf("idempotent apply=%#v err=%v", replayed, err)
	}
	read, err := storage.GetResource(ctx, projectOne, "Agent", "live")
	if err != nil || read.Digest != digest || !jsonEquivalent(read.Document, resourceDocument) {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	resources, more, err := storage.ListResources(ctx, projectOne, "Agent", Page{Limit: MaxPageLimit})
	if err != nil || more {
		t.Fatalf("list resources more=%v err=%v", more, err)
	}
	var listed *Resource
	for index := range resources {
		if resources[index].Name == "live" {
			listed = &resources[index]
			break
		}
	}
	if listed == nil || !jsonEquivalent(listed.Document, resourceDocument) {
		t.Fatalf("listed resource=%#v", listed)
	}
	if _, err := storage.GetResource(ctx, projectTwo, "Agent", "live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project read must be hidden, got %v", err)
	}
	run, err := storage.CreateRun(ctx, Run{Scope: projectOne, ID: "run-live-opaque", Kind: "AgentRun", DefinitionDigest: digest, RequestedBy: "test"})
	if err != nil || run.ID != "run-live-opaque" {
		t.Fatalf("create opaque run=%#v err=%v", run, err)
	}
	updated, err := storage.SetRunStatus(ctx, projectOne, run.ID, "Running", "")
	if err != nil || updated.Status != "Running" {
		t.Fatalf("update run=%#v err=%v", updated, err)
	}
	events, err := storage.ListEvents(ctx, projectOne, run.ID, 0)
	if err != nil || len(events) != 1 || events[0].Type != "run.status_changed" {
		t.Fatalf("status events=%#v err=%v", events, err)
	}
	defaultEvent, err := storage.AppendEvent(ctx, Event{Scope: projectOne, RunID: run.ID, Type: "default"})
	if err != nil || string(defaultEvent.Payload) != `{}` {
		t.Fatalf("default event=%#v err=%v", defaultEvent, err)
	}
	escapedPayload := []byte(`{"message":"line one\nline two with \\ path, \"quotes\", and unicode: \u2603","nested":{"ok":true}}`)
	if _, err := storage.AppendEvent(ctx, Event{Scope: projectOne, RunID: run.ID, Type: "assistant.message", Payload: escapedPayload}); err != nil {
		t.Fatalf("append escaped event: %v", err)
	}
	if _, err := storage.AppendEvent(ctx, Event{Scope: projectOne, RunID: run.ID, Type: "duplicate", Payload: []byte(`{"x":1,"x":2}`)}); err == nil {
		t.Fatal("duplicate-key event payload was accepted")
	}
	events, err = storage.ListEvents(ctx, projectOne, run.ID, 0)
	if err != nil || len(events) != 3 || !json.Valid(events[2].Payload) {
		t.Fatalf("escaped event replay=%#v err=%v", events, err)
	}
	if !jsonEquivalent(events[2].Payload, escapedPayload) {
		t.Fatalf("escaped event payload changed: got=%s want=%s", events[2].Payload, escapedPayload)
	}

	if err := storage.AppendAudit(ctx, AuditEvent{Scope: projectOne, PrincipalID: "test", Action: "json.default", ResourceType: "resource", ResourceID: "live", Decision: "allowed"}); err != nil {
		t.Fatalf("append default audit metadata: %v", err)
	}
	auditMetadata := []byte(`{"message":"line one\nline two with \\ path, \"quotes\", and unicode: ☃","nested":{"items":[1,2,3]}}`)
	if err := storage.AppendAudit(ctx, AuditEvent{Scope: projectOne, PrincipalID: "test", Action: "json.readback", ResourceType: "resource", ResourceID: "live", Decision: "allowed", Metadata: auditMetadata}); err != nil {
		t.Fatalf("append escaped audit metadata: %v", err)
	}
	if err := storage.AppendAudit(ctx, AuditEvent{Scope: projectOne, PrincipalID: "test", Action: "json.duplicate", ResourceType: "resource", ResourceID: "live", Decision: "allowed", Metadata: []byte(`{"x":1,"x":2}`)}); err == nil {
		t.Fatal("duplicate-key audit metadata was accepted")
	}
	audits, _, err := storage.ListAudit(ctx, projectOne, Page{Limit: MaxPageLimit})
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	var foundAudit *AuditEvent
	for index := range audits {
		if audits[index].Action == "json.readback" {
			foundAudit = &audits[index]
			break
		}
	}
	if foundAudit == nil || !jsonEquivalent(foundAudit.Metadata, auditMetadata) {
		t.Fatalf("escaped audit metadata=%#v", foundAudit)
	}
	var foundDefaultAudit *AuditEvent
	for index := range audits {
		if audits[index].Action == "json.default" {
			foundDefaultAudit = &audits[index]
			break
		}
	}
	if foundDefaultAudit == nil || string(foundDefaultAudit.Metadata) != `{}` {
		t.Fatalf("default audit metadata=%#v", foundDefaultAudit)
	}
	if _, err := storage.GetRun(ctx, projectTwo, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project run must be hidden, got %v", err)
	}

	keyHash := strings.Repeat("b", 64)
	fingerprint := strings.Repeat("c", 64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	owners := 0
	ownerToken := ""
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("%064x", i+1)
			claim, claimErr := storage.ClaimRunSignal(ctx, projectOne, run.ID, keyHash, fingerprint, token, time.Minute)
			if claimErr != nil {
				t.Errorf("claim %d: %v", i, claimErr)
				return
			}
			if claim.Owner {
				mu.Lock()
				owners++
				ownerToken = token
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if owners != 1 {
		t.Fatalf("concurrent PostgreSQL owners=%d, want one", owners)
	}
	if err := storage.AcceptRunSignal(ctx, projectOne, run.ID, keyHash, fingerprint, ownerToken); err != nil {
		t.Fatalf("accept PostgreSQL signal claim: %v", err)
	}
	replay, err := storage.ClaimRunSignal(ctx, projectOne, run.ID, keyHash, fingerprint, strings.Repeat("d", 64), time.Minute)
	if err != nil || replay.State != RunSignalAccepted || replay.Owner {
		t.Fatalf("accepted PostgreSQL replay=%#v err=%v", replay, err)
	}
	if _, err := storage.ClaimRunSignal(ctx, projectOne, run.ID, keyHash, strings.Repeat("e", 64), strings.Repeat("f", 64), time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("PostgreSQL fingerprint conflict=%v", err)
	}

	artifactDocument := []byte(`{"title":"artifact \"quotes\"","body":"line one\nline two with \\ path and Unicode: ☃","value":2,"nested":{"items":[{"ok":true}]}}`)
	version := ArtifactVersion{
		Scope: projectOne, ArtifactID: "artifact-json-live", VersionID: "artifact-json-live-v1", RunID: run.ID,
		VersionNumber: 1, Document: artifactDocument, ContentObjectKey: "content/artifact-json-live-v1", SourceObjectKey: "source/artifact-json-live-v1",
	}
	storedVersion, err := storage.PutArtifactVersion(ctx, version)
	if err != nil || !jsonEquivalent(storedVersion.Document, artifactDocument) {
		t.Fatalf("put artifact version=%#v err=%v", storedVersion, err)
	}
	replayVersion := version
	replayVersion.Document = []byte(`{"nested":{"items":[{"ok":true}]},"value":2.0,"body":"line one\nline two with \\ path and Unicode: ☃","title":"artifact \"quotes\""}`)
	replayedVersion, err := storage.PutArtifactVersion(ctx, replayVersion)
	if err != nil || !jsonEquivalent(replayedVersion.Document, artifactDocument) {
		t.Fatalf("replayed artifact version=%#v err=%v", replayedVersion, err)
	}
	readVersion, err := storage.GetArtifactVersion(ctx, projectOne, version.ArtifactID, version.VersionID)
	if err != nil || !jsonEquivalent(readVersion.Document, artifactDocument) {
		t.Fatalf("read artifact version=%#v err=%v", readVersion, err)
	}
	versions, err := storage.ListArtifactVersions(ctx, projectOne, version.ArtifactID)
	if err != nil || len(versions) != 1 || !jsonEquivalent(versions[0].Document, artifactDocument) {
		t.Fatalf("list artifact versions=%#v err=%v", versions, err)
	}
}

func jsonEquivalent(tested, expected []byte) bool {
	var testedValue, expectedValue any
	testedDecoder := json.NewDecoder(strings.NewReader(string(tested)))
	testedDecoder.UseNumber()
	expectedDecoder := json.NewDecoder(strings.NewReader(string(expected)))
	expectedDecoder.UseNumber()
	if testedDecoder.Decode(&testedValue) != nil || expectedDecoder.Decode(&expectedValue) != nil {
		return false
	}
	return reflect.DeepEqual(testedValue, expectedValue)
}
