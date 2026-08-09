package localengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// This is intentionally opt-in, matching the existing PostgreSQL live tests.
// It exercises the real local orchestration/runtime-event tables, RLS context, lease claim,
// atomic run status/event update, and a successful runner transition.
func TestPostgreSQLLocalEngineLive(t *testing.T) {
	databaseURL := os.Getenv("AGW_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGW_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	var present bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.local_workflows') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("003_local_orchestration.sql has not been applied")
	}

	scope := store.Scope{
		OrganizationID: "00000000-0000-4000-8000-000000000101",
		ProjectID:      "00000000-0000-4000-8000-000000000111",
	}
	seedTenant(t, ctx, db, scope)
	storage, err := store.NewPostgreSQL(db)
	if err != nil {
		t.Fatal(err)
	}
	manifest := workflow.Manifest{
		Name: "live-local", Revision: "sha256:" + strings.Repeat("a", 64),
		Steps: []workflow.Step{
			{ID: "agent-one", AgentRef: "agent/live"},
			{ID: "agent-two", AgentRef: "agent/live", Needs: []string{"agent-one"}},
		},
	}
	runID := fmt.Sprintf("local-live-%d", time.Now().UnixNano())
	run, err := storage.CreateRun(ctx, store.Run{
		Scope: scope, ID: runID, Kind: "AgentRun", DefinitionDigest: manifest.Revision, RequestedBy: "localengine-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	activities := &liveActivities{}
	engine := New(db, activities)
	engine.Options.Scopes = []store.Scope{scope}
	engine.Options.PollInterval = time.Millisecond
	if err := engine.Enqueue(ctx, EnqueueInput{Run: run, Manifest: manifest, InputRef: "input://live"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Enqueue(ctx, EnqueueInput{Run: run, Manifest: manifest, InputRef: "input://live"}); err != nil {
		t.Fatalf("idempotent enqueue: %v", err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		worked, runErr := engine.RunOnce(ctx, "live-worker")
		if runErr != nil {
			t.Fatalf("run once worked=%v err=%v", worked, runErr)
		}
		if !worked {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		updated, getErr := storage.GetRun(ctx, scope, runID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if updated.Status == "Succeeded" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	updated, err := storage.GetRun(ctx, scope, runID)
	if err != nil || updated.Status != "Succeeded" {
		t.Fatalf("updated run=%#v err=%v", updated, err)
	}
	events, err := storage.ListEvents(ctx, scope, runID, 0)
	if err != nil || len(events) != 9 {
		t.Fatalf("local status events=%#v err=%v", events, err)
	}
	runtimeEvents := 0
	for _, event := range events {
		if event.Type == "model.requested" || event.Type == "assistant.message" {
			runtimeEvents++
		}
	}
	if runtimeEvents != 4 || activities.calls(runID) != 4 {
		t.Fatalf("runtime events were not persisted idempotently: events=%d status_calls=%d first=%v second=%v", runtimeEvents, activities.calls(runID), activities.after("live-task-agent-one"), activities.after("live-task-agent-two"))
	}
	if got := activities.after("live-task-agent-one"); !reflect.DeepEqual(got, []uint64{0, 2}) {
		t.Fatalf("first task cursor sequence=%v, want [0 2]", got)
	}
	if got := activities.after("live-task-agent-two"); !reflect.DeepEqual(got, []uint64{0, 2}) {
		t.Fatalf("second task cursor was not reset=%v, want [0 2]", got)
	}
	var cursor int64
	var cursorTask string
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT runner_event_task_id,runner_event_cursor FROM local_workflows
		WHERE organization_id=$1 AND project_id=$2 AND run_id=$3`, scope.OrganizationID, scope.ProjectID, runID).Scan(&cursorTask, &cursor); err != nil {
		t.Fatal(err)
	}
	if cursorTask != "live-task-agent-two" || cursor != 2 {
		t.Fatalf("durable runner event cursor=%d task=%q, want task live-task-agent-two cursor 2", cursor, cursorTask)
	}
	if _, err := storage.GetRun(ctx, store.Scope{OrganizationID: scope.OrganizationID, ProjectID: "00000000-0000-4000-8000-000000000112"}, runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-project run was visible: %v", err)
	}
	assertLocalRLSHidesOtherScope(t, ctx, db, scope, runID)

	approvalManifest := workflow.Manifest{
		Name: "live-approval", Revision: "sha256:" + strings.Repeat("c", 64),
		Steps: []workflow.Step{{ID: "review", Approval: &workflow.ApprovalSpec{ID: "review", Reason: "live test"}}},
	}
	approvalRunID := fmt.Sprintf("local-approval-%d", time.Now().UnixNano())
	approvalRun, err := storage.CreateRun(ctx, store.Run{
		Scope: scope, ID: approvalRunID, Kind: "WorkflowRun", DefinitionDigest: approvalManifest.Revision, RequestedBy: "localengine-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.Options.PollInterval = time.Millisecond
	if err := engine.Enqueue(ctx, EnqueueInput{Run: approvalRun, Manifest: approvalManifest}); err != nil {
		t.Fatal(err)
	}
	if worked, err := engine.RunOnce(ctx, "live-worker"); err != nil || !worked {
		t.Fatalf("approval enqueue transition worked=%v err=%v", worked, err)
	}
	command := Command{Kind: "approval", Target: "workflow", IdempotencyKey: "approval-live", ApprovalID: "review", Decision: "approved", StepID: "review"}
	if err := engine.EnqueueCommand(ctx, scope, approvalRunID, command); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnqueueCommand(ctx, scope, approvalRunID, command); err != nil {
		t.Fatalf("idempotent command replay: %v", err)
	}
	var approvalWorked bool
	for attempt := 0; attempt < 20; attempt++ {
		approvalWorked, err = engine.RunOnce(ctx, "live-worker")
		if err != nil {
			t.Fatal(err)
		}
		if approvalWorked {
			updated, getErr := storage.GetRun(ctx, scope, approvalRunID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if updated.Status == "Succeeded" {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	updatedApproval, err := storage.GetRun(ctx, scope, approvalRunID)
	if err != nil || updatedApproval.Status != "Succeeded" {
		t.Fatalf("approval run=%#v err=%v worked=%v", updatedApproval, err, approvalWorked)
	}
}

func assertLocalRLSHidesOtherScope(t *testing.T, ctx context.Context, db *sql.DB, scope store.Scope, runID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, "00000000-0000-4000-8000-000000000112"); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM local_workflows WHERE run_id=$1`, runID).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("local workflow crossed RLS boundary: %d rows", visible)
	}
}

func seedTenant(t *testing.T, ctx context.Context, db *sql.DB, scope store.Scope) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO organizations(id,name) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`, scope.OrganizationID, "localengine-"+scope.OrganizationID+"-"+scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,organization_id,name) VALUES ($1,$2,'localengine-live') ON CONFLICT (id) DO NOTHING`, scope.ProjectID, scope.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type liveActivities struct {
	mu             sync.Mutex
	statusCalls    map[string]int
	runStatusCalls map[string]int
	afterSequences map[string][]uint64
}

func (*liveActivities) ScheduleRunnerTask(_ context.Context, input workflow.ScheduleRunnerTaskInput) (workflow.ScheduleRunnerTaskResult, error) {
	return workflow.ScheduleRunnerTaskResult{Status: "scheduled", TaskID: "live-task-" + input.StepID}, nil
}
func (*liveActivities) CancelRunnerTask(context.Context, workflow.CancelRunnerTaskInput) error {
	return nil
}
func (*liveActivities) ResumeRunnerTask(context.Context, workflow.ResumeRunnerTaskInput) error {
	return nil
}
func (a *liveActivities) StatusRunnerTask(_ context.Context, input workflow.StatusRunnerTaskInput) (workflow.StatusRunnerTaskResult, error) {
	a.mu.Lock()
	if a.statusCalls == nil {
		a.statusCalls = make(map[string]int)
	}
	if a.runStatusCalls == nil {
		a.runStatusCalls = make(map[string]int)
	}
	if a.afterSequences == nil {
		a.afterSequences = make(map[string][]uint64)
	}
	a.statusCalls[input.TaskID]++
	a.runStatusCalls[input.RunID]++
	a.afterSequences[input.TaskID] = append(a.afterSequences[input.TaskID], input.AfterSequence)
	calls := a.statusCalls[input.TaskID]
	a.mu.Unlock()
	result := workflow.StatusRunnerTaskResult{
		Events: []workflow.RunnerRuntimeEvent{
			{Sequence: 1, Type: "model.requested", Payload: []byte(`{"model":"live"}`)},
			{Sequence: 2, Type: "assistant.message", Payload: []byte(`{"message":"live runtime"}`)},
		},
		EventCursor: 2,
	}
	if calls == 1 {
		result.Status = "running"
		return result, nil
	}
	result.Status, result.Output = "succeeded", liveArtifact()
	return result, nil
}

func (a *liveActivities) calls(runID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runStatusCalls[runID]
}

func (a *liveActivities) after(taskID string) []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.afterSequences[taskID]...)
}

func liveArtifact() workflow.ArtifactRef {
	return workflow.ArtifactRef{ID: "live-output", URI: "file:///artifacts/live-output", Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 1, MediaType: "text/plain"}
}
