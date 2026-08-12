// Package recording is a deliberately boring MCP HTTP upstream for the Phase
// 0 composition. It records only safe evidence and rejects requests without
// the backend credential canary that agentgateway must inject.
package recording

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

type Server struct {
	canary string
	mu     sync.Mutex
	calls  int
	valid  int
	last   string
}

const sessionID = "phase0-recording-session"

type Evidence struct {
	Calls       int    `json:"calls"`
	CanaryValid int    `json:"canaryValid"`
	LastDigest  string `json:"lastArgumentsDigest,omitempty"`
}

func New(canary string) (*Server, error) {
	if canary == "" || len(canary) > 128 {
		return nil, errors.New("recording canary is required and bounded")
	}
	return &Server{canary: canary}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/evidence", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		s.mu.Lock()
		evidence := Evidence{Calls: s.calls, CanaryValid: s.valid, LastDigest: s.last}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, evidence)
	})
	mux.HandleFunc("/mcp", s.handleMCP)
	return mux
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if r.Header.Get("x-phase0-canary") != s.canary {
		// Do not reveal the expected canary in the response or logs.
		writeRPCError(w, nil, -32001, "backend credential missing")
		return
	}
	if r.Body == nil {
		writeRPCError(w, nil, -32600, "invalid JSON-RPC")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, strictjson.MaxDocumentBytes+1))
	if err != nil || len(body) > strictjson.MaxDocumentBytes {
		writeRPCError(w, nil, -32600, "request too large")
		return
	}
	if err := strictjson.ValidateObject(body); err != nil {
		writeRPCError(w, nil, -32600, "invalid JSON-RPC")
		return
	}
	var request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.JSONRPC != "2.0" {
		writeRPCError(w, nil, -32600, "invalid JSON-RPC")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeRPCError(w, nil, -32600, "invalid JSON-RPC")
		return
	}
	// Streamable HTTP servers return a session identifier from initialize. The
	// real agentgateway upstream client uses that identifier for the subsequent
	// initialized notification and tool call; omitting it makes the fixture
	// look valid to a permissive unit test but invalid at the live composition
	// boundary.
	w.Header().Set("MCP-Protocol-Version", "2025-06-18")
	if request.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	}

	switch request.Method {
	case "initialize":
		writeRPCResult(w, request.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "phase0-recording", "version": "0.1.0"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeRPCResult(w, request.ID, map[string]any{
			"tools": []any{map[string]any{
				"name":        "record",
				"description": "records a safe Phase 0 call",
				"inputSchema": map[string]any{"type": "object"},
			}},
		})
	case "tools/call":
		if err := strictjson.ValidateObject(request.Params); err != nil {
			writeRPCError(w, request.ID, -32602, "invalid tool call")
			return
		}
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(request.Params, &params) != nil || params.Name != "record" || strictjson.ValidateObject(params.Arguments) != nil {
			writeRPCError(w, request.ID, -32602, "invalid tool call")
			return
		}
		argumentsDigest := digest(params.Arguments)
		s.mu.Lock()
		s.calls++
		s.valid++
		s.last = argumentsDigest
		s.mu.Unlock()
		// The canary is intentionally absent from this response. The guard sees
		// only this result, never the credential that agentgateway used.
		writeRPCResult(w, request.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "recorded"}},
			"isError": false,
		})
	default:
		writeRPCError(w, request.ID, -32601, "method not found")
	}
}

func (s *Server) Evidence() Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Evidence{Calls: s.calls, CanaryValid: s.valid, LastDigest: s.last}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func ValidateEvidence(e Evidence) error {
	if e.Calls != e.CanaryValid {
		return fmt.Errorf("credential canary failed for %d calls", e.Calls)
	}
	return nil
}
