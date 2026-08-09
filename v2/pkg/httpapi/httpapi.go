// Package httpapi exposes the v2 control-plane HTTP API.
//
// The package deliberately uses net/http directly. It is a thin transport
// boundary: authorization, tenant scope, resource validation, audit records,
// and durable state are delegated to the v2 packages supplied by the caller.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
)

const (
	defaultMaxBodyBytes int64 = 1 << 20
	defaultPageSize           = 1000
	maxPageSize               = 5000
	maxSignalBodyBytes  int64 = 16 << 10
	signalClaimLease          = 30 * time.Second
)

var (
	ErrAuthenticatorUnavailable = errors.New("authenticator unavailable")
	ErrUnauthenticated          = errors.New("unauthenticated")
	ErrInvalidPrincipal         = errors.New("invalid principal")
	ErrWorkflowNotFound         = errors.New("workflow execution not found")
	identifierPattern           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	dnsNamePattern              = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	digestPattern               = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Authenticator resolves the request to a principal. Implementations should
// return ErrUnauthenticated for invalid or missing credentials. A nil
// Authenticator is never treated as anonymous access; protected endpoints
// fail closed.
type Authenticator interface {
	Authenticate(*http.Request) (authz.Principal, error)
}

type principalContextKey struct{}

// PrincipalFromContext returns the authenticated principal installed by the
// HTTP handler. It is useful to downstream handlers mounted behind this API.
func PrincipalFromContext(ctx context.Context) (authz.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(authz.Principal)
	return principal, ok
}

// Options controls the transport boundary. Zero values are safe defaults.
type Options struct {
	MaxBodyBytes       int64
	ProductionValidate bool
	Development        bool
	Now                func() time.Time
	RunStarter         RunStarter
	RunSignaler        RunSignaler
	ReadyCheck         func(context.Context) error
	ArtifactCatalog    store.ArtifactCatalog
	ArtifactStore      *artifact.Store
}

// RunStartRequest is the immutable handoff from the HTTP control plane to the
// durable workflow engine. Payload bytes and credentials are deliberately not
// included; callers must externalize input and pass only a bounded reference.
type RunStartRequest struct {
	Scope       store.Scope
	Run         store.Run
	AgentRef    string
	WorkflowRef string
	InputRef    string
}

// RunStarter starts or confirms an idempotently named durable execution.
// Implementations must treat Run.ID as the workflow id and reject a different
// execution attempting to reuse it.
type RunStarter interface {
	StartRun(context.Context, RunStartRequest) error
}

// RunSignal is a bounded control command. It intentionally contains references
// and decisions only; it never carries prompts, reply content, credentials, or
// other user payloads. Target is workflow for the top-level run or agent for a
// deterministic child workflow named runID/agent/stepID.
type RunSignal struct {
	Kind           string
	Target         string
	IdempotencyKey string
	Reason         string
	ApprovalID     string
	Decision       string
	StepID         string
	TaskID         string
	ReplyRef       string
}

// RunSignaler delivers an already-authorized run command to the durable
// workflow engine. Implementations must use the run ID and target exactly as
// supplied; they must not create a workflow or reinterpret a reference.
type RunSignaler interface {
	SignalRun(context.Context, store.Scope, string, RunSignal) error
}

// Handler is the v2 HTTP control-plane handler.
type Handler struct {
	storage     store.Store
	auth        Authenticator
	maxBody     int64
	production  bool
	now         func() time.Time
	runStarter  RunStarter
	runSignaler RunSignaler
	readyCheck  func(context.Context) error
	artifacts   store.ArtifactCatalog
	objects     *artifact.Store
	sequence    uint64
}

// New constructs a fail-closed control-plane handler. Passing a nil
// Authenticator is allowed for health probes and local startup diagnostics,
// but every protected request will be rejected.
func New(storage store.Store, authenticator Authenticator, options Options) *Handler {
	maxBody := options.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	catalog := options.ArtifactCatalog
	if catalog == nil {
		catalog, _ = storage.(store.ArtifactCatalog)
	}
	return &Handler{
		storage: storage,
		auth:    authenticator,
		maxBody: maxBody,
		// Production validation is the safe default. A caller must opt into
		// development validation explicitly; a zero Options value must not
		// silently permit unpinned images or direct network access.
		production:  options.ProductionValidate || !options.Development,
		now:         now,
		runStarter:  options.RunStarter,
		runSignaler: options.RunSignaler,
		readyCheck:  options.ReadyCheck,
		artifacts:   catalog,
		objects:     options.ArtifactStore,
	}
}

// NewHandler is retained as a convenient constructor for callers that do not
// need custom options. Production validation is enabled by default here.
func NewHandler(storage store.Store, authenticator Authenticator) *Handler {
	return New(storage, authenticator, Options{ProductionValidate: true})
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := requestID(r, &h.sequence)
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	if isHealthPath(r.URL.Path) {
		h.handleHealth(w, r, requestID)
		return
	}
	if isReadyPath(r.URL.Path) {
		h.handleReady(w, r, requestID)
		return
	}

	route, ok := parseRoute(r.URL.Path)
	if !ok {
		writeError(w, requestID, http.StatusNotFound, "not_found", "resource not found")
		return
	}

	principal, authErr := h.authenticate(r)
	if authErr != nil {
		decision := "denied"
		if route.scope.OrganizationID != "" {
			_ = h.audit(r.Context(), route.scope, "anonymous", route.action(r.Method), route.resourceID(), decision, requestID)
		}
		if errors.Is(authErr, ErrAuthenticatorUnavailable) {
			writeError(w, requestID, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		} else {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, requestID, http.StatusUnauthorized, "unauthorized", "authentication required")
		}
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))

	action := route.action(r.Method)
	decision := authz.Authorize(principal, authz.Request{
		OrganizationID: route.scope.OrganizationID,
		ProjectID:      route.scope.ProjectID,
		Action:         action,
	})
	if !decision.Allowed {
		_ = h.audit(r.Context(), route.scope, principal.ID, action, route.resourceID(), "denied", requestID)
		writeError(w, requestID, http.StatusForbidden, "forbidden", "the principal is not authorized for this scope")
		return
	}
	if err := h.audit(r.Context(), route.scope, principal.ID, action, route.resourceID(), "allowed", requestID); err != nil {
		writeError(w, requestID, http.StatusServiceUnavailable, "audit_unavailable", "the request could not be recorded")
		return
	}

	switch route.kind {
	case routeResource:
		h.handleResource(w, r, requestID, route)
	case routeCollection:
		h.handleCollection(w, r, requestID, route)
	case routeRun:
		if route.runID == "" && r.Method == http.MethodGet {
			h.handleCollection(w, r, requestID, route)
		} else {
			h.handleRun(w, r, requestID, route)
		}
	case routeUsage, routeAudit:
		h.handleCollection(w, r, requestID, route)
	case routeEvents:
		h.handleEvents(w, r, requestID, route)
	case routeArtifact:
		h.handleArtifact(w, r, requestID, route)
	default:
		writeError(w, requestID, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (h *Handler) authenticate(r *http.Request) (authz.Principal, error) {
	if h.auth == nil {
		return authz.Principal{}, ErrAuthenticatorUnavailable
	}
	principal, err := h.auth.Authenticate(r)
	if err != nil {
		return authz.Principal{}, err
	}
	if strings.TrimSpace(principal.ID) == "" {
		return authz.Principal{}, ErrInvalidPrincipal
	}
	return principal, nil
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request, requestID string) {
	writeJSON(w, http.StatusOK, requestID, map[string]any{"status": "ok"})
}

func (h *Handler) handleReady(w http.ResponseWriter, r *http.Request, requestID string) {
	if h.storage == nil || h.auth == nil {
		writeError(w, requestID, http.StatusServiceUnavailable, "not_ready", "control plane is not ready")
		return
	}
	if h.readyCheck != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := h.readyCheck(ctx); err != nil {
			writeError(w, requestID, http.StatusServiceUnavailable, "dependency_unavailable", "a required control-plane dependency is unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, requestID, map[string]any{"status": "ready"})
}

func (h *Handler) handleResource(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute) {
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		if route.kindName == "" || route.name == "" {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "resource kind and name are required")
			return
		}
		data, err := readJSONBody(w, r, h.maxBody)
		if err != nil {
			writeBodyError(w, requestID, err)
			return
		}
		resource, err := decodeResource(data)
		if err != nil {
			writeError(w, requestID, http.StatusBadRequest, "invalid_resource", "request body is not a valid resource")
			return
		}
		meta := resource.Meta()
		if spec.ResourceKind(resource) != route.kindName || meta.Metadata.Name != route.name {
			writeError(w, requestID, http.StatusBadRequest, "scope_mismatch", "resource identity does not match the request path")
			return
		}
		if meta.Metadata.Namespace != "" && meta.Metadata.Namespace != route.scope.ProjectID {
			writeError(w, requestID, http.StatusForbidden, "scope_mismatch", "resource namespace is outside the requested project")
			return
		}
		if project, ok := resource.(*spec.Project); ok && project.Spec.OrganizationRef != route.scope.OrganizationID {
			writeError(w, requestID, http.StatusForbidden, "scope_mismatch", "project belongs to a different organization")
			return
		}
		if err := spec.ValidateAll([]spec.Resource{resource}, spec.ValidationOptions{Production: h.production}); err != nil {
			writeError(w, requestID, http.StatusBadRequest, "validation_failed", "resource validation failed")
			return
		}
		digest, err := spec.RevisionDigest(resource)
		if err != nil {
			writeError(w, requestID, http.StatusInternalServerError, "internal", "resource could not be fingerprinted")
			return
		}
		document, err := spec.AsJSON(resource)
		if err != nil {
			writeError(w, requestID, http.StatusInternalServerError, "internal", "resource could not be encoded")
			return
		}
		stored, err := h.storage.ApplyResource(r.Context(), store.Resource{
			Scope: route.scope, Kind: route.kindName, Name: route.name, Digest: digest,
			AppliedBy: principalID(r.Context()), Document: document,
		})
		if err != nil {
			writeStoreError(w, requestID, err)
			return
		}
		status := http.StatusCreated
		if stored.Revision > 1 && len(stored.Document) > 0 {
			// Memory and PostgreSQL-compatible stores return the existing revision
			// for an idempotent apply. The body remains safe to replay.
			status = http.StatusOK
		}
		writeJSON(w, status, requestID, resourceResponse(stored))
	case http.MethodGet:
		resource, err := h.storage.GetResource(r.Context(), route.scope, route.kindName, route.name)
		if err != nil {
			writeStoreError(w, requestID, err)
			return
		}
		writeJSON(w, http.StatusOK, requestID, resourceResponse(resource))
	default:
		methodNotAllowed(w, requestID, http.MethodGet, http.MethodPut, http.MethodPost)
	}
}

func (h *Handler) handleCollection(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, requestID, http.MethodGet)
		return
	}
	page, err := collectionPage(r)
	if err != nil {
		writeError(w, requestID, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	switch route.kind {
	case routeCollection:
		kinds := route.collectionKinds
		if route.collection == "resources" {
			if supplied := strings.TrimSpace(r.URL.Query().Get("kind")); supplied != "" {
				var normalized []string
				for _, kind := range strings.Split(supplied, ",") {
					kind = strings.TrimSpace(kind)
					if !validIdentifier(kind) {
						writeError(w, requestID, http.StatusBadRequest, "invalid_kind", "resource kind is invalid")
						return
					}
					normalized = append(normalized, kind)
				}
				kinds = strings.Join(normalized, ",")
			}
		}
		resources, hasMore, listErr := h.storage.ListResources(r.Context(), route.scope, kinds, page)
		if listErr != nil {
			writeStoreError(w, requestID, listErr)
			return
		}
		items := make([]map[string]any, 0, len(resources))
		for _, resource := range resources {
			items = append(items, resourceResponse(resource))
		}
		writeCollection(w, requestID, items, page, hasMore)
	case routeRun:
		runs, hasMore, listErr := h.storage.ListRuns(r.Context(), route.scope, page)
		if listErr != nil {
			writeStoreError(w, requestID, listErr)
			return
		}
		items := make([]map[string]any, 0, len(runs))
		for _, run := range runs {
			items = append(items, runResponse(run))
		}
		writeCollection(w, requestID, items, page, hasMore)
	case routeUsage:
		usage, usageErr := h.storage.GetUsage(r.Context(), route.scope)
		if usageErr != nil {
			writeStoreError(w, requestID, usageErr)
			return
		}
		data := map[string]any{"usage": usageResponse(usage)}
		if route.collection == "quotas" {
			data["quotas"] = h.projectQuotas(r.Context(), route.scope)
		}
		writeJSON(w, http.StatusOK, requestID, data)
	case routeAudit:
		audits, hasMore, listErr := h.storage.ListAudit(r.Context(), route.scope, page)
		if listErr != nil {
			writeStoreError(w, requestID, listErr)
			return
		}
		items := make([]map[string]any, 0, len(audits))
		for _, event := range audits {
			items = append(items, map[string]any{
				"principalId": event.PrincipalID, "action": event.Action,
				"resourceType": event.ResourceType, "resourceId": event.ResourceID,
				"decision": event.Decision, "metadata": redactJSON(event.Metadata),
				"createdAt": event.CreatedAt,
			})
		}
		writeCollection(w, requestID, items, page, hasMore)
	default:
		writeError(w, requestID, http.StatusNotFound, "not_found", "resource not found")
	}
}

func collectionPage(r *http.Request) (store.Page, error) {
	page := store.Page{}
	for key, target := range map[string]*int{"limit": &page.Limit, "offset": &page.Offset} {
		value := strings.TrimSpace(r.URL.Query().Get(key))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return store.Page{}, fmt.Errorf("%s must be an integer", key)
		}
		*target = parsed
	}
	return page.Normalize()
}

func writeCollection(w http.ResponseWriter, requestID string, items []map[string]any, page store.Page, hasMore bool) {
	nextOffset := any(nil)
	if hasMore {
		nextOffset = page.Offset + len(items)
	}
	writeJSON(w, http.StatusOK, requestID, map[string]any{
		"items": items, "limit": page.Limit, "offset": page.Offset,
		"hasMore": hasMore, "nextOffset": nextOffset,
	})
}

func (h *Handler) handleRun(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute) {
	switch r.Method {
	case http.MethodPost:
		if route.signal != "" {
			h.handleRunSignal(w, r, requestID, route)
			return
		}
		if route.runID != "" {
			writeError(w, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "run creation must target the runs collection")
			return
		}
		data, err := readJSONBody(w, r, h.maxBody)
		if err != nil {
			writeBodyError(w, requestID, err)
			return
		}
		var input runCreateRequest
		if err := decodeStrictJSON(data, &input); err != nil {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
			return
		}
		if err := input.validate(); err != nil {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "run request is invalid")
			return
		}
		id := input.ID
		if id == "" {
			if input.IdempotencyKey != "" {
				id = deterministicRunID(route.scope, input.IdempotencyKey)
			} else {
				id = randomID("run-")
			}
		}
		run, err := h.storage.CreateRun(r.Context(), store.Run{
			Scope: route.scope, ID: id, Kind: input.Kind,
			DefinitionDigest: input.DefinitionDigest, RequestedBy: principalID(r.Context()),
			IdempotencyKey: input.IdempotencyKey, Status: "Pending",
		})
		status := http.StatusCreated
		if errors.Is(err, store.ErrConflict) {
			existing, getErr := h.storage.GetRun(r.Context(), route.scope, id)
			if getErr == nil && existing.IdempotencyKey == input.IdempotencyKey && input.IdempotencyKey != "" {
				run, err = existing, nil
				status = http.StatusOK
			}
		}
		if err != nil {
			writeStoreError(w, requestID, err)
			return
		}
		if h.runStarter == nil {
			writeError(w, requestID, http.StatusServiceUnavailable, "workflow_unavailable", "durable workflow execution is not configured")
			return
		}
		if err := h.runStarter.StartRun(r.Context(), RunStartRequest{
			Scope: route.scope, Run: run, AgentRef: input.AgentRef,
			WorkflowRef: input.WorkflowRef, InputRef: input.InputRef,
		}); err != nil {
			payload, _ := json.Marshal(map[string]string{"reason": "workflow dispatch failed", "requestId": requestID})
			_, _ = h.storage.AppendEvent(r.Context(), store.Event{Scope: route.scope, RunID: run.ID, Type: "run.dispatch_failed", Payload: payload})
			writeError(w, requestID, http.StatusServiceUnavailable, "workflow_unavailable", "durable workflow execution could not be started")
			return
		}
		writeJSON(w, status, requestID, runResponse(run))
	case http.MethodGet:
		if route.signal != "" {
			methodNotAllowed(w, requestID, http.MethodPost)
			return
		}
		if route.runID == "" {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "run id is required")
			return
		}
		run, err := h.storage.GetRun(r.Context(), route.scope, route.runID)
		if err != nil {
			writeStoreError(w, requestID, err)
			return
		}
		writeJSON(w, http.StatusOK, requestID, runResponse(run))
	default:
		if route.signal != "" {
			methodNotAllowed(w, requestID, http.MethodPost)
			return
		}
		methodNotAllowed(w, requestID, http.MethodGet, http.MethodPost)
	}
}

func (h *Handler) handleRunSignal(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute) {
	run, err := h.storage.GetRun(r.Context(), route.scope, route.runID)
	if err != nil {
		writeStoreError(w, requestID, err)
		return
	}
	if isTerminalRunStatus(run.Status) {
		if route.signal == signalCancel && run.Status == "Cancelled" {
			// Cancellation is safe to replay after the durable state has reached
			// its terminal state. No new Temporal signal is necessary.
			writeJSON(w, http.StatusOK, requestID, runResponse(run))
			return
		}
		writeError(w, requestID, http.StatusConflict, "run_terminal", "the run is already terminal")
		return
	}
	if h.runSignaler == nil {
		writeError(w, requestID, http.StatusServiceUnavailable, "workflow_unavailable", "durable workflow signaling is not configured")
		return
	}

	data, err := readJSONBody(w, r, minInt64(h.maxBody, maxSignalBodyBytes))
	if err != nil {
		writeBodyError(w, requestID, err)
		return
	}
	signal, err := decodeRunSignal(route.signal, data, r.Header.Get("Idempotency-Key"), requestID)
	if err != nil {
		writeError(w, requestID, http.StatusBadRequest, "invalid_request", "run signal request is invalid")
		return
	}
	fingerprint := runSignalFingerprint(signal)
	keyHash := hashString(signal.IdempotencyKey)
	claimTokenHash := hashString(randomID("signal-"))
	claim, err := h.storage.ClaimRunSignal(r.Context(), route.scope, route.runID, keyHash, fingerprint, claimTokenHash, signalClaimLease)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, requestID, http.StatusConflict, "idempotency_conflict", "the idempotency key was used for a different signal")
			return
		}
		writeStoreError(w, requestID, err)
		return
	}
	if claim.State == store.RunSignalAccepted {
		writeJSON(w, http.StatusOK, requestID, map[string]any{"run": runResponse(run), "signal": signal.Kind, "target": signal.Target})
		return
	}
	if !claim.Owner {
		w.Header().Set("Retry-After", strconv.Itoa(int(signalClaimLease/time.Second)))
		writeError(w, requestID, http.StatusConflict, "signal_in_flight", "another caller is delivering this signal; retry after the delivery lease expires")
		return
	}
	if claim.Owner {
		requestedPayload, _ := json.Marshal(map[string]string{
			"kind": signal.Kind, "target": signal.Target, "idempotency": keyHash, "fingerprint": fingerprint,
		})
		if _, err := h.storage.AppendEvent(r.Context(), store.Event{Scope: route.scope, RunID: route.runID, Type: "run.signal_requested", Payload: requestedPayload}); err != nil {
			writeStoreError(w, requestID, err)
			return
		}
	}
	// The raw key is an HTTP admission detail. Pass only its digest beyond the
	// claim boundary so a Temporal adapter cannot accidentally persist it.
	signal.IdempotencyKey = keyHash
	if err := h.runSignaler.SignalRun(r.Context(), route.scope, route.runID, signal); err != nil {
		failedPayload, _ := json.Marshal(map[string]string{"kind": signal.Kind, "target": signal.Target, "idempotency": keyHash, "fingerprint": fingerprint})
		_, _ = h.storage.AppendEvent(r.Context(), store.Event{Scope: route.scope, RunID: route.runID, Type: "run.signal_failed", Payload: failedPayload})
		if errors.Is(err, ErrWorkflowNotFound) {
			writeError(w, requestID, http.StatusConflict, "workflow_not_found", "the durable workflow execution was not found")
			return
		}
		writeError(w, requestID, http.StatusServiceUnavailable, "workflow_unavailable", "the durable workflow could not accept the signal")
		return
	}
	if err := h.storage.AcceptRunSignal(r.Context(), route.scope, route.runID, keyHash, fingerprint, claimTokenHash); err != nil {
		// Temporal accepted the signal, but the durable claim could not be
		// acknowledged. A caller may safely retry after the lease; the response
		// must not falsely report a durable acceptance.
		writeError(w, requestID, http.StatusServiceUnavailable, "idempotency_unavailable", "the signal was delivered but its durable acceptance could not be recorded")
		return
	}
	acceptedPayload, _ := json.Marshal(map[string]string{"kind": signal.Kind, "target": signal.Target, "idempotency": keyHash, "fingerprint": fingerprint})
	if _, err := h.storage.AppendEvent(r.Context(), store.Event{Scope: route.scope, RunID: route.runID, Type: "run.signal_accepted", Payload: acceptedPayload}); err != nil {
		// The Temporal call already succeeded, so surface an ambiguous outcome
		// instead of claiming a durable audit record was written.
		writeError(w, requestID, http.StatusServiceUnavailable, "audit_unavailable", "the signal was delivered but its audit record could not be written")
		return
	}
	writeJSON(w, http.StatusAccepted, requestID, map[string]any{"run": runResponse(run), "signal": signal.Kind, "target": signal.Target})
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, requestID, http.MethodGet)
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "after must be a non-negative integer")
			return
		}
		after = parsed
	} else if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "Last-Event-ID must be a non-negative integer")
			return
		}
		after = parsed
	}
	limit := defaultPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > maxPageSize {
			writeError(w, requestID, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 5000")
			return
		}
		limit = parsed
	}
	events, err := h.storage.ListEvents(r.Context(), route.scope, route.runID, after)
	if err != nil {
		writeStoreError(w, requestID, err)
		return
	}
	if len(events) > limit {
		events = events[:limit]
	}
	stream := r.URL.Query().Get("stream") == "1" || r.URL.Query().Get("stream") == "true" || strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
	if stream {
		h.writeSSEStream(w, r, requestID, route, events, r.URL.Query().Get("follow") != "false")
		return
	}
	response := make([]eventResponse, 0, len(events))
	for _, event := range events {
		response = append(response, eventResponse{Sequence: event.Sequence, Type: event.Type, Payload: redactJSON(event.Payload), CreatedAt: event.CreatedAt})
	}
	writeJSON(w, http.StatusOK, requestID, map[string]any{"events": response})
}

func (h *Handler) handleArtifact(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, requestID, http.MethodGet)
		return
	}
	if h.artifacts == nil {
		writeError(w, requestID, http.StatusServiceUnavailable, "artifact_catalog_unavailable", "the artifact catalog is not configured")
		return
	}
	if route.artifactID == "" {
		records, err := h.artifacts.ListArtifactVersions(r.Context(), route.scope, "")
		if err != nil {
			writeStoreError(w, requestID, err)
			return
		}
		versions, err := decodeArtifactVersions(records)
		if err != nil {
			writeError(w, requestID, http.StatusInternalServerError, "artifact_catalog_invalid", "the artifact catalog contains an invalid version")
			return
		}
		writeJSON(w, http.StatusOK, requestID, versions)
		return
	}
	if route.versionID == "" {
		records, err := h.artifacts.ListArtifactVersions(r.Context(), route.scope, route.artifactID)
		if err != nil {
			writeStoreError(w, requestID, err)
			return
		}
		if len(records) == 0 {
			writeError(w, requestID, http.StatusNotFound, "not_found", "artifact not found")
			return
		}
		versions, err := decodeArtifactVersions(records)
		if err != nil {
			writeError(w, requestID, http.StatusInternalServerError, "artifact_catalog_invalid", "the artifact catalog contains an invalid version")
			return
		}
		writeJSON(w, http.StatusOK, requestID, map[string]any{"artifactId": route.artifactID, "versions": versions})
		return
	}
	record, err := h.artifacts.GetArtifactVersion(r.Context(), route.scope, route.artifactID, route.versionID)
	if err != nil {
		writeStoreError(w, requestID, err)
		return
	}
	version, err := decodeArtifactVersion(record)
	if err != nil {
		writeError(w, requestID, http.StatusInternalServerError, "artifact_catalog_invalid", "the artifact catalog contains an invalid version")
		return
	}
	if !route.artifactContent {
		writeJSON(w, http.StatusOK, requestID, version)
		return
	}
	if h.objects == nil {
		writeError(w, requestID, http.StatusServiceUnavailable, "artifact_storage_unavailable", "artifact storage is not configured")
		return
	}
	opened, err := h.objects.Open(r.Context(), route.scope.OrganizationID, route.scope.ProjectID, record.RunID, record.ContentObjectKey)
	if err != nil {
		writeError(w, requestID, http.StatusServiceUnavailable, "artifact_storage_unavailable", "artifact content is unavailable")
		return
	}
	defer opened.Body.Close()
	if opened.SizeBytes != version.Content.SizeBytes {
		writeError(w, requestID, http.StatusInternalServerError, "artifact_integrity_failed", "artifact content failed integrity validation")
		return
	}
	verified, err := stageVerifiedArtifact(opened.Body, opened.SizeBytes, version.Content.Digest)
	if err != nil {
		writeError(w, requestID, http.StatusInternalServerError, "artifact_integrity_failed", "artifact content failed integrity validation")
		return
	}
	defer func() {
		name := verified.Name()
		_ = verified.Close()
		_ = os.Remove(name)
	}()
	w.Header().Set("Content-Type", version.Content.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(opened.SizeBytes, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="artifact-`+version.VersionID+`"`)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.CopyN(w, verified, opened.SizeBytes)
}

// stageVerifiedArtifact prevents corrupted or replaced object bytes from
// reaching a browser. A private temporary file keeps memory use bounded and
// lets the handler verify the complete digest before committing headers.
func stageVerifiedArtifact(body io.Reader, size int64, expectedDigest string) (*os.File, error) {
	if body == nil || size < 0 || !digestPattern.MatchString(expectedDigest) {
		return nil, errors.New("invalid artifact integrity metadata")
	}
	temporary, err := os.CreateTemp("", "agw-artifact-download-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		name := temporary.Name()
		_ = temporary.Close()
		_ = os.Remove(name)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(body, size+1))
	if err != nil || written != size {
		cleanup()
		return nil, errors.New("artifact size mismatch")
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != expectedDigest {
		cleanup()
		return nil, errors.New("artifact digest mismatch")
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return temporary, nil
}

func decodeArtifactVersions(records []store.ArtifactVersion) ([]artifactcatalog.Version, error) {
	versions := make([]artifactcatalog.Version, 0, len(records))
	for _, record := range records {
		version, err := decodeArtifactVersion(record)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, nil
}

func decodeArtifactVersion(record store.ArtifactVersion) (artifactcatalog.Version, error) {
	var version artifactcatalog.Version
	if err := decodeStrictJSON(record.Document, &version); err != nil {
		return artifactcatalog.Version{}, err
	}
	if version.ArtifactID != record.ArtifactID || version.VersionID != record.VersionID {
		return artifactcatalog.Version{}, errors.New("artifact catalog identity mismatch")
	}
	return version, nil
}

type routeKind uint8

const (
	routeResource routeKind = iota + 1
	routeCollection
	routeRun
	routeEvents
	routeArtifact
	routeUsage
	routeAudit
)

type parsedRoute struct {
	kind                  routeKind
	scope                 store.Scope
	kindName              string
	name                  string
	runID                 string
	signal                string
	artifactID, versionID string
	artifactContent       bool
	collection            string
	collectionKinds       string
}

const (
	signalCancel   = "cancel"
	signalApproval = "approval"
	signalReply    = "reply"
)

func (r parsedRoute) action(method string) authz.Action {
	switch r.kind {
	case routeResource:
		if method == http.MethodPut || method == http.MethodPost {
			return authz.ActionApply
		}
		return authz.ActionRead
	case routeCollection, routeUsage, routeAudit:
		return authz.ActionRead
	case routeRun:
		if method == http.MethodGet {
			return authz.ActionRead
		}
		if r.signal == signalApproval {
			return authz.ActionApprove
		}
		return authz.ActionRun
	case routeEvents, routeArtifact:
		return authz.ActionRead
	default:
		return authz.ActionRead
	}
}

func (r parsedRoute) resourceID() string {
	switch r.kind {
	case routeResource:
		return r.kindName + "/" + r.name
	case routeCollection, routeUsage, routeAudit:
		return r.collection
	case routeRun, routeEvents:
		if r.signal != "" {
			return "run/" + r.runID + "/" + r.signal
		}
		return "run/" + r.runID
	case routeArtifact:
		if r.artifactID == "" {
			return "artifact"
		}
		if r.versionID == "" {
			return "artifact/" + r.artifactID
		}
		return "artifact/" + r.artifactID + "/version/" + r.versionID
	default:
		return "request"
	}
}

func parseRoute(path string) (parsedRoute, bool) {
	parts := splitPath(path)
	if len(parts) < 6 || parts[0] != "api" || parts[1] != "v1alpha1" || (parts[2] != "organizations" && parts[2] != "orgs") || parts[4] != "projects" {
		return parsedRoute{}, false
	}
	if !validName(parts[3]) || !validName(parts[5]) {
		return parsedRoute{}, false
	}
	route := parsedRoute{scope: store.Scope{OrganizationID: parts[3], ProjectID: parts[5]}}
	if len(parts) >= 7 && parts[6] == "resources" {
		if len(parts) == 7 {
			route.kind, route.collection = routeCollection, "resources"
			return route, true
		}
		if len(parts) != 9 || !validIdentifier(parts[7]) || !validName(parts[8]) {
			return parsedRoute{}, false
		}
		route.kind, route.kindName, route.name = routeResource, parts[7], parts[8]
		return route, true
	}
	if len(parts) >= 7 && parts[6] == "runs" {
		route.kind = routeRun
		if len(parts) == 7 {
			return route, true
		}
		if len(parts) == 8 && validIdentifier(parts[7]) {
			route.runID = parts[7]
			return route, true
		}
		if len(parts) == 9 && parts[8] == "events" && validIdentifier(parts[7]) {
			route.kind, route.runID = routeEvents, parts[7]
			return route, true
		}
		if len(parts) == 9 && validIdentifier(parts[7]) {
			switch parts[8] {
			case "cancel":
				route.signal = signalCancel
			case "approval", "approve":
				route.signal = signalApproval
			case "reply", "user-reply":
				route.signal = signalReply
			default:
				return parsedRoute{}, false
			}
			route.runID = parts[7]
			return route, true
		}
	}
	if len(parts) == 7 {
		switch parts[6] {
		case "artifacts":
			route.kind = routeArtifact
		case "definitions":
			route.kind, route.collection, route.collectionKinds = routeCollection, "definitions", "Agent,Workflow,ToolSet,SkillSet"
		case "approvals":
			route.kind, route.collection, route.collectionKinds = routeCollection, "approvals", spec.KindApproval
		case "runners":
			route.kind, route.collection, route.collectionKinds = routeCollection, "runners", spec.KindRunner
		case "entitlements":
			route.kind, route.collection, route.collectionKinds = routeCollection, "entitlements", spec.KindEntitlement
		case "model-routes":
			route.kind, route.collection, route.collectionKinds = routeCollection, "model-routes", spec.KindModelRoute
		case "quotas", "usage":
			route.kind, route.collection = routeUsage, parts[6]
		case "audit":
			route.kind, route.collection = routeAudit, "audit"
		default:
			return parsedRoute{}, false
		}
		return route, true
	}
	if len(parts) >= 7 && parts[6] == "artifacts" {
		route.kind = routeArtifact
		if len(parts) == 7 {
			return route, true
		}
		if len(parts) == 8 && validIdentifier(parts[7]) {
			route.artifactID = parts[7]
			return route, true
		}
		if len(parts) >= 10 && len(parts) <= 11 && validIdentifier(parts[7]) && parts[8] == "versions" && validIdentifier(parts[9]) {
			route.artifactID, route.versionID = parts[7], parts[9]
			if len(parts) == 11 {
				if parts[10] != "content" {
					return parsedRoute{}, false
				}
				route.artifactContent = true
			}
			return route, true
		}
	}
	return parsedRoute{}, false
}

func (h *Handler) audit(ctx context.Context, scope store.Scope, principalID string, action authz.Action, resourceID, decision, requestID string) error {
	if h.storage == nil {
		return errors.New("store unavailable")
	}
	metadata, _ := json.Marshal(map[string]string{"requestId": requestID})
	return h.storage.AppendAudit(ctx, store.AuditEvent{
		Scope: scope, PrincipalID: nonEmpty(principalID, "anonymous"), Action: string(action),
		ResourceType: "http", ResourceID: nonEmpty(resourceID, "request"), Decision: decision, Metadata: metadata,
	})
}

func principalID(ctx context.Context) string {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return "anonymous"
	}
	return principal.ID
}

type runCreateRequest struct {
	ID               string `json:"id,omitempty"`
	Kind             string `json:"kind"`
	DefinitionDigest string `json:"definitionDigest"`
	IdempotencyKey   string `json:"idempotencyKey,omitempty"`
	AgentRef         string `json:"agentRef,omitempty"`
	WorkflowRef      string `json:"workflowRef,omitempty"`
	InputRef         string `json:"inputRef,omitempty"`
}

type runCancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

type runApprovalRequest struct {
	Target     string `json:"target,omitempty"`
	ApprovalID string `json:"approvalId"`
	StepID     string `json:"stepId,omitempty"`
	Decision   string `json:"decision"`
}

type runReplyRequest struct {
	Target   string `json:"target,omitempty"`
	StepID   string `json:"stepId,omitempty"`
	TaskID   string `json:"taskId,omitempty"`
	ReplyRef string `json:"replyRef"`
}

func decodeRunSignal(kind string, data []byte, suppliedKey, requestID string) (RunSignal, error) {
	idempotencyKey := strings.TrimSpace(suppliedKey)
	if idempotencyKey == "" {
		// X-Request-ID is commonly reused by a test/client middleware across
		// related calls. Scope the fallback per signal kind so an approval and a
		// reply cannot collide when Idempotency-Key is omitted.
		idempotencyKey = requestID + ":" + kind
	}
	if len(idempotencyKey) == 0 || len(idempotencyKey) > 256 || strings.ContainsAny(idempotencyKey, "\r\n\x00") {
		return RunSignal{}, errors.New("idempotency key is not bounded")
	}
	signal := RunSignal{Kind: kind, Target: "workflow", IdempotencyKey: idempotencyKey}
	switch kind {
	case signalCancel:
		var input runCancelRequest
		if err := decodeStrictJSON(data, &input); err != nil {
			return RunSignal{}, err
		}
		if err := validateSignalText(input.Reason, 512, "reason"); err != nil {
			return RunSignal{}, err
		}
		signal.Reason = input.Reason
	case signalApproval:
		var input runApprovalRequest
		if err := decodeStrictJSON(data, &input); err != nil {
			return RunSignal{}, err
		}
		if input.Target == "" {
			input.Target = "workflow"
		}
		if input.Target != "workflow" && input.Target != "agent" {
			return RunSignal{}, errors.New("target must be workflow or agent")
		}
		if err := validateSignalText(input.ApprovalID, 256, "approvalId"); err != nil {
			return RunSignal{}, err
		}
		if err := validateOptionalSignalText(input.StepID, 256, "stepId"); err != nil {
			return RunSignal{}, err
		}
		if input.Target == "agent" && input.StepID == "" {
			return RunSignal{}, errors.New("stepId is required for an agent target")
		}
		if input.Decision != "approved" && input.Decision != "denied" {
			return RunSignal{}, errors.New("decision must be approved or denied")
		}
		signal.Target, signal.ApprovalID, signal.StepID, signal.Decision = input.Target, input.ApprovalID, input.StepID, input.Decision
	case signalReply:
		var input runReplyRequest
		if err := decodeStrictJSON(data, &input); err != nil {
			return RunSignal{}, err
		}
		if input.Target == "" {
			input.Target = "workflow"
		}
		if input.Target != "workflow" && input.Target != "agent" {
			return RunSignal{}, errors.New("target must be workflow or agent")
		}
		if err := validateImmutableReference(input.ReplyRef); err != nil {
			return RunSignal{}, err
		}
		if err := validateOptionalSignalText(input.StepID, 256, "stepId"); err != nil {
			return RunSignal{}, err
		}
		if err := validateOptionalSignalText(input.TaskID, 256, "taskId"); err != nil {
			return RunSignal{}, err
		}
		if input.Target == "agent" && input.StepID == "" {
			return RunSignal{}, errors.New("stepId is required for an agent target")
		}
		signal.Target, signal.StepID, signal.TaskID, signal.ReplyRef = input.Target, input.StepID, input.TaskID, input.ReplyRef
	default:
		return RunSignal{}, errors.New("unsupported run signal")
	}
	return signal, nil
}

func validateSignalText(value string, limit int, field string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return validateOptionalSignalText(value, limit, field)
}

func validateOptionalSignalText(value string, limit int, field string) error {
	if len(value) > limit || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s is not bounded", field)
	}
	return nil
}

func validateImmutableReference(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 4096 || strings.ContainsAny(value, " \t\r\n\x00") {
		return errors.New("replyRef must be a bounded immutable reference")
	}
	return nil
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "Succeeded", "Failed", "Cancelled", "Lost":
		return true
	default:
		return false
	}
}

func runSignalFingerprint(signal RunSignal) string {
	data, _ := json.Marshal(struct {
		Kind, Target, Reason, ApprovalID, Decision, StepID, TaskID, ReplyRef string
	}{signal.Kind, signal.Target, signal.Reason, signal.ApprovalID, signal.Decision, signal.StepID, signal.TaskID, signal.ReplyRef})
	return hashString(string(data))
}

func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (r runCreateRequest) validate() error {
	if r.Kind != spec.KindAgentRun && r.Kind != spec.KindWorkflowRun {
		return errors.New("kind must be AgentRun or WorkflowRun")
	}
	if !digestPattern.MatchString(r.DefinitionDigest) {
		return errors.New("definition digest must be a sha256 digest")
	}
	if r.ID != "" && !validIdentifier(r.ID) {
		return errors.New("invalid run id")
	}
	if len(r.IdempotencyKey) > 256 {
		return errors.New("idempotency key is too long")
	}
	if len(r.InputRef) > 4096 || strings.ContainsAny(r.InputRef, "\r\n") {
		return errors.New("inputRef is not a bounded reference")
	}
	if r.Kind == spec.KindAgentRun && r.AgentRef == "" {
		return errors.New("agentRef is required for AgentRun")
	}
	if r.Kind == spec.KindWorkflowRun && r.WorkflowRef == "" {
		return errors.New("workflowRef is required for WorkflowRun")
	}
	return nil
}

type eventResponse struct {
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

func resourceResponse(resource store.Resource) map[string]any {
	return map[string]any{
		"kind": resource.Kind, "name": resource.Name, "digest": resource.Digest,
		"revision": resource.Revision, "appliedBy": resource.AppliedBy,
		"createdAt": resource.CreatedAt, "document": redactJSON(resource.Document),
	}
}

func runResponse(run store.Run) map[string]any {
	return map[string]any{
		"id": run.ID, "kind": run.Kind, "status": run.Status, "condition": run.Condition,
		"definitionDigest": run.DefinitionDigest, "requestedBy": run.RequestedBy,
		"createdAt": run.CreatedAt, "updatedAt": run.UpdatedAt,
	}
}

func usageResponse(usage store.Usage) map[string]any {
	return map[string]any{
		"activeRuns": usage.ActiveRuns, "totalRuns": usage.TotalRuns,
		"artifactVersions": usage.ArtifactVersions, "artifactBytes": usage.ArtifactBytes,
	}
}

func (h *Handler) projectQuotas(ctx context.Context, scope store.Scope) any {
	resource, err := h.storage.GetResource(ctx, scope, spec.KindProject, scope.ProjectID)
	if err != nil {
		return map[string]any{}
	}
	var document struct {
		Spec struct {
			Quotas map[string]any `json:"quotas"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(resource.Document, &document); err != nil || document.Spec.Quotas == nil {
		return map[string]any{}
	}
	return redactValue(document.Spec.Quotas, "")
}

func decodeResource(data []byte) (spec.Resource, error) {
	var envelope map[string]json.RawMessage
	if err := decodeStrictJSON(data, &envelope); err != nil {
		return nil, err
	}
	var meta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if raw := envelope["apiVersion"]; len(raw) == 0 || json.Unmarshal(raw, &meta.APIVersion) != nil {
		return nil, errors.New("apiVersion is required")
	}
	if raw := envelope["kind"]; len(raw) == 0 || json.Unmarshal(raw, &meta.Kind) != nil {
		return nil, errors.New("kind is required")
	}
	if meta.APIVersion != spec.APIVersion {
		return nil, errors.New("unsupported apiVersion")
	}
	var resource spec.Resource
	switch meta.Kind {
	case spec.KindOrganization:
		resource = &spec.Organization{}
	case spec.KindProject:
		resource = &spec.Project{}
	case spec.KindAgent:
		resource = &spec.Agent{}
	case spec.KindSkillSet:
		resource = &spec.SkillSet{}
	case spec.KindToolSet:
		resource = &spec.ToolSet{}
	case spec.KindSandboxProfile:
		resource = &spec.SandboxProfile{}
	case spec.KindModelRoute:
		resource = &spec.ModelRoute{}
	case spec.KindWorkflow:
		resource = &spec.Workflow{}
	case spec.KindAgentRun:
		resource = &spec.AgentRun{}
	case spec.KindWorkflowRun:
		resource = &spec.WorkflowRun{}
	case spec.KindApproval:
		resource = &spec.Approval{}
	case spec.KindArtifact:
		resource = &spec.Artifact{}
	case spec.KindRunner:
		resource = &spec.Runner{}
	case spec.KindCredential:
		resource = &spec.Credential{}
	case spec.KindEntitlement:
		resource = &spec.Entitlement{}
	default:
		return nil, errors.New("unsupported resource kind")
	}
	if err := decodeStrictJSON(data, resource); err != nil {
		return nil, err
	}
	return resource, nil
}

func decodeStrictJSON(data []byte, target any) error {
	if err := strictjson.Validate(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func readJSONBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/json" && !strings.HasSuffix(contentType, "+json") {
		return nil, bodyError{status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"}
	}
	if r.ContentLength > limit {
		return nil, bodyError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large"}
	}
	limited := io.LimitReader(r.Body, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, bodyError{status: http.StatusBadRequest, code: "invalid_request"}
	}
	if int64(len(data)) > limit {
		return nil, bodyError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large"}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, bodyError{status: http.StatusBadRequest, code: "invalid_request"}
	}
	return data, nil
}

type bodyError struct {
	status int
	code   string
}

func (e bodyError) Error() string { return e.code }

func writeBodyError(w http.ResponseWriter, requestID string, err error) {
	var body bodyError
	if errors.As(err, &body) {
		writeError(w, requestID, body.status, body.code, safeMessage(body.code))
		return
	}
	writeError(w, requestID, http.StatusBadRequest, "invalid_request", "request body is invalid")
}

func writeStoreError(w http.ResponseWriter, requestID string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, requestID, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, requestID, http.StatusConflict, "conflict", "request conflicts with existing state")
	default:
		writeError(w, requestID, http.StatusInternalServerError, "internal", "the control plane could not complete the request")
	}
}

func writeJSON(w http.ResponseWriter, status int, requestID string, data any) {
	writeResponse(w, status, requestID, map[string]any{"data": data})
}

func writeError(w http.ResponseWriter, requestID string, status int, code, message string) {
	writeResponse(w, status, requestID, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeResponse(w http.ResponseWriter, status int, requestID string, response map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	response["requestId"] = requestID
	data, err := json.Marshal(response)
	if err != nil {
		// All response types are standard JSON values. This is a defensive
		// fallback that cannot expose the original object or an internal error.
		status = http.StatusInternalServerError
		data = []byte(`{"error":{"code":"internal","message":"the control plane could not encode the response"},"requestId":"` + escapeJSONString(requestID) + `"}`)
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func (h *Handler) writeSSEStream(w http.ResponseWriter, r *http.Request, requestID string, route parsedRoute, events []store.Event, follow bool) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	writeEvents := func(events []store.Event) int64 {
		var last int64
		for _, event := range events {
			last = event.Sequence
			payload := map[string]any{"sequence": event.Sequence, "type": event.Type, "payload": redactJSON(event.Payload), "createdAt": event.CreatedAt}
			data, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, safeEventType(event.Type), data)
		}
		return last
	}
	last := writeEvents(events)
	if len(events) == 0 {
		_, _ = io.WriteString(w, ": heartbeat "+requestID+"\n\n")
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	if !follow {
		return
	}
	poll := time.NewTicker(time.Second)
	heartbeat := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": heartbeat "+requestID+"\n\n")
			flusher.Flush()
		case <-poll.C:
			pending, err := h.storage.ListEvents(r.Context(), route.scope, route.runID, last)
			if err != nil {
				_, _ = io.WriteString(w, "event: stream.error\ndata: {\"code\":\"stream_unavailable\"}\n\n")
				flusher.Flush()
				return
			}
			if len(pending) > 0 {
				last = writeEvents(pending)
				flusher.Flush()
			}
		}
	}
}

func methodNotAllowed(w http.ResponseWriter, requestID string, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}

func isHealthPath(path string) bool { return path == "/healthz" || path == "/health" }
func isReadyPath(path string) bool  { return path == "/readyz" || path == "/ready" }

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" || strings.Contains(trimmed, "//") {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	for i := range parts {
		if parts[i] == "" {
			return nil
		}
	}
	return parts
}

func validName(value string) bool       { return dnsNamePattern.MatchString(value) }
func validIdentifier(value string) bool { return identifierPattern.MatchString(value) }

func requestID(r *http.Request, sequence *uint64) string {
	if supplied := r.Header.Get("X-Request-ID"); validIdentifier(supplied) {
		return supplied
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("req-%d", atomic.AddUint64(sequence, 1))
}

func randomID(prefix string) string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return prefix + hex.EncodeToString(random[:])
	}
	return prefix + strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
}

func deterministicRunID(scope store.Scope, key string) string {
	data := []byte(scope.OrganizationID + "\x00" + scope.ProjectID + "\x00" + key)
	sum := sha256.Sum256(data)
	return "run-" + hex.EncodeToString(sum[:])
}

func redactJSON(data []byte) json.RawMessage {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return json.RawMessage(`null`)
	}
	return marshalRedacted(value)
}

func marshalRedacted(value any) json.RawMessage {
	redacted := redactValue(value, "")
	data, err := json.Marshal(redacted)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return data
}

func redactValue(value any, key string) any {
	if isSensitiveKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, child := range typed {
			if childKey == "value" {
				if name, ok := typed["name"].(string); ok && isSensitiveKey(name) {
					out[childKey] = "[REDACTED]"
					continue
				}
			}
			out[childKey] = redactValue(child, childKey)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactValue(child, "")
		}
		return out
	default:
		return value
	}
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"secret", "token", "password", "apikey", "api_key", "authorization", "privatekey", "private_key"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func safeEventType(value string) string {
	if !identifierPattern.MatchString(value) {
		return "event"
	}
	return value
}

func safeMessage(code string) string {
	switch code {
	case "payload_too_large":
		return "request body exceeds the configured limit"
	case "unsupported_media_type":
		return "content type must be application/json"
	default:
		return "request body is invalid"
	}
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func escapeJSONString(value string) string {
	data, _ := json.Marshal(value)
	if len(data) >= 2 {
		return string(data[1 : len(data)-1])
	}
	return ""
}
