package recording

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const testCanary = "phase0-only-canary"

func TestCanaryIsRequiredAndNeverReturned(t *testing.T) {
	server, err := New(testCanary)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	body := []byte(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"record","arguments":{"message":"hello"}}}`)
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("content-type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	missingBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || server.Evidence().Calls != 0 || bytes.Contains(missingBody, []byte(testCanary)) {
		t.Fatalf("missing canary was not safely rejected: status=%d evidence=%#v body=%s", response.StatusCode, server.Evidence(), missingBody)
	}

	request, err = http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-phase0-canary", testCanary)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	validBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || bytes.Contains(validBody, []byte(testCanary)) {
		t.Fatalf("valid canary response leaked credential or failed: status=%d body=%s", response.StatusCode, validBody)
	}
	if evidence := server.Evidence(); evidence.Calls != 1 || evidence.CanaryValid != 1 {
		t.Fatalf("invalid evidence: %#v", evidence)
	} else if err := ValidateEvidence(evidence); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHandshakeAndToolsListDoNotCountAsToolCalls(t *testing.T) {
	server, err := New(testCanary)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	for _, requestBody := range []string{
		`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`,
	} {
		request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("x-phase0-canary", testCanary)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		if requestBody == `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}` {
			if got := response.Header.Get("Mcp-Session-Id"); got != sessionID {
				t.Fatalf("initialize did not return the streamable HTTP session id: %q", got)
			}
		}
		response.Body.Close()
		if value["jsonrpc"] != "2.0" {
			t.Fatalf("invalid MCP response: %#v", value)
		}
	}
	if got := server.Evidence().Calls; got != 0 {
		t.Fatalf("handshake/list counted as tool calls: %d", got)
	}
}

func TestMCPRejectsAmbiguousAndOversizedRequests(t *testing.T) {
	server, err := New(testCanary)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	requests := []string{
		`{"jsonrpc":"2.0","jsonrpc":"2.0","id":"duplicate","method":"tools/call","params":{"name":"record","arguments":{"message":"hello"}}}`,
		`{"jsonrpc":"2.0","id":"unknown","method":"tools/call","params":{"name":"record","arguments":{"message":"hello"}},"unexpected":true}`,
		`{"jsonrpc":"2.0","id":"params-duplicate","method":"tools/call","params":{"name":"record","name":"record","arguments":{"message":"hello"}}}`,
	}
	for _, body := range requests {
		request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("x-phase0-canary", testCanary)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte("invalid")) {
			t.Fatalf("ambiguous request was not rejected: status=%d body=%s", response.StatusCode, responseBody)
		}
	}

	oversized := `{"jsonrpc":"2.0","id":"oversized","method":"tools/call","params":{"name":"record","arguments":{"message":"` + strings.Repeat("x", strictjson.MaxDocumentBytes) + `"}}}`
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(oversized))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-phase0-canary", testCanary)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte("too large")) {
		t.Fatalf("oversized request was not rejected: status=%d body=%s", response.StatusCode, responseBody)
	}
	if evidence := server.Evidence(); evidence.Calls != 0 || evidence.CanaryValid != 0 {
		t.Fatalf("rejected requests changed evidence: %#v", evidence)
	}
}
