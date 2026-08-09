package localengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgreSQLFaultInjectionReclaimsWorkerLeaseAndConsumesOneCommand
// simulates a worker dying after claiming durable state but before persisting
// progress. A fresh Engine instance waits for the lease and completes the
// already-persisted command exactly once.
func TestPostgreSQLFaultInjectionReclaimsWorkerLeaseAndConsumesOneCommand(t *testing.T) {
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
	var workflowsPresent bool
	if err := database.QueryRowContext(ctx, `SELECT to_regclass('public.local_workflows') IS NOT NULL`).Scan(&workflowsPresent); err != nil {
		t.Fatal(err)
	}
	if !workflowsPresent {
		t.Fatal("003_local_orchestration.sql has not been applied")
	}
	scope := store.Scope{
		OrganizationID: "00000000-0000-5000-8000-000000000301",
		ProjectID:      "00000000-0000-5000-8000-000000000311",
	}
	seedTenant(t, ctx, database, scope)
	storage, err := store.NewPostgreSQL(database)
	if err != nil {
		t.Fatal(err)
	}
	manifest := workflow.Manifest{
		Name: "fault-recovery", Revision: "sha256:" + strings.Repeat("a", 64),
		Steps: []workflow.Step{{ID: "review", Approval: &workflow.ApprovalSpec{ID: "review", Reason: "fault test"}}},
	}
	runID := fmt.Sprintf("command-fault-%d", time.Now().UnixNano())
	run, err := storage.CreateRun(ctx, store.Run{
		Scope: scope, ID: runID, Kind: "WorkflowRun", DefinitionDigest: manifest.Revision, RequestedBy: "fault-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := New(database, &liveActivities{})
	engine.Options.Scopes = []store.Scope{scope}
	engine.Options.LeaseDuration = 40 * time.Millisecond
	engine.Options.PollInterval = time.Millisecond
	if err := engine.Enqueue(ctx, EnqueueInput{Run: run, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		if _, err := engine.RunOnce(ctx, "worker-before-crash"); err != nil {
			t.Fatal(err)
		}
		current, getErr := storage.GetRun(ctx, scope, runID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == "WaitingApproval" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := storage.GetRun(ctx, scope, runID)
	if err != nil || current.Status != "WaitingApproval" {
		t.Fatalf("workflow did not reach approval state: run=%#v err=%v", current, err)
	}
	command := Command{Kind: "approval", Target: "workflow", IdempotencyKey: "fault-approval", ApprovalID: "review", Decision: "approved", StepID: "review"}
	if err := engine.EnqueueCommand(ctx, scope, runID, command); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnqueueCommand(ctx, scope, runID, command); err != nil {
		t.Fatalf("idempotent command replay failed: %v", err)
	}
	if err := engine.EnqueueCommand(ctx, scope, runID, Command{Kind: "approval", Target: "workflow", IdempotencyKey: "fault-approval", ApprovalID: "review", Decision: "denied", StepID: "review"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("command key reuse with different payload returned %v, want conflict", err)
	}
	count, state := localFaultCommandStats(t, ctx, database, scope, runID)
	if count != 1 || state != "pending" {
		t.Fatalf("durable command rows before crash=%d state=%q, want one pending row", count, state)
	}

	time.Sleep(5 * time.Millisecond)
	if _, err := engine.claim(ctx, scope, "worker-crashed"); err != nil {
		t.Fatalf("claim state before simulated worker crash: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	restarted := New(database, &liveActivities{})
	restarted.Options.Scopes = []store.Scope{scope}
	restarted.Options.LeaseDuration = 40 * time.Millisecond
	restarted.Options.PollInterval = time.Millisecond
	worked, err := restarted.RunOnce(ctx, "worker-restarted")
	if err != nil || !worked {
		t.Fatalf("restarted worker worked=%t err=%v", worked, err)
	}
	completed, err := storage.GetRun(ctx, scope, runID)
	if err != nil || completed.Status != "Succeeded" {
		t.Fatalf("restarted worker did not complete command: run=%#v err=%v", completed, err)
	}
	count, state = localFaultCommandStats(t, ctx, database, scope, runID)
	if count != 1 || state != "consumed" {
		t.Fatalf("durable command rows after recovery=%d state=%q, want one consumed row", count, state)
	}
}

func TestPostgreSQLFaultInjectionRuntimeEventReplayIsIdempotent(t *testing.T) {
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
	stamp := uint64(time.Now().UnixNano()) & 0xffffffffffff
	scope := store.Scope{
		OrganizationID: fmt.Sprintf("00000000-0000-6000-8%03x-%012x", (stamp>>12)&0xfff, stamp),
		ProjectID:      fmt.Sprintf("00000000-0000-6000-8%03x-%012x", ((stamp>>12)+1)&0xfff, stamp+1),
	}
	seedTenant(t, ctx, database, scope)
	storage, err := store.NewPostgreSQL(database)
	if err != nil {
		t.Fatal(err)
	}
	manifest := workflow.Manifest{
		Name: "runtime-replay", Revision: "sha256:" + strings.Repeat("a", 64),
		Steps: []workflow.Step{{ID: "agent", AgentRef: "agent/replay"}},
	}
	runID := fmt.Sprintf("runtime-replay-%d", time.Now().UnixNano())
	run, err := storage.CreateRun(ctx, store.Run{
		Scope: scope, ID: runID, Kind: "AgentRun", DefinitionDigest: manifest.Revision, RequestedBy: "fault-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := New(database, &liveActivities{})
	engine.Options.Scopes = []store.Scope{scope}
	if err := engine.Enqueue(ctx, EnqueueInput{Run: run, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.claim(ctx, scope, "runtime-replay-worker")
	if err != nil {
		t.Fatal(err)
	}
	status := workflow.StatusRunnerTaskResult{
		Status: "running", EventCursor: 2,
		Events: []workflow.RunnerRuntimeEvent{
			{Sequence: 1, Type: "model.requested", Payload: []byte(`{"model":"replay"}`)},
			{Sequence: 2, Type: "assistant.message", Payload: []byte(`{"message":"<function_results> first \\ path and unicode: \u2603"}`)},
		},
	}
	if cursor, err := engine.persistRunnerRuntimeEvents(ctx, claimed, "replay-task", status); err != nil || cursor != 2 {
		t.Fatalf("persist first runtime response cursor=%d err=%v", cursor, err)
	}
	if cursor, err := engine.persistRunnerRuntimeEvents(ctx, claimed, "replay-task", status); err != nil || cursor != 2 {
		t.Fatalf("persist replay runtime response cursor=%d err=%v", cursor, err)
	}
	conflicting := status
	conflicting.Events = append([]workflow.RunnerRuntimeEvent(nil), status.Events...)
	conflicting.Events[0].Payload = []byte(`{"model":"retargeted"}`)
	if _, err := engine.persistRunnerRuntimeEvents(ctx, claimed, "replay-task", conflicting); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("source-sequence reuse returned %v, want conflict", err)
	}
	events, err := storage.ListEvents(ctx, scope, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "model.requested" || events[1].Type != "assistant.message" {
		t.Fatalf("replay changed durable runtime history: %#v", events)
	}
}

func localFaultCommandStats(t *testing.T, ctx context.Context, database *sql.DB, scope store.Scope, runID string) (int, string) {
	t.Helper()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	var count int
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(state),'') FROM local_workflow_commands WHERE organization_id=$1 AND project_id=$2 AND run_id=$3`, scope.OrganizationID, scope.ProjectID, runID).Scan(&count, &state); err != nil {
		t.Fatal(err)
	}
	return count, state
}
