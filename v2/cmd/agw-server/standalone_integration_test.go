package main

// This is an opt-in live test for the owner-operated standalone profile. It
// uses the agw-server composition functions, PostgreSQL migrations, the real
// local engine, and the real Unix runner client. The runner is a small fake
// control endpoint: it authenticates every request with Linux SO_PEERCRED and
// returns a deterministic artifact, so no model, sandbox image, or secret is
// required.
//
// Run with an existing disposable PostgreSQL URL:
//
//   AGW_POSTGRES_TEST_URL='postgres://...' go test ./cmd/agw-server -run TestStandaloneLocalEngineLive -v
//
// Or explicitly allow the test to start an exact-name disposable Docker
// PostgreSQL container:
//
//   AGW_STANDALONE_LIVE=1 go test ./cmd/agw-server -run TestStandaloneLocalEngineLive -v
//
// The Docker path removes the exact container with `docker rm -fv`; it does
// not create a named volume. Without either opt-in variable the test skips.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v2/pkg/httpapi"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runnerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sys/unix"
)

func TestStandaloneLocalEngineLive(t *testing.T) {
	databaseURL := standalonePostgresURL(t)
	database := openStandaloneDatabase(t, databaseURL)
	defer database.Close()

	applyStandaloneMigrations(t, database, databaseURL)
	scope := bootstrapStandaloneTenant(t, database)

	runner := startStandaloneFakeRunner(t)
	t.Setenv("AGW_DATABASE_URL", databaseURL)
	t.Setenv("AGW_ORCHESTRATION_MODE", "local")
	t.Setenv("AGW_AUTH_MODE", "local")
	t.Setenv("AGW_AUTH_TOKEN", standaloneSecret(t, 32))
	t.Setenv("AGW_AUTH_PRINCIPAL_ID", "standalone-live-owner")
	t.Setenv("AGW_LOCAL_ORGANIZATION_ID", scope.OrganizationID)
	t.Setenv("AGW_LOCAL_PROJECT_ID", scope.ProjectID)
	t.Setenv("AGW_RUNNER_TRANSPORT", "unix")
	t.Setenv("AGW_RUNNER_SOCKET", runner.socketPath)
	t.Setenv("AGW_RUNNER_REQUEST_TIMEOUT", "2s")
	t.Setenv("AGW_RUNNER_UID", "65532")
	artifactRoot := t.TempDir()
	if err := os.Chmod(artifactRoot, 0700); err != nil {
		t.Fatalf("harden live artifact root: %v", err)
	}
	t.Setenv("AGW_ARTIFACT_ROOT", artifactRoot)
	brokerRoot := t.TempDir()
	if err := os.Chmod(brokerRoot, 0700); err != nil {
		t.Fatalf("harden live broker root: %v", err)
	}
	t.Setenv("AGW_BROKER_ROOT", brokerRoot)

	first := startConfiguredStandalone(t, "first")
	if response := first.get("/readyz"); response.StatusCode != http.StatusOK {
		body := readResponseBody(t, response)
		t.Fatalf("configured standalone server is not ready: status=%d body=%s", response.StatusCode, body)
	}

	profile := standaloneSandboxProfile(scope.ProjectID)
	profileDigest := standaloneApplyResource(t, first.server, first.token, scope, profile)
	_ = profileDigest
	agent := standaloneAgent(scope.ProjectID, profile.Metadata.Name)
	agentDigest := standaloneApplyResource(t, first.server, first.token, scope, agent)

	runID := "standalone-live-run"
	createPath := standaloneScopePath(scope) + "/runs"
	createBody := map[string]any{
		"id":               runID,
		"kind":             spec.KindAgentRun,
		"definitionDigest": agentDigest,
		"idempotencyKey":   "standalone-live-idempotency",
		"agentRef":         agent.Metadata.Name,
		"inputRef":         "input://standalone/live",
	}
	created := first.doJSON(http.MethodPost, createPath, createBody)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("authenticated run creation failed: status=%d body=%s", created.StatusCode, readResponseBody(t, created))
	}
	_ = readResponseBody(t, created)

	runPath := createPath + "/" + runID
	deadline := time.Now().Add(15 * time.Second)
	var finalRun map[string]any
	for time.Now().Before(deadline) {
		response := first.get(runPath)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("authenticated run read failed: status=%d body=%s", response.StatusCode, readResponseBody(t, response))
		}
		finalRun = decodeJSONResponse(t, response)
		status := standaloneRunStatus(finalRun)
		if status == "Succeeded" {
			break
		}
		if status == "Failed" || status == "Cancelled" || status == "Lost" {
			t.Fatalf("local engine reached terminal failure: %s", status)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if standaloneRunStatus(finalRun) != "Succeeded" {
		t.Fatalf("local engine did not reach success before timeout: %#v", finalRun)
	}
	if runner.scheduleCount() == 0 {
		t.Fatal("local engine never called the authenticated Unix runner schedule endpoint")
	}
	if runner.authenticatedRequestCount() == 0 {
		t.Fatal("fake runner did not observe authenticated Unix peer credentials")
	}
	if input := runner.lastSchedule(); input.Execution.Backend != "podman" || input.Execution.IsolationGrade != "standard/shared-kernel" {
		t.Fatalf("local engine did not send the standalone Podman execution contract: %#v", input.Execution)
	}

	// Exercise the status endpoint explicitly as well. The local engine may not
	// need it for an immediately-succeeded schedule response, but the production
	// runner client and endpoint must still agree on the status contract.
	dispatch, err := runnerdispatch.NewUnix(runner.socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("create Unix runner client for status assertion: %v", err)
	}
	status, err := dispatch.StatusRunnerTask(context.Background(), workflow.StatusRunnerTaskInput{
		OrganizationID: scope.OrganizationID, ProjectID: scope.ProjectID, RunID: runID, TaskID: runner.lastTaskID(),
	})
	if err != nil || status.Status != "succeeded" {
		t.Fatalf("fake runner status endpoint result=%#v err=%v", status, err)
	}
	if runner.statusCount() == 0 {
		t.Fatal("fake runner status endpoint was not exercised")
	}
	assertStandaloneArtifactCatalog(t, first, scope, runID)

	assertStandalonePersistedState(t, first.database, scope, runID)
	first.Close()

	// Recompose the control plane against the same database and socket. No
	// in-memory run or manifest is reused; the second engine must observe the
	// terminal state persisted by the first process-shaped composition.
	second := startConfiguredStandalone(t, "restart")
	defer second.Close()
	response := second.get(runPath)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("restart-composed server could not read persisted run: status=%d body=%s", response.StatusCode, readResponseBody(t, response))
	}
	if status := standaloneRunStatus(decodeJSONResponse(t, response)); status != "Succeeded" {
		t.Fatalf("persisted run was not terminal after restart: %s", status)
	}
	if response = second.get("/readyz"); response.StatusCode != http.StatusOK {
		t.Fatalf("restart-composed server is not ready: status=%d body=%s", response.StatusCode, readResponseBody(t, response))
	}
}

type standaloneControlPlane struct {
	server    *httptest.Server
	database  *sql.DB
	storage   store.Store
	artifacts *artifact.Store
	token     string
	execution configuredRunEngine
	cancel    context.CancelFunc
	done      chan struct{}
	once      sync.Once
}

func startConfiguredStandalone(t *testing.T, workerSuffix string) *standaloneControlPlane {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	storage, database, err := configureStore(ctx)
	if err != nil {
		t.Fatalf("configureStore for standalone live test: %v", err)
	}
	authenticator, authDatabase, exchange, err := configureAuthenticator(ctx, database)
	if err != nil {
		if database != nil {
			database.Close()
		}
		t.Fatalf("configureAuthenticator for standalone live test: %v", err)
	}
	if authDatabase != nil && authDatabase != database {
		authDatabase.Close()
	}
	artifactStorage, err := configureArtifactStorage()
	if err != nil {
		if database != nil {
			database.Close()
		}
		t.Fatalf("configureArtifactStorage for standalone live test: %v", err)
	}
	execution, err := configureRunEngine(storage, database, artifactStorage)
	if err != nil {
		if database != nil {
			database.Close()
		}
		t.Fatalf("configureRunEngine for standalone live test: %v", err)
	}

	handler := httpapi.New(storage, authenticator, httpapi.Options{
		MaxBodyBytes:       1 << 20,
		ProductionValidate: true,
		RunStarter:         execution.Starter,
		RunSignaler:        execution.Signaler,
		ArtifactStore:      artifactStorage,
		ReadyCheck: func(checkContext context.Context) error {
			var readiness []error
			if database != nil {
				readiness = append(readiness, database.PingContext(checkContext))
			}
			if execution.Ready != nil {
				readiness = append(readiness, execution.Ready(checkContext))
			}
			return errors.Join(readiness...)
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/auth/token", exchange)
	mux.Handle("/", otelhttp.NewHandler(handler, "agw.http.standalone-live"))
	httpServer := httptest.NewServer(mux)

	workerContext, workerCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := execution.Run(workerContext)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("local engine worker %s stopped unexpectedly: %v", workerSuffix, err)
		}
	}()

	plane := &standaloneControlPlane{
		server: httpServer, database: database, storage: storage, artifacts: artifactStorage,
		token:     strings.TrimSpace(os.Getenv("AGW_AUTH_TOKEN")),
		execution: execution, cancel: workerCancel, done: done,
	}
	t.Cleanup(plane.Close)
	return plane
}

func assertStandaloneArtifactCatalog(t *testing.T, plane *standaloneControlPlane, scope store.Scope, runID string) {
	t.Helper()
	body := "<!doctype html><title>Standalone artifact</title><p>catalog-e2e-ok</p>"
	metadata, err := plane.artifacts.Put(context.Background(), artifact.PutRequest{
		OrganizationID: scope.OrganizationID, ProjectID: scope.ProjectID, RunID: runID,
		Name: "standalone.html", MediaType: "text/html", Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatalf("store standalone artifact: %v", err)
	}
	version, err := artifactcatalog.NewRunOutput(artifactcatalog.PublishInput{
		ArtifactID: metadata.ID, VersionID: metadata.ID, Title: "Standalone artifact",
		URI:    "artifact://catalog/" + metadata.ID + "/" + metadata.ID,
		Digest: metadata.Digest, MediaType: metadata.MediaType, SizeBytes: metadata.SizeBytes,
		CreatedAt: time.Now().UTC(), ContentKind: artifactcatalog.ContentKindSinglePage,
	})
	if err != nil {
		t.Fatalf("create standalone artifact contract: %v", err)
	}
	document, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	catalog, ok := plane.storage.(store.ArtifactCatalog)
	if !ok {
		t.Fatal("standalone store does not implement artifact catalog")
	}
	if _, err := catalog.PutArtifactVersion(context.Background(), store.ArtifactVersion{
		Scope: scope, ArtifactID: version.ArtifactID, VersionID: version.VersionID,
		RunID: runID, VersionNumber: 1, Document: document,
		ContentObjectKey: metadata.ObjectKey, SourceObjectKey: metadata.ObjectKey,
	}); err != nil {
		t.Fatalf("publish standalone artifact: %v", err)
	}
	base := standaloneScopePath(scope) + "/artifacts/" + version.ArtifactID
	list := plane.get(standaloneScopePath(scope) + "/artifacts")
	if list.StatusCode != http.StatusOK || !strings.Contains(readResponseBody(t, list), version.ArtifactID) {
		t.Fatal("standalone artifact catalog did not return the published version")
	}
	content := plane.get(base + "/versions/" + version.VersionID + "/content")
	if content.StatusCode != http.StatusOK {
		t.Fatalf("standalone artifact content status=%d body=%s", content.StatusCode, readResponseBody(t, content))
	}
	if got := readResponseBody(t, content); got != body {
		t.Fatalf("standalone artifact content=%q", got)
	}
}

func (p *standaloneControlPlane) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		if p.server != nil {
			p.server.Close()
		}
		if p.cancel != nil {
			p.cancel()
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
		if p.execution.Close != nil {
			p.execution.Close()
		}
		if p.database != nil {
			p.database.Close()
		}
	})
}

func (p *standaloneControlPlane) get(path string) *http.Response {
	request, err := http.NewRequest(http.MethodGet, p.server.URL+path, nil)
	if err != nil {
		panic(err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	response, err := p.server.Client().Do(request)
	if err != nil {
		panic(err)
	}
	return response
}

func (p *standaloneControlPlane) doJSON(method, path string, body any) *http.Response {
	data, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	request, err := http.NewRequest(method, p.server.URL+path, strings.NewReader(string(data)))
	if err != nil {
		panic(err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.server.Client().Do(request)
	if err != nil {
		panic(err)
	}
	return response
}

func standaloneApplyResource(t *testing.T, server *httptest.Server, token string, scope store.Scope, resource spec.Resource) string {
	t.Helper()
	digest, err := spec.RevisionDigest(resource)
	if err != nil {
		t.Fatalf("digest %s/%s: %v", resource.Meta().Kind, resource.Meta().Metadata.Name, err)
	}
	document, err := spec.AsJSON(resource)
	if err != nil {
		t.Fatalf("encode %s/%s: %v", resource.Meta().Kind, resource.Meta().Metadata.Name, err)
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+standaloneScopePath(scope)+"/resources/"+resource.Meta().Kind+"/"+resource.Meta().Metadata.Name, strings.NewReader(string(document)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("apply %s/%s: %v", resource.Meta().Kind, resource.Meta().Metadata.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("apply %s/%s status=%d body=%s", resource.Meta().Kind, resource.Meta().Metadata.Name, response.StatusCode, body)
	}
	return digest
}

func standaloneSandboxProfile(namespace string) *spec.SandboxProfile {
	digest := "sha256:" + strings.Repeat("f", 64)
	return &spec.SandboxProfile{
		ResourceMeta: spec.ResourceMeta{
			TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: spec.KindSandboxProfile},
			Metadata: spec.ObjectMeta{Name: "standalone", Namespace: namespace},
		},
		Spec: spec.SandboxProfileSpec{
			Backend:    "podman",
			Image:      "ghcr.io/astatide/standalone-runtime@" + digest,
			Resources:  spec.ResourceLimits{CPU: "1", Memory: "256Mi", Disk: "1Gi", PIDs: 128},
			Filesystem: spec.FilesystemSpec{Root: "read-only", Workspace: "/workspace"},
			Network:    spec.NetworkSpec{Mode: "none", DirectInternet: false},
		},
	}
}

func standaloneAgent(namespace, profile string) *spec.Agent {
	digest := "sha256:" + strings.Repeat("f", 64)
	return &spec.Agent{
		ResourceMeta: spec.ResourceMeta{
			TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: spec.KindAgent},
			Metadata: spec.ObjectMeta{Name: "worker", Namespace: namespace},
		},
		Spec: spec.AgentSpec{
			Runtime:           spec.RuntimeSpec{Harness: "fake-test", Image: "ghcr.io/astatide/standalone-runtime@" + digest},
			Instructions:      spec.InstructionsSpec{Inline: "Return the deterministic live-test artifact."},
			SandboxProfileRef: profile,
			Limits:            spec.RunLimits{Timeout: "1m"},
		},
	}
}

func standaloneScopePath(scope store.Scope) string {
	return "/api/v1alpha1/organizations/" + scope.OrganizationID + "/projects/" + scope.ProjectID
}

func standaloneRunStatus(response map[string]any) string {
	data, ok := response["data"].(map[string]any)
	if !ok {
		return ""
	}
	status, _ := data["status"].(string)
	return status
}

func decodeJSONResponse(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	var value map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&value); err != nil {
		t.Fatalf("decode HTTP response: %v", err)
	}
	return value
}

func readResponseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "<unreadable response>"
	}
	return string(data)
}

func assertStandalonePersistedState(t *testing.T, database *sql.DB, scope store.Scope, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin persisted state check: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, scope.ProjectID); err != nil {
		t.Fatalf("set tenant for persisted state check: %v", err)
	}
	var status, state string
	if err := tx.QueryRowContext(ctx, `SELECT status::text,state::text FROM local_workflows WHERE organization_id=$1 AND project_id=$2 AND run_id=$3`, scope.OrganizationID, scope.ProjectID, runID).Scan(&status, &state); err != nil {
		t.Fatalf("read persisted local workflow: %v", err)
	}
	if status != "Succeeded" || !strings.Contains(state, `"succeeded"`) {
		t.Fatalf("persisted local workflow status=%q state=%s", status, state)
	}
}

func openStandaloneDatabase(t *testing.T, databaseURL string) *sql.DB {
	t.Helper()
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL test database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pingErr error
	for {
		pingErr = database.PingContext(ctx)
		if pingErr == nil {
			break
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			database.Close()
			t.Fatalf("ping PostgreSQL test database: %v", safeStandaloneError(pingErr, databaseURL))
		case <-timer.C:
		}
	}
	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	return database
}

func applyStandaloneMigrations(t *testing.T, database *sql.DB, databaseURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agw_standalone_live_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("create standalone migration ledger: %v", safeStandaloneError(err, databaseURL))
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve standalone migration directory")
	}
	migrationDir := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations")
	for _, name := range []string{"001_initial.sql", "002_run_signal_idempotency.sql", "003_local_orchestration.sql", "004_artifact_catalog.sql"} {
		var applied bool
		if err := database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agw_standalone_live_migrations WHERE version=$1)`, name).Scan(&applied); err != nil {
			t.Fatalf("read standalone migration ledger: %v", safeStandaloneError(err, databaseURL))
		}
		if applied {
			continue
		}
		data, err := os.ReadFile(filepath.Join(migrationDir, name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := database.ExecContext(ctx, string(data)); err != nil {
			t.Fatalf("apply migration %s: %v", name, safeStandaloneError(err, databaseURL))
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO agw_standalone_live_migrations(version) VALUES ($1)`, name); err != nil {
			t.Fatalf("record migration %s: %v", name, safeStandaloneError(err, databaseURL))
		}
	}
}

func bootstrapStandaloneTenant(t *testing.T, database *sql.DB) store.Scope {
	t.Helper()
	organizationID := standaloneUUID(t)
	projectID := standaloneUUID(t)
	scope := store.Scope{OrganizationID: organizationID, ProjectID: projectID}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin standalone tenant bootstrap: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, organizationID, projectID); err != nil {
		t.Fatalf("set bootstrap tenant context: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO organizations(id,name) VALUES ($1,$2)`, organizationID, "standalone-live-"+strings.TrimPrefix(organizationID, "00000000-0000-4")); err != nil {
		t.Fatalf("insert standalone organization: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,organization_id,name) VALUES ($1,$2,$3)`, projectID, organizationID, "standalone-live"); err != nil {
		t.Fatalf("insert standalone project: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit standalone tenant bootstrap: %v", err)
	}
	return scope
}

func standaloneUUID(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate standalone UUID: %v", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func standaloneSecret(t *testing.T, size int) string {
	t.Helper()
	if size < 32 {
		size = 32
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate standalone auth token: %v", err)
	}
	return hex.EncodeToString(raw)
}

func standalonePostgresURL(t *testing.T) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv("AGW_POSTGRES_TEST_URL")); value != "" {
		return value
	}
	if !standaloneTruthy(os.Getenv("AGW_STANDALONE_LIVE")) {
		t.Skip("set AGW_POSTGRES_TEST_URL, or AGW_STANDALONE_LIVE=1 to allow a disposable Docker PostgreSQL")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("AGW_STANDALONE_LIVE is set but Docker is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runStandaloneCommand(ctx, "docker", "info"); err != nil {
		t.Skip("AGW_STANDALONE_LIVE is set but the Docker daemon is unavailable")
	}
	name := fmt.Sprintf("agw-standalone-live-pg-%d", time.Now().UnixNano())
	password := standaloneSecret(t, 24)
	if err := runStandaloneCommand(ctx, "docker", "run", "--detach", "--name", name, "--publish", "127.0.0.1::5432", "--env", "POSTGRES_PASSWORD="+password, "--env", "POSTGRES_DB=agw_standalone_live", "postgres:latest"); err != nil {
		t.Fatalf("start disposable PostgreSQL container: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = runStandaloneCommand(cleanupContext, "docker", "rm", "--force", "--volumes", name)
	})
	portOutput, err := standaloneCommandOutput(ctx, "docker", "port", name, "5432/tcp")
	if err != nil {
		t.Fatalf("inspect disposable PostgreSQL port: %v", err)
	}
	line := strings.TrimSpace(strings.SplitN(portOutput, "\n", 2)[0])
	_, port, err := net.SplitHostPort(line)
	if err != nil || port == "" {
		t.Fatalf("invalid disposable PostgreSQL port mapping")
	}
	return (&url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: net.JoinHostPort("127.0.0.1", port), Path: "/agw_standalone_live", RawQuery: "sslmode=disable"}).String()
}

func runStandaloneCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func standaloneCommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stderr = io.Discard
	output, err := command.Output()
	return string(output), err
}

func standaloneTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func safeStandaloneError(err error, databaseURL string) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), databaseURL, "<database-url>"))
}

type standalonePeerUIDKey struct{}

type standaloneFakeRunner struct {
	server        *http.Server
	listener      net.Listener
	socketPath    string
	mu            sync.Mutex
	schedule      []workflow.ScheduleRunnerTaskInput
	status        int
	authenticated int
	lastTask      string
}

func startStandaloneFakeRunner(t *testing.T) *standaloneFakeRunner {
	t.Helper()
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "runner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on fake runner Unix socket: %v", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		t.Fatalf("harden fake runner socket: %v", err)
	}
	fake := &standaloneFakeRunner{listener: listener, socketPath: socketPath}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", fake.handleReady)
	mux.HandleFunc("/v1/tasks/schedule", fake.handleSchedule)
	mux.HandleFunc("/v1/tasks/status", fake.handleStatus)
	mux.HandleFunc("/v1/tasks/cancel", fake.handleNoContent)
	mux.HandleFunc("/v1/tasks/resume", fake.handleNoContent)
	fake.server = &http.Server{
		Handler: mux,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			uid, ok := standaloneUnixPeerUID(connection)
			if !ok {
				return ctx
			}
			return context.WithValue(ctx, standalonePeerUIDKey{}, uid)
		},
	}
	go func() { _ = fake.server.Serve(listener) }()
	t.Cleanup(fake.close)
	return fake
}

func (f *standaloneFakeRunner) authenticate(w http.ResponseWriter, r *http.Request) bool {
	uid, ok := r.Context().Value(standalonePeerUIDKey{}).(uint32)
	if !ok || int(uid) != os.Getuid() {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	f.mu.Lock()
	f.authenticated++
	f.mu.Unlock()
	return true
}

func (f *standaloneFakeRunner) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !f.authenticate(w, r) {
		return
	}
	writeStandaloneJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func (f *standaloneFakeRunner) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !f.authenticate(w, r) {
		return
	}
	var input workflow.ScheduleRunnerTaskInput
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.schedule = append(f.schedule, input)
	f.lastTask = "task-standalone-live"
	f.mu.Unlock()
	writeStandaloneJSON(w, http.StatusOK, map[string]any{
		"status": "succeeded", "task_id": "task-standalone-live", "output": standaloneArtifact(),
	})
}

func (f *standaloneFakeRunner) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !f.authenticate(w, r) {
		return
	}
	var input workflow.StatusRunnerTaskInput
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.status++
	f.mu.Unlock()
	writeStandaloneJSON(w, http.StatusOK, map[string]any{"status": "succeeded", "output": standaloneArtifact()})
}

func (f *standaloneFakeRunner) handleNoContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !f.authenticate(w, r) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *standaloneFakeRunner) close() {
	if f == nil {
		return
	}
	if f.server != nil {
		_ = f.server.Close()
	}
	if f.listener != nil {
		_ = f.listener.Close()
	}
	_ = os.Remove(f.socketPath)
}

func (f *standaloneFakeRunner) scheduleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.schedule)
}

func (f *standaloneFakeRunner) statusCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *standaloneFakeRunner) authenticatedRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authenticated
}

func (f *standaloneFakeRunner) lastSchedule() workflow.ScheduleRunnerTaskInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.schedule) == 0 {
		return workflow.ScheduleRunnerTaskInput{}
	}
	return f.schedule[len(f.schedule)-1]
}

func (f *standaloneFakeRunner) lastTaskID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTask
}

func standaloneArtifact() workflow.ArtifactRef {
	return workflow.ArtifactRef{ID: "standalone-live-artifact", URI: "memory://standalone-live/artifact", Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1, MediaType: "application/json"}
}

func writeStandaloneJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func standaloneUnixPeerUID(connection net.Conn) (uint32, bool) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	var credential *unix.Ucred
	var credentialErr error
	rawConnection, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, false
	}
	controlErr := rawConnection.Control(func(fd uintptr) {
		credential, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if controlErr != nil || credentialErr != nil || credential == nil {
		return 0, false
	}
	return credential.Uid, true
}
