package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgreSQLFaultInjectionEffectLedgerIsSingleClaimAndTerminallyUnknown
// uses the real PostgreSQL effect ledger as the durable side of an ambiguous
// MCP write. The transport test lives in toolbroker; this test proves the
// database cannot produce two owners and cannot later rewrite unknown to a
// successful outcome.
func TestPostgreSQLFaultInjectionEffectLedgerIsSingleClaimAndTerminallyUnknown(t *testing.T) {
	databaseURL := os.Getenv("AGW_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGW_TEST_DATABASE_URL is not configured")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	var effectsPresent bool
	if err := database.QueryRowContext(ctx, `SELECT to_regclass('public.effects') IS NOT NULL`).Scan(&effectsPresent); err != nil {
		t.Fatal(err)
	}
	if !effectsPresent {
		t.Fatal("001_initial.sql has not been applied")
	}
	scope := Scope{
		OrganizationID: "00000000-0000-5000-8000-000000000201",
		ProjectID:      "00000000-0000-5000-8000-000000000211",
	}
	seedFaultStoreTenant(t, ctx, database, scope)
	storage, err := NewPostgreSQL(database)
	if err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("effect-fault-%d", time.Now().UnixNano())
	if _, err := storage.CreateRun(ctx, Run{
		Scope: scope, ID: runID, Kind: "AgentRun",
		DefinitionDigest: "sha256:" + strings.Repeat("a", 64), RequestedBy: "fault-test",
	}); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	var group sync.WaitGroup
	var mu sync.Mutex
	owners := 0
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			claimed, claimErr := storage.Claim(ctx, scope.OrganizationID, scope.ProjectID, runID, "external-write", "sha256:"+strings.Repeat("b", 64))
			if claimErr != nil {
				t.Errorf("effect claim %d: %v", i, claimErr)
				return
			}
			if claimed {
				mu.Lock()
				owners++
				mu.Unlock()
			}
		}(i)
	}
	group.Wait()
	if owners != 1 {
		t.Fatalf("concurrent PostgreSQL effect owners=%d, want exactly one", owners)
	}
	if err := storage.Complete(ctx, scope.OrganizationID, scope.ProjectID, runID, "external-write", "unknown", []byte(`{"transport":"connection_dropped"}`)); err != nil {
		t.Fatalf("complete ambiguous effect: %v", err)
	}
	if err := storage.Complete(ctx, scope.OrganizationID, scope.ProjectID, runID, "external-write", "unknown", []byte(`{"transport":"connection_dropped"}`)); err != nil {
		t.Fatalf("idempotent unknown completion: %v", err)
	}
	if err := storage.Complete(ctx, scope.OrganizationID, scope.ProjectID, runID, "external-write", "succeeded", []byte(`{"ok":true}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown effect was rewritten to success: %v", err)
	}
}

func seedFaultStoreTenant(t *testing.T, ctx context.Context, database *sql.DB, scope Scope) {
	t.Helper()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO organizations(id,name) VALUES ($1,'store-fault-live') ON CONFLICT (id) DO NOTHING`, scope.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,organization_id,name) VALUES ($1,$2,'store-fault-live') ON CONFLICT (id) DO NOTHING`, scope.ProjectID, scope.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
