package runbroker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnixServerAuthorizesBeforeHandlerAndCapturesPeer(t *testing.T) {
	clock := newTestClock()
	manager := newTestManager(t, clock)
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	directory := privateSocketDir(t)
	socketPath := filepath.Join(directory, "run.sock")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		peer, peerOK := PeerCredentialsFromContext(request.Context())
		session, sessionOK := SessionFromContext(request.Context())
		if !peerOK || !sessionOK || peer.UID != uint32(os.Geteuid()) || session.ID != result.Session.ID {
			http.Error(writer, "missing authorization context", http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
	})
	server, err := NewUnixServer(UnixServerConfig{
		SocketPath:   socketPath,
		SessionID:    result.Session.ID,
		Binding:      result.Session.Binding,
		PolicyDigest: result.Session.PolicyDigest,
		Sessions:     manager,
		Handler:      handler,
		Peer:         PeerPolicy{UID: uint32Ptr(uint32(os.Geteuid()))},
		MaxBodyBytes: 16,
	})
	if err != nil {
		t.Fatalf("NewUnixServer: %v", err)
	}
	serveContext, cancelServe := context.WithCancel(context.Background())
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(serveContext) }()
	client := unixHTTPClient(server.SocketPath())

	request := newAuthorizedRequest(t, result, result.Session.AllowedModels[0], result.Session.PolicyDigest, "hello")
	response := doWithRetry(t, client, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorized status = %d", response.StatusCode)
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(data) != "hello" {
		t.Fatalf("authorized response = %q", data)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after authorized request = %d", got)
	}

	denied := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "wrong token", mutate: func(request *http.Request) {
			request.Header.Set(HeaderAuthorization, "Bearer agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		}},
		{name: "wrong session", mutate: func(request *http.Request) {
			request.Header.Set(HeaderSessionID, "ags_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		}},
		{name: "wrong model", mutate: func(request *http.Request) { request.Header.Set(HeaderModel, "model-denied") }},
		{name: "wrong policy", mutate: func(request *http.Request) { request.Header.Set(HeaderPolicyDigest, "sha256:other") }},
		{name: "duplicate auth", mutate: func(request *http.Request) {
			request.Header.Add(HeaderAuthorization, request.Header.Get(HeaderAuthorization))
		}},
	}
	for _, test := range denied {
		t.Run(test.name, func(t *testing.T) {
			request := newAuthorizedRequest(t, result, result.Session.AllowedModels[0], result.Session.PolicyDigest, "hello")
			test.mutate(request)
			response := doWithRetry(t, client, request)
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
			}
			_ = response.Body.Close()
		})
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("denied requests reached handler: %d calls", got)
	}

	cancelServe()
	select {
	case serveErr := <-serveErrors:
		if serveErr != nil {
			t.Fatalf("Serve after cancellation: %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop after context cancellation")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after shutdown: %v", err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}

func TestUnixServerLockAndSocketPermissions(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(privateSocketDir(t), "run.sock")
	config := UnixServerConfig{
		SocketPath:   socketPath,
		SessionID:    result.Session.ID,
		Binding:      result.Session.Binding,
		PolicyDigest: result.Session.PolicyDigest,
		Sessions:     manager,
		Handler:      http.NotFoundHandler(),
	}
	first, err := NewUnixServer(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.httpServer.WriteTimeout != 0 {
		t.Fatalf("default write timeout truncates streaming responses: %s", first.httpServer.WriteTimeout)
	}
	second, err := NewUnixServer(config)
	if second != nil || !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second server = %#v, error = %v", second, err)
	}
	socketInfo, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if socketInfo.Mode().Perm() != 0600 || socketInfo.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode/type = %v", socketInfo.Mode())
	}
	lockInfo, err := os.Stat(socketPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if lockInfo.Mode().Perm() != 0600 {
		t.Fatalf("lock mode = %v", lockInfo.Mode())
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	third, err := NewUnixServer(config)
	if err != nil {
		t.Fatalf("server after first close: %v", err)
	}
	if err := third.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnixServerRejectsUnsafeSocketDirectories(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	config := UnixServerConfig{
		SocketPath:   filepath.Join(link, "run.sock"),
		SessionID:    result.Session.ID,
		Binding:      result.Session.Binding,
		PolicyDigest: result.Session.PolicyDigest,
		Sessions:     manager,
		Handler:      http.NotFoundHandler(),
	}
	if _, err := NewUnixServer(config); !errors.Is(err, ErrSocketUnsafe) {
		t.Fatalf("symlink directory error = %v", err)
	}
	if err := os.Chmod(private, 0755); err != nil {
		t.Fatal(err)
	}
	config.SocketPath = filepath.Join(private, "run.sock")
	if _, err := NewUnixServer(config); !errors.Is(err, ErrSocketUnsafe) {
		t.Fatalf("shared directory error = %v", err)
	}
}

func TestUnixServerRejectsOversizedBodyBeforeHandler(t *testing.T) {
	manager := newTestManager(t, newTestClock())
	result, err := manager.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server, err := NewUnixServer(UnixServerConfig{
		SocketPath:   filepath.Join(privateSocketDir(t), "run.sock"),
		SessionID:    result.Session.ID,
		Binding:      result.Session.Binding,
		PolicyDigest: result.Session.PolicyDigest,
		Sessions:     manager,
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			calls.Add(1)
		}),
		MaxBodyBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	contextRun, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(contextRun) }()
	request := newAuthorizedRequest(t, result, "model-a", result.Session.PolicyDigest, "12345")
	response := doWithRetry(t, unixHTTPClient(server.SocketPath()), request)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d", response.StatusCode)
	}
	_ = response.Body.Close()
	if calls.Load() != 0 {
		t.Fatal("oversized body reached handler")
	}
	_ = server.Close(context.Background())
}

func newAuthorizedRequest(t *testing.T, result CreateResult, model, policy, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://runbroker.invalid/v1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(HeaderAuthorization, "Bearer "+result.Token.String())
	request.Header.Set(HeaderSessionID, string(result.Session.ID))
	request.Header.Set(HeaderModel, model)
	request.Header.Set(HeaderPolicyDigest, policy)
	return request
}

func unixHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
}

func doWithRetry(t *testing.T, client *http.Client, request *http.Request) *http.Response {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		clone := request.Clone(context.Background())
		if request.Body != nil {
			// Tests use small immutable bodies and recreate them through the
			// request's GetBody when available.
			if request.GetBody == nil {
				t.Fatal("test request has no GetBody")
			}
			body, err := request.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			clone.Body = body
		}
		response, err := client.Do(clone)
		if err == nil {
			return response
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Unix HTTP request never succeeded")
	return nil
}

func uint32Ptr(value uint32) *uint32 { return &value }

func privateSocketDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	directory := filepath.Join(parent, "run")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}
