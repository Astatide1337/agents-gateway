package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
)

var testRuntimeExitToken = []byte("runtime-exit-token-32-bytes-long!!")

func newTestRuntimeHTTPHandler(t *testing.T) (*RuntimeHandler, *RuntimeSupervisor) {
	t.Helper()
	supervisor, err := NewRuntimeSupervisor(RuntimeConfig{
		RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA, Store: newTestObjectStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewRuntimeHandler(RuntimeHandlerConfig{Supervisor: supervisor, ProcessExitToken: testRuntimeExitToken})
	if err != nil {
		t.Fatal(err)
	}
	return handler, supervisor
}

func runtimeRequest(path, contentType string, body []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:43123"
	request.Header.Set("Content-Type", contentType)
	return request
}

func postRuntimeEvent(handler http.Handler, line []byte) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeRequest(RuntimeEventsPath, RuntimeEventMediaType, line))
	return recorder
}

func postRuntimeExit(t *testing.T, handler http.Handler, code int, token []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, err := RuntimeProcessExitPayload(code)
	if err != nil {
		t.Fatal(err)
	}
	request := runtimeRequest(RuntimeProcessExitPath, "application/json", body)
	if len(token) > 0 {
		request.Header.Set("Authorization", "Bearer "+string(token))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func postRuntimePhase(t *testing.T, handler http.Handler, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := RuntimePhaseTransitionPayload(requestID, testRunUID, testSpecDigest, testBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	return postRuntimePhaseBody(handler, body)
}

func postRuntimePhaseBody(handler http.Handler, body []byte) *httptest.ResponseRecorder {
	request := runtimeRequest(RuntimePhasePath, "application/json", body)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func newTestRuntimePhaseHandler(t *testing.T, target TrustedPhaseTransitioner) *RuntimeHandler {
	t.Helper()
	supervisor, err := NewRuntimeSupervisor(RuntimeConfig{
		RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA, Store: newTestObjectStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	phaseSupervisor := NewPhaseSupervisor()
	handler, err := NewRuntimeHandler(RuntimeHandlerConfig{
		Supervisor: supervisor, ProcessExitToken: testRuntimeExitToken,
		PhaseTransitioner: target, PhaseSupervisor: phaseSupervisor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func newTestPairedRuntimePhaseHandler(t *testing.T) (*RuntimeHandler, *Broker) {
	t.Helper()
	return newTestPairedRuntimePhaseHandlerForIdentity(t, testRunUID, testSpecDigest, testBaseSHA)
}

func newTestPairedRuntimePhaseHandlerForIdentity(t *testing.T, runUID, specDigest, baseSHA string) (*RuntimeHandler, *Broker) {
	t.Helper()
	config := newTestConfig(testBrokerOptions{})
	config.RunUID, config.SpecDigest, config.BaseSHA = runUID, specDigest, baseSHA
	phaseSupervisor := NewPhaseSupervisor()
	config.PhaseTransitionAuthorizer = phaseSupervisor.Authorizer()
	target, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRuntimeSupervisor(RuntimeConfig{
		RunUID: runUID, SpecDigest: specDigest, BaseSHA: baseSHA, Store: newTestObjectStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewRuntimeHandler(RuntimeHandlerConfig{
		Supervisor: supervisor, ProcessExitToken: testRuntimeExitToken,
		PhaseTransitioner: target, PhaseSupervisor: phaseSupervisor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, target
}

func TestRuntimeHandlerEarlyTerminalCannotFinalizeWithoutProcessExit(t *testing.T) {
	handler, supervisor := newTestRuntimeHTTPHandler(t)
	started := runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)
	terminal := runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`)
	if response := postRuntimeEvent(handler, started); response.Code != http.StatusNoContent {
		t.Fatalf("run.started status=%d body=%s", response.Code, response.Body.String())
	}
	if response := postRuntimeEvent(handler, terminal); response.Code != http.StatusNoContent {
		t.Fatalf("run.completed status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := supervisor.Completion(); !errors.Is(err, runtimeevents.ErrNotReady) {
		t.Fatalf("agent-authored terminal produced completion: %v", err)
	}
	if response := postRuntimeExit(t, handler, 0, nil); response.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated process exit status=%d", response.Code)
	}
	if _, err := supervisor.Completion(); !errors.Is(err, runtimeevents.ErrNotReady) {
		t.Fatalf("unauthenticated process exit produced completion: %v", err)
	}

	response := postRuntimeExit(t, handler, 0, testRuntimeExitToken)
	if response.Code != http.StatusCreated {
		t.Fatalf("authenticated process exit status=%d body=%s", response.Code, response.Body.String())
	}
	var completion runtimeevents.CompletionRecord
	if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	if completion.RunUID != testRunUID || completion.SpecDigest != testSpecDigest || completion.BaseSHA != testBaseSHA || completion.TerminalType != runtimeevents.TerminalCompleted {
		t.Fatalf("completion identity=%#v", completion)
	}
}

func TestRuntimeHandlerProvesExactEventReplayAndRejectsConflict(t *testing.T) {
	handler, supervisor := newTestRuntimeHTTPHandler(t)
	started := runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)
	if response := postRuntimeEvent(handler, started); response.Code != http.StatusNoContent {
		t.Fatalf("initial event status=%d", response.Code)
	}
	replay := postRuntimeEvent(handler, started)
	if replay.Code != http.StatusNoContent || replay.Header().Get("X-AGW-Idempotent-Replay") != "true" {
		t.Fatalf("exact replay status=%d headers=%v", replay.Code, replay.Header())
	}
	conflict := runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"different","sandbox_id":"sandbox"}`)
	if response := postRuntimeEvent(handler, conflict); response.Code != http.StatusConflict {
		t.Fatalf("conflicting replay status=%d body=%s", response.Code, response.Body.String())
	}
	if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventHeartbeat, 2, false, `{}`)); response.Code != http.StatusConflict {
		t.Fatalf("poisoned stream accepted event: status=%d", response.Code)
	}
	if projection := supervisor.collector.Projection(); projection.AcceptedFrames != 1 || projection.NextSeq != 2 {
		t.Fatalf("projection after replay conflict=%#v", projection)
	}
}

func TestRuntimeHandlerMalformedAndOutOfOrderEventsFailClosed(t *testing.T) {
	t.Run("out of order", func(t *testing.T) {
		handler, _ := newTestRuntimeHTTPHandler(t)
		if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventHeartbeat, 2, false, `{}`)); response.Code != http.StatusConflict {
			t.Fatalf("out-of-order status=%d", response.Code)
		}
		if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)); response.Code != http.StatusConflict {
			t.Fatalf("event accepted after sequence rejection: %d", response.Code)
		}
	})

	t.Run("multiple lines", func(t *testing.T) {
		handler, _ := newTestRuntimeHTTPHandler(t)
		body := append(runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`), runtimeLine(t, proto.EventHeartbeat, 2, false, `{}`)...)
		if response := postRuntimeEvent(handler, body); response.Code != http.StatusBadRequest {
			t.Fatalf("multi-line status=%d", response.Code)
		}
		if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)); response.Code != http.StatusConflict {
			t.Fatalf("event accepted after malformed request: %d", response.Code)
		}
	})

	t.Run("wrong identity", func(t *testing.T) {
		handler, _ := newTestRuntimeHTTPHandler(t)
		line := runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)
		line = bytes.Replace(line, []byte(`"run_id":"`+testRunUID+`"`), []byte(`"run_id":"caller-selected"`), 1)
		if response := postRuntimeEvent(handler, line); response.Code != http.StatusBadRequest {
			t.Fatalf("wrong identity status=%d", response.Code)
		}
	})
}

func TestRuntimeHandlerTerminalReplayAndPostTerminalData(t *testing.T) {
	handler, supervisor := newTestRuntimeHTTPHandler(t)
	started := runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)
	terminal := runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`)
	_ = postRuntimeEvent(handler, started)
	_ = postRuntimeEvent(handler, terminal)
	if response := postRuntimeEvent(handler, terminal); response.Code != http.StatusNoContent || response.Header().Get("X-AGW-Idempotent-Replay") != "true" {
		t.Fatalf("terminal replay status=%d", response.Code)
	}
	if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventHeartbeat, 3, false, `{}`)); response.Code != http.StatusConflict {
		t.Fatalf("post-terminal event status=%d", response.Code)
	}
	if response := postRuntimeExit(t, handler, 0, testRuntimeExitToken); response.Code != http.StatusConflict {
		t.Fatalf("poisoned terminal stream finalized: %d", response.Code)
	}
	if _, err := supervisor.Completion(); err == nil {
		t.Fatal("poisoned terminal stream produced completion")
	}
}

func TestRuntimeHandlerConflictingDuplicateTerminalFailsClosed(t *testing.T) {
	handler, supervisor := newTestRuntimeHTTPHandler(t)
	_ = postRuntimeEvent(handler, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`))
	_ = postRuntimeEvent(handler, runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`))
	conflicting := runtimeLine(t, proto.EventRunFailed, 2, true, `{"error":{"code":"different_terminal","message":"failed"}}`)
	if response := postRuntimeEvent(handler, conflicting); response.Code != http.StatusConflict {
		t.Fatalf("conflicting terminal status=%d body=%s", response.Code, response.Body.String())
	}
	if response := postRuntimeExit(t, handler, 0, testRuntimeExitToken); response.Code != http.StatusConflict {
		t.Fatalf("conflicting terminal stream finalized: %d", response.Code)
	}
	if _, err := supervisor.Completion(); err == nil {
		t.Fatal("conflicting terminal stream produced completion")
	}
}

func TestRuntimeHandlerProcessExitReplayIsExact(t *testing.T) {
	handler, supervisor := newTestRuntimeHTTPHandler(t)
	_ = postRuntimeEvent(handler, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`))
	_ = postRuntimeEvent(handler, runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`))
	first := postRuntimeExit(t, handler, 0, testRuntimeExitToken)
	if first.Code != http.StatusCreated {
		t.Fatalf("first process exit status=%d body=%s", first.Code, first.Body.String())
	}
	replay := postRuntimeExit(t, handler, 0, testRuntimeExitToken)
	if replay.Code != http.StatusOK || replay.Header().Get("X-AGW-Idempotent-Replay") != "true" || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("process-exit replay status=%d headers=%v", replay.Code, replay.Header())
	}
	if response := postRuntimeExit(t, handler, 1, testRuntimeExitToken); response.Code != http.StatusConflict {
		t.Fatalf("conflicting process exit status=%d", response.Code)
	}
	completion, err := supervisor.Completion()
	if err != nil || completion.TerminalType != runtimeevents.TerminalCompleted {
		t.Fatalf("immutable completion=%#v error=%v", completion, err)
	}
}

func TestRuntimeHandlerEarlyOrMalformedProcessExitClosesBoundary(t *testing.T) {
	t.Run("exit before terminal", func(t *testing.T) {
		handler, supervisor := newTestRuntimeHTTPHandler(t)
		_ = postRuntimeEvent(handler, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`))
		if response := postRuntimeExit(t, handler, 0, testRuntimeExitToken); response.Code != http.StatusConflict {
			t.Fatalf("early exit status=%d", response.Code)
		}
		if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`)); response.Code != http.StatusConflict {
			t.Fatalf("terminal accepted after process exit: %d", response.Code)
		}
		if _, err := supervisor.Completion(); !errors.Is(err, runtimeevents.ErrNoTerminal) {
			t.Fatalf("early exit completion error=%v", err)
		}
	})

	t.Run("malformed exit", func(t *testing.T) {
		handler, supervisor := newTestRuntimeHTTPHandler(t)
		request := runtimeRequest(RuntimeProcessExitPath, "application/json", []byte(`{"schema":"agents-gateway.process-exit.v1","code":0,"runUID":"caller"}`))
		request.Header.Set("Authorization", "Bearer "+string(testRuntimeExitToken))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("malformed exit status=%d", recorder.Code)
		}
		if response := postRuntimeEvent(handler, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)); response.Code != http.StatusConflict {
			t.Fatalf("event accepted after malformed exit: %d", response.Code)
		}
		if _, err := supervisor.Completion(); !errors.Is(err, runtimeevents.ErrNotReady) {
			t.Fatalf("malformed exit completion error=%v", err)
		}
	})
}

func TestRuntimeHandlerConcurrentExactReplayIsSingleAcceptance(t *testing.T) {
	handler, supervisor := newTestRuntimeHTTPHandler(t)
	line := runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)
	const callers = 16
	statuses := make(chan int, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses <- postRuntimeEvent(handler, line).Code
		}()
	}
	wait.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusNoContent {
			t.Fatalf("concurrent replay status=%d", status)
		}
	}
	if projection := supervisor.collector.Projection(); projection.AcceptedFrames != 1 || projection.NextSeq != 2 {
		t.Fatalf("concurrent replay projection=%#v", projection)
	}
}

func TestRuntimeHandlerConstructionAndLoopbackBoundary(t *testing.T) {
	supervisor, err := NewRuntimeSupervisor(RuntimeConfig{RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA, Store: newTestObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntimeHandler(RuntimeHandlerConfig{Supervisor: supervisor, ProcessExitToken: []byte("short")}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("short exit token error=%v", err)
	}
	handler, err := NewRuntimeHandler(RuntimeHandlerConfig{Supervisor: supervisor, ProcessExitToken: testRuntimeExitToken})
	if err != nil {
		t.Fatal(err)
	}
	request := runtimeRequest(RuntimeEventsPath, RuntimeEventMediaType, runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`))
	request.RemoteAddr = "198.51.100.10:43123"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("remote runtime request status=%d", recorder.Code)
	}
	if _, err := RuntimeProcessExitPayload(256); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid exit code error=%v", err)
	}
	if body, err := RuntimeProcessExitPayload(-1); err != nil || !strings.Contains(string(body), `"code":-1`) || strings.Contains(string(body), testRunUID) {
		t.Fatalf("process-exit payload=%s error=%v", body, err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := supervisor.ProcessExit(cancelled, runtimeevents.ProcessExit{Known: true}); err == nil {
		t.Fatal("cancelled process exit was accepted")
	}
}

func TestRuntimeHandlerPhaseTransitionRequiresLoopbackAndHostCapability(t *testing.T) {
	withoutPhase, _ := newTestRuntimeHTTPHandler(t)
	if response := postRuntimePhase(t, withoutPhase, "request-1"); response.Code != http.StatusNotFound {
		t.Fatalf("disabled phase endpoint status=%d, want not found", response.Code)
	}

	missing := newTestConfig(testBrokerOptions{})
	missing.PhaseTransitionAuthorizer = nil
	missingBroker, err := New(missing)
	if err != nil {
		t.Fatal(err)
	}
	missingHandler := newTestRuntimePhaseHandler(t, missingBroker)
	if response := postRuntimePhase(t, missingHandler.TrustedPhaseHandler(), "request-1"); response.Code != http.StatusForbidden {
		t.Fatalf("missing broker authority status=%d, want forbidden", response.Code)
	}
	// The agent-facing handler never receives the private supervisor capability.
	body, err := RuntimePhaseTransitionPayload("request-remote", testRunUID, testSpecDigest, testBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	request := runtimeRequest(RuntimePhasePath, "application/json", body)
	request.RemoteAddr = "127.0.0.1:4321"
	recorder := httptest.NewRecorder()
	missingHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("remote phase status=%d, want not found", recorder.Code)
	}
	if got := missingBroker.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("missing-authority phase=%q, want explore", got)
	}
}

func TestRuntimeHandlerPhaseTransitionIsHostAuthorizedAndIdempotent(t *testing.T) {
	handler, b := newTestPairedRuntimePhaseHandler(t)
	trusted := handler.TrustedPhaseHandler()
	first := postRuntimePhase(t, trusted, "request-1")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first phase transition status=%d body=%s", first.Code, first.Body.String())
	}
	replay := postRuntimePhase(t, trusted, "request-1")
	if replay.Code != http.StatusNoContent || replay.Header().Get("X-AGW-Idempotent-Replay") != "true" {
		t.Fatalf("phase replay status=%d headers=%v", replay.Code, replay.Header())
	}
	if conflict := postRuntimePhase(t, trusted, "request-2"); conflict.Code != http.StatusConflict {
		t.Fatalf("conflicting phase request status=%d, want conflict", conflict.Code)
	}
	if unauthorized := postRuntimePhase(t, handler, "request-1"); unauthorized.Code != http.StatusNotFound {
		t.Fatalf("agent-facing replay status=%d, want not found", unauthorized.Code)
	}
	if got := b.CurrentPhase(); got != v1alpha1.ToolProfileEdit {
		t.Fatalf("phase=%q, want edit", got)
	}
}

func TestRuntimeHandlerPhaseTransitionBindsImmutableRunIdentity(t *testing.T) {
	foreignHandler, foreignBroker := newTestPairedRuntimePhaseHandlerForIdentity(t,
		"other-run", "sha256:"+strings.Repeat("c", 64), strings.Repeat("d", 40))
	body, err := RuntimePhaseTransitionPayload("cross-run", testRunUID, testSpecDigest, testBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	response := postRuntimePhaseBody(foreignHandler.TrustedPhaseHandler(), body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("cross-run phase status=%d body=%s, want bad request", response.Code, response.Body.String())
	}
	if got := foreignBroker.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("cross-run request changed phase to %q", got)
	}

	stale, err := RuntimePhaseTransitionPayload("stale-spec", "other-run", testSpecDigest, strings.Repeat("d", 40))
	if err != nil {
		t.Fatal(err)
	}
	response = postRuntimePhaseBody(foreignHandler.TrustedPhaseHandler(), stale)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("stale phase status=%d body=%s, want bad request", response.Code, response.Body.String())
	}
	if got := foreignBroker.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("stale request changed phase to %q", got)
	}
}

func TestRuntimeHandlerPhaseTransitionDeniesInvalidBrokerAuthority(t *testing.T) {
	config := newTestConfig(testBrokerOptions{})
	config.PhaseTransitionAuthorizer = PhaseTransitionAuthorizerFunc(func(context.Context) error {
		return ErrPhaseTransitionDenied
	})
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	handler := newTestRuntimePhaseHandler(t, b)
	trusted := handler.TrustedPhaseHandler()
	first := postRuntimePhase(t, trusted, "request-1")
	if first.Code != http.StatusForbidden {
		t.Fatalf("invalid broker authority status=%d, want forbidden", first.Code)
	}
	replay := postRuntimePhase(t, trusted, "request-1")
	if replay.Code != http.StatusForbidden || replay.Header().Get("X-AGW-Idempotent-Replay") != "true" {
		t.Fatalf("denied phase replay status=%d headers=%v", replay.Code, replay.Header())
	}
	if got := b.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("phase=%q, want explore", got)
	}
}

func TestTrustedPhaseHandlerRoundTripsOnlyOverPrivateSocket(t *testing.T) {
	runtimeHandler, b := newTestPairedRuntimePhaseHandler(t)
	path := filepath.Join(t.TempDir(), "phase.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := &http.Server{Handler: runtimeHandler.TrustedPhaseHandler()}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	client := &http.Client{Transport: transport}
	body, err := RuntimePhaseTransitionPayload("unix-request", testRunUID, testSpecDigest, testBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://phase.local"+RuntimePhasePath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		_ = server.Close()
		t.Fatal(err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if response.StatusCode != http.StatusNoContent {
		_ = server.Close()
		t.Fatalf("trusted Unix phase status=%d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if got := b.CurrentPhase(); got != v1alpha1.ToolProfileEdit {
		_ = server.Close()
		t.Fatalf("phase=%q, want edit", got)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
	transport.CloseIdleConnections()
}
