package runbroker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct{ nanos atomic.Int64 }

func newTestClock() *testClock {
	clock := &testClock{}
	clock.nanos.Store(time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC).UnixNano())
	return clock
}

func (clock *testClock) Now() time.Time { return time.Unix(0, clock.nanos.Load()).UTC() }

func (clock *testClock) Advance(duration time.Duration) {
	clock.nanos.Add(duration.Nanoseconds())
}

func testCreateRequest() CreateRequest {
	return CreateRequest{
		Binding: SessionBinding{
			OrgID:     "org-1",
			ProjectID: "project-1",
			UserID:    "user-1",
			RunID:     "run-1",
		},
		AllowedModels: []string{"model-a", "model-b"},
		PolicyDigest:  "sha256:policy-1",
		TTL:           15 * time.Minute,
	}
}

func newTestManager(t *testing.T, clock *testClock) *Manager {
	t.Helper()
	manager, err := NewManager(ManagerConfig{Clock: clock.Now, MaxTTL: time.Hour})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

func authorizationFor(result CreateResult) AuthorizeRequest {
	return AuthorizeRequest{
		SessionID:    result.Session.ID,
		Token:        result.Token,
		Binding:      result.Session.Binding,
		Model:        result.Session.AllowedModels[0],
		PolicyDigest: result.Session.PolicyDigest,
	}
}

func TestCreateAndAuthorizeStoresOnlyHash(t *testing.T) {
	clock := newTestClock()
	manager := newTestManager(t, clock)
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Session.ID == "" || result.Token == "" {
		t.Fatal("Create returned empty session ID or token")
	}
	if result.Session.ID == SessionID(result.Token) {
		t.Fatal("session ID and token unexpectedly match")
	}
	if got := manager.sessions[result.Session.ID].tokenHash; got != sha256.Sum256([]byte(result.Token)) {
		t.Fatal("manager does not store the SHA-256 token hash")
	}
	if got := fmt.Sprintf("%v", manager.sessions[result.Session.ID]); containsString(got, string(result.Token)) {
		t.Fatal("raw bearer token appears in the stored record")
	}

	authorized, err := manager.Authorize(context.Background(), authorizationFor(result))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if authorized.ID != result.Session.ID || authorized.PolicyDigest != result.Session.PolicyDigest {
		t.Fatalf("unexpected authorized session: %#v", authorized)
	}
	// Public slices are defensive copies; callers cannot mutate policy state.
	authorized.AllowedModels[0] = "mutated"
	authorizedAgain, err := manager.Authorize(context.Background(), authorizationFor(result))
	if err != nil || authorizedAgain.AllowedModels[0] != "model-a" {
		t.Fatalf("session policy was mutable through returned slice: %v %#v", err, authorizedAgain)
	}
}

func TestCreateProducesIndependentRandomCredentials(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	seenIDs := make(map[SessionID]struct{})
	seenTokens := make(map[BearerToken]struct{})
	for i := 0; i < 128; i++ {
		request := testCreateRequest()
		request.Binding.RunID = fmt.Sprintf("run-%d", i)
		result, err := manager.Create(context.Background(), request)
		if err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
		if _, exists := seenIDs[result.Session.ID]; exists {
			t.Fatal("duplicate session ID")
		}
		if _, exists := seenTokens[result.Token]; exists {
			t.Fatal("duplicate bearer token")
		}
		seenIDs[result.Session.ID] = struct{}{}
		seenTokens[result.Token] = struct{}{}
	}
}

func TestAuthorizeRejectsAdversarialScopes(t *testing.T) {
	clock := newTestClock()
	manager := newTestManager(t, clock)
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	wrongToken, err := randomOpaque(tokenPrefix)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		edit func(*AuthorizeRequest)
		want error
	}{
		{name: "cross run", edit: func(request *AuthorizeRequest) { request.Binding.RunID = "run-2" }, want: ErrCrossRun},
		{name: "cross project", edit: func(request *AuthorizeRequest) { request.Binding.ProjectID = "project-2" }, want: ErrWrongBinding},
		{name: "wrong policy", edit: func(request *AuthorizeRequest) { request.PolicyDigest = "sha256:other" }, want: ErrWrongPolicy},
		{name: "wrong model", edit: func(request *AuthorizeRequest) { request.Model = "model-denied" }, want: ErrWrongModel},
		{name: "valid wrong token", edit: func(request *AuthorizeRequest) {
			request.Token = BearerToken(wrongToken)
		}, want: ErrUnauthorized},
		{name: "malformed token", edit: func(request *AuthorizeRequest) { request.Token = BearerToken("not-a-token") }, want: ErrMalformedToken},
		{name: "malformed session", edit: func(request *AuthorizeRequest) { request.SessionID = "run-1" }, want: ErrMalformedSessionID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := authorizationFor(result)
			test.edit(&request)
			if _, err := manager.Authorize(context.Background(), request); !errors.Is(err, test.want) {
				t.Fatalf("Authorize error = %v, want %v", err, test.want)
			}
		})
	}

	clock.Advance(16 * time.Minute)
	if _, err := manager.Authorize(context.Background(), authorizationFor(result)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired session error = %v", err)
	}
}

func TestRevokeIsIdempotentAndContextAware(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(context.Background(), result.Session.ID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := manager.Revoke(context.Background(), result.Session.ID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	unknownID, err := randomOpaque(sessionPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(context.Background(), SessionID(unknownID)); err != nil {
		t.Fatalf("unknown Revoke: %v", err)
	}
	if _, err := manager.Authorize(context.Background(), authorizationFor(result)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked session error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Revoke(canceled, result.Session.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Revoke = %v", err)
	}
}

func TestDeleteForgetsCompletedSession(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(context.Background(), result.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(context.Background(), result.Session.ID); err != nil {
		t.Fatal(err)
	}
	if manager.SessionCount() != 0 {
		t.Fatalf("completed session was retained: %d", manager.SessionCount())
	}
	if _, err := manager.Authorize(context.Background(), authorizationFor(result)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("forgotten session authorization = %v", err)
	}
	if err := manager.Delete(context.Background(), result.Session.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestManagerConcurrentAuthorizeAndRevoke(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	request := authorizationFor(result)
	var group sync.WaitGroup
	for i := 0; i < 64; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = manager.Authorize(context.Background(), request)
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		_ = manager.Revoke(context.Background(), result.Session.ID)
	}()
	group.Wait()
	if _, err := manager.Authorize(context.Background(), request); !errors.Is(err, ErrRevoked) {
		t.Fatalf("post-race authorization = %v", err)
	}
}

func TestCreateValidationAndCancellation(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	cases := []CreateRequest{
		{},
		func() CreateRequest { request := testCreateRequest(); request.AllowedModels = nil; return request }(),
		func() CreateRequest { request := testCreateRequest(); request.TTL = 2 * time.Hour; return request }(),
		func() CreateRequest { request := testCreateRequest(); request.Binding.RunID = " bad"; return request }(),
	}
	for i, request := range cases {
		if _, err := manager.Create(context.Background(), request); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid request %d error = %v", i, err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Create(canceled, testCreateRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Create = %v", err)
	}
}

func containsString(value, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
