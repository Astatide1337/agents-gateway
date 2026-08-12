package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func markerHandler(status int) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(status)
	})
}

func TestRouterMountsOnlyExactBrokerPaths(t *testing.T) {
	router := newBrokerRouter(
		markerHandler(http.StatusOK), markerHandler(http.StatusCreated),
		markerHandler(http.StatusAccepted), markerHandler(http.StatusNoContent),
	)
	tests := []struct {
		path string
		want int
	}{
		{path: "/v1/responses", want: http.StatusOK},
		{path: "/mcp", want: http.StatusCreated},
		{path: "/v1/artifacts/output", want: http.StatusAccepted},
		{path: "/v1/artifacts/create", want: http.StatusAccepted},
		{path: "/v1/runtime/events", want: http.StatusNoContent},
		{path: "/v1/runtime/phase", want: http.StatusNotFound},
		{path: "/v1", want: http.StatusNotFound},
		{path: "/v1/runtime/process-exit", want: http.StatusNotFound},
		{path: "/v1/runtime/events/", want: http.StatusNotFound},
		{path: "/mcp/extra", want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+test.path, nil)
			request.RemoteAddr = "127.0.0.1:40000"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestRouterRejectsRemoteQueryAndOversizedHeaders(t *testing.T) {
	router := newBrokerRouter(markerHandler(http.StatusOK), markerHandler(http.StatusOK), markerHandler(http.StatusOK), markerHandler(http.StatusOK))
	remote := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	remote.RemoteAddr = "192.0.2.10:40000"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, remote)
	if response.Code != http.StatusNotFound {
		t.Fatalf("remote status = %d", response.Code)
	}

	query := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp?debug=true", nil)
	query.RemoteAddr = "127.0.0.1:40000"
	response = httptest.NewRecorder()
	router.ServeHTTP(response, query)
	if response.Code != http.StatusNotFound {
		t.Fatalf("query status = %d", response.Code)
	}

	large := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	large.RemoteAddr = "127.0.0.1:40000"
	large.Header.Set("X-Large", strings.Repeat("x", maxHeaderValueBytes+1))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, large)
	if response.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("large header status = %d", response.Code)
	}
}

func TestLoopbackAddressAndRemoteValidation(t *testing.T) {
	for _, address := range []string{"127.0.0.1:1", "127.0.0.1:8081", "127.0.0.1:65535"} {
		if !validLoopbackAddress(address) {
			t.Errorf("validLoopbackAddress(%q) = false", address)
		}
	}
	for _, address := range []string{"0.0.0.0:8081", ":8081", "127.0.0.1:0", "127.0.0.1:65536", "[::1]:8081"} {
		if validLoopbackAddress(address) {
			t.Errorf("validLoopbackAddress(%q) = true", address)
		}
	}
	if !isLoopbackRemote("127.0.0.1:42") || !isLoopbackRemote("[::1]:42") {
		t.Fatal("loopback remote rejected")
	}
	if isLoopbackRemote("192.0.2.1:42") || isLoopbackRemote("not-an-address") {
		t.Fatal("non-loopback remote accepted")
	}
}
