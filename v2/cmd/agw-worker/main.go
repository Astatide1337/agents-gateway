// agw-worker runs deterministic Temporal workflows and the narrow activities
// that dispatch work to an mTLS-authenticated runner host.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/orchestration"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runnerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runstate"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/telemetry"
	"github.com/Astatide1337/agents-gateway/v2/pkg/temporalconfig"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

func main() { os.Exit(run()) }

func run() int {
	telemetryConfig, err := telemetry.FromEnv("agw-worker")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: %v\n", err)
		return 1
	}
	telemetryShutdown, err := telemetry.Setup(context.Background(), telemetryConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: configure telemetry: %v\n", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "agw-worker: telemetry shutdown: %v\n", err)
		}
	}()

	temporalOptions, err := temporalconfig.Config{
		Address:        strings.TrimSpace(os.Getenv("AGW_TEMPORAL_ADDRESS")),
		Namespace:      envOr("AGW_TEMPORAL_NAMESPACE", "default"),
		Environment:    strings.TrimSpace(os.Getenv("AGW_ENVIRONMENT")),
		ServerName:     strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_SERVER_NAME")),
		CAFile:         strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_CA_FILE")),
		ClientCertFile: strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_CERT_FILE")),
		ClientKeyFile:  strings.TrimSpace(os.Getenv("AGW_TEMPORAL_TLS_KEY_FILE")),
		Insecure:       envBool("AGW_TEMPORAL_INSECURE"),
	}.ClientOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: configure Temporal: %v\n", err)
		return 1
	}
	temporalClient, err := client.Dial(temporalOptions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: connect Temporal: %v\n", err)
		return 1
	}
	defer temporalClient.Close()
	database, storage, err := configureStore(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: configure state store: %v\n", err)
		return 1
	}
	defer database.Close()

	runnerClient, err := configureRunnerClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: configure runner: %v\n", err)
		return 1
	}

	taskQueue := envOr("AGW_TEMPORAL_TASK_QUEUE", orchestration.DefaultTaskQueue)
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     envInt("AGW_WORKER_MAX_ACTIVITIES", 32),
		MaxConcurrentWorkflowTaskExecutionSize: envInt("AGW_WORKER_MAX_WORKFLOWS", 64),
	})
	temporalWorker.RegisterWorkflowWithOptions(agentworkflow.Workflow, temporalworkflow.RegisterOptions{Name: agentworkflow.WorkflowName})
	temporalWorker.RegisterWorkflowWithOptions(agentworkflow.AgentRunWorkflow, temporalworkflow.RegisterOptions{Name: agentworkflow.AgentRunName})
	agentworkflow.RegisterActivities(temporalWorker, runnerClient)
	agentworkflow.RegisterRunStateActivities(temporalWorker, runstate.StoreActivity{Store: storage})

	if err := temporalWorker.Run(worker.InterruptCh()); err != nil {
		fmt.Fprintf(os.Stderr, "agw-worker: %v\n", err)
		return 1
	}
	return 0
}

func configureRunnerClient() (*runnerdispatch.Client, error) {
	timeout := envDuration("AGW_RUNNER_REQUEST_TIMEOUT", 30*time.Second)
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGW_RUNNER_TRANSPORT"))) {
	case "unix":
		return runnerdispatch.NewUnix(envOr("AGW_RUNNER_SOCKET", "/run/agw-runner/runner.sock"), timeout)
	case "", "mtls":
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

func configureStore(ctx context.Context) (*sql.DB, store.Store, error) {
	databaseURL := strings.TrimSpace(os.Getenv("AGW_DATABASE_URL"))
	if databaseURL == "" {
		return nil, nil, errors.New("AGW_DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, nil, err
	}
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := database.PingContext(pingCtx); err != nil {
		database.Close()
		return nil, nil, err
	}
	storage, err := store.NewPostgreSQL(database)
	if err != nil {
		database.Close()
		return nil, nil, err
	}
	return database, storage, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "true")
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

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
