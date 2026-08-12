package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func sandboxFixture(uid string, conditions ...Condition) Sandbox {
	var sandbox Sandbox
	sandbox.Metadata.Name = "phase0-argo-sandbox"
	sandbox.Metadata.Namespace = "agw-runs"
	sandbox.Metadata.UID = uid
	sandbox.Metadata.Generation = 1
	sandbox.Status.Conditions = conditions
	return sandbox
}

func condition(kind, status, reason, transition string) Condition {
	return Condition{Type: kind, Status: status, ObservedGeneration: 1, Reason: reason, LastTransitionTime: transition}
}

func TestEvaluateResolvesByTypeAndRejectsUnsafeState(t *testing.T) {
	const readyTime = "2026-08-11T12:00:00Z"
	const finishTime = "2026-08-11T12:00:05Z"
	tests := []struct {
		name    string
		sandbox Sandbox
		target  string
		want    Decision
		wantErr bool
	}{
		{
			name:    "missing condition remains pending",
			sandbox: sandboxFixture("uid-1"),
			target:  ConditionReady,
			want:    Pending,
		},
		{
			name: "ready true resolves by type regardless of order",
			sandbox: sandboxFixture("uid-2",
				condition(ConditionFinished, "False", "", finishTime),
				condition(ConditionReady, "True", "DependenciesReady", readyTime)),
			target: ConditionReady,
			want:   Satisfied,
		},
		{
			name: "upstream successful terminal shape is accepted",
			sandbox: sandboxFixture("uid-3",
				condition(ConditionFinished, "True", FinishedSucceeded, finishTime),
				condition(ConditionReady, "False", FinishedSucceeded, finishTime)),
			target: ConditionFinished,
			want:   Satisfied,
		},
		{
			name: "ready cannot be manufactured from terminal state",
			sandbox: sandboxFixture("uid-4",
				condition(ConditionFinished, "True", FinishedSucceeded, readyTime),
				condition(ConditionReady, "False", FinishedSucceeded, finishTime)),
			target:  ConditionReady,
			wantErr: true,
		},
		{
			name: "pod failure is rejected",
			sandbox: sandboxFixture("uid-5",
				condition(ConditionFinished, "True", FinishedFailed, finishTime),
				condition(ConditionReady, "True", "DependenciesReady", readyTime)),
			target:  ConditionFinished,
			wantErr: true,
		},
		{
			name: "unknown finished reason is rejected",
			sandbox: sandboxFixture("uid-6",
				condition(ConditionFinished, "True", "Other", finishTime),
				condition(ConditionReady, "True", "DependenciesReady", readyTime)),
			target:  ConditionFinished,
			wantErr: true,
		},
		{
			name: "duplicate condition is rejected",
			sandbox: sandboxFixture("uid-7",
				condition(ConditionReady, "True", "", readyTime),
				condition(ConditionReady, "True", "", readyTime)),
			target:  ConditionReady,
			wantErr: true,
		},
		{
			name: "stale observed generation is rejected",
			sandbox: func() Sandbox {
				value := sandboxFixture("uid-8", condition(ConditionReady, "True", "DependenciesReady", readyTime))
				value.Status.Conditions[0].ObservedGeneration = 0
				return value
			}(),
			target:  ConditionReady,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Evaluate(test.sandbox, test.target)
			if test.wantErr {
				if err == nil {
					t.Fatalf("Evaluate() error = nil, want error; evaluation=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != test.want {
				t.Fatalf("Evaluate() decision = %s, want %s", got.Decision, test.want)
			}
		})
	}
}

func TestWaitPollsAndWritesIdentityBoundMarkers(t *testing.T) {
	const readyTime = "2026-08-11T12:00:00Z"
	const finishTime = "2026-08-11T12:00:05Z"
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count := requests.Add(1)
		conditions := []Condition{condition(ConditionReady, "False", "DependenciesNotReady", readyTime)}
		if count >= 2 {
			conditions = []Condition{condition(ConditionReady, "True", "DependenciesReady", readyTime), condition(ConditionFinished, "False", "", finishTime)}
		}
		if count >= 3 {
			// This is the exact successful terminal condition shape emitted by
			// Agent Sandbox v0.5.4: Ready becomes False/PodSucceeded.
			conditions = []Condition{condition(ConditionFinished, "True", FinishedSucceeded, finishTime), condition(ConditionReady, "False", FinishedSucceeded, finishTime)}
		}
		sandbox := sandboxFixture("uid-live", conditions...)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(sandbox)
	}))
	defer server.Close()

	client := &Client{
		HTTPClient: server.Client(),
		ServerURL:  mustURL(t, server.URL),
	}
	markerDir := t.TempDir()
	readyMarker := filepath.Join(markerDir, "ready.json")
	finishedMarker := filepath.Join(markerDir, "finished.json")

	if err := Wait(context.Background(), client, Config{
		Namespace: "agw-runs", Name: "phase0-argo-sandbox", Condition: ConditionReady,
		Timeout: 2 * time.Second, PollInterval: time.Millisecond, MarkerPath: readyMarker,
	}, nil); err != nil {
		t.Fatalf("ready Wait() error = %v", err)
	}
	if err := Wait(context.Background(), client, Config{
		Namespace: "agw-runs", Name: "phase0-argo-sandbox", Condition: ConditionFinished,
		Timeout: 2 * time.Second, PollInterval: time.Millisecond, MarkerPath: finishedMarker,
		RequiredMarkerPath: readyMarker,
	}, nil); err != nil {
		t.Fatalf("finished Wait() error = %v", err)
	}
	if requests.Load() < 3 {
		t.Fatalf("HTTP requests = %d, want at least 3", requests.Load())
	}
	if err := verifyMarker(finishedMarker, "agw-runs", "phase0-argo-sandbox", "uid-live", ConditionFinished); err != nil {
		t.Fatalf("finished marker invalid: %v", err)
	}
}

func TestWaitFinishedRequiresReadyMarker(t *testing.T) {
	client := &Client{HTTPClient: http.DefaultClient, ServerURL: mustURL(t, "http://127.0.0.1")}
	err := Wait(context.Background(), client, Config{
		Namespace: "agw-runs", Name: "phase0-argo-sandbox", Condition: ConditionFinished,
		Timeout: time.Second, PollInterval: time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("Wait() error = nil without a required Ready marker")
	}
}

func TestNewClientRejectsMissingTokenAndUnsafeURLShape(t *testing.T) {
	if _, err := NewClient("https://kubernetes.default.svc", "", []byte("invalid")); err == nil {
		t.Fatal("NewClient() error = nil without bearer token")
	}
	for _, raw := range []string{
		"http://kubernetes.default.svc",
		"https://user@kubernetes.default.svc",
		"https://kubernetes.default.svc?token=leak",
		"https://kubernetes.default.svc#fragment",
	} {
		if _, err := NewClient(raw, "token", []byte("invalid")); err == nil {
			t.Fatalf("NewClient(%q) error = nil", raw)
		}
	}
}

func TestGetRejectsTrailingJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"apiVersion":"agents.x-k8s.io/v1beta1","kind":"Sandbox","metadata":{"name":"sandbox","namespace":"agw-runs","uid":"uid-live","generation":1},"status":{"conditions":[]}}{}`))
	}))
	defer server.Close()

	client := &Client{HTTPClient: server.Client(), ServerURL: mustURL(t, server.URL)}
	if _, err := client.Get(context.Background(), "agw-runs", "sandbox"); err == nil {
		t.Fatal("Get() error = nil for trailing JSON")
	}
}

func TestVerifyMarkerRejectsUnknownAndTrailingJSON(t *testing.T) {
	base := fmt.Sprintf(`{"version":%q,"namespace":"agw-runs","name":"phase0-argo-sandbox","uid":"uid-live","condition":"Ready","observedAt":"2026-08-11T12:00:00Z"}`, MarkerVersion)
	for name, data := range map[string]string{
		"unknown":  base[:len(base)-1] + `,"extra":true}`,
		"trailing": base + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "marker.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyMarker(path, "agw-runs", "phase0-argo-sandbox", "uid-live", ConditionReady); err == nil {
				t.Fatal("verifyMarker() error = nil for malformed marker")
			}
		})
	}
}

func TestWaitFailsOnAuthorizationAndStaleMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer server.Close()
	client := &Client{HTTPClient: server.Client(), ServerURL: mustURL(t, server.URL)}
	if err := Wait(context.Background(), client, Config{
		Namespace: "agw-runs", Name: "phase0-argo-sandbox", Condition: ConditionReady,
		Timeout: time.Second, PollInterval: time.Millisecond,
	}, nil); err == nil {
		t.Fatal("Wait() error = nil for HTTP 403")
	}

	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sandbox := sandboxFixture("new-uid", condition(ConditionReady, "True", "DependenciesReady", "2026-08-11T12:00:00Z"))
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(sandbox)
	}))
	defer server.Close()
	client = &Client{HTTPClient: server.Client(), ServerURL: mustURL(t, server.URL)}
	markerPath := filepath.Join(t.TempDir(), "ready.json")
	if err := os.WriteFile(markerPath, []byte(fmt.Sprintf(`{"version":%q,"namespace":"agw-runs","name":"phase0-argo-sandbox","uid":"old-uid","condition":"Ready","observedAt":"2026-08-11T12:00:00Z"}`, MarkerVersion)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Wait(context.Background(), client, Config{
		Namespace: "agw-runs", Name: "phase0-argo-sandbox", Condition: ConditionFinished,
		Timeout: time.Second, PollInterval: time.Millisecond, RequiredMarkerPath: markerPath,
	}, nil); err == nil {
		t.Fatal("Wait() error = nil for stale marker")
	}
}

func TestWaitFailsImmediatelyWhenSandboxDisappears(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"sandbox deleted"}`))
	}))
	defer server.Close()

	client := &Client{HTTPClient: server.Client(), ServerURL: mustURL(t, server.URL)}
	err := Wait(context.Background(), client, Config{
		Namespace: "agw-runs", Name: "phase0-argo-sandbox", Condition: ConditionReady,
		Timeout: time.Second, PollInterval: time.Millisecond,
	}, nil)
	if !errors.Is(err, ErrSandboxMissing) {
		t.Fatalf("Wait() error=%v, want ErrSandboxMissing", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests=%d, want one terminal read", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
