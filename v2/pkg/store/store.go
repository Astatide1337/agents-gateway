// Package store defines the durable control-plane state boundary. Production
// uses PostgreSQL; Memory is intentionally strict and exists for tests and the
// local API smoke path.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	// ErrSignalInFlight means another caller currently owns the delivery lease.
	// A caller may retry the same idempotency key after the lease expires.
	ErrSignalInFlight = errors.New("run signal delivery is in flight")
	// ErrSignalClaimLost means a caller attempted to acknowledge a claim that
	// has already been reclaimed by another delivery attempt.
	ErrSignalClaimLost = errors.New("run signal claim is no longer owned")
)

type Scope struct{ OrganizationID, ProjectID string }

func (s Scope) Validate() error {
	if s.OrganizationID == "" || s.ProjectID == "" {
		return errors.New("organization and project are required")
	}
	return nil
}

type Resource struct {
	Scope
	Kind, Name, Digest, AppliedBy string
	Revision                      int64
	Document                      []byte
	CreatedAt                     time.Time
}

type Run struct {
	Scope
	ID, Kind, DefinitionDigest, RequestedBy, Status, Condition, IdempotencyKey string
	CreatedAt, UpdatedAt                                                       time.Time
}

type Event struct {
	Scope
	RunID, Type string
	Sequence    int64
	Payload     []byte
	CreatedAt   time.Time
}

type AuditEvent struct {
	Scope
	PrincipalID, Action, ResourceType, ResourceID, Decision string
	Metadata                                                []byte
	CreatedAt                                               time.Time
}

// Page bounds collection reads. Offset pagination is deliberately explicit
// here: callers cannot make an unbounded query, and the same contract is used
// by the in-memory and PostgreSQL stores.
type Page struct {
	Limit  int
	Offset int
}

const (
	DefaultPageLimit = 100
	MaxPageLimit     = 500
	MaxPageOffset    = 1_000_000
)

func (p Page) Normalize() (Page, error) {
	if p.Limit == 0 {
		p.Limit = DefaultPageLimit
	}
	if p.Limit < 1 || p.Limit > MaxPageLimit {
		return Page{}, fmt.Errorf("page limit must be between 1 and %d", MaxPageLimit)
	}
	if p.Offset < 0 || p.Offset > MaxPageOffset {
		return Page{}, fmt.Errorf("page offset must be between 0 and %d", MaxPageOffset)
	}
	return p, nil
}

type Usage struct {
	ActiveRuns       int64
	TotalRuns        int64
	ArtifactVersions int64
	ArtifactBytes    int64
}

// ArtifactVersion is one immutable catalog entry. Document is the canonical
// artifactcatalog.Version JSON contract; object keys are trusted server-side
// routing metadata and are never returned directly to a browser or sandbox.
type ArtifactVersion struct {
	Scope
	ArtifactID, VersionID, RunID, ContentObjectKey, SourceObjectKey string
	VersionNumber                                                   int64
	Document                                                        []byte
	CreatedAt                                                       time.Time
}

// ArtifactCatalog is separate from Store so alternate control-plane stores
// can opt into artifacts without implementing unrelated execution methods.
type ArtifactCatalog interface {
	PutArtifactVersion(context.Context, ArtifactVersion) (ArtifactVersion, error)
	ListArtifactVersions(context.Context, Scope, string) ([]ArtifactVersion, error)
	GetArtifactVersion(context.Context, Scope, string, string) (ArtifactVersion, error)
}

// ArtifactCatalogListLimit bounds one catalog response until cursor-based
// pagination is part of the public API.
const ArtifactCatalogListLimit = 500

const (
	RunSignalPending  = "pending"
	RunSignalAccepted = "accepted"
)

// RunSignalClaim is the result of an atomic idempotency claim. The store keeps
// only hashes and a random claim-token hash; raw idempotency keys and signal
// payloads never enter durable state.
type RunSignalClaim struct {
	State string
	Owner bool
}

type Store interface {
	ApplyResource(context.Context, Resource) (Resource, error)
	GetResource(context.Context, Scope, string, string) (Resource, error)
	ListResources(context.Context, Scope, string, Page) ([]Resource, bool, error)
	CreateRun(context.Context, Run) (Run, error)
	GetRun(context.Context, Scope, string) (Run, error)
	ListRuns(context.Context, Scope, Page) ([]Run, bool, error)
	SetRunStatus(context.Context, Scope, string, string, string) (Run, error)
	AppendEvent(context.Context, Event) (Event, error)
	ListEvents(context.Context, Scope, string, int64) ([]Event, error)
	AppendAudit(context.Context, AuditEvent) error
	ListAudit(context.Context, Scope, Page) ([]AuditEvent, bool, error)
	GetUsage(context.Context, Scope) (Usage, error)
	ClaimRunSignal(context.Context, Scope, string, string, string, string, time.Duration) (RunSignalClaim, error)
	AcceptRunSignal(context.Context, Scope, string, string, string, string) error
}

type Memory struct {
	mu        sync.RWMutex
	resources map[string][]Resource
	runs      map[string]Run
	events    map[string][]Event
	audit     []AuditEvent
	effects   map[string]effect
	signals   map[string]signalClaim
	artifacts map[string][]ArtifactVersion
	now       func() time.Time
}

func NewMemory() *Memory {
	return &Memory{resources: map[string][]Resource{}, runs: map[string]Run{}, events: map[string][]Event{}, effects: map[string]effect{}, signals: map[string]signalClaim{}, artifacts: map[string][]ArtifactVersion{}, now: func() time.Time { return time.Now().UTC() }}
}

type effect struct {
	Digest string
	State  string
	Result []byte
}

type signalClaim struct {
	Fingerprint    string
	ClaimTokenHash string
	State          string
	ClaimedAt      time.Time
	LeaseExpires   time.Time
	AcceptedAt     time.Time
}

func (m *Memory) ApplyResource(_ context.Context, resource Resource) (Resource, error) {
	if err := resource.Scope.Validate(); err != nil {
		return Resource{}, err
	}
	if resource.Kind == "" || resource.Name == "" || resource.Digest == "" || len(resource.Document) == 0 {
		return Resource{}, errors.New("kind, name, digest, and document are required")
	}
	if err := ValidateJSONDocument(resource.Document); err != nil {
		return Resource{}, fmt.Errorf("resource document is not valid JSON: %w", err)
	}
	normalizedDocument, err := NormalizeJSONDocument(resource.Document)
	if err != nil {
		return Resource{}, fmt.Errorf("normalize resource document: %w", err)
	}
	resource.Document = normalizedDocument
	m.mu.Lock()
	defer m.mu.Unlock()
	key := resourceKey(resource.Scope, resource.Kind, resource.Name)
	revisions := m.resources[key]
	if len(revisions) > 0 && revisions[len(revisions)-1].Digest == resource.Digest {
		if !JSONDocumentsEqual(revisions[len(revisions)-1].Document, resource.Document) {
			return Resource{}, ErrConflict
		}
		return cloneResource(revisions[len(revisions)-1]), nil
	}
	resource.Revision = int64(len(revisions) + 1)
	resource.CreatedAt = m.now()
	resource.Document = append([]byte(nil), resource.Document...)
	m.resources[key] = append(revisions, resource)
	return cloneResource(resource), nil
}

func (m *Memory) GetResource(_ context.Context, scope Scope, kind, name string) (Resource, error) {
	if err := scope.Validate(); err != nil {
		return Resource{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	revisions := m.resources[resourceKey(scope, kind, name)]
	if len(revisions) == 0 {
		return Resource{}, ErrNotFound
	}
	return cloneResource(revisions[len(revisions)-1]), nil
}

func (m *Memory) ListResources(_ context.Context, scope Scope, kind string, page Page) ([]Resource, bool, error) {
	page, err := page.Normalize()
	if err != nil {
		return nil, false, err
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]Resource, 0)
	prefix := scope.OrganizationID + "/" + scope.ProjectID + "/"
	for key, revisions := range m.resources {
		if !strings.HasPrefix(key, prefix) || len(revisions) == 0 {
			continue
		}
		resource := revisions[len(revisions)-1]
		if kindMatches(resource.Kind, kind) {
			items = append(items, cloneResource(resource))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			if items[i].Kind == items[j].Kind {
				return items[i].Name < items[j].Name
			}
			return items[i].Kind < items[j].Kind
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return pageResources(items, page)
}

func kindMatches(value, filter string) bool {
	if strings.TrimSpace(filter) == "" {
		return true
	}
	for _, candidate := range strings.Split(filter, ",") {
		if strings.TrimSpace(candidate) == value {
			return true
		}
	}
	return false
}

func (m *Memory) CreateRun(_ context.Context, run Run) (Run, error) {
	if err := run.Scope.Validate(); err != nil {
		return Run{}, err
	}
	if run.ID == "" || run.Kind == "" || run.DefinitionDigest == "" || run.RequestedBy == "" {
		return Run{}, errors.New("run identity, kind, definition digest, and requester are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := runKey(run.Scope, run.ID)
	if _, ok := m.runs[key]; ok {
		return Run{}, ErrConflict
	}
	if run.IdempotencyKey != "" {
		for _, existing := range m.runs {
			if existing.Scope == run.Scope && existing.IdempotencyKey == run.IdempotencyKey {
				return existing, nil
			}
		}
	}
	if run.Status == "" {
		run.Status = "Pending"
	}
	run.CreatedAt, run.UpdatedAt = m.now(), m.now()
	m.runs[key] = run
	return run, nil
}

func (m *Memory) PutArtifactVersion(_ context.Context, version ArtifactVersion) (ArtifactVersion, error) {
	if err := validateArtifactVersion(version); err != nil {
		return ArtifactVersion{}, err
	}
	normalizedDocument, err := NormalizeJSONDocument(version.Document)
	if err != nil {
		return ArtifactVersion{}, fmt.Errorf("normalize artifact document: %w", err)
	}
	version.Document = normalizedDocument
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runKey(version.Scope, version.RunID)]; !ok {
		return ArtifactVersion{}, ErrNotFound
	}
	key := artifactKey(version.Scope, version.ArtifactID)
	for _, existing := range m.artifacts[key] {
		if existing.VersionID == version.VersionID || existing.VersionNumber == version.VersionNumber {
			if existing.VersionID == version.VersionID && JSONDocumentsEqual(existing.Document, version.Document) {
				return cloneArtifactVersion(existing), nil
			}
			return ArtifactVersion{}, ErrConflict
		}
	}
	version.CreatedAt = m.now()
	version.Document = append([]byte(nil), version.Document...)
	m.artifacts[key] = append(m.artifacts[key], version)
	sort.Slice(m.artifacts[key], func(i, j int) bool { return m.artifacts[key][i].VersionNumber < m.artifacts[key][j].VersionNumber })
	return cloneArtifactVersion(version), nil
}

func (m *Memory) ListArtifactVersions(_ context.Context, scope Scope, artifactID string) ([]ArtifactVersion, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []ArtifactVersion
	if artifactID != "" {
		for _, version := range m.artifacts[artifactKey(scope, artifactID)] {
			result = append(result, cloneArtifactVersion(version))
		}
	} else {
		prefix := scope.OrganizationID + "/" + scope.ProjectID + "/"
		for key, versions := range m.artifacts {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			for _, version := range versions {
				result = append(result, cloneArtifactVersion(version))
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].VersionID > result[j].VersionID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	if len(result) > ArtifactCatalogListLimit {
		result = result[:ArtifactCatalogListLimit]
	}
	return result, nil
}

func (m *Memory) GetArtifactVersion(_ context.Context, scope Scope, artifactID, versionID string) (ArtifactVersion, error) {
	if err := scope.Validate(); err != nil {
		return ArtifactVersion{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.artifacts[artifactKey(scope, artifactID)]
	for index := len(versions) - 1; index >= 0; index-- {
		if versionID == "" || versions[index].VersionID == versionID {
			return cloneArtifactVersion(versions[index]), nil
		}
	}
	return ArtifactVersion{}, ErrNotFound
}

func (m *Memory) GetRun(_ context.Context, scope Scope, id string) (Run, error) {
	if err := scope.Validate(); err != nil {
		return Run{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	run, ok := m.runs[runKey(scope, id)]
	if !ok {
		return Run{}, ErrNotFound
	}
	return run, nil
}

func (m *Memory) ListRuns(_ context.Context, scope Scope, page Page) ([]Run, bool, error) {
	page, err := page.Normalize()
	if err != nil {
		return nil, false, err
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]Run, 0)
	prefix := scope.OrganizationID + "/" + scope.ProjectID + "/"
	for key, run := range m.runs {
		if strings.HasPrefix(key, prefix) {
			items = append(items, run)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return pageRuns(items, page)
}

func (m *Memory) SetRunStatus(_ context.Context, scope Scope, id, status, condition string) (Run, error) {
	if err := scope.Validate(); err != nil {
		return Run{}, err
	}
	if !validRunStatus(status) {
		return Run{}, errors.New("invalid run status")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := runKey(scope, id)
	run, ok := m.runs[key]
	if !ok {
		return Run{}, ErrNotFound
	}
	if run.Status == status && run.Condition == condition {
		return run, nil
	}
	if !validRunTransition(run.Status, status) {
		return Run{}, fmt.Errorf("%w: invalid run transition %s -> %s", ErrConflict, run.Status, status)
	}
	run.Status, run.Condition, run.UpdatedAt = status, condition, m.now()
	m.runs[key] = run
	payload, _ := json.Marshal(map[string]string{"status": status, "condition": condition})
	event := Event{Scope: scope, RunID: id, Type: "run.status_changed", Payload: payload, CreatedAt: m.now()}
	eventKey := runKey(scope, id)
	event.Sequence = int64(len(m.events[eventKey]) + 1)
	m.events[eventKey] = append(m.events[eventKey], event)
	return run, nil
}

func (m *Memory) AppendEvent(_ context.Context, event Event) (Event, error) {
	if err := event.Scope.Validate(); err != nil {
		return Event{}, err
	}
	if len(event.Payload) == 0 {
		event.Payload = []byte(`{}`)
	}
	normalizedPayload, err := NormalizeJSONDocument(event.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("event payload is not valid JSON: %w", err)
	}
	event.Payload = normalizedPayload
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runKey(event.Scope, event.RunID)]; !ok {
		return Event{}, ErrNotFound
	}
	key := runKey(event.Scope, event.RunID)
	event.Sequence = int64(len(m.events[key]) + 1)
	event.CreatedAt = m.now()
	event.Payload = append([]byte(nil), event.Payload...)
	m.events[key] = append(m.events[key], event)
	return event, nil
}

func (m *Memory) ListEvents(_ context.Context, scope Scope, runID string, after int64) ([]Event, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[runKey(scope, runID)]; !ok {
		return nil, ErrNotFound
	}
	var result []Event
	for _, event := range m.events[runKey(scope, runID)] {
		if event.Sequence > after {
			event.Payload = append([]byte(nil), event.Payload...)
			result = append(result, event)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result, nil
}

func (m *Memory) AppendAudit(_ context.Context, event AuditEvent) error {
	if event.OrganizationID == "" || event.ProjectID == "" || event.PrincipalID == "" || event.Action == "" || event.ResourceType == "" || event.ResourceID == "" || event.Decision == "" {
		return errors.New("complete audit event is required")
	}
	if len(event.Metadata) == 0 {
		event.Metadata = []byte(`{}`)
	}
	normalizedMetadata, err := NormalizeJSONDocument(event.Metadata)
	if err != nil {
		return fmt.Errorf("audit metadata is not valid JSON: %w", err)
	}
	event.Metadata = normalizedMetadata
	m.mu.Lock()
	defer m.mu.Unlock()
	event.CreatedAt = m.now()
	event.Metadata = append([]byte(nil), event.Metadata...)
	m.audit = append(m.audit, event)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, scope Scope, page Page) ([]AuditEvent, bool, error) {
	page, err := page.Normalize()
	if err != nil {
		return nil, false, err
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]AuditEvent, 0)
	for index := len(m.audit) - 1; index >= 0; index-- {
		event := m.audit[index]
		if event.Scope == scope {
			event.Metadata = append([]byte(nil), event.Metadata...)
			items = append(items, event)
		}
	}
	return pageAudit(items, page)
}

func (m *Memory) GetUsage(_ context.Context, scope Scope) (Usage, error) {
	if err := scope.Validate(); err != nil {
		return Usage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var usage Usage
	prefix := scope.OrganizationID + "/" + scope.ProjectID + "/"
	for key, run := range m.runs {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		usage.TotalRuns++
		if run.Status != "Succeeded" && run.Status != "Failed" && run.Status != "Cancelled" && run.Status != "Lost" {
			usage.ActiveRuns++
		}
	}
	for key, versions := range m.artifacts {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		usage.ArtifactVersions += int64(len(versions))
		for _, version := range versions {
			usage.ArtifactBytes += int64(len(version.Document))
		}
	}
	return usage, nil
}

func (m *Memory) ClaimRunSignal(_ context.Context, scope Scope, runID, keyHash, fingerprint, claimTokenHash string, lease time.Duration) (RunSignalClaim, error) {
	if err := scope.Validate(); err != nil {
		return RunSignalClaim{}, err
	}
	if runID == "" || keyHash == "" || fingerprint == "" || claimTokenHash == "" || lease <= 0 {
		return RunSignalClaim{}, errors.New("run, signal hashes, claim token, and positive lease are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runKey(scope, runID)]; !ok {
		return RunSignalClaim{}, ErrNotFound
	}
	key := signalKey(scope, runID, keyHash)
	now := m.now()
	current, exists := m.signals[key]
	if !exists {
		m.signals[key] = signalClaim{Fingerprint: fingerprint, ClaimTokenHash: claimTokenHash, State: RunSignalPending, ClaimedAt: now, LeaseExpires: now.Add(lease)}
		return RunSignalClaim{State: RunSignalPending, Owner: true}, nil
	}
	if current.Fingerprint != fingerprint {
		return RunSignalClaim{}, ErrConflict
	}
	if current.State == RunSignalAccepted {
		return RunSignalClaim{State: RunSignalAccepted}, nil
	}
	if current.LeaseExpires.After(now) {
		return RunSignalClaim{State: RunSignalPending}, nil
	}
	current.ClaimTokenHash, current.ClaimedAt, current.LeaseExpires = claimTokenHash, now, now.Add(lease)
	m.signals[key] = current
	return RunSignalClaim{State: RunSignalPending, Owner: true}, nil
}

func (m *Memory) AcceptRunSignal(_ context.Context, scope Scope, runID, keyHash, fingerprint, claimTokenHash string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if runID == "" || keyHash == "" || fingerprint == "" || claimTokenHash == "" {
		return errors.New("run, signal hashes, and claim token are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := signalKey(scope, runID, keyHash)
	current, ok := m.signals[key]
	if !ok {
		return ErrNotFound
	}
	if current.Fingerprint != fingerprint {
		return ErrConflict
	}
	if current.State == RunSignalAccepted {
		return nil
	}
	if current.ClaimTokenHash != claimTokenHash {
		return ErrSignalClaimLost
	}
	now := m.now()
	current.State, current.AcceptedAt = RunSignalAccepted, now
	m.signals[key] = current
	return nil
}

func (m *Memory) Claim(_ context.Context, organizationID, projectID, runID, key, digest string) (bool, error) {
	scope := Scope{OrganizationID: organizationID, ProjectID: projectID}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if runID == "" || key == "" || digest == "" {
		return false, errors.New("run, effect key, and request digest are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runKey(scope, runID)]; !ok {
		return false, ErrNotFound
	}
	id := runKey(scope, runID) + "/" + key
	if _, exists := m.effects[id]; exists {
		return false, nil
	}
	m.effects[id] = effect{Digest: digest, State: "claimed"}
	return true, nil
}

func (m *Memory) Complete(_ context.Context, organizationID, projectID, runID, key, state string, result []byte) error {
	if state != "succeeded" && state != "failed" && state != "unknown" {
		return errors.New("invalid terminal effect state")
	}
	scope := Scope{OrganizationID: organizationID, ProjectID: projectID}
	if err := scope.Validate(); err != nil {
		return err
	}
	if len(result) == 0 {
		result = []byte(`null`)
	}
	if err := ValidateJSONDocument(result); err != nil {
		return fmt.Errorf("effect result must be valid JSON: %w", err)
	}
	normalizedResult, err := NormalizeJSONDocument(result)
	if err != nil {
		return fmt.Errorf("normalize effect result: %w", err)
	}
	result = normalizedResult
	m.mu.Lock()
	defer m.mu.Unlock()
	id := runKey(scope, runID) + "/" + key
	current, ok := m.effects[id]
	if !ok {
		return ErrNotFound
	}
	if current.State != "claimed" {
		if current.State == state && JSONDocumentsEqual(current.Result, result) {
			return nil
		}
		return ErrConflict
	}
	current.State, current.Result = state, append([]byte(nil), result...)
	m.effects[id] = current
	return nil
}

func resourceKey(scope Scope, kind, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", scope.OrganizationID, scope.ProjectID, kind, name)
}
func runKey(scope Scope, id string) string {
	return fmt.Sprintf("%s/%s/%s", scope.OrganizationID, scope.ProjectID, id)
}
func signalKey(scope Scope, runID, keyHash string) string {
	return fmt.Sprintf("%s/%s/%s/%s", scope.OrganizationID, scope.ProjectID, runID, keyHash)
}
func artifactKey(scope Scope, artifactID string) string {
	return fmt.Sprintf("%s/%s/%s", scope.OrganizationID, scope.ProjectID, artifactID)
}
func cloneResource(resource Resource) Resource {
	resource.Document = append([]byte(nil), resource.Document...)
	return resource
}

func cloneArtifactVersion(version ArtifactVersion) ArtifactVersion {
	version.Document = append([]byte(nil), version.Document...)
	return version
}

func pageResources(items []Resource, page Page) ([]Resource, bool, error) {
	return pageSlice(items, page, func(index int) Resource { return items[index] })
}

func pageRuns(items []Run, page Page) ([]Run, bool, error) {
	return pageSlice(items, page, func(index int) Run { return items[index] })
}

func pageAudit(items []AuditEvent, page Page) ([]AuditEvent, bool, error) {
	return pageSlice(items, page, func(index int) AuditEvent { return items[index] })
}

func pageSlice[T any](items []T, page Page, get func(int) T) ([]T, bool, error) {
	if page.Offset >= len(items) {
		return []T{}, false, nil
	}
	end := page.Offset + page.Limit
	if end > len(items) {
		end = len(items)
	}
	result := make([]T, end-page.Offset)
	for index := range result {
		result[index] = get(page.Offset + index)
	}
	return result, end < len(items), nil
}

func validateArtifactVersion(version ArtifactVersion) error {
	if err := version.Scope.Validate(); err != nil {
		return err
	}
	if version.ArtifactID == "" || version.VersionID == "" || version.RunID == "" || version.ContentObjectKey == "" || version.SourceObjectKey == "" || version.VersionNumber < 1 || len(version.Document) == 0 {
		return errors.New("complete artifact version is required")
	}
	if err := ValidateJSONDocument(version.Document); err != nil {
		return fmt.Errorf("artifact version document must be valid JSON: %w", err)
	}
	return nil
}

func validRunStatus(status string) bool {
	switch status {
	case "Pending", "Scheduled", "Starting", "Running", "WaitingApproval", "WaitingCapacity", "Succeeded", "Failed", "Cancelled", "Lost":
		return true
	default:
		return false
	}
}

func validRunTransition(from, to string) bool {
	if from == to {
		return true
	}
	if from == "Succeeded" || from == "Failed" || from == "Cancelled" || from == "Lost" {
		return false
	}
	switch to {
	case "Failed", "Cancelled", "Lost":
		return true
	case "Scheduled":
		return from == "Pending" || from == "WaitingCapacity"
	case "Starting":
		return from == "Pending" || from == "Scheduled" || from == "WaitingCapacity"
	case "Running":
		return from == "Pending" || from == "Scheduled" || from == "Starting" || from == "WaitingApproval" || from == "WaitingCapacity"
	case "WaitingApproval":
		return from == "Scheduled" || from == "Starting" || from == "Running"
	case "WaitingCapacity":
		return from == "Pending" || from == "Scheduled" || from == "Starting" || from == "Running"
	case "Succeeded":
		return from == "Scheduled" || from == "Starting" || from == "Running" || from == "WaitingApproval"
	default:
		return false
	}
}
