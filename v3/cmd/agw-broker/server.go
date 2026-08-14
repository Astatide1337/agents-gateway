package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/broker"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	maxHeaderBytes       = 16 << 10
	maxHeaderCount       = 64
	maxHeaderValueBytes  = 4096
	requestTimeout       = 5 * time.Minute
	serverReadHeaderTime = 5 * time.Second
	serverIdleTimeout    = 60 * time.Second
	shutdownTimeout      = 10 * time.Second
)

type brokerRuntime struct {
	server *http.Server
}

func newBrokerRuntime(ctx context.Context, config brokerConfig, storage storageDependencies) (*brokerRuntime, error) {
	if ctx == nil || storage.ArtifactStore == nil || storage.Effects == nil || storage.RuntimeStore == nil {
		return nil, configError{code: "broker_dependencies_invalid"}
	}
	credentials, err := newFileCredentialResolver(config.ToolSet, config.ModelRoute, config.SecretFiles, config.CredentialsDir)
	if err != nil {
		return nil, err
	}
	resultStore := storage.ArtifactStore
	if config.ResultScratchDir != "" {
		resultStore, err = broker.NewFilesystemResultStore(config.ResultScratchDir, int64(strictjson.MaxDocumentBytes))
		if err != nil {
			return nil, configError{code: "result_scratch_initialization_failed"}
		}
	}
	policyManifest, err := loadPolicyContract(config.ContextPackDir, config.RunUID, config.SpecDigest, config.BaseSHA)
	if err != nil {
		return nil, configError{code: "policy_contract_initialization_failed"}
	}
	policyBroker, err := broker.New(broker.Config{
		RunUID: config.RunUID, SpecDigest: config.SpecDigest, BaseSHA: config.BaseSHA,
		ToolSet: config.ToolSet, ModelRoute: config.ModelRoute,
		Credentials: credentials, Effects: storage.Effects, Artifacts: storage.ArtifactStore, ResultStore: resultStore,
		WorkspaceRoot: config.WorkspaceDir, ContextRoot: config.ContextPackDir,
		PolicyContract: policyManifest,
		Pricing:        config.Pricing, RequireApprovalForMutations: config.RequireApproval,
		MaxToolCalls: config.MaxToolCalls, MaxCostUSD: config.MaxCostUSD, MaxModelTokens: config.MaxModelTokens,
	})
	if err != nil {
		return nil, configError{code: "broker_policy_initialization_failed"}
	}
	if config.ContextPackDir != "" {
		if _, err := policyBroker.PublishContextArtifact(ctx); err != nil {
			// Keep the externally visible startup code stable while retaining the
			// bounded validation reason in the sidecar log. This is the only
			// useful diagnostic for a sealed context-pack mismatch.
			log.Printf("context artifact initialization detail: %v", err)
			return nil, configError{code: "context_artifact_initialization_failed"}
		}
	}
	mcpHandler, err := policyBroker.MCPHandler()
	if err != nil {
		return nil, configError{code: "mcp_handler_initialization_failed"}
	}
	artifactHandler, err := broker.NewArtifactHandler(policyBroker)
	if err != nil {
		return nil, configError{code: "artifact_handler_initialization_failed"}
	}

	// RuntimeHandler retains a process-exit authorization value for its trusted
	// process-wait boundary. The command deliberately does not wire a phase
	// transitioner or supervisor: Explore -> Edit remains denied until a real
	// independently observing deployment adapter exists.
	exitToken, err := newRuntimeExitToken()
	if err != nil {
		return nil, err
	}
	defer wipeBytes(exitToken)
	supervisor, err := broker.NewRuntimeSupervisor(broker.RuntimeConfig{
		RunUID: config.RunUID, SpecDigest: config.SpecDigest, BaseSHA: config.BaseSHA,
		Store: storage.RuntimeStore,
	})
	if err != nil {
		return nil, configError{code: "runtime_supervisor_initialization_failed"}
	}
	runtimeHandler, err := broker.NewRuntimeHandler(broker.RuntimeHandlerConfig{
		Supervisor: supervisor, ProcessExitToken: exitToken,
	})
	if err != nil {
		return nil, configError{code: "runtime_handler_initialization_failed"}
	}

	handler := newBrokerRouter(policyBroker, mcpHandler, artifactHandler, runtimeHandler, policyBroker)
	server := &http.Server{
		Addr:              config.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTime,
		ReadTimeout:       requestTimeout,
		WriteTimeout:      requestTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return &brokerRuntime{server: server}, nil
}

func loadPolicyContract(root, runUID, specDigest, baseSHA string) (policycontract.Compiled, error) {
	if root == "" || runUID == "" || specDigest == "" || baseSHA == "" {
		return policycontract.Compiled{}, policycontract.ErrInvalidInput
	}
	name := filepath.Join(root, filepath.FromSlash(policycontract.ManifestPath))
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > policycontract.MaxManifestBytes {
		return policycontract.Compiled{}, policycontract.ErrInvalidInput
	}
	file, err := os.Open(name)
	if err != nil {
		return policycontract.Compiled{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, policycontract.MaxManifestBytes+1))
	if err != nil || len(body) == 0 || len(body) > policycontract.MaxManifestBytes {
		return policycontract.Compiled{}, policycontract.ErrInvalidInput
	}
	compiled, err := policycontract.DecodeManifest(body)
	if err != nil {
		return policycontract.Compiled{}, err
	}
	manifest := compiled.Manifest()
	if manifest.BaseSHA != baseSHA || manifest.ResolvedSpecDigest != specDigest {
		return policycontract.Compiled{}, policycontract.ErrInvalidInput
	}
	return compiled, nil
}

func newBrokerRouter(policyHandler http.Handler, mcpHandler, artifactHandler, runtimeHandler http.Handler, anthropicHandler ...http.Handler) http.Handler {
	var messages http.Handler
	if len(anthropicHandler) > 0 {
		messages = anthropicHandler[0]
	}
	return boundedRouter{
		policyHandler: policyHandler, mcpHandler: mcpHandler,
		artifactHandler: artifactHandler, runtimeHandler: runtimeHandler, anthropicHandler: messages,
	}
}

type boundedRouter struct {
	policyHandler    http.Handler
	mcpHandler       http.Handler
	artifactHandler  http.Handler
	runtimeHandler   http.Handler
	anthropicHandler http.Handler
}

func (r boundedRouter) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request == nil || response == nil || request.URL == nil || request.URL.RawQuery != "" || !isLoopbackRemote(request.RemoteAddr) {
		writeStaticHTTPError(response, http.StatusNotFound)
		return
	}
	if !boundedHeaders(request.Header) {
		writeStaticHTTPError(response, http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	var next http.Handler
	switch request.URL.Path {
	case "/v1/responses":
		next = r.policyHandler
	case "/v1/messages", "/api/v1/messages":
		next = r.anthropicHandler
	case "/mcp":
		next = r.mcpHandler
	case "/v1/artifacts/output", "/v1/artifacts/create":
		next = r.artifactHandler
	case broker.RuntimeEventsPath:
		// Runtime events are the sole runtime path enabled on the agent-facing
		// listener. No phase-supervisor endpoint is exposed until an independent
		// deployment adapter is proven.
		next = r.runtimeHandler
	default:
		writeStaticHTTPError(response, http.StatusNotFound)
		return
	}
	if next == nil {
		writeStaticHTTPError(response, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
	defer cancel()
	next.ServeHTTP(response, request.WithContext(ctx))
}

func isLoopbackRemote(remote string) bool {
	if remote == "" {
		return false
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func boundedHeaders(header http.Header) bool {
	if len(header) > maxHeaderCount {
		return false
	}
	total := 0
	for key, values := range header {
		if len(key) == 0 || len(key) > maxHeaderValueBytes || len(values) > maxHeaderCount {
			return false
		}
		total += len(key)
		if total > maxHeaderBytes {
			return false
		}
		for _, value := range values {
			if len(value) > maxHeaderValueBytes {
				return false
			}
			total += len(value)
			if total > maxHeaderBytes {
				return false
			}
		}
	}
	return true
}

func writeStaticHTTPError(response http.ResponseWriter, status int) {
	if response == nil {
		return
	}
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, `{"error":"request rejected"}`)
}

func (r *brokerRuntime) serve(ctx context.Context) error {
	if r == nil || r.server == nil || ctx == nil {
		return configError{code: "server_initialization_failed"}
	}
	listener, err := net.Listen("tcp", r.server.Addr)
	if err != nil {
		return configError{code: "loopback_listener_failed"}
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- r.server.Serve(listener)
	}()
	shutdown := func(serveCompleted bool) error {
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownErr := r.server.Shutdown(shutdownContext)
		if shutdownErr != nil {
			return configError{code: "http_server_shutdown_failed"}
		}
		if !serveCompleted {
			serveErr := <-serveDone
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return configError{code: "http_server_failed"}
			}
		}
		return nil
	}

	select {
	case err := <-serveDone:
		if shutdownErr := shutdown(true); shutdownErr != nil {
			return shutdownErr
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return configError{code: "http_server_failed"}
	case <-ctx.Done():
		return shutdown(false)
	}
}
