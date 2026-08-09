package modelbroker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResponsesProxyStreamsAndInjectsCredentialAtBoundary(t *testing.T) {
	var gotAuthorization string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Set-Cookie", "provider-secret-cookie")
		w.Header().Set("OpenAI-Request-Id", "req-test")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer upstream.Close()

	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   upstream.URL + "/v1/responses",
		AllowedModels: []string{"gpt-test"},
		Credential: func(context.Context) ([]byte, error) {
			return []byte("provider-secret"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	requestBody := `{"model":"gpt-test","input":[{"role":"user","content":"hello"}],"stream":true}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer caller-token")
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(data) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("status=%d body=%q", response.StatusCode, data)
	}
	if gotAuthorization != "Bearer provider-secret" {
		t.Fatalf("unexpected upstream authorization %q", gotAuthorization)
	}
	if string(gotBody) != requestBody {
		t.Fatalf("request body changed: %s", gotBody)
	}
	if response.Header.Get("Set-Cookie") != "" || response.Header.Get("OpenAI-Request-Id") != "req-test" {
		t.Fatalf("response header allowlist failed: %#v", response.Header)
	}
}

func TestResponsesProxyRejectsBeforeResolvingCredential(t *testing.T) {
	var calls int
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   "http://127.0.0.1:1/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential: func(context.Context) ([]byte, error) {
			calls++
			return []byte("secret"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]*http.Request{
		"wrong method": httptest.NewRequest(http.MethodGet, "http://proxy/v1/responses", nil),
		"wrong path":   httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", strings.NewReader(`{"model":"allowed"}`)),
		"wrong model":  httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"denied"}`)),
	} {
		t.Run(name, func(t *testing.T) {
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, request)
			if recorder.Code < 400 {
				t.Fatalf("expected rejection, got %d", recorder.Code)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("credential source called %d times for rejected requests", calls)
	}
}

func TestResponsesProxyEnforcesLimitsAndModelType(t *testing.T) {
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:         "http://127.0.0.1:1/v1/responses",
		AllowedModels:       []string{"allowed"},
		Credential:          func(context.Context) ([]byte, error) { return []byte("secret"), nil },
		MaxHeaderBytes:      1024,
		MaxRequestBodyBytes: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		body   string
		header string
		status int
	}{
		{name: "body too large", body: `{"model":"allowed","x":"this is too large"}`, status: http.StatusRequestEntityTooLarge},
		{name: "model missing", body: `{}`, status: http.StatusBadRequest},
		{name: "model not string", body: `{"model":42}`, status: http.StatusBadRequest},
		{name: "header too large", body: `{"model":"allowed"}`, header: strings.Repeat("x", 2048), status: http.StatusRequestHeaderFieldsTooLarge},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			if testCase.header != "" {
				request.Header.Set("X-Large", testCase.header)
			}
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, request)
			if recorder.Code != testCase.status {
				t.Fatalf("expected %d, got %d", testCase.status, recorder.Code)
			}
		})
	}
}

func TestResponsesProxyErrorsDoNotLeakUpstreamBodyOrCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "provider-secret")
		http.Error(w, "provider-secret response body", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   upstream.URL + "/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("provider-secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"allowed"}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected upstream status, got %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "provider-secret") || recorder.Header().Get("Set-Cookie") != "" {
		t.Fatalf("provider data leaked: headers=%#v body=%q", recorder.Header(), recorder.Body.String())
	}
}

func TestResponsesProxyIgnoresProxyEnvironmentAndRejectsRedirects(t *testing.T) {
	oldHTTPProxy, oldHTTPSProxy := os.Getenv("HTTP_PROXY"), os.Getenv("HTTPS_PROXY")
	t.Cleanup(func() {
		_ = os.Setenv("HTTP_PROXY", oldHTTPProxy)
		_ = os.Setenv("HTTPS_PROXY", oldHTTPSProxy)
	})
	_ = os.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	_ = os.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   upstream.URL + "/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"allowed"}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy environment affected request: %d", recorder.Code)
	}

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, upstream.URL+"/v1/responses", http.StatusFound)
	}))
	defer redirect.Close()
	redirectProxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   redirect.URL + "/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	redirectRequest := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"allowed"}`))
	redirectRequest.Header.Set("Content-Type", "application/json")
	redirectProxy.ServeHTTP(recorder, redirectRequest)
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), upstream.URL) {
		t.Fatalf("redirect was not rejected safely: code=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesProxyCancelsUpstreamRequest(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:     "https://api.example.test/v1/responses",
		AllowedModels:   []string{"allowed"},
		Credential:      func(context.Context) ([]byte, error) { return []byte("secret"), nil },
		UpstreamTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		close(canceled)
		return nil, request.Context().Err()
	})
	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"allowed"}`)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		proxy.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy handler did not return after cancellation")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNewResponsesProxyValidatesHTTPSAndConfiguration(t *testing.T) {
	valid := ResponsesProxyConfig{
		UpstreamURL:   "https://api.example.test/v1/responses",
		AllowedModels: []string{"model"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	}
	if _, err := NewResponsesProxy(valid); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"http://api.example.test/v1/responses",
		"https://api.example.test/v1/chat/completions",
		"https://user:pass@api.example.test/v1/responses",
		"https://api.example.test/v1/responses?x=1",
		"https://api.example.test/v1/responses#fragment",
	} {
		config := valid
		config.UpstreamURL = endpoint
		if _, err := NewResponsesProxy(config); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if _, err := NewResponsesProxy(ResponsesProxyConfig{UpstreamURL: "https://api.example.test/v1/responses", AllowedModels: []string{"model"}}); err == nil {
		t.Fatal("accepted missing credential source")
	}
}

func TestResponsesProxyConcurrentRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	defer upstream.Close()
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   upstream.URL + "/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"allowed"}`))
			request.Header.Set("Content-Type", "application/json")
			response, requestErr := http.DefaultClient.Do(request)
			if requestErr != nil {
				t.Errorf("request failed: %v", requestErr)
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}()
	}
	group.Wait()
}

func TestResponsesProxyUsesTLSMinimumForHTTPSTransport(t *testing.T) {
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   "https://api.example.test/v1/responses",
		AllowedModels: []string{"model"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := proxy.client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS13 || transport.Proxy != nil {
		t.Fatalf("unsafe transport: %#v", transport)
	}
}

func TestResponsesProxyRequestModelRejectsTrailingJSON(t *testing.T) {
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   "http://127.0.0.1:1/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"allowed"} {}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected trailing JSON rejection, got %d", recorder.Code)
	}
}

func TestResponsesProxyRejectsDuplicateOrPaddedModel(t *testing.T) {
	proxy, err := NewResponsesProxy(ResponsesProxyConfig{
		UpstreamURL:   "http://127.0.0.1:1/v1/responses",
		AllowedModels: []string{"allowed"},
		Credential:    func(context.Context) ([]byte, error) { return []byte("secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"model":"denied","model":"allowed"}`, `{"model":" allowed "}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %q: expected bad request, got %d", body, recorder.Code)
		}
	}
}

func TestRequestModelDoesNotAcceptNestedModel(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"input": map[string]string{"model": "allowed"}})
	if _, err := requestModel(body); err == nil {
		t.Fatal("accepted nested model")
	}
}
