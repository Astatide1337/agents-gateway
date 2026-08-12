package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	RuntimeEventsPath      = "/v1/runtime/events"
	RuntimeProcessExitPath = "/v1/runtime/process-exit"
	RuntimePhasePath       = "/v1/runtime/phase"

	RuntimeEventMediaType = "application/x-ndjson"
	runtimeExitSchema     = "agents-gateway.process-exit.v1"
	runtimePhaseSchema    = "agents-gateway.phase-transition.v1"

	DefaultMaxRuntimeExitBytes int64 = 1024
	MaxRuntimeExitBytes        int64 = 64 << 10
	minRuntimeExitTokenBytes         = 32
	maxRuntimeExitTokenBytes         = 512
)

// RuntimeHandlerConfig binds the HTTP boundary to one server-side run
// identity. ProcessExitToken authenticates the process waiter, not the model
// process. The integration must keep it out of the agent's environment,
// workspace, command line, event stream, and logs.
type RuntimeHandlerConfig struct {
	Supervisor       *RuntimeSupervisor
	ProcessExitToken []byte
	MaxExitBytes     int64

	// PhaseTransitioner and PhaseSupervisor are an optional, separate
	// host-supervisor capability. The phase endpoint is only served by
	// TrustedPhaseHandler. A deployment adapter must mount that handler on a
	// separately authenticated transport; the agent-facing loopback listener
	// never receives the capability.
	PhaseTransitioner TrustedPhaseTransitioner
	PhaseSupervisor   *PhaseSupervisor
}

// RuntimeHandler accepts harness-authored events and a separately
// authenticated process-exit observation. Runtime events remain the harness's
// opinion: even a valid run.completed event cannot create a completion record
// without the process observation, and neither signal replaces the
// independently executed Gate.
//
// The handler is suitable for mounting directly on a loopback listener in
// cmd/agw-broker. ServeHTTP also verifies RemoteAddr as a second defense.
type RuntimeHandler struct {
	supervisor        *RuntimeSupervisor
	exitAuthorization []byte
	phaseTransitioner TrustedPhaseTransitioner
	phaseSupervisor   *PhaseSupervisor
	maxExitBytes      int64

	mu       sync.Mutex
	accepted map[uint64][sha256.Size]byte
	exit     *runtimeExitReceipt
	phase    *runtimePhaseReceipt
	fatal    bool
}

type runtimeExitReceipt struct {
	digest     [sha256.Size]byte
	completion runtimeevents.CompletionRecord
	completed  bool
	errStatus  int
}

type runtimePhaseReceipt struct {
	digest  [sha256.Size]byte
	status  int
	success bool
}

type runtimeExitRequest struct {
	Schema string `json:"schema"`
	Code   *int   `json:"code"`
}

// NewRuntimeHandler constructs the shared state for both runtime endpoints.
// The token is copied and never returned. It must be a high-entropy printable
// bearer token available only to the process waiter.
func NewRuntimeHandler(config RuntimeHandlerConfig) (*RuntimeHandler, error) {
	if config.Supervisor == nil || config.Supervisor.collector == nil || !validRuntimeExitToken(config.ProcessExitToken) {
		return nil, ErrInvalidConfig
	}
	if (config.PhaseTransitioner == nil) != (config.PhaseSupervisor == nil) {
		return nil, ErrInvalidConfig
	}
	maxExitBytes := config.MaxExitBytes
	if maxExitBytes == 0 {
		maxExitBytes = DefaultMaxRuntimeExitBytes
	}
	if maxExitBytes < 64 || maxExitBytes > MaxRuntimeExitBytes {
		return nil, ErrInvalidConfig
	}
	authorization := make([]byte, len("Bearer ")+len(config.ProcessExitToken))
	copy(authorization, "Bearer ")
	copy(authorization[len("Bearer "):], config.ProcessExitToken)
	return &RuntimeHandler{
		supervisor: config.Supervisor, exitAuthorization: authorization,
		phaseTransitioner: config.PhaseTransitioner, phaseSupervisor: config.PhaseSupervisor,
		maxExitBytes: maxExitBytes, accepted: make(map[uint64][sha256.Size]byte),
	}, nil
}

func validRuntimeExitToken(token []byte) bool {
	if len(token) < minRuntimeExitTokenBytes || len(token) > maxRuntimeExitTokenBytes {
		return false
	}
	for _, value := range token {
		if value < 0x21 || value > 0x7e {
			return false
		}
	}
	return true
}

func (h *RuntimeHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if h == nil || h.supervisor == nil || request == nil || !isLoopbackRequest(request) {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if request.URL.RawQuery != "" {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeBrokerHTTPError(w, http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case RuntimeEventsPath:
		h.handleEvent(w, request)
	case RuntimeProcessExitPath:
		h.handleProcessExit(w, request)
	case RuntimePhasePath:
		// The phase path is private-supervisor-only. A request arriving on the
		// agent-facing loopback listener has no in-memory capability context.
		writeBrokerHTTPError(w, http.StatusNotFound)
	default:
		writeBrokerHTTPError(w, http.StatusNotFound)
	}
}

// TrustedPhaseHandler returns the only HTTP handler that can authorize a
// phase transition. It must be mounted exclusively on a transport whose OS
// permissions and authenticated caller identify a proven trusted supervisor.
// It deliberately has no bearer-token or agent-supplied approval mechanism.
func (h *RuntimeHandler) TrustedPhaseHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if h == nil || h.supervisor == nil || h.phaseTransitioner == nil || h.phaseSupervisor == nil || request == nil || request.URL == nil || request.URL.RawQuery != "" || request.URL.Path != RuntimePhasePath {
			writeBrokerHTTPError(w, http.StatusNotFound)
			return
		}
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeBrokerHTTPError(w, http.StatusMethodNotAllowed)
			return
		}
		if len(request.Header.Values("Authorization")) != 0 {
			// This contract is capability-based. Reject bearer headers rather
			// than allowing callers to infer that a token is meaningful here.
			writeBrokerHTTPError(w, http.StatusNotFound)
			return
		}
		h.handlePhaseTransition(w, request.WithContext(h.phaseSupervisor.withAuthority(request.Context())))
	})
}

func (h *RuntimeHandler) handleEvent(w http.ResponseWriter, request *http.Request) {
	if !contentTypeIs(request.Header.Get("Content-Type"), RuntimeEventMediaType) {
		writeBrokerHTTPError(w, http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBounded(request.Body, int64(runtimeproto.MaxFrameBytes+1))
	if err != nil {
		h.poison()
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	frame, digest, err := strictRuntimeEvent(body, h.supervisor.runUID)
	if err != nil {
		// Let the collector record the same permanent rejection whenever the
		// body reached the strict protocol boundary. The handler's own fatal
		// bit also covers failures that cannot be passed through safely.
		_ = h.supervisor.AcceptRuntimeLine(request.Context(), body)
		h.poison()
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fatal {
		writeBrokerHTTPError(w, http.StatusConflict)
		return
	}
	if existing, ok := h.accepted[frame.Seq]; ok {
		if subtle.ConstantTimeCompare(existing[:], digest[:]) == 1 {
			w.Header().Set("X-AGW-Idempotent-Replay", "true")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.fatal = true
		_ = h.supervisor.AcceptRuntimeLine(request.Context(), body)
		writeBrokerHTTPError(w, http.StatusConflict)
		return
	}
	if err := h.supervisor.AcceptRuntimeLine(request.Context(), body); err != nil {
		h.fatal = true
		writeBrokerHTTPError(w, runtimeBoundaryStatus(err))
		return
	}
	h.accepted[frame.Seq] = digest
	w.WriteHeader(http.StatusNoContent)
}

func strictRuntimeEvent(body []byte, runUID string) (proto.Envelope, [sha256.Size]byte, error) {
	var zero proto.Envelope
	var zeroDigest [sha256.Size]byte
	line := body
	if bytes.HasSuffix(line, []byte{'\n'}) {
		line = line[:len(line)-1]
	}
	if bytes.HasSuffix(line, []byte{'\r'}) {
		line = line[:len(line)-1]
	}
	if len(line) == 0 || bytes.ContainsAny(line, "\r\n") {
		return zero, zeroDigest, ErrInvalidRequest
	}
	frame, err := runtimeproto.ParseLine(line)
	if err != nil || frame.Kind != proto.KindEvent || frame.RunID != runUID {
		return zero, zeroDigest, ErrInvalidRequest
	}
	canonical, err := strictjson.Normalize(line)
	if err != nil || strictjson.ValidateObject(canonical) != nil {
		return zero, zeroDigest, ErrInvalidRequest
	}
	return frame, sha256.Sum256(canonical), nil
}

func (h *RuntimeHandler) handleProcessExit(w http.ResponseWriter, request *http.Request) {
	if !h.authorizedProcessExit(request) {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	if !contentTypeIs(request.Header.Get("Content-Type"), "application/json") {
		writeBrokerHTTPError(w, http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBounded(request.Body, h.maxExitBytes)
	if err != nil {
		h.poison()
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	exit, digest, err := strictProcessExit(body)
	if err != nil {
		h.poison()
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exit != nil {
		if subtle.ConstantTimeCompare(h.exit.digest[:], digest[:]) != 1 {
			h.fatal = true
			writeBrokerHTTPError(w, http.StatusConflict)
			return
		}
		if h.exit.completed {
			w.Header().Set("X-AGW-Idempotent-Replay", "true")
			h.writeCompletion(w, http.StatusOK, h.exit.completion)
			return
		}
		writeBrokerHTTPError(w, h.exit.errStatus)
		return
	}
	if h.fatal {
		writeBrokerHTTPError(w, http.StatusConflict)
		return
	}
	h.exit = &runtimeExitReceipt{digest: digest}
	completion, err := h.supervisor.ProcessExit(request.Context(), runtimeevents.ProcessExit{Known: true, Code: exit})
	if err != nil {
		status := runtimeBoundaryStatus(err)
		h.exit.errStatus = status
		h.fatal = true
		writeBrokerHTTPError(w, status)
		return
	}
	h.exit.completion = completion
	h.exit.completed = true
	h.writeCompletion(w, http.StatusCreated, completion)
}

type runtimePhaseRequest struct {
	Schema     string `json:"schema"`
	Operation  string `json:"operation"`
	RequestID  string `json:"request_id"`
	RunUID     string `json:"run_uid"`
	SpecDigest string `json:"spec_digest"`
	BaseSHA    string `json:"base_sha"`
}

func (h *RuntimeHandler) handlePhaseTransition(w http.ResponseWriter, request *http.Request) {
	// A loopback address alone is not an authority: the agent shares the pod
	// network namespace with this process. The capability must have been added
	// by TrustedPhaseHandler on the private supervisor transport.
	if h.phaseTransitioner == nil || h.phaseSupervisor == nil || request == nil || !h.phaseSupervisor.hasAuthority(request.Context()) {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	if !contentTypeIs(request.Header.Get("Content-Type"), "application/json") {
		writeBrokerHTTPError(w, http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBounded(request.Body, 1024)
	if err != nil {
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	phaseRequest, digest, err := strictPhaseTransition(body, h.supervisor.runUID, h.supervisor.specDigest, h.supervisor.baseSHA)
	if err != nil {
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}
	_ = phaseRequest // strictPhaseTransition binds the operation to this run.

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phase != nil {
		if subtle.ConstantTimeCompare(h.phase.digest[:], digest[:]) != 1 {
			writeBrokerHTTPError(w, http.StatusConflict)
			return
		}
		w.Header().Set("X-AGW-Idempotent-Replay", "true")
		if h.phase.success {
			w.WriteHeader(http.StatusNoContent)
		} else {
			writeBrokerHTTPError(w, h.phase.status)
		}
		return
	}

	if err := h.phaseTransitioner.TransitionToEdit(request.Context()); err != nil {
		status := phaseTransitionStatus(err)
		if status == http.StatusForbidden {
			h.phase = &runtimePhaseReceipt{digest: digest, status: status}
		}
		writeBrokerHTTPError(w, status)
		return
	}
	h.phase = &runtimePhaseReceipt{digest: digest, status: http.StatusNoContent, success: true}
	w.WriteHeader(http.StatusNoContent)
}

func strictPhaseTransition(body []byte, runUID, specDigest, baseSHA string) (runtimePhaseRequest, [sha256.Size]byte, error) {
	var request runtimePhaseRequest
	var zeroDigest [sha256.Size]byte
	if len(body) == 0 || strictjson.ValidateObject(body) != nil || decodeStrictObject(body, &request) != nil {
		return request, zeroDigest, ErrInvalidRequest
	}
	if request.Schema != runtimePhaseSchema || request.Operation != "explore-to-edit" || !validPhaseRequestID(request.RequestID) ||
		request.RunUID != runUID || request.SpecDigest != specDigest || request.BaseSHA != baseSHA {
		return request, zeroDigest, ErrInvalidRequest
	}
	canonical, err := strictjson.Normalize(body)
	if err != nil {
		return request, zeroDigest, ErrInvalidRequest
	}
	return request, sha256.Sum256(canonical), nil
}

func validPhaseRequestID(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func phaseTransitionStatus(err error) int {
	switch {
	case errors.Is(err, ErrPhaseTransitionDenied):
		return http.StatusForbidden
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	default:
		return http.StatusServiceUnavailable
	}
}

func strictProcessExit(body []byte) (int, [sha256.Size]byte, error) {
	var zeroDigest [sha256.Size]byte
	if len(body) == 0 || strictjson.ValidateObject(body) != nil {
		return 0, zeroDigest, ErrInvalidRequest
	}
	var request runtimeExitRequest
	if decodeStrictObject(body, &request) != nil || request.Schema != runtimeExitSchema || request.Code == nil || *request.Code < -1 || *request.Code > 255 {
		return 0, zeroDigest, ErrInvalidRequest
	}
	canonical, err := strictjson.Normalize(body)
	if err != nil {
		return 0, zeroDigest, ErrInvalidRequest
	}
	return *request.Code, sha256.Sum256(canonical), nil
}

func (h *RuntimeHandler) authorizedProcessExit(request *http.Request) bool {
	if h == nil || request == nil || len(h.exitAuthorization) == 0 {
		return false
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) != len(h.exitAuthorization) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(values[0]), h.exitAuthorization) == 1
}

func (h *RuntimeHandler) writeCompletion(w http.ResponseWriter, status int, completion runtimeevents.CompletionRecord) {
	body, err := json.Marshal(completion)
	if err != nil || len(body) == 0 || len(body) > runtimeevents.MaxCompletionBytes {
		h.fatal = true
		writeBrokerHTTPError(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *RuntimeHandler) poison() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fatal = true
	h.mu.Unlock()
}

func runtimeBoundaryStatus(err error) int {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, runtimeevents.ErrAmbiguous), errors.Is(err, runtimeevents.ErrStoreUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, runtimeevents.ErrInvalidFrame), errors.Is(err, runtimeevents.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, runtimeevents.ErrWrongRun), errors.Is(err, runtimeevents.ErrSequence), errors.Is(err, runtimeevents.ErrRejected), errors.Is(err, runtimeevents.ErrStreamClosed), errors.Is(err, runtimeevents.ErrNoTerminal), errors.Is(err, runtimeevents.ErrProcessExitUnknown), errors.Is(err, runtimeevents.ErrProcessExitFailed), errors.Is(err, runtimeevents.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

// RuntimeProcessExitPayload returns the strict body expected by the trusted
// process waiter. It contains no run identity because identity is fixed by the
// server-side RuntimeSupervisor.
func RuntimeProcessExitPayload(exitCode int) ([]byte, error) {
	if exitCode < -1 || exitCode > 255 {
		return nil, ErrInvalidRequest
	}
	body, err := json.Marshal(struct {
		Schema string `json:"schema"`
		Code   int    `json:"code"`
	}{Schema: runtimeExitSchema, Code: exitCode})
	if err != nil || len(body) > int(DefaultMaxRuntimeExitBytes) || strings.ContainsAny(string(body), "\r\n") {
		return nil, ErrInvalidRequest
	}
	return body, nil
}

// RuntimePhaseTransitionPayload returns the strict body expected by the
// trusted host supervisor. The immutable run identity is included so a
// supervisor routed to the wrong private socket fails closed instead of
// authorizing a different run. RequestID remains only an idempotency key for
// retries.
func RuntimePhaseTransitionPayload(requestID, runUID, specDigest, baseSHA string) ([]byte, error) {
	if !validPhaseRequestID(requestID) || runUID == "" || specDigest == "" || baseSHA == "" {
		return nil, ErrInvalidRequest
	}
	body, err := json.Marshal(runtimePhaseRequest{
		Schema: runtimePhaseSchema, Operation: "explore-to-edit", RequestID: requestID,
		RunUID: runUID, SpecDigest: specDigest, BaseSHA: baseSHA,
	})
	if err != nil || len(body) > 1024 || strings.ContainsAny(string(body), "\r\n") {
		return nil, ErrInvalidRequest
	}
	return body, nil
}
