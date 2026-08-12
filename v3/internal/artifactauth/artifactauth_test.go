package artifactauth

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, time.August, 11, 15, 0, 0, 0, time.UTC)

type fakeMinter struct {
	mu       sync.Mutex
	lease    MintedLease
	err      error
	requests []MintRequest
}

func (f *fakeMinter) Mint(_ context.Context, request MintRequest) (MintedLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return f.lease, f.err
}

func (f *fakeMinter) requestSnapshot() []MintRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MintRequest(nil), f.requests...)
}

type fakeStaticSource struct {
	mu       sync.Mutex
	creds    CredentialSet
	err      error
	requests []StaticRequest
}

func (f *fakeStaticSource) Load(_ context.Context, request StaticRequest) (CredentialSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return f.creds, f.err
}

func (f *fakeStaticSource) requestSnapshot() []StaticRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StaticRequest(nil), f.requests...)
}

func TestNewFailsClosedOnIncompleteConfiguration(t *testing.T) {
	cases := []Config{
		{},
		{Static: StaticFallbackConfig{Enabled: true}},
		{Minter: &fakeMinter{}, Static: StaticFallbackConfig{SourceRef: "objectstore"}},
		{Minter: &fakeMinter{}, Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore", Source: &fakeStaticSource{}, MaxLifetime: MinLeaseLifetime - time.Second}},
		{Minter: &fakeMinter{}, Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore", Source: &fakeStaticSource{}, MaxLifetime: MaxStaticLeaseLifetime + time.Second}},
		{Minter: &fakeMinter{}, Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore?secret=x", Source: &fakeStaticSource{}}},
	}
	for index, config := range cases {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("case %d New() error = %v, want ErrInvalidConfig", index, err)
		}
	}

	staticOnly, err := New(Config{
		Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore", Source: &fakeStaticSource{}},
		Clock:  func() time.Time { return testNow },
	})
	if err != nil || staticOnly == nil {
		t.Fatalf("static-only New() = (%v, %v)", staticOnly, err)
	}
}

func TestIssueShortLivedLeaseKeepsCredentialsOutOfMetadata(t *testing.T) {
	request := testRequest()
	minter := &fakeMinter{}
	minter.lease = MintedLease{
		Credentials: CredentialSet{AccessKeyID: "AKIA-lease", SecretAccessKey: "secret-value", SessionToken: "session-value"},
		Scope:       request.Scope,
		ExpiresAt:   testNow.Add(10 * time.Minute),
		LeaseID:     "provider-lease-1",
	}
	issuer, err := New(Config{Minter: minter, Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}

	result, err := issuer.Issue(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata.Mode != ModeShortLived || result.Metadata.StaticCopy || !result.Metadata.Revocable || !result.Metadata.CredentialExpiryKnown {
		t.Fatalf("short-lived metadata security markers = %#v", result.Metadata)
	}
	if err := result.Metadata.Verify(); err != nil {
		t.Fatalf("Metadata.Verify() error = %v", err)
	}
	wantScope := request.Scope
	wantScope.Endpoint = strings.TrimSuffix(wantScope.Endpoint, "/")
	if result.Metadata.Scope != wantScope {
		t.Fatalf("scope = %#v, want %#v", result.Metadata.Scope, wantScope)
	}
	if !validDigest(result.Metadata.LeaseIDDigest) || !validDigest(result.Metadata.ScopeDigest) || !validDigest(result.Metadata.ContractDigest) {
		t.Fatalf("metadata digests are not valid: %#v", result.Metadata)
	}

	data, err := result.CopySecretData()
	if err != nil {
		t.Fatal(err)
	}
	wantData := map[string][]byte{
		AccessKeyIDKey:     []byte("AKIA-lease"),
		SecretAccessKeyKey: []byte("secret-value"),
		SessionTokenKey:    []byte("session-value"),
	}
	if !reflect.DeepEqual(data, wantData) {
		t.Fatalf("Secret data = %#v, want %#v", data, wantData)
	}
	data[AccessKeyIDKey][0] = 'x'
	second, err := result.CopySecretData()
	if err != nil || string(second[AccessKeyIDKey]) != "AKIA-lease" {
		t.Fatalf("CopySecretData() did not deep-copy data: %#v, %v", second, err)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AKIA-lease", "secret-value", "session-value", "provider-lease-1"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("JSON result leaked sensitive/provider value %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"mode":"short-lived"`) {
		t.Fatalf("JSON result omitted safe metadata: %s", encoded)
	}

	requests := minter.requestSnapshot()
	if len(requests) != 1 || requests[0].Scope != wantScope || requests[0].Run != request.Run || requests[0].Purpose != request.Purpose || requests[0].TTL != request.TTL {
		t.Fatalf("provider request = %#v", requests)
	}
	if !validDigest(requests[0].IdempotencyKey) || strings.Contains(requests[0].IdempotencyKey, request.Run.UID) {
		t.Fatalf("provider idempotency key is not opaque/bounded: %q", requests[0].IdempotencyKey)
	}
}

func TestProjectSecretDataIsIdempotentAndConflictSafe(t *testing.T) {
	result := issueShortLived(t, testRequest())
	dst := map[string][]byte{"existing": []byte("keep")}
	if err := result.ProjectSecretData(dst); err != nil {
		t.Fatal(err)
	}
	if string(dst["existing"]) != "keep" || len(dst) != 4 {
		t.Fatalf("projection changed unrelated data: %#v", dst)
	}
	if err := result.ProjectSecretData(dst); err != nil {
		t.Fatalf("idempotent projection error = %v", err)
	}

	conflict := map[string][]byte{AccessKeyIDKey: []byte("different")}
	conflict["new"] = []byte("must remain")
	if err := result.ProjectSecretData(conflict); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("conflicting projection error = %v, want ErrProjectionConflict", err)
	}
	if _, exists := conflict[SecretAccessKeyKey]; exists {
		t.Fatal("conflicting projection partially mutated the destination")
	}
}

func TestIssueRejectsScopeAndLeaseViolations(t *testing.T) {
	base := testRequest()
	cases := []struct {
		name   string
		mutate func(*Request, *MintedLease)
		want   error
	}{
		{name: "provider scope bucket", mutate: func(_ *Request, lease *MintedLease) { lease.Scope.Bucket = "other-bucket" }, want: ErrInvalidScope},
		{name: "provider scope permissions", mutate: func(_ *Request, lease *MintedLease) { lease.Scope.Permissions.Write = !lease.Scope.Permissions.Write }, want: ErrInvalidScope},
		{name: "provider expiry too soon", mutate: func(_ *Request, lease *MintedLease) { lease.ExpiresAt = testNow.Add(time.Second) }, want: ErrInvalidExpiry},
		{name: "provider expiry too late", mutate: func(request *Request, lease *MintedLease) { lease.ExpiresAt = testNow.Add(request.TTL + time.Second) }, want: ErrInvalidExpiry},
		{name: "provider missing lease id", mutate: func(_ *Request, lease *MintedLease) { lease.LeaseID = "" }, want: ErrInvalidCredentials},
		{name: "provider invalid secret", mutate: func(_ *Request, lease *MintedLease) { lease.Credentials.SecretAccessKey = " secret" }, want: ErrInvalidCredentials},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			minter := &fakeMinter{lease: validMintedLease(request)}
			tc.mutate(&request, &minter.lease)
			issuer, err := New(Config{Minter: minter, Clock: func() time.Time { return testNow }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := issuer.Issue(t.Context(), request); !errors.Is(err, tc.want) {
				t.Fatalf("Issue() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestIssueDoesNotFallbackForNonAvailabilityFailures(t *testing.T) {
	static := &fakeStaticSource{creds: validCredentials()}
	minter := &fakeMinter{err: errors.New("provider response leaked AKIA-private-secret")}
	issuer, err := New(Config{
		Minter: minter,
		Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore", Source: static},
		Clock:  func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = issuer.Issue(t.Context(), Request{Run: testRequest().Run, Scope: testRequest().Scope, Purpose: "work", TTL: 5 * time.Minute, AllowStaticFallback: true})
	if !errors.Is(err, ErrMintFailed) || strings.Contains(err.Error(), "AKIA-private-secret") {
		t.Fatalf("non-availability error = %v, want redacted ErrMintFailed", err)
	}
	if got := len(static.requestSnapshot()); got != 0 {
		t.Fatalf("static fallback ran after provider policy failure: %d calls", got)
	}
}

func TestStaticFallbackRequiresTwoOptInsAndIsMarked(t *testing.T) {
	static := &fakeStaticSource{creds: validCredentials()}
	minter := &fakeMinter{err: errors.Join(ErrProviderUnavailable, errors.New("contains secret-value"))}
	issuer, err := New(Config{
		Minter: minter,
		Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore", Source: static, MaxLifetime: 10 * time.Minute},
		Clock:  func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	request := testRequest()
	if _, err := issuer.Issue(t.Context(), request); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("fallback without request opt-in = %v, want provider unavailable", err)
	}
	if len(static.requestSnapshot()) != 0 {
		t.Fatal("static source was called without request opt-in")
	}

	request.AllowStaticFallback = true
	request.TTL = 5 * time.Minute
	result, err := issuer.Issue(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata.Mode != ModeStaticCopy || !result.Metadata.StaticCopy || result.Metadata.Revocable || result.Metadata.CredentialExpiryKnown {
		t.Fatalf("static metadata security markers = %#v", result.Metadata)
	}
	if err := result.Metadata.Verify(); err != nil {
		t.Fatalf("static Metadata.Verify() error = %v", err)
	}
	if result.Metadata.ExpiresAt != testNow.Add(5*time.Minute) {
		t.Fatalf("static expiry = %s, want bounded request expiry", result.Metadata.ExpiresAt)
	}
	wantScope := request.Scope
	wantScope.Endpoint = strings.TrimSuffix(wantScope.Endpoint, "/")
	if got := static.requestSnapshot(); len(got) != 1 || got[0].SourceRef != "objectstore" || got[0].Scope != wantScope || got[0].Run != request.Run {
		t.Fatalf("static request = %#v", got)
	}
	encoded, _ := json.Marshal(result.Metadata)
	for _, forbidden := range []string{"objectstore", "access-value", "secret-value", "session-value"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("static metadata leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestStaticFallbackRejectsUnboundedRequestedLifetimeAndRedactsSourceErrors(t *testing.T) {
	static := &fakeStaticSource{err: errors.New("source had secret-value and https://user:pass@example")}
	issuer, err := New(Config{
		Static: StaticFallbackConfig{Enabled: true, SourceRef: "objectstore", Source: static, MaxLifetime: 5 * time.Minute},
		Clock:  func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.AllowStaticFallback = true
	request.TTL = 10 * time.Minute
	if _, err := issuer.Issue(t.Context(), request); !errors.Is(err, ErrStaticFallbackDisabled) {
		t.Fatalf("unbounded static lifetime error = %v, want ErrStaticFallbackDisabled", err)
	}
	if len(static.requestSnapshot()) != 0 {
		t.Fatal("static source was called for an over-bound request")
	}

	request.TTL = 5 * time.Minute
	if _, err := issuer.Issue(t.Context(), request); !errors.Is(err, ErrStaticSourceFailed) || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "user:pass") {
		t.Fatalf("source error = %v, want redacted ErrStaticSourceFailed", err)
	}
}

func TestContextErrorsArePreservedButProviderErrorsAreRedacted(t *testing.T) {
	for _, contextErr := range []error{context.Canceled, context.DeadlineExceeded} {
		minter := &fakeMinter{err: contextErr}
		issuer, err := New(Config{Minter: minter, Clock: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := issuer.Issue(t.Context(), testRequest()); !errors.Is(err, contextErr) {
			t.Errorf("context error = %v, want %v", err, contextErr)
		}
	}
}

func TestIssueConcurrentUse(t *testing.T) {
	request := testRequest()
	minter := &fakeMinter{lease: validMintedLease(request)}
	issuer, err := New(Config{Minter: minter, Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 32
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		go func() {
			result, issueErr := issuer.Issue(context.Background(), request)
			if issueErr == nil {
				issueErr = result.Metadata.Verify()
				if issueErr == nil {
					_, issueErr = result.CopySecretData()
				}
			}
			errs <- issueErr
		}()
	}
	for index := 0; index < workers; index++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Issue() error = %v", err)
		}
	}
	if got := len(minter.requestSnapshot()); got != workers {
		t.Fatalf("provider calls = %d, want %d", got, workers)
	}
}

func TestMetadataTamperingFailsProjection(t *testing.T) {
	result := issueShortLived(t, testRequest())
	result.Metadata.Scope.Prefix = "other-prefix"
	if _, err := result.CopySecretData(); !errors.Is(err, ErrMetadata) {
		t.Fatalf("tampered metadata CopySecretData() = %v, want ErrMetadata", err)
	}
	if err := result.ProjectSecretData(map[string][]byte{}); !errors.Is(err, ErrMetadata) {
		t.Fatalf("tampered metadata ProjectSecretData() = %v, want ErrMetadata", err)
	}
}

func TestInvalidRequestsAreRejectedBeforeProviderCall(t *testing.T) {
	minter := &fakeMinter{lease: validMintedLease(testRequest())}
	issuer, err := New(Config{Minter: minter, Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	cases := []Request{
		{Run: testRequest().Run, Scope: testRequest().Scope, Purpose: "", TTL: 5 * time.Minute},
		{Run: testRequest().Run, Scope: testRequest().Scope, Purpose: "work", TTL: MinLeaseLifetime - time.Second},
		{Run: testRequest().Run, Scope: testRequest().Scope, Purpose: "work", TTL: MaxLeaseLifetime + time.Second},
		{Run: testRequest().Run, Scope: Scope{Bucket: "agw-artifacts", Prefix: "runs/../escape", Permissions: Permissions{Read: true}}, Purpose: "work", TTL: 5 * time.Minute},
		{Run: testRequest().Run, Scope: Scope{Bucket: "agw-artifacts", Prefix: "runs/other-run", Permissions: Permissions{Read: true}}, Purpose: "work", TTL: 5 * time.Minute},
		{Run: testRequest().Run, Scope: Scope{Bucket: "agw-artifacts", Prefix: "runs/" + testRequest().Run.UID + "-evil", Permissions: Permissions{Read: true}}, Purpose: "work", TTL: 5 * time.Minute},
		{Run: testRequest().Run, Scope: Scope{Bucket: "agw-artifacts", Prefix: "runs/" + testRequest().Run.UID + "/child", Permissions: Permissions{Read: true}}, Purpose: "work", TTL: 5 * time.Minute},
		{Run: testRequest().Run, Scope: Scope{Bucket: "agw-artifacts", Prefix: "runs/job", Endpoint: "https://user:pass@example.com", Permissions: Permissions{Read: true}}, Purpose: "work", TTL: 5 * time.Minute},
		{Run: testRequest().Run, Scope: Scope{Bucket: "agw-artifacts", Prefix: "runs/job", Permissions: Permissions{}}, Purpose: "work", TTL: 5 * time.Minute},
	}
	for index, request := range cases {
		if _, err := issuer.Issue(t.Context(), request); err == nil {
			t.Errorf("case %d accepted invalid request", index)
		}
	}
	if got := len(minter.requestSnapshot()); got != 0 {
		t.Fatalf("provider was called for invalid requests: %d", got)
	}
}

func issueShortLived(t *testing.T, request Request) Result {
	t.Helper()
	minter := &fakeMinter{lease: validMintedLease(request)}
	issuer, err := New(Config{Minter: minter, Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := issuer.Issue(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func validMintedLease(request Request) MintedLease {
	return MintedLease{
		Credentials: validCredentials(),
		Scope:       request.Scope,
		ExpiresAt:   testNow.Add(10 * time.Minute),
		LeaseID:     "provider-lease-1",
	}
}

func validCredentials() CredentialSet {
	return CredentialSet{AccessKeyID: "access-value", SecretAccessKey: "secret-value", SessionToken: "session-value"}
}

func testRequest() Request {
	return Request{
		Run: RunIdentity{Namespace: "agw-runs", Name: "repair-427", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15"},
		Scope: Scope{
			Endpoint:    "https://objects.example.com/",
			Region:      "us-east-1",
			Bucket:      "agw-artifacts",
			Prefix:      "runs/1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15",
			Permissions: Permissions{Read: true, Write: true},
		},
		Purpose: "work",
		TTL:     10 * time.Minute,
	}
}
