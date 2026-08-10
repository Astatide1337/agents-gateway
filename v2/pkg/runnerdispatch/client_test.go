package runnerdispatch

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"http://runner", "https://user@runner", "https://runner/path", "https://runner?x=1"} {
		if _, err := validateEndpoint(endpoint); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if endpoint, err := validateEndpoint("https://runner.internal"); err != nil || endpoint != "https://runner.internal" {
		t.Fatalf("endpoint=%q err=%v", endpoint, err)
	}
}

func TestUnixClientUsesOnlyConfiguredSocket(t *testing.T) {
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "runner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "agw-runner" || r.URL.Path != "/v1/tasks/status" {
			t.Fatalf("unexpected Unix request: host=%q path=%q", r.Host, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"running"}`)
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	client, err := NewUnix(socketPath, time.Second)
	if err != nil {
		t.Fatalf("new Unix client: %v", err)
	}
	result, err := client.StatusRunnerTask(context.Background(), agentworkflow.StatusRunnerTaskInput{
		OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: "task",
	})
	if err != nil || result.Status != "running" {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	_ = listener.Close()
	_ = os.Remove(socketPath)
	if _, err := client.StatusRunnerTask(context.Background(), agentworkflow.StatusRunnerTaskInput{}); err == nil {
		t.Fatal("Unix client must not fall back to TCP when its socket is absent")
	}
}

func TestUnixClientRejectsUnsafeSocketPaths(t *testing.T) {
	for _, path := range []string{"", ".", "runner.sock", "/"} {
		if _, err := NewUnix(path, time.Second); err == nil {
			t.Fatalf("accepted unsafe Unix socket path %q", path)
		}
	}
}

func TestStrictBoundedDispatch(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/tasks/schedule" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request: %s %#v", request.URL.Path, request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"idempotency_key":"run/workflow/step"`) {
			t.Fatalf("idempotency key missing: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"scheduled","task_id":"task-1"}`)), Header: make(http.Header)}, nil
	})
	client := &Client{endpoint: "https://runner.internal", http: &http.Client{Transport: transport, Timeout: time.Second}}
	result, err := client.ScheduleRunnerTask(context.Background(), agentworkflow.ScheduleRunnerTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "workflow", StepID: "step", AgentRef: "agent/fixer", IdempotencyKey: "run/workflow/step"})
	if err != nil || result.Status != "scheduled" || result.TaskID != "task-1" {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"scheduled","task_id":"task-1","secret":"leak"}`)), Header: make(http.Header)}, nil
	})
	if _, err := client.ScheduleRunnerTask(context.Background(), agentworkflow.ScheduleRunnerTaskInput{IdempotencyKey: "key"}); err == nil {
		t.Fatal("unknown response fields must be rejected")
	}
}

func TestStatusCarriesRuntimeCursorAndBoundedEvents(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/tasks/status" {
			t.Fatalf("unexpected status path: %s", request.URL.Path)
		}
		var input agentworkflow.StatusRunnerTaskInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatalf("decode status request: %v", err)
		}
		if input.AfterSequence != 17 {
			t.Fatalf("after cursor=%d, want 17", input.AfterSequence)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"status":"running","event_cursor":19,"event_history_start":18,"events":[{"sequence":18,"type":"model.requested","payload":{"model":"test"}},{"sequence":19,"type":"assistant.message","payload":{"message":"ok"}}]}`)),
			Header:     make(http.Header),
		}, nil
	})
	client := &Client{endpoint: "https://runner.internal", http: &http.Client{Transport: transport, Timeout: time.Second}}
	result, err := client.StatusRunnerTask(context.Background(), agentworkflow.StatusRunnerTaskInput{
		OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: "task", AfterSequence: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "running" || result.EventCursor != 19 || len(result.Events) != 2 || result.Events[1].Type != "assistant.message" {
		t.Fatalf("unexpected runtime status: %#v", result)
	}
}

func TestReadyUsesAuthenticatedClientAndFailsClosed(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/readyz" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("unexpected readiness request: %s %s %#v", request.Method, request.URL.Path, request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ready":true}`)), Header: make(http.Header)}, nil
	})
	client := &Client{endpoint: "https://runner.internal", http: &http.Client{Transport: transport, Timeout: time.Second}}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if requests != 1 {
		t.Fatalf("readiness requests=%d", requests)
	}
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"ready":false}`)), Header: make(http.Header)}, nil
	})
	if err := client.Ready(context.Background()); err == nil {
		t.Fatal("unready runner was accepted")
	}
}
