package runsecret

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testNow = time.Date(2026, time.August, 11, 15, 0, 0, 0, time.UTC)

func TestMaterializeCreatesImmutableOwnerScopedProjection(t *testing.T) {
	run, snapshot := fixture()
	skills := &SecretKeyRef{SecretName: "skills-provider", Key: "api-key"}
	objects := sourceSecrets()
	tracked := newTrackingClient(t, objects...)
	minter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	m := newMaterializer(t, tracked, minter, skills)
	digest := digestOf(t, snapshot)

	result, err := m.Materialize(context.Background(), run, snapshot, digest)
	if err != nil {
		t.Fatal(err)
	}
	if result.SecretName != "" && result.SecretName != runSecretName(run) {
		t.Fatalf("Secret name=%q", result.SecretName)
	}
	if result.SecretName != runSecretName(run) || result.Namespace != run.Namespace {
		t.Fatalf("result identity=%#v", result)
	}
	if result.CloneSecretKey != CloneSecretKey || result.SkillsSecretKey != SkillsSecretKey {
		t.Fatalf("special projection keys=%#v", result)
	}
	if len(result.BrokerSecretKeys) != 2 || result.LogicalRefToProjectedKey["mcp-github"] == "" || result.LogicalRefToProjectedKey["openrouter"] == "" {
		t.Fatalf("broker projection=%#v", result)
	}
	if !reflect.DeepEqual(result.BrokerSecretKeys, []string{
		result.LogicalRefToProjectedKey["mcp-github"],
		result.LogicalRefToProjectedKey["openrouter"],
	}) {
		t.Fatalf("broker key order is not deterministic: %#v", result.BrokerSecretKeys)
	}

	secret := getSecret(t, tracked, run.Namespace, result.SecretName)
	if secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable || len(secret.StringData) != 0 {
		t.Fatalf("Secret hardening=%#v", secret)
	}
	if secret.Annotations[specDigestAnnotationKey] != digest || secret.Annotations[contractAnnotationKey] == "" {
		t.Fatalf("Secret identity annotations=%v", secret.Annotations)
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue || secret.Labels[runUIDLabelKey] != string(run.UID) {
		t.Fatalf("Secret labels=%v", secret.Labels)
	}
	if len(secret.OwnerReferences) != 1 || !matchesOwner(secret.OwnerReferences[0], run) {
		t.Fatalf("ownerReferences=%#v", secret.OwnerReferences)
	}
	if string(secret.Data[CloneSecretKey]) != "ghs_read_only" || string(secret.Data[SkillsSecretKey]) != "skill_secret" {
		t.Fatalf("special credential values are wrong")
	}
	if string(secret.Data[result.LogicalRefToProjectedKey["mcp-github"]]) != "mcp_secret" || string(secret.Data[result.LogicalRefToProjectedKey["openrouter"]]) != "model_secret" {
		t.Fatalf("broker credential values are wrong")
	}
	if _, leaked := secret.Data["ignored-private-key"]; leaked {
		t.Fatal("unselected source Secret key was materialized")
	}
	if _, leaked := secret.Data["publish-write"]; leaked {
		t.Fatal("publish credential key was materialized")
	}
	if len(minter.repos) != 1 || minter.repos[0] != snapshot.Spec.Source.Repo {
		t.Fatalf("clone token scope calls=%v", minter.repos)
	}
}

func TestMaterializeRejectsSourceCredentialOutsideOperatorAllowlist(t *testing.T) {
	run, snapshot := fixture()
	tracked := newTrackingClient(t, sourceSecrets()...)
	m, err := New(Config{
		Client:               tracked,
		CloneTokenMinter:     &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)},
		SourceNamespace:      SourceNamespace,
		AllowedSourceSecrets: []string{"mcp-github", "skills-provider"},
		SkillsToken:          &SecretKeyRef{SecretName: "skills-provider", Key: "api-key"},
		Clock:                func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot)); !errors.Is(err, ErrSourceSecretNotAllowed) {
		t.Fatalf("allowlist violation error=%v, want %v", err, ErrSourceSecretNotAllowed)
	}
	if len(tracked.gets) != 0 {
		t.Fatalf("allowlist violation performed Kubernetes reads: %#v", tracked.gets)
	}
}

func TestMaterializeIsIdempotentAndDoesNotRemintOrReReadSources(t *testing.T) {
	run, snapshot := fixture()
	tracked := newTrackingClient(t, sourceSecrets()...)
	minter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	m := newMaterializer(t, tracked, minter, nil)
	digest := digestOf(t, snapshot)

	first, err := m.Materialize(context.Background(), run, snapshot, digest)
	if err != nil {
		t.Fatal(err)
	}
	getsAfterCreate := len(tracked.gets)
	minter.err = errors.New("token value should never be exposed")
	second, err := m.Materialize(context.Background(), run, snapshot, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || len(minter.repos) != 1 {
		t.Fatalf("idempotent result/minter calls: %#v %#v", first, minter.repos)
	}
	if len(tracked.gets) != getsAfterCreate+1 {
		t.Fatalf("second call performed source reads: before=%d after=%d gets=%v", getsAfterCreate, len(tracked.gets), tracked.gets)
	}
}

func TestMaterializeProjectsOneExactPerRunArtifactLease(t *testing.T) {
	run, snapshot := fixture()
	tracked := newTrackingClient(t, sourceSecrets()...)
	cloneMinter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	leaseCalls := 0
	issuer, err := artifactauth.New(artifactauth.Config{
		Clock: func() time.Time { return testNow },
		Minter: artifactauth.MinterFunc(func(_ context.Context, request artifactauth.MintRequest) (artifactauth.MintedLease, error) {
			leaseCalls++
			return artifactauth.MintedLease{
				Credentials: artifactauth.CredentialSet{AccessKeyID: "AKIA_TEST", SecretAccessKey: "secret-test", SessionToken: "session-test"},
				Scope:       request.Scope, ExpiresAt: testNow.Add(request.TTL), LeaseID: "lease-test",
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := artifactauth.Scope{
		Bucket: "agw-artifacts", Region: "us-east-1",
		Prefix:      "agents-gateway/v3/runs/" + string(run.UID),
		Permissions: artifactauth.Permissions{Read: true, Write: true},
	}
	materializer, err := New(Config{
		Client: tracked, CloneTokenMinter: cloneMinter, Clock: func() time.Time { return testNow },
		ArtifactIssuer: issuer, ArtifactScope: scope, ArtifactTTL: 45 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := materializer.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if result.ArtifactAccessKeyIDSecretKey != artifactauth.AccessKeyIDKey || result.ArtifactSecretAccessKeySecretKey != artifactauth.SecretAccessKeyKey || result.ArtifactSessionTokenSecretKey != artifactauth.SessionTokenKey {
		t.Fatalf("artifact projection metadata=%#v", result)
	}
	secret := getSecret(t, tracked, run.Namespace, result.SecretName)
	if string(secret.Data[artifactauth.AccessKeyIDKey]) != "AKIA_TEST" || string(secret.Data[artifactauth.SecretAccessKeyKey]) != "secret-test" || string(secret.Data[artifactauth.SessionTokenKey]) != "session-test" {
		t.Fatal("scoped artifact credentials were not projected exactly")
	}
	if _, err := materializer.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot)); err != nil {
		t.Fatal(err)
	}
	if leaseCalls != 1 {
		t.Fatalf("artifact lease was reminted during idempotent reconcile: %d", leaseCalls)
	}
}

func TestVerifyMaterializeUsesFreshReadOnlyProjectionAndLease(t *testing.T) {
	run, snapshot := fixture()
	tracked := newTrackingClient(t, sourceSecrets()...)
	cloneMinter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	var requests []artifactauth.Request
	issuer, err := artifactauth.New(artifactauth.Config{
		Clock: func() time.Time { return testNow },
		Minter: artifactauth.MinterFunc(func(_ context.Context, request artifactauth.MintRequest) (artifactauth.MintedLease, error) {
			requests = append(requests, artifactauth.Request{Run: request.Run, Scope: request.Scope, Purpose: request.Purpose, TTL: request.TTL})
			return artifactauth.MintedLease{
				Credentials: artifactauth.CredentialSet{AccessKeyID: "AKIA_" + request.Purpose, SecretAccessKey: "secret_" + request.Purpose, SessionToken: "session_" + request.Purpose},
				Scope:       request.Scope, ExpiresAt: testNow.Add(request.TTL), LeaseID: request.Purpose,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := digestOf(t, snapshot)
	work, err := New(Config{
		Client: tracked, CloneTokenMinter: cloneMinter, Clock: func() time.Time { return testNow },
		Phase: PhaseWork, ArtifactIssuer: issuer,
		ArtifactScope: artifactauth.Scope{Bucket: "agw-artifacts", Region: "us-east-1", Prefix: "agents-gateway/v3/runs/" + string(run.UID), Permissions: artifactauth.Permissions{Read: true, Write: true}},
		ArtifactTTL:   45 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	workResult, err := work.Materialize(context.Background(), run, snapshot, digest)
	if err != nil {
		t.Fatal(err)
	}
	verify, err := New(Config{
		Client: tracked, CloneTokenMinter: cloneMinter, Clock: func() time.Time { return testNow },
		Phase: PhaseVerify, ArtifactIssuer: issuer,
		ArtifactScope: artifactauth.Scope{Bucket: "agw-artifacts", Region: "us-east-1", Prefix: "agents-gateway/v3/runs/" + string(run.UID), Permissions: artifactauth.Permissions{Read: true}},
		ArtifactTTL:   45 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyResult, err := verify.Materialize(context.Background(), run, snapshot, digest)
	if err != nil {
		t.Fatal(err)
	}
	if workResult.SecretName == verifyResult.SecretName || verifyResult.Phase != PhaseVerify || verifyResult.SecretName != workload.VerifySecretName(string(run.UID)) {
		t.Fatalf("phase Secret identity was not separated: work=%#v verify=%#v", workResult, verifyResult)
	}
	verifySecret := getSecret(t, tracked, run.Namespace, verifyResult.SecretName)
	if len(verifySecret.Data) != 4 || string(verifySecret.Data[CloneSecretKey]) != "ghs_read_only" {
		t.Fatalf("verify Secret projected unexpected keys/data: %#v", verifySecret.Data)
	}
	for _, key := range []string{artifactauth.AccessKeyIDKey, artifactauth.SecretAccessKeyKey, artifactauth.SessionTokenKey} {
		if _, ok := verifySecret.Data[key]; !ok {
			t.Fatalf("verify Secret missing fixed artifact key %q: %#v", key, verifySecret.Data)
		}
	}
	if string(verifySecret.Data[artifactauth.AccessKeyIDKey]) != "AKIA_verify-fetch" || string(verifySecret.Data[artifactauth.SecretAccessKeyKey]) != "secret_verify-fetch" {
		t.Fatalf("verify Secret received the wrong lease: %#v", verifySecret.Data)
	}
	if verifySecret.Labels[phaseLabelKey] != string(PhaseVerify) || !matchesOwner(verifySecret.OwnerReferences[0], run) {
		t.Fatalf("verify Secret ownership/phase=%#v", verifySecret)
	}
	if len(requests) != 2 || requests[0].Purpose != "work-broker" || requests[0].Scope.Permissions.Write != true || requests[1].Purpose != "verify-fetch" || requests[1].Scope.Permissions != (artifactauth.Permissions{Read: true}) {
		t.Fatalf("lease requests did not separate work and verify scopes: %#v", requests)
	}
	if _, err := verify.Materialize(context.Background(), run, snapshot, digest); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || len(cloneMinter.repos) != 2 {
		t.Fatalf("verify reconcile reminted credentials: leases=%d clone-mints=%d", len(requests), len(cloneMinter.repos))
	}
}

func TestMaterializeRejectsPathShapedRunUIDBeforeArtifactLease(t *testing.T) {
	run, snapshot := fixture()
	run.UID = types.UID("run/../escape")
	snapshot.Run.UID = string(run.UID)
	tracked := newTrackingClient(t, sourceSecrets()...)
	cloneMinter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	leaseCalls := 0
	issuer, err := artifactauth.New(artifactauth.Config{
		Clock: func() time.Time { return testNow },
		Minter: artifactauth.MinterFunc(func(_ context.Context, request artifactauth.MintRequest) (artifactauth.MintedLease, error) {
			leaseCalls++
			return artifactauth.MintedLease{
				Credentials: artifactauth.CredentialSet{AccessKeyID: "AKIA_TEST", SecretAccessKey: "secret-test", SessionToken: "session-test"},
				Scope:       request.Scope, ExpiresAt: testNow.Add(request.TTL), LeaseID: "lease-test",
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{
		Client: tracked, CloneTokenMinter: cloneMinter, ArtifactIssuer: issuer,
		ArtifactScope: artifactauth.Scope{Bucket: "agw-artifacts", Region: "us-east-1", Prefix: "agents-gateway/v3/runs/" + string(run.UID), Permissions: artifactauth.Permissions{Read: true, Write: true}},
		ArtifactTTL:   45 * time.Minute, Clock: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("path-shaped run UID error=%v, want ErrInvalidInput", err)
	}
	if leaseCalls != 0 || len(tracked.gets) != 0 {
		t.Fatalf("path-shaped run UID crossed trust boundary: leaseCalls=%d gets=%v", leaseCalls, tracked.gets)
	}
}

func TestMaterializeUsesOperatorConfiguredSourceNamespace(t *testing.T) {
	run, snapshot := fixture()
	objects := sourceSecrets()
	for _, object := range objects {
		object.SetNamespace("custom-system")
	}
	tracked := newTrackingClient(t, objects...)
	minter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	materializer, err := New(Config{
		Client: tracked, CloneTokenMinter: minter, SourceNamespace: "custom-system",
		Clock: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot)); err != nil {
		t.Fatal(err)
	}
	for _, key := range tracked.gets {
		if key.Name == "mcp-github" || key.Name == "openrouter" {
			if key.Namespace != "custom-system" {
				t.Fatalf("credential read escaped configured namespace: %s", key.Namespace)
			}
		}
	}
	if _, err := New(Config{Client: tracked, CloneTokenMinter: minter, SourceNamespace: "INVALID_NAMESPACE"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid source namespace error=%v", err)
	}
}

func TestMaterializeAcceptsAValidatedCreateRace(t *testing.T) {
	run, snapshot := fixture()
	digest := digestOf(t, snapshot)
	base := newTrackingClient(t, sourceSecrets()...)
	minter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	m := newMaterializer(t, base, minter, nil)

	layout, err := makeLayout(snapshot, PhaseWork, nil, m.limits, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := m.buildData(context.Background(), snapshot, layout)
	if err != nil {
		t.Fatal(err)
	}
	createdByOther := newSecret(run, runSecretName(run), digest, layout.contractDigest, data)
	race := &createRaceClient{trackingClient: base, winner: createdByOther}
	raceMaterializer := newMaterializer(t, race, minter, nil)
	result, err := raceMaterializer.Materialize(context.Background(), run, snapshot, digest)
	if err != nil {
		t.Fatal(err)
	}
	if result.SecretName != createdByOther.Name || race.createCalls != 1 {
		t.Fatalf("race result=%#v creates=%d", result, race.createCalls)
	}
	if minter.repos == nil || len(minter.repos) == 0 {
		t.Fatal("race path did not build the candidate Secret")
	}
}

func TestMaterializeRejectsForeignOwnerDigestAndContractCollisions(t *testing.T) {
	run, snapshot := fixture()
	digest := digestOf(t, snapshot)
	base := newTrackingClient(t, sourceSecrets()...)
	m := newMaterializer(t, base, &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}, nil)
	if _, err := m.Materialize(context.Background(), run, snapshot, digest); err != nil {
		t.Fatal(err)
	}
	original := getSecret(t, base, run.Namespace, runSecretName(run))

	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
		want   error
	}{
		{
			name: "foreign owner",
			mutate: func(secret *corev1.Secret) {
				secret.OwnerReferences[0].UID = types.UID("foreign")
			},
			want: ErrOwnershipConflict,
		},
		{
			name: "digest collision",
			mutate: func(secret *corev1.Secret) {
				secret.Annotations[specDigestAnnotationKey] = "sha256:" + strings.Repeat("b", 64)
			},
			want: ErrSpecDigestConflict,
		},
		{
			name: "projection collision",
			mutate: func(secret *corev1.Secret) {
				secret.Data["unexpected"] = []byte("another-secret")
			},
			want: ErrSecretContractConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := original.DeepCopy()
			tc.mutate(candidate)
			clientForTest := newTrackingClient(t, candidate)
			materializer := newMaterializer(t, clientForTest, &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}, nil)
			_, err := materializer.Materialize(context.Background(), run, snapshot, digest)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestMaterializeNeverReadsOrProjectsPublishCredential(t *testing.T) {
	run, snapshot := fixture()
	tracked := newTrackingClient(t, sourceSecrets()...)
	m := newMaterializer(t, tracked, &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}, nil)
	if _, err := m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot)); err != nil {
		t.Fatal(err)
	}
	for _, key := range tracked.gets {
		if key.Namespace == SourceNamespace && key.Name == snapshot.Spec.Publish.CredentialRef {
			t.Fatal("publish credential was read")
		}
	}

	// A shared logical reference is rejected before any source GET or mint. It
	// is not safe to infer that a token is read-only merely from its Secret
	// name, because the same Secret is also the controller's write credential.
	snapshot.ToolSet.Servers[0].CredentialsRef = snapshot.Spec.Publish.CredentialRef
	digest := digestOf(t, snapshot)
	tracked = newTrackingClient(t, sourceSecrets()...)
	minter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	m = newMaterializer(t, tracked, minter, nil)
	if _, err := m.Materialize(context.Background(), run, snapshot, digest); !errors.Is(err, ErrPublishCredentialOverlap) {
		t.Fatalf("error=%v, want publish overlap", err)
	}
	if len(tracked.gets) != 0 || len(minter.repos) != 0 {
		t.Fatalf("publish overlap performed external reads: gets=%v mints=%v", tracked.gets, minter.repos)
	}
}

func TestMaterializeValidatesReadOnlyCloneTokenContract(t *testing.T) {
	run, snapshot := fixture()
	variants := []CloneToken{
		{Value: "token", Repository: "github.com/other/repo", ContentsRead: true, ExpiresAt: testNow.Add(time.Hour)},
		{Value: "token", Repository: snapshot.Spec.Source.Repo, ContentsRead: true, ContentsWrite: true, ExpiresAt: testNow.Add(time.Hour)},
		{Value: "token", Repository: snapshot.Spec.Source.Repo, ContentsRead: true, ExpiresAt: testNow.Add(-time.Second)},
		{Value: "token\nleak", Repository: snapshot.Spec.Source.Repo, ContentsRead: true, ExpiresAt: testNow.Add(time.Hour)},
	}
	for index, token := range variants {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			tracked := newTrackingClient(t, sourceSecrets()...)
			m := newMaterializer(t, tracked, &fakeMinter{token: token}, nil)
			_, err := m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot))
			if !errors.Is(err, ErrCloneToken) {
				t.Fatalf("error=%v, want clone-token rejection", err)
			}
		})
	}
}

func TestMaterializeEnforcesBoundsAndRedactsFailures(t *testing.T) {
	run, snapshot := fixture()
	tracked := newTrackingClient(t, sourceSecrets()...)
	minter := &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}
	m, err := New(Config{
		Client:           tracked,
		CloneTokenMinter: minter,
		Limits:           Limits{MaxSourceSecrets: 1, MaxProjectedKeys: 8, MaxValueBytes: 64, MaxTotalBytes: 512},
		Clock:            func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot)); !errors.Is(err, ErrBounds) {
		t.Fatalf("source bound error=%v", err)
	}

	minter = &fakeMinter{err: errors.New("upstream contained super-secret-token")}
	tracked = newTrackingClient(t, sourceSecrets()...)
	m = newMaterializer(t, tracked, minter, nil)
	_, err = m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot))
	if !errors.Is(err, ErrCloneToken) || strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("unredacted minter error=%v", err)
	}

	tooLarge := sourceSecrets()
	tooLarge[0].(*corev1.Secret).Data[SourceTokenKey] = []byte(strings.Repeat("x", DefaultMaxValueBytes+1))
	tracked = newTrackingClient(t, tooLarge...)
	m = newMaterializer(t, tracked, &fakeMinter{token: fakeCloneToken(snapshot.Spec.Source.Repo)}, nil)
	_, err = m.Materialize(context.Background(), run, snapshot, digestOf(t, snapshot))
	if !errors.Is(err, ErrSourceDataInvalid) || strings.Contains(err.Error(), "x") {
		t.Fatalf("unredacted source error=%v", err)
	}
}

func TestNewRejectsUnboundedConfigurationAndSkillsKeyIsExplicit(t *testing.T) {
	tracked := newTrackingClient(t)
	minter := &fakeMinter{}
	for _, limits := range []Limits{
		{MaxSourceSecrets: MaxMaxSourceSecrets + 1},
		{MaxProjectedKeys: MaxMaxProjectedKeys + 1},
		{MaxValueBytes: MaxMaxValueBytes + 1},
		{MaxTotalBytes: MaxMaxTotalBytes + 1},
		{MaxValueBytes: 64, MaxTotalBytes: 32},
	} {
		if _, err := New(Config{Client: tracked, CloneTokenMinter: minter, Limits: limits}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("limits=%#v error=%v", limits, err)
		}
	}
	if _, err := New(Config{Client: tracked, CloneTokenMinter: minter, SkillsToken: &SecretKeyRef{SecretName: "skills", Key: "../token"}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid skills key error=%v", err)
	}
}

type fakeMinter struct {
	token CloneToken
	err   error
	repos []string
}

func (m *fakeMinter) MintReadOnlyContentsToken(_ context.Context, repository string) (CloneToken, error) {
	m.repos = append(m.repos, repository)
	if m.err != nil {
		return CloneToken{}, m.err
	}
	return m.token, nil
}

type trackingClient struct {
	client.Client
	gets []client.ObjectKey
}

func newTrackingClient(t *testing.T, objects ...client.Object) *trackingClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &trackingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

func (c *trackingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	c.gets = append(c.gets, key)
	return c.Client.Get(ctx, key, object, opts...)
}

type createRaceClient struct {
	*trackingClient
	winner      *corev1.Secret
	createCalls int
}

func (c *createRaceClient) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	c.createCalls++
	if c.createCalls == 1 {
		if err := c.Client.Create(ctx, c.winner); err != nil {
			return err
		}
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, object.GetName())
	}
	return c.Client.Create(ctx, object, opts...)
}

func newMaterializer(t *testing.T, kube KubeClient, minter CloneTokenMinter, skills *SecretKeyRef) *Materializer {
	t.Helper()
	m, err := New(Config{Client: kube, CloneTokenMinter: minter, SkillsToken: skills, Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func fixture() (*v1alpha1.AgentRun, resolved.Snapshot) {
	inlineTask := "fix the nil dereference"
	inlineInstructions := "keep the change focused"
	run := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Namespace: "agw-runs", Name: "repair-427", UID: types.UID("1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15"), Generation: 3,
	}}
	snapshot := resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID), Generation: run.Generation},
		BaseSHA:       strings.Repeat("e", 40),
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "issue-fixer", GateRef: "go-default",
			Source:  v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1},
			Task:    v1alpha1.TaskSpec{Inline: &inlineTask},
			Publish: v1alpha1.PublishSpec{Mode: v1alpha1.PublishPullRequest, CredentialRef: "github-publish"},
		},
		Task: inlineTask, Instructions: inlineInstructions,
		Agent: v1alpha1.AgentSpec{
			Runtime:      v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/runtime@sha256:" + strings.Repeat("a", 64)},
			Instructions: v1alpha1.InstructionsSpec{Inline: &inlineInstructions},
			ToolSetRef:   "github-readonly", ModelRouteRef: "default-codex",
			Skills: []v1alpha1.SkillRef{{Name: "codebase-design", Ref: "https://skills.example.test/codebase-design", Digest: "sha256:" + strings.Repeat("b", 64)}},
		},
		ToolSet: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{
			{Name: "github", Ref: "https://mcp.example.test/mcp", CredentialsRef: "mcp-github"},
		}},
		ModelRoute: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{
			{Name: "openrouter", Kind: "openrouter-responses", Model: "nvidia/nemotron-3-ultra-550b-a55b:free", CredentialRef: "openrouter", Priority: 1},
		}},
	}
	return run, snapshot
}

func sourceSecrets() []client.Object {
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mcp-github", Namespace: SourceNamespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{SourceTokenKey: []byte("mcp_secret"), "ignored-private-key": []byte("must-not-copy")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "openrouter", Namespace: SourceNamespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{SourceTokenKey: []byte("model_secret"), "unselected": []byte("must-not-copy")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "skills-provider", Namespace: SourceNamespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"api-key": []byte("skill_secret"), "other": []byte("must-not-copy")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-publish", Namespace: SourceNamespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{SourceTokenKey: []byte("publish-write")}},
	}
}

func fakeCloneToken(repository string) CloneToken {
	return CloneToken{Value: "ghs_read_only", Repository: repository, ContentsRead: true, ExpiresAt: testNow.Add(time.Hour)}
}

func digestOf(t *testing.T, snapshot resolved.Snapshot) string {
	t.Helper()
	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func getSecret(t *testing.T, kube KubeClient, namespace, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

func runSecretName(run *v1alpha1.AgentRun) string {
	return workload.WorkSecretName(string(run.UID))
}
