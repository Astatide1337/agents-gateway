// agw-server is the small, net/http control-plane entrypoint for Agents
// Gateway v2. Persistent database and runner integrations are intentionally
// outside this bounded surface; the command uses the v2 Store interface so a
// durable implementation can be supplied without changing the HTTP package.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerfactory"
	"github.com/Astatide1337/agents-gateway/v2/pkg/httpapi"
	"github.com/Astatide1337/agents-gateway/v2/pkg/identity"
	"github.com/Astatide1337/agents-gateway/v2/pkg/localengine"
	"github.com/Astatide1337/agents-gateway/v2/pkg/oidcauth"
	"github.com/Astatide1337/agents-gateway/v2/pkg/orchestration"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runnerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/serviceauth"
	"github.com/Astatide1337/agents-gateway/v2/pkg/skills"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/telemetry"
	"github.com/Astatide1337/agents-gateway/v2/pkg/temporalconfig"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.temporal.io/sdk/client"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("agw-server", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	addr := flags.String("addr", envOr("AGW_SERVER_ADDR", ":8094"), "HTTP listen address")
	maxBody := flags.Int64("max-body-bytes", envInt64("AGW_HTTP_MAX_BODY_BYTES", 1<<20), "maximum JSON request body size")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *maxBody <= 0 {
		fmt.Fprintln(os.Stderr, "agw-server: max-body-bytes must be positive")
		return 2
	}
	telemetryConfig, err := telemetry.FromEnv("agw-server")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-server: %v\n", err)
		return 1
	}
	telemetryShutdown, err := telemetry.Setup(context.Background(), telemetryConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-server: configure telemetry: %v\n", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			log.Printf("telemetry shutdown: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	storage, database, err := configureStore(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-server: %v\n", err)
		return 1
	}
	if database != nil {
		defer database.Close()
	}
	authenticator, authDatabase, exchangeHandler, err := configureAuthenticator(ctx, database)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-server: %v\n", err)
		return 1
	}
	if authDatabase != nil && authDatabase != database {
		defer authDatabase.Close()
	}
	artifactStorage, err := configureArtifactStorage()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-server: configure artifact storage: %v\n", err)
		return 1
	}
	execution, err := configureRunEngine(storage, database, artifactStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-server: %v\n", err)
		return 1
	}
	defer execution.Close()
	apiHandler := httpapi.New(storage, authenticator, httpapi.Options{
		MaxBodyBytes:       *maxBody,
		ProductionValidate: true,
		RunStarter:         execution.Starter,
		RunSignaler:        execution.Signaler,
		ArtifactStore:      artifactStorage,
		ReadyCheck: func(ctx context.Context) error {
			var readinessErrors []error
			if database != nil {
				readinessErrors = append(readinessErrors, database.PingContext(ctx))
			}
			if authDatabase != nil && authDatabase != database {
				readinessErrors = append(readinessErrors, authDatabase.PingContext(ctx))
			}
			if execution.Ready != nil {
				readinessErrors = append(readinessErrors, execution.Ready(ctx))
			}
			return errors.Join(readinessErrors...)
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/auth/token", exchangeHandler)
	mux.Handle("/", apiHandler)

	server := &http.Server{
		Addr:              *addr,
		Handler:           otelhttp.NewHandler(mux, "agw.http"),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streaming run events are long-lived. Per-request body limits and
		// upstream proxy idle timeouts protect ordinary requests.
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	log.Printf("agw-server listening on %s", *addr)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.ListenAndServe() }()
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var engineErrors <-chan error
	if execution.Run != nil {
		channel := make(chan error, 1)
		engineErrors = channel
		go func() { channel <- execution.Run(shutdownContext) }()
	}
	shutdown := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
	select {
	case <-shutdownContext.Done():
		if err := shutdown(); err != nil {
			fmt.Fprintf(os.Stderr, "agw-server: graceful shutdown: %v\n", err)
			return 1
		}
		return 0
	case err := <-engineErrors:
		stop()
		shutdownErr := shutdown()
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "agw-server: local execution engine: %v\n", err)
			return 1
		}
		if shutdownErr != nil {
			fmt.Fprintf(os.Stderr, "agw-server: graceful shutdown: %v\n", shutdownErr)
			return 1
		}
		return 0
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "agw-server: %v\n", err)
		return 1
	}
}

type configuredRunEngine struct {
	Starter  httpapi.RunStarter
	Signaler httpapi.RunSignaler
	Ready    func(context.Context) error
	Run      func(context.Context) error
	Close    func()
}

func configureRunEngine(storage store.Store, database *sql.DB, artifacts *artifact.Store) (configuredRunEngine, error) {
	switch strings.ToLower(strings.TrimSpace(envOr("AGW_ORCHESTRATION_MODE", "temporal"))) {
	case "local":
		if database == nil {
			return configuredRunEngine{}, errors.New("local orchestration requires PostgreSQL")
		}
		runnerClient, err := configureLocalRunnerClient()
		if err != nil {
			return configuredRunEngine{}, fmt.Errorf("configure local runner: %w", err)
		}
		brokeredActivities, err := configureBrokeredActivities(storage, runnerClient, artifacts)
		if err != nil {
			return configuredRunEngine{}, fmt.Errorf("configure run broker: %w", err)
		}
		engine := localengine.New(database, brokeredActivities)
		engine.Compiler = orchestration.Compiler{
			Store:       storage,
			SandboxUser: configuredSandboxUser(),
		}
		localScope := store.Scope{
			OrganizationID: strings.TrimSpace(os.Getenv("AGW_LOCAL_ORGANIZATION_ID")),
			ProjectID:      strings.TrimSpace(os.Getenv("AGW_LOCAL_PROJECT_ID")),
		}
		if err := localScope.Validate(); err != nil {
			return configuredRunEngine{}, errors.New("local orchestration requires AGW_LOCAL_ORGANIZATION_ID and AGW_LOCAL_PROJECT_ID")
		}
		engine.Options.Scopes = []store.Scope{localScope}
		hostname, err := os.Hostname()
		if err != nil || strings.TrimSpace(hostname) == "" {
			hostname = "standalone"
		}
		workerID := fmt.Sprintf("%s/%d", hostname, os.Getpid())
		return configuredRunEngine{
			Starter: engine, Signaler: engine,
			Ready: runnerClient.Ready,
			Run:   func(ctx context.Context) error { return engine.Run(ctx, workerID) },
			Close: func() { _ = brokeredActivities.Close() },
		}, nil
	case "temporal":
		starter, temporalClient, err := configureWorkflowStarter(storage)
		if err != nil {
			return configuredRunEngine{}, err
		}
		return configuredRunEngine{
			Starter: starter, Signaler: starter,
			Ready: func(ctx context.Context) error {
				_, err := temporalClient.CheckHealth(ctx, nil)
				return err
			},
			Close: temporalClient.Close,
		}, nil
	default:
		return configuredRunEngine{}, errors.New("AGW_ORCHESTRATION_MODE must be local or temporal")
	}
}

func configureBrokeredActivities(storage store.Store, runnerClient *runnerdispatch.Client, artifacts *artifact.Store) (*brokerdispatch.Activities, error) {
	effects, ok := storage.(interface {
		Claim(context.Context, string, string, string, string, string) (bool, error)
		Complete(context.Context, string, string, string, string, string, []byte) error
	})
	if !ok {
		return nil, errors.New("configured store does not implement the tool effect ledger")
	}
	if artifacts == nil {
		return nil, errors.New("artifact storage is required")
	}
	uid, err := envUint32("AGW_RUNNER_UID", 65532)
	if err != nil || uid == 0 {
		return nil, errors.New("AGW_RUNNER_UID must be a non-zero uint32")
	}
	gid, err := envUint32("AGW_RUNNER_GID", uid)
	if err != nil || gid == 0 {
		return nil, errors.New("AGW_RUNNER_GID must be a non-zero uint32")
	}
	manager, err := runbroker.NewManager(runbroker.ManagerConfig{MaxTTL: 24*time.Hour + 10*time.Minute})
	if err != nil {
		return nil, err
	}
	factory := &brokerfactory.Factory{
		Store:         storage,
		Artifacts:     artifacts,
		Effects:       effects,
		OpenAIURL:     strings.TrimSpace(os.Getenv("AGW_OPENAI_RESPONSES_URL")),
		OpenRouterURL: strings.TrimSpace(os.Getenv("AGW_OPENROUTER_RESPONSES_URL")),
	}
	skillsClient, err := configureSkillsGateway()
	if err != nil {
		return nil, err
	}
	factory.Skills = skillsClient
	return brokerdispatch.New(runnerClient, brokerdispatch.Config{
		BrokerRoot: envOr("AGW_BROKER_ROOT", "/run/agw-broker"),
		Manager:    manager,
		Factory:    factory,
		UserID:     envOr("AGW_AUTH_PRINCIPAL_ID", "local-owner"),
		RunnerUID:  uid,
		RunnerGID:  gid,
		SessionTTL: 35 * time.Minute,
	})
}

func configureArtifactStorage() (*artifact.Store, error) {
	artifactRoot := envOr("AGW_ARTIFACT_ROOT", "/var/lib/agw/artifacts")
	localObjects, err := artifact.NewLocalObjectClient(artifactRoot)
	if err != nil {
		return nil, fmt.Errorf("open local artifact store: %w", err)
	}
	return artifact.New(localObjects, artifact.Config{
		Bucket:      "local",
		Prefix:      "runs",
		MaxBytes:    8 << 20,
		DownloadTTL: 5 * time.Minute,
	})
}

func configureSkillsGateway() (*skills.GatewayClient, error) {
	endpoint := strings.TrimSpace(os.Getenv("AGW_SKILLS_GATEWAY_URL"))
	if endpoint == "" {
		return nil, nil
	}
	tokenEnvironment := envOr("AGW_SKILLS_GATEWAY_TOKEN_ENV", "SKILLS_GATEWAY_AUTH_TOKEN")
	client, err := skills.NewGatewayClient(endpoint, func(context.Context) (string, error) {
		value, exists := os.LookupEnv(tokenEnvironment)
		if !exists || strings.TrimSpace(value) == "" {
			return "", errors.New("Skills Gateway credential is unavailable")
		}
		return value, nil
	})
	if err != nil {
		return nil, errors.New("AGW_SKILLS_GATEWAY_URL is invalid")
	}
	return client, nil
}

func configureLocalRunnerClient() (*runnerdispatch.Client, error) {
	timeout := envDuration("AGW_RUNNER_REQUEST_TIMEOUT", 30*time.Second)
	switch strings.ToLower(strings.TrimSpace(envOr("AGW_RUNNER_TRANSPORT", "unix"))) {
	case "unix":
		return runnerdispatch.NewUnix(envOr("AGW_RUNNER_SOCKET", "/run/agw-runner/runner.sock"), timeout)
	case "mtls":
		return runnerdispatch.New(runnerdispatch.Config{
			Endpoint:       strings.TrimSpace(os.Getenv("AGW_RUNNER_ENDPOINT")),
			ServerName:     strings.TrimSpace(os.Getenv("AGW_RUNNER_TLS_SERVER_NAME")),
			CAFile:         strings.TrimSpace(os.Getenv("AGW_RUNNER_TLS_CA_FILE")),
			ClientCertFile: strings.TrimSpace(os.Getenv("AGW_RUNNER_TLS_CERT_FILE")),
			ClientKeyFile:  strings.TrimSpace(os.Getenv("AGW_RUNNER_TLS_KEY_FILE")),
			Timeout:        timeout,
		})
	default:
		return nil, errors.New("AGW_RUNNER_TRANSPORT must be unix or mtls")
	}
}

func configureWorkflowStarter(storage store.Store) (orchestration.Starter, client.Client, error) {
	configuration := temporalconfig.Config{
		Address:        strings.TrimSpace(os.Getenv("AGW_TEMPORAL_ADDRESS")),
		Namespace:      envOr("AGW_TEMPORAL_NAMESPACE", "default"),
		Environment:    strings.TrimSpace(os.Getenv("AGW_ENVIRONMENT")),
		ServerName:     strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_SERVER_NAME")),
		CAFile:         strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_CA_FILE")),
		ClientCertFile: strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_CERT_FILE")),
		ClientKeyFile:  strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_KEY_FILE")),
		Insecure:       strings.EqualFold(strings.TrimSpace(os.Getenv("AGW_TEMPORAL_INSECURE")), "true"),
	}
	options, err := configuration.ClientOptions()
	if err != nil {
		return orchestration.Starter{}, nil, fmt.Errorf("configure Temporal: %w", err)
	}
	temporalClient, err := client.Dial(options)
	if err != nil {
		return orchestration.Starter{}, nil, fmt.Errorf("connect Temporal: %w", err)
	}
	starter := orchestration.Starter{
		Store: storage, Temporal: temporalClient, Signals: temporalClient,
		TaskQueue: envOr("AGW_TEMPORAL_TASK_QUEUE", orchestration.DefaultTaskQueue),
	}
	return starter, temporalClient, nil
}

func configureStore(ctx context.Context) (store.Store, *sql.DB, error) {
	databaseURL := strings.TrimSpace(os.Getenv("AGW_DATABASE_URL"))
	if databaseURL == "" {
		if strings.EqualFold(os.Getenv("AGW_ENVIRONMENT"), "development") {
			return store.NewMemory(), nil, nil
		}
		return nil, nil, errors.New("AGW_DATABASE_URL is required outside development")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	database.SetMaxOpenConns(30)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(30 * time.Minute)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	postgres, err := store.NewPostgreSQL(database)
	if err != nil {
		database.Close()
		return nil, nil, err
	}
	return postgres, database, nil
}

func configureAuthenticator(ctx context.Context, database *sql.DB) (httpapi.Authenticator, *sql.DB, http.Handler, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGW_AUTH_MODE"))) {
	case "local":
		token := strings.TrimSpace(os.Getenv("AGW_AUTH_TOKEN"))
		if len(token) < 32 {
			return nil, nil, nil, errors.New("local authentication requires an AGW_AUTH_TOKEN of at least 32 characters")
		}
		principalID := envOr("AGW_AUTH_PRINCIPAL_ID", "local-owner")
		principal := authz.Principal{ID: principalID, Type: authz.PrincipalHuman, InstanceRole: authz.RoleInstanceAdmin}
		return tokenAuthenticator{token: []byte(token), principal: principal}, nil, serviceauth.Handler{}, nil
	case "oidc":
		oidcIssuer := strings.TrimSpace(os.Getenv("AGW_OIDC_ISSUER"))
		audience := strings.TrimSpace(os.Getenv("AGW_OIDC_AUDIENCE"))
		authDatabase := database
		if authURL := strings.TrimSpace(os.Getenv("AGW_AUTH_DATABASE_URL")); authURL != "" {
			var err error
			authDatabase, err = sql.Open("pgx", authURL)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("open authorization database: %w", err)
			}
			if err := authDatabase.PingContext(ctx); err != nil {
				authDatabase.Close()
				return nil, nil, nil, fmt.Errorf("connect authorization database: %w", err)
			}
		}
		if authDatabase == nil {
			return nil, nil, nil, errors.New("OIDC authentication requires PostgreSQL memberships")
		}
		resolver := membershipResolver{database: authDatabase, bootstrapSubject: strings.TrimSpace(os.Getenv("AGW_BOOTSTRAP_ADMIN_SUBJECT"))}
		authenticator, err := oidcauth.NewWithOptions(ctx, oidcIssuer, audience, resolver, oidcauth.Options{TokenHeader: strings.TrimSpace(os.Getenv("AGW_OIDC_TOKEN_HEADER"))})
		if err != nil {
			if authDatabase != database {
				authDatabase.Close()
			}
			return nil, nil, nil, fmt.Errorf("configure OIDC: %w", err)
		}
		serviceIssuer, err := configureServiceIssuer()
		if err != nil {
			if authDatabase != database {
				authDatabase.Close()
			}
			return nil, nil, nil, err
		}
		combined := serviceauth.CombinedAuthenticator{Humans: authenticator, Issuer: serviceIssuer}
		exchange := serviceauth.Handler{Accounts: serviceauth.PostgreSQL{Database: authDatabase}, Issuer: serviceIssuer, TTL: 10 * time.Minute}
		return combined, authDatabase, exchange, nil
	case "dev-static":
		if !strings.EqualFold(os.Getenv("AGW_ENVIRONMENT"), "development") || !strings.EqualFold(os.Getenv("AGW_ALLOW_INSECURE_DEV_AUTH"), "true") {
			return nil, nil, nil, errors.New("dev-static authentication requires AGW_ENVIRONMENT=development and AGW_ALLOW_INSECURE_DEV_AUTH=true")
		}
		authenticator := newTokenAuthenticatorFromEnv()
		if authenticator == nil {
			return nil, nil, nil, errors.New("AGW_AUTH_TOKEN is required for dev-static authentication")
		}
		return authenticator, nil, serviceauth.Handler{}, nil
	default:
		return nil, nil, nil, errors.New("AGW_AUTH_MODE must be local, oidc, or explicitly gated dev-static")
	}
}

func configureServiceIssuer() (*identity.Issuer, error) {
	encoded := strings.TrimSpace(os.Getenv("AGW_SERVICE_TOKEN_PRIVATE_KEY"))
	if encoded == "" {
		return nil, errors.New("AGW_SERVICE_TOKEN_PRIVATE_KEY is required for production service identity")
	}
	var private []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		private, err = encoding.DecodeString(encoded)
		if err == nil {
			break
		}
	}
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("AGW_SERVICE_TOKEN_PRIVATE_KEY must be a base64-encoded Ed25519 private key")
	}
	issuer, err := identity.NewIssuer(
		envOr("AGW_SERVICE_TOKEN_ISSUER", "agents-gateway"),
		envOr("AGW_SERVICE_TOKEN_AUDIENCE", "agents-gateway-api"),
		envOr("AGW_SERVICE_TOKEN_KEY_ID", "primary"),
		ed25519.PrivateKey(private),
	)
	if err != nil {
		return nil, fmt.Errorf("configure service token issuer: %w", err)
	}
	return issuer, nil
}

type membershipResolver struct {
	database         *sql.DB
	bootstrapSubject string
}

func (r membershipResolver) ResolvePrincipal(ctx context.Context, subject string) (authz.Principal, error) {
	principal := authz.Principal{ID: subject, Type: authz.PrincipalHuman, OrgRoles: map[string]authz.Role{}, ProjectRoles: map[string]authz.Role{}}
	if subject != "" && subject == r.bootstrapSubject {
		principal.InstanceRole = authz.RoleInstanceAdmin
		return principal, nil
	}
	rows, err := r.database.QueryContext(ctx, `
		SELECT organization_id::text, coalesce(project_id::text, ''), role
		FROM memberships WHERE principal_id=$1`, subject)
	if err != nil {
		return authz.Principal{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var organizationID, projectID string
		var role authz.Role
		if err := rows.Scan(&organizationID, &projectID, &role); err != nil {
			return authz.Principal{}, err
		}
		if projectID == "" {
			principal.OrgRoles[organizationID] = role
		} else {
			principal.ProjectRoles[organizationID+"/"+projectID] = role
		}
	}
	if err := rows.Err(); err != nil {
		return authz.Principal{}, err
	}
	return principal, nil
}

type tokenAuthenticator struct {
	token     []byte
	principal authz.Principal
}

func (a tokenAuthenticator) Authenticate(r *http.Request) (authz.Principal, error) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return authz.Principal{}, httpapi.ErrUnauthenticated
	}
	presented := strings.TrimSpace(value[len("Bearer "):])
	if presented == "" || subtle.ConstantTimeCompare([]byte(presented), a.token) != 1 {
		return authz.Principal{}, httpapi.ErrUnauthenticated
	}
	return a.principal, nil
}

func newTokenAuthenticatorFromEnv() httpapi.Authenticator {
	token := strings.TrimSpace(os.Getenv("AGW_AUTH_TOKEN"))
	if token == "" {
		return nil
	}
	principalID := envOr("AGW_AUTH_PRINCIPAL_ID", "configured-token")
	role := parseRole(os.Getenv("AGW_AUTH_ROLE"))
	principal := authz.Principal{ID: principalID, Type: authz.PrincipalServiceAccount}
	if role == authz.RoleInstanceAdmin {
		principal.InstanceRole = role
	} else if organization := strings.TrimSpace(os.Getenv("AGW_AUTH_ORGANIZATION_ID")); organization != "" {
		principal.OrgRoles = map[string]authz.Role{organization: role}
		if project := strings.TrimSpace(os.Getenv("AGW_AUTH_PROJECT_ID")); project != "" && role != "" {
			principal.ProjectRoles = map[string]authz.Role{organization + "/" + project: role}
		}
	}
	return tokenAuthenticator{token: []byte(token), principal: principal}
}

func parseRole(value string) authz.Role {
	switch authz.Role(strings.TrimSpace(value)) {
	case authz.RoleInstanceAdmin, authz.RoleOrgAdmin, authz.RoleProjectEditor, authz.RoleApprover, authz.RoleViewer:
		return authz.Role(strings.TrimSpace(value))
	default:
		// A token without an explicit role authenticates but cannot authorize.
		return ""
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func configuredSandboxUser() string {
	uid := envOr("AGW_RUNNER_UID", "65532")
	gid := envOr("AGW_RUNNER_GID", uid)
	return uid + ":" + gid
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	var parsed int64
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		return fallback
	}
	return parsed
}

func envUint32(name string, fallback uint32) (uint32, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(parsed), nil
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
