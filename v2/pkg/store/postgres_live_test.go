package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	applied, err := storage.ApplyResource(ctx, Resource{Scope: projectOne, Kind: "Agent", Name: "live", Digest: digest, Document: []byte(`{"kind":"Agent"}`), AppliedBy: "test"})
	if err != nil || applied.Revision != 1 {
		t.Fatalf("apply=%#v err=%v", applied, err)
	}
	read, err := storage.GetResource(ctx, projectOne, "Agent", "live")
	if err != nil || read.Digest != digest {
		t.Fatalf("read=%#v err=%v", read, err)
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
	escapedPayload := []byte(`{"message":"line one\nline two with \\ path and unicode: \u2603"}`)
	if _, err := storage.AppendEvent(ctx, Event{Scope: projectOne, RunID: run.ID, Type: "assistant.message", Payload: escapedPayload}); err != nil {
		t.Fatalf("append escaped event: %v", err)
	}
	events, err = storage.ListEvents(ctx, projectOne, run.ID, 0)
	if err != nil || len(events) != 2 || !json.Valid(events[1].Payload) {
		t.Fatalf("escaped event replay=%#v err=%v", events, err)
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
}
