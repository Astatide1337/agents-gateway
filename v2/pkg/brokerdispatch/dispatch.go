package brokerdispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

type sessionRecord struct {
	mu          sync.Mutex
	identityKey string
	fingerprint string
	result      runbroker.CreateResult
	directory   string
	directoryIn os.FileInfo
	server      *runbroker.UnixServer
	taskID      string
	closed      bool
}

// Activities is a workflow.RunnerActivities implementation that owns the
// lifecycle of brokered task sessions while delegating actual scheduling and
// execution to Inner.
type Activities struct {
	inner workflow.RunnerActivities
	cfg   Config
	root  string

	mu         sync.Mutex
	closed     bool
	byIdentity map[string]*sessionRecord
	byTaskID   map[string]*sessionRecord
}

// New validates and prepares one private broker root. Reconciliation removes
// only stale, wrapper-owned session directories whose socket lock is not held.
func New(inner workflow.RunnerActivities, config Config) (*Activities, error) {
	if inner == nil {
		return nil, errors.Join(ErrInvalidConfig, errors.New("runner activities are required"))
	}
	if config.Manager == nil {
		return nil, errors.Join(ErrInvalidConfig, errors.New("broker manager is required"))
	}
	if config.Factory == nil {
		return nil, errors.Join(ErrInvalidConfig, errors.New("handler factory is required"))
	}
	if strings.TrimSpace(config.UserID) == "" {
		return nil, errors.Join(ErrInvalidConfig, errors.New("user identity is required"))
	}
	if config.RunnerUID == 0 || config.RunnerGID == 0 {
		return nil, errors.Join(ErrInvalidConfig, errors.New("runner uid and gid must be non-zero"))
	}
	if config.SetSandboxSessionID == nil {
		config.SetSandboxSessionID = SetBrokerSession
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = defaultSessionTTL
	}
	root := filepath.Clean(config.BrokerRoot)
	if err := ensurePrivateRoot(root); err != nil {
		return nil, err
	}
	a := &Activities{
		inner:      inner,
		cfg:        config,
		root:       root,
		byIdentity: make(map[string]*sessionRecord),
		byTaskID:   make(map[string]*sessionRecord),
	}
	if err := a.Reconcile(context.Background()); err != nil {
		return nil, err
	}
	return a, nil
}

var _ workflow.RunnerActivities = (*Activities)(nil)

func (a *Activities) ScheduleRunnerTask(ctx context.Context, input workflow.ScheduleRunnerTaskInput) (workflow.ScheduleRunnerTaskResult, error) {
	if err := contextErr(ctx); err != nil {
		return workflow.ScheduleRunnerTaskResult{}, err
	}
	identityKey, fingerprint, err := sessionFingerprint(input)
	if err != nil {
		return workflow.ScheduleRunnerTaskResult{}, fmt.Errorf("%w: fingerprint broker session: %v", ErrInvalidConfig, err)
	}
	wantsBroker := input.Contract.ModelRoute != nil || input.Contract.ToolSet != nil

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return workflow.ScheduleRunnerTaskResult{}, ErrClosed
	}
	record := a.byIdentity[identityKey]
	if record != nil && record.fingerprint != fingerprint {
		a.mu.Unlock()
		return workflow.ScheduleRunnerTaskResult{}, ErrIdentityMismatch
	}
	if !wantsBroker {
		a.mu.Unlock()
		return a.inner.ScheduleRunnerTask(ctx, input)
	}
	if record == nil {
		record, err = a.createSessionLocked(ctx, input, identityKey, fingerprint)
		if err != nil {
			a.mu.Unlock()
			return workflow.ScheduleRunnerTaskResult{}, err
		}
		a.byIdentity[identityKey] = record
	}
	a.mu.Unlock()

	record.mu.Lock()
	defer record.mu.Unlock()
	if record.closed {
		return workflow.ScheduleRunnerTaskResult{}, ErrSessionClosed
	}
	prepared := input
	prepared.Execution.Network = runner.NetworkBrokered
	prepared.Execution.DirectInternet = false
	if err := a.cfg.SetSandboxSessionID(&prepared.Execution, string(record.result.Session.ID)); err != nil {
		_ = a.cleanupRecordLocked(record)
		if errors.Is(err, ErrIdentityMismatch) {
			return workflow.ScheduleRunnerTaskResult{}, ErrIdentityMismatch
		}
		return workflow.ScheduleRunnerTaskResult{}, ErrInvalidConfig
	}
	result, callErr := a.inner.ScheduleRunnerTask(ctx, prepared)
	if callErr != nil {
		return result, callErr
	}
	a.mu.Lock()
	if result.TaskID != "" {
		record.taskID = result.TaskID
		a.byTaskID[result.TaskID] = record
	}
	a.mu.Unlock()
	if isTerminalStatus(result.Status) {
		if cleanupErr := a.cleanupRecordLocked(record); cleanupErr != nil {
			return workflow.ScheduleRunnerTaskResult{}, cleanupErr
		}
	}
	return result, nil
}

func (a *Activities) CancelRunnerTask(ctx context.Context, input workflow.CancelRunnerTaskInput) error {
	record := a.recordForTask(input.TaskID)
	err := a.inner.CancelRunnerTask(ctx, input)
	if record == nil {
		return err
	}
	record.mu.Lock()
	cleanupErr := a.cleanupRecordLocked(record)
	record.mu.Unlock()
	if err != nil {
		return err
	}
	return cleanupErr
}

func (a *Activities) ResumeRunnerTask(ctx context.Context, input workflow.ResumeRunnerTaskInput) error {
	return a.inner.ResumeRunnerTask(ctx, input)
}

func (a *Activities) StatusRunnerTask(ctx context.Context, input workflow.StatusRunnerTaskInput) (workflow.StatusRunnerTaskResult, error) {
	result, err := a.inner.StatusRunnerTask(ctx, input)
	if err != nil {
		return result, err
	}
	record := a.recordForTask(input.TaskID)
	if record == nil || !isTerminalStatus(result.Status) {
		return result, nil
	}
	record.mu.Lock()
	cleanupErr := a.cleanupRecordLocked(record)
	record.mu.Unlock()
	if cleanupErr != nil {
		return workflow.StatusRunnerTaskResult{}, cleanupErr
	}
	return result, nil
}

func (a *Activities) createSessionLocked(ctx context.Context, input workflow.ScheduleRunnerTaskInput, identityKey, fingerprint string) (*sessionRecord, error) {
	binding := runbroker.SessionBinding{
		OrgID:     input.OrganizationID,
		ProjectID: input.ProjectID,
		UserID:    a.cfg.UserID,
		RunID:     input.RunID,
	}
	handlerSpec, err := a.cfg.Factory.NewHandler(ctx, HandlerRequest{Input: input, Binding: binding})
	if err != nil {
		var setupError *handlerSetupError
		if errors.As(err, &setupError) {
			return nil, fmt.Errorf("%w: resolve broker handler at %s", ErrInvalidConfig, setupError.stage)
		}
		return nil, fmt.Errorf("%w: resolve broker handler", ErrInvalidConfig)
	}
	if handlerSpec.Handler == nil || handlerSpec.AllowedModel == "" || handlerSpec.PolicyDigest == "" {
		return nil, fmt.Errorf("%w: broker handler omitted a required field", ErrInvalidConfig)
	}
	ttl := handlerSpec.TTL
	if ttl <= 0 {
		ttl = a.cfg.SessionTTL
	}
	created, err := a.cfg.Manager.Create(ctx, runbroker.CreateRequest{
		Binding:       binding,
		AllowedModels: []string{handlerSpec.AllowedModel},
		PolicyDigest:  handlerSpec.PolicyDigest,
		TTL:           ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: create broker session: %v", ErrInvalidConfig, err)
	}
	directory, directoryInfo, err := createSessionDirectory(a.root, string(created.Session.ID))
	if err != nil {
		_ = a.cfg.Manager.Revoke(context.Background(), created.Session.ID)
		_ = a.cfg.Manager.Delete(context.Background(), created.Session.ID)
		return nil, err
	}
	if handlerSpec.PrepareSandbox != nil {
		if err := handlerSpec.PrepareSandbox(ctx, directory); err != nil {
			_ = a.cfg.Manager.Revoke(context.Background(), created.Session.ID)
			_ = a.cfg.Manager.Delete(context.Background(), created.Session.ID)
			_ = removeOwnedDirectory(directory, directoryInfo)
			return nil, fmt.Errorf("%w: prepare brokered sandbox inputs", ErrInvalidConfig)
		}
	}
	server, err := runbroker.NewUnixServer(runbroker.UnixServerConfig{
		SocketPath:   filepath.Join(directory, "broker.sock"),
		SessionID:    created.Session.ID,
		Binding:      binding,
		PolicyDigest: handlerSpec.PolicyDigest,
		Sessions:     a.cfg.Manager,
		Peer: runbroker.PeerPolicy{
			UID: uint32Pointer(a.cfg.RunnerUID),
			GID: uint32Pointer(a.cfg.RunnerGID),
		},
		Handler: handlerSpec.Handler,
	})
	if err != nil {
		_ = a.cfg.Manager.Revoke(context.Background(), created.Session.ID)
		_ = a.cfg.Manager.Delete(context.Background(), created.Session.ID)
		_ = removeOwnedDirectory(directory, directoryInfo)
		return nil, fmt.Errorf("%w: create broker Unix server: %v", ErrInvalidConfig, err)
	}
	if err := writeClientConfig(directory, clientConfig{
		SessionID:       string(created.Session.ID),
		BearerToken:     created.Token.String(),
		PolicyDigest:    handlerSpec.PolicyDigest,
		AllowedModel:    handlerSpec.AllowedModel,
		ModelURL:        ModelLoopbackURL,
		ToolsURL:        ToolsLoopbackURL,
		ArtifactURL:     ArtifactLoopbackURL,
		ToolsEnabled:    input.Contract.ToolSet != nil,
		ArtifactEnabled: true,
	}); err != nil {
		_ = server.Close(context.Background())
		_ = a.cfg.Manager.Revoke(context.Background(), created.Session.ID)
		_ = a.cfg.Manager.Delete(context.Background(), created.Session.ID)
		_ = removeOwnedDirectory(directory, directoryInfo)
		return nil, fmt.Errorf("%w: write broker client configuration: %v", ErrInvalidConfig, err)
	}
	go func() { _ = server.Serve(context.Background()) }()
	return &sessionRecord{
		identityKey: identityKey,
		fingerprint: fingerprint,
		result:      created,
		directory:   directory,
		directoryIn: directoryInfo,
		server:      server,
	}, nil
}

func (a *Activities) recordForTask(taskID string) *sessionRecord {
	if taskID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.byTaskID[taskID]
}

// cleanupRecordLocked must be called with record.mu held. The order is
// intentional: close listener, revoke, delete, then remove the exact owned
// directory.
func (a *Activities) cleanupRecordLocked(record *sessionRecord) error {
	if record == nil || record.closed {
		return nil
	}
	record.closed = true
	var failed bool
	if record.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		err := record.server.Close(ctx)
		cancel()
		if err != nil {
			failed = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	err := a.cfg.Manager.Revoke(ctx, record.result.Session.ID)
	cancel()
	if err != nil {
		failed = true
	}
	ctx, cancel = context.WithTimeout(context.Background(), cleanupTimeout)
	err = a.cfg.Manager.Delete(ctx, record.result.Session.ID)
	cancel()
	if err != nil {
		failed = true
	}
	if err := removeOwnedDirectory(record.directory, record.directoryIn); err != nil {
		failed = true
	}
	a.mu.Lock()
	delete(a.byIdentity, record.identityKey)
	if record.taskID != "" {
		delete(a.byTaskID, record.taskID)
	}
	a.mu.Unlock()
	if failed {
		return ErrUnsafePath
	}
	return nil
}

// Reconcile removes stale wrapper-owned session directories. A held
// broker.sock.lock means another process owns the live listener, so that
// directory is left untouched.
func (a *Activities) Reconcile(ctx context.Context) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(a.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrUnsafePath
	}
	a.mu.Lock()
	active := make(map[string]struct{}, len(a.byIdentity))
	for _, record := range a.byIdentity {
		active[record.directory] = struct{}{}
	}
	a.mu.Unlock()
	for _, entry := range entries {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if !strings.HasPrefix(entry.Name(), "ags_") {
			continue
		}
		if err := runner.ValidateBrokerSessionID(entry.Name()); err != nil {
			return ErrUnsafePath
		}
		path := filepath.Join(a.root, entry.Name())
		if _, ok := active[path]; ok {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return ErrUnsafePath
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return ErrUnsafePath
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || uint32(stat.Uid) != uint32(os.Geteuid()) {
			return ErrUnsafePath
		}
		held, lockErr := lockIsHeld(filepath.Join(path, "broker.sock.lock"))
		if lockErr != nil || held {
			if held {
				continue
			}
			return ErrUnsafePath
		}
		if err := removeOwnedDirectory(path, info); err != nil {
			return ErrUnsafePath
		}
	}
	return nil
}

// Close stops and removes every session owned by this wrapper. It is safe to
// call repeatedly.
func (a *Activities) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	records := make([]*sessionRecord, 0, len(a.byIdentity))
	for _, record := range a.byIdentity {
		records = append(records, record)
	}
	a.mu.Unlock()
	var failed bool
	for _, record := range records {
		record.mu.Lock()
		if err := a.cleanupRecordLocked(record); err != nil {
			failed = true
		}
		record.mu.Unlock()
	}
	if failed {
		return ErrUnsafePath
	}
	return nil
}

func isTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "failed", "cancelled", "lost":
		return true
	default:
		return false
	}
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func uint32Pointer(value uint32) *uint32 { return &value }
