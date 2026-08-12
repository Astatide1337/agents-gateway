package runplan

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runsecret"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	planTestNow = time.Date(2026, time.August, 11, 15, 0, 0, 0, time.UTC)
	planKeyOnce sync.Once
	planKeyPEM  []byte
	planKeyErr  error
)

func TestPlanWorkUsesReadOnlyCloneTokenAndBuildsHardenedTopology(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()

	run, snapshot := planFixture()
	tracked := newPlanClient(t, planSourceObjects(t, "custom-system")...)
	factory := newPlanFactory(t, tracked, server, "custom-system")

	plan, err := factory.PlanWork(context.Background(), run, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Owner != run || plan.Role != sandbox.RoleWork || plan.SpecDigest != digestPlan(snapshot) {
		t.Fatalf("plan identity=%#v", plan)
	}
	if plan.Sandbox == nil || plan.Sandbox.Namespace != run.Namespace {
		t.Fatalf("sandbox identity=%#v", plan.Sandbox)
	}
	if got := plan.Sandbox.Spec.Lifecycle.ShutdownTime.Time; !got.Equal(planTestNow.Add(45 * time.Minute)) {
		t.Fatalf("shutdownTime=%s, want %s", got, planTestNow.Add(45*time.Minute))
	}

	pod := plan.Sandbox.Spec.PodTemplate.Spec
	if !reflect.DeepEqual(containerNames(pod.InitContainers), []string{"clone", "skills", "context", "lockdown"}) {
		t.Fatalf("init topology=%v", containerNames(pod.InitContainers))
	}
	if !reflect.DeepEqual(containerNames(pod.Containers), []string{"agent", "broker"}) {
		t.Fatalf("regular topology=%v", containerNames(pod.Containers))
	}
	if pod.HostUsers == nil || *pod.HostUsers || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("work pod did not explicitly disable host users and service-account token mounting")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("work pod does not use RuntimeDefault seccomp")
	}

	secret := getPlanSecret(t, tracked, run.Namespace, workload.WorkSecretName(string(run.UID)))
	if secret.Data[runsecret.CloneSecretKey] == nil || secret.Data[runsecret.SkillsSecretKey] == nil {
		t.Fatalf("special projection keys=%v", secret.Data)
	}
	if string(secret.Data[runsecret.CloneSecretKey]) != "ghs_clone_token" {
		t.Fatal("clone token was not projected")
	}
	if _, ok := secret.Data[GitHubPrivateKeyKey]; ok {
		t.Fatal("GitHub App private key entered the run Secret")
	}
	if _, ok := secret.Data[GitHubAppIDKey]; ok {
		t.Fatal("GitHub App ID entered the run Secret")
	}
	if _, ok := secret.Data[GitHubInstallationIDKey]; ok {
		t.Fatal("GitHub installation ID entered the run Secret")
	}

	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if containsSecretMount(container) && container.Name == "agent" {
			t.Fatalf("agent received a Secret mount: %#v", container.VolumeMounts)
		}
	}
	if got := server.RequestCount(); got != 1 {
		t.Fatalf("GitHub token exchange count=%d, want 1", got)
	}
	body := server.LastRequestBody()
	if !reflect.DeepEqual(body.Repositories, []string{"Astatide1337/agents-gateway"}) || body.Permissions["contents"] != "read" || body.Permissions["pull_requests"] != "" {
		t.Fatalf("GitHub request scope=%#v", body)
	}
	if strings.Contains(string(mustJSON(t, plan.Sandbox)), "publish-write-secret") || strings.Contains(string(mustJSON(t, plan.Sandbox)), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("publish/private-key material appeared in Sandbox manifest")
	}
}

func TestPlanWorkIsIdempotentWithoutRemintingOrRecreating(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	run, snapshot := planFixture()
	tracked := newPlanClient(t, planSourceObjects(t, "agw-system")...)
	factory := newPlanFactory(t, tracked, server, "agw-system")

	first, err := factory.PlanWork(context.Background(), run, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	createsAfterFirst := tracked.creates
	second, err := factory.PlanWork(context.Background(), run, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if tracked.creates != createsAfterFirst || tracked.creates != 1 {
		t.Fatalf("Secret creates=%d, want one", tracked.creates)
	}
	if server.RequestCount() != 1 {
		t.Fatalf("token exchanges=%d, want one", server.RequestCount())
	}
	if !reflect.DeepEqual(first.Sandbox, second.Sandbox) || first.SpecDigest != second.SpecDigest {
		t.Fatal("repeated planning was not deterministic")
	}

	logicalSourceGets := 0
	for _, item := range tracked.gets {
		if item.Namespace == "agw-system" && (item.Name == "mcp-credential" || item.Name == "model-credential" || item.Name == "skills-provider") {
			logicalSourceGets++
		}
	}
	if logicalSourceGets != 3 {
		t.Fatalf("logical source GETs=%d, want one each on first materialization only: %#v", logicalSourceGets, tracked.gets)
	}
}

func TestPlanWorkUsesConfiguredSystemNamespaceForAppAndLogicalCredentials(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	run, snapshot := planFixture()
	tracked := newPlanClient(t, planSourceObjects(t, "operator-config")...)
	factory := newPlanFactory(t, tracked, server, "operator-config")
	if _, err := factory.PlanWork(context.Background(), run, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, item := range tracked.gets {
		if item.Name == snapshot.Spec.Publish.CredentialRef || item.Name == "mcp-credential" || item.Name == "model-credential" || item.Name == "skills-provider" {
			if item.Namespace != "operator-config" {
				t.Fatalf("credential GET escaped configured namespace: %#v", item)
			}
		}
	}
	if countGet(tracked.gets, "operator-config", snapshot.Spec.Publish.CredentialRef) != 1 {
		t.Fatalf("GitHub App Secret GETs=%v", tracked.gets)
	}
}

func TestPlanWorkRejectsUnsafeInputsBeforeReadingCredentials(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	run, snapshot := planFixture()

	tests := []struct {
		name   string
		mutate func(*resolved.Snapshot)
		want   error
	}{
		{name: "scheme repository", mutate: func(s *resolved.Snapshot) { s.Spec.Source.Repo = "https://github.com/Astatide1337/agents-gateway" }, want: ErrInvalidRepository},
		{name: "wrong host", mutate: func(s *resolved.Snapshot) { s.Spec.Source.Repo = "gitlab.com/Astatide1337/agents-gateway" }, want: ErrInvalidRepository},
		{name: "repository traversal", mutate: func(s *resolved.Snapshot) { s.Spec.Source.Repo = "github.com/../agents-gateway" }, want: ErrInvalidRepository},
		{name: "timeout too long", mutate: func(s *resolved.Snapshot) { s.Spec.Limits.Timeout = "25h" }, want: ErrInvalidTimeout},
		{name: "timeout malformed", mutate: func(s *resolved.Snapshot) { s.Spec.Limits.Timeout = "45 minutes" }, want: ErrInvalidTimeout},
		{name: "publish overlap", mutate: func(s *resolved.Snapshot) { s.ToolSet.Servers[0].CredentialsRef = s.Spec.Publish.CredentialRef }, want: ErrPublishOverlap},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := snapshot
			test.mutate(&candidate)
			run.Status.SpecDigest = digestPlan(candidate)
			tracked := newPlanClient(t, planSourceObjects(t, "agw-system")...)
			factory := newPlanFactory(t, tracked, server, "agw-system")
			_, err := factory.PlanWork(context.Background(), run, candidate)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			if len(tracked.gets) != 0 {
				t.Fatalf("unsafe input caused credential GETs: %#v", tracked.gets)
			}
		})
	}
}

func TestPlanWorkRejectsPathShapedRunUIDBeforeCredentialRead(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	run, snapshot := planFixture()
	run.UID = types.UID("run/../escape")
	snapshot.Run.UID = string(run.UID)
	run.Status.SpecDigest = digestPlan(snapshot)
	tracked := newPlanClient(t, planSourceObjects(t, "agw-system")...)
	factory := newPlanFactory(t, tracked, server, "agw-system")
	if _, err := factory.PlanWork(context.Background(), run, snapshot); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("path-shaped run UID error=%v, want ErrInvalidIdentity", err)
	}
	if len(tracked.gets) != 0 {
		t.Fatalf("path-shaped run UID caused credential reads: %#v", tracked.gets)
	}
}

func TestNewRejectsNonDigestPinnedImagesAndUnsafeShutdownBounds(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	tracked := newPlanClient(t)
	config := planConfig(tracked, server, "agw-system")
	config.CloneImage = "ghcr.io/astatide/agw-clone:latest"
	if _, err := New(config); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("tagged image error=%v", err)
	}
	config = planConfig(tracked, server, "agw-system")
	config.MaxShutdownDuration = sandbox.DefaultMaxShutdownDuration + time.Second
	if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("excessive shutdown bound error=%v", err)
	}
	config = planConfig(tracked, server, "INVALID_NAMESPACE")
	if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid namespace error=%v", err)
	}
}

func TestNewRejectsAgentGatewayEnablementBeforeRunEffects(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	tracked := newPlanClient(t)
	config := planConfig(tracked, server, "agw-system")
	config.AgentGateway = workload.AgentGatewaySidecarOptions{Enabled: true}

	if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unsupported agentgateway configuration error=%v, want ErrInvalidConfig", err)
	}
	if len(tracked.gets) != 0 {
		t.Fatalf("unsupported agentgateway configuration caused credential reads: %#v", tracked.gets)
	}
}

func TestPlanWorkRejectsMalformedAppSecretsWithoutLeakingValues(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	run, snapshot := planFixture()
	privateMarker := "PRIVATE-KEY-SENTINEL"

	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
		want   error
	}{
		{name: "wrong type", mutate: func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS }, want: ErrAppSecretInvalid},
		{name: "mutable", mutate: func(s *corev1.Secret) { value := false; s.Immutable = &value }, want: ErrAppSecretInvalid},
		{name: "missing fixed key", mutate: func(s *corev1.Secret) { delete(s.Data, GitHubAppIDKey) }, want: ErrAppSecretInvalid},
		{name: "extra key", mutate: func(s *corev1.Secret) { s.Data["unexpected"] = []byte("unexpected") }, want: ErrAppSecretInvalid},
		{name: "bad app id", mutate: func(s *corev1.Secret) { s.Data[GitHubAppIDKey] = []byte("not-an-id") }, want: ErrAppSecretInvalid},
		{name: "bad private key", mutate: func(s *corev1.Secret) { s.Data[GitHubPrivateKeyKey] = []byte(privateMarker) }, want: ErrAppSecretInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := planSourceObjects(t, "agw-system")
			app := objects[0].(*corev1.Secret)
			test.mutate(app)
			tracked := newPlanClient(t, objects...)
			factory := newPlanFactory(t, tracked, server, "agw-system")
			_, err := factory.PlanWork(context.Background(), run, snapshot)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), privateMarker) || strings.Contains(err.Error(), "ghs_clone_token") {
				t.Fatalf("credential material leaked in error: %q", err)
			}
		})
	}
}

func TestPlanWorkMapsCredentialReadErrorsToRedactedSentinel(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	base := planConfig(nil, server, "agw-system")
	base.Client = errorPlanClient{message: "backend leaked ghs_sensitive_value and PRIVATE-KEY-SENTINEL"}
	factory, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	run, snapshot := planFixture()
	_, err = factory.PlanWork(context.Background(), run, snapshot)
	if !errors.Is(err, ErrAppSecretRead) || strings.Contains(err.Error(), "ghs_sensitive_value") || strings.Contains(err.Error(), "PRIVATE-KEY-SENTINEL") {
		t.Fatalf("redaction failure: %v", err)
	}
}

func TestCloneTokenAdapterPreservesExactRepositoryAndReadOnlyProfile(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	minter, err := githubapp.NewForTest(githubapp.Config{
		AppID: 1, InstallationID: 42, PrivateKeyPEM: planPEM(t), BaseURL: server.server.URL,
		HTTPClient: server.server.Client(), Clock: func() time.Time { return planTestNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := cloneTokenAdapter{minter: minter}
	token, err := adapter.MintReadOnlyContentsToken(context.Background(), "github.com/Astatide1337/agents-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if token.Repository != "github.com/Astatide1337/agents-gateway" || !token.ContentsRead || token.ContentsWrite || token.Value != "ghs_clone_token" {
		t.Fatalf("adapted token=%#v", token)
	}
	if _, err := adapter.MintReadOnlyContentsToken(context.Background(), "https://github.com/Astatide1337/agents-gateway"); !errors.Is(err, ErrInvalidRepository) {
		t.Fatalf("unsafe repository error=%v", err)
	}
}

func TestPlanWorkUsesOperatorGitHubAppForCloneWhenPublishIsDisabled(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	run, snapshot := planFixture()
	snapshot.Spec.Publish = v1alpha1.PublishSpec{Mode: v1alpha1.PublishNone}
	run.Status.SpecDigest = digestPlan(snapshot)
	tracked := newPlanClient(t, planSourceObjects(t, "agw-system")...)
	factory := newPlanFactory(t, tracked, server, "agw-system")
	if _, err := factory.PlanWork(context.Background(), run, snapshot); err != nil {
		t.Fatalf("plan publish-none work: %v", err)
	}
	if countGet(tracked.gets, "agw-system", "github-app") != 1 {
		t.Fatalf("operator-approved GitHub App was not used: %#v", tracked.gets)
	}
}

func TestPinSourceBindsExactCommitIntoResolvedDigest(t *testing.T) {
	server := newGitHubTokenServer(t)
	defer server.Close()
	_, snapshot := planFixture()
	snapshot.BaseSHA = ""
	preliminaryBody, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	preliminaryDigest, err := canonical.ResolvedSpecDigest(preliminaryBody)
	if err != nil {
		t.Fatal(err)
	}
	factory := newPlanFactory(t, newPlanClient(t, planSourceObjects(t, "agw-system")...), server, "agw-system")
	result, err := factory.PinSource(context.Background(), resolved.Result{Snapshot: snapshot, Canonical: preliminaryBody, Digest: preliminaryDigest})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.BaseSHA != strings.Repeat("e", 40) || result.Digest == preliminaryDigest {
		t.Fatalf("source was not pinned: base=%q digest=%q", result.Snapshot.BaseSHA, result.Digest)
	}
	if decoded, err := resolved.Decode(result.Canonical, result.Digest); err != nil || decoded.BaseSHA != result.Snapshot.BaseSHA {
		t.Fatalf("pinned snapshot did not verify: decoded=%#v err=%v", decoded, err)
	}
}

func TestPinSourcePreservesBoundedGitHubResolutionClasses(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		baseRef   string
		class     githubapp.ResolutionClass
		permanent bool
		retryable bool
	}{
		{name: "invalid ref", baseRef: "main?source-secret", class: githubapp.ClassInvalidReference, permanent: true},
		{name: "not found", status: http.StatusNotFound, baseRef: "main", class: githubapp.ClassNotFound, permanent: true},
		{name: "unauthorized", status: http.StatusUnauthorized, baseRef: "main", class: githubapp.ClassUnauthorized, permanent: true},
		{name: "forbidden", status: http.StatusForbidden, baseRef: "main", class: githubapp.ClassUnauthorized, permanent: true},
		{name: "rate limited", status: http.StatusTooManyRequests, baseRef: "main", class: githubapp.ClassRateLimited, retryable: true},
		{name: "server error", status: http.StatusInternalServerError, baseRef: "main", class: githubapp.ClassServer, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newGitHubTokenServer(t)
			defer server.Close()
			server.mu.Lock()
			server.commitStatus = test.status
			server.responseSecret = "runplan-source-response-secret"
			server.mu.Unlock()

			_, snapshot := planFixture()
			snapshot.BaseSHA = ""
			snapshot.Spec.Source.BaseRef = test.baseRef
			body, err := canonical.CanonicalizeResolvedSpec(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := canonical.ResolvedSpecDigest(body)
			if err != nil {
				t.Fatal(err)
			}
			factory := newPlanFactory(t, newPlanClient(t, planSourceObjects(t, "agw-system")...), server, "agw-system")
			_, err = factory.PinSource(context.Background(), resolved.Result{Snapshot: snapshot, Canonical: body, Digest: digest})
			if err == nil {
				t.Fatal("source pin unexpectedly succeeded")
			}
			if got, ok := SourceResolutionClassOf(err); !ok || got != test.class {
				t.Fatalf("class=%q ok=%t, want %q: %v", got, ok, test.class, err)
			}
			if IsPermanentSourceResolutionError(err) != test.permanent || githubapp.IsRetryableClass(test.class) != test.retryable {
				t.Fatalf("permanent/retryable=%t/%t, want %t/%t", IsPermanentSourceResolutionError(err), githubapp.IsRetryableClass(test.class), test.permanent, test.retryable)
			}
			if !errors.Is(err, ErrMinter) || strings.Contains(err.Error(), "runplan-source-response-secret") || strings.Contains(err.Error(), "ghs_clone_token") {
				t.Fatalf("source error compatibility or redaction failure: %v", err)
			}
		})
	}
}

func TestSourceResolutionClassificationMapsLocalConfigAndUnknownErrorsFailClosed(t *testing.T) {
	if !IsPermanentSourceResolutionError(ErrInvalidRepository) || !IsPermanentSourceResolutionError(ErrAppSecretInvalid) || !IsPermanentSourceResolutionError(ErrMinter) {
		t.Fatal("local source configuration errors were not terminally classified")
	}
	if class, ok := SourceResolutionClassOf(ErrAppSecretRead); !ok || class != githubapp.ClassTransport || !githubapp.IsRetryableClass(class) {
		t.Fatalf("secret read classification=%q/%t, want retryable transport", class, ok)
	}
	if IsPermanentSourceResolutionError(errors.New("unclassified source failure")) {
		t.Fatal("unknown source failure was not fail-closed")
	}
}

type planTokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

type planTokenServer struct {
	server         *httptest.Server
	mu             sync.Mutex
	calls          int
	body           planTokenRequest
	tokenStatus    int
	commitStatus   int
	responseSecret string
}

func newGitHubTokenServer(t *testing.T) *planTokenServer {
	t.Helper()
	result := &planTokenServer{}
	result.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.EscapedPath() == "/repos/Astatide1337/agents-gateway/commits/main" {
			result.mu.Lock()
			status := result.commitStatus
			secret := result.responseSecret
			result.mu.Unlock()
			if status == 0 {
				status = http.StatusOK
			}
			response.WriteHeader(status)
			if status != http.StatusOK {
				_, _ = response.Write([]byte(`{"message":"` + secret + `"}`))
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"sha":"` + strings.Repeat("e", 40) + `"}`))
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/42/access_tokens" {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 32<<10))
		if err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var decoded planTokenRequest
		if err := json.Unmarshal(body, &decoded); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		result.mu.Lock()
		result.calls++
		result.body = decoded
		result.mu.Unlock()
		result.mu.Lock()
		status := result.tokenStatus
		secret := result.responseSecret
		result.mu.Unlock()
		if status == 0 {
			status = http.StatusCreated
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(status)
		if status != http.StatusCreated {
			_, _ = response.Write([]byte(`{"message":"` + secret + `"}`))
			return
		}
		_, _ = response.Write([]byte(`{"token":"ghs_clone_token","expires_at":"2026-08-11T16:00:00Z","permissions":{"contents":"read"},"repository_selection":"selected","repositories":[{"full_name":"Astatide1337/agents-gateway"}]}`))
	}))
	return result
}

func (s *planTokenServer) Close() { s.server.Close() }

func (s *planTokenServer) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *planTokenServer) LastRequestBody() planTokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

func planConfig(c SecretReader, server *planTokenServer, namespace string) Config {
	digest := strings.Repeat("a", 64)
	return Config{
		Client:                c,
		SystemNamespace:       namespace,
		GitHubAppSecret:       "github-app",
		CredentialSecretNames: []string{"mcp-credential", "model-credential", "skills-provider"},
		SkillsToken:           &runsecret.SecretKeyRef{SecretName: "skills-provider", Key: "api-key"},
		SkillsEndpoint:        "https://skills.example.test/mcp",
		CloneImage:            "ghcr.io/astatide/agw-clone@sha256:" + digest,
		SkillsImage:           "ghcr.io/astatide/agw-skills@sha256:" + digest,
		ContextImage:          "ghcr.io/astatide/agw-context@sha256:" + digest,
		LockdownImage:         "ghcr.io/astatide/agw-lockdown@sha256:" + digest,
		BrokerImage:           "ghcr.io/astatide/agw-broker@sha256:" + digest,
		Clock:                 func() time.Time { return planTestNow },
		GitHubAPIBaseURL:      server.server.URL,
		GitHubHTTPClient:      server.server.Client(),
		MinterFactory: func(config githubapp.Config) (*githubapp.Minter, error) {
			return githubapp.NewForTest(config)
		},
	}
}

func newPlanFactory(t *testing.T, client SecretReader, server *planTokenServer, namespace string) *Factory {
	t.Helper()
	factory, err := New(planConfig(client, server, namespace))
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func planFixture() (*v1alpha1.AgentRun, resolved.Snapshot) {
	run := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name:       "run-one",
		Namespace:  "agw-runs",
		UID:        types.UID("run-uid-one"),
		Generation: 7,
	}}
	inlineTask := "Fix the issue"
	inlineInstructions := "Work only in the allowed scope"
	snapshot := resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID), Generation: run.Generation},
		BaseSHA:       strings.Repeat("e", 40),
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "agent-one", GateRef: "gate-one",
			Source:    v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/agents-gateway", BaseRef: "main", Depth: 1},
			Task:      v1alpha1.TaskSpec{Inline: &inlineTask},
			Workspace: v1alpha1.WorkspaceSpec{Size: "8Gi"},
			Publish:   v1alpha1.PublishSpec{Mode: v1alpha1.PublishPullRequest, CredentialRef: "github-app"},
			Limits:    v1alpha1.LimitsSpec{Timeout: "45m", MaxToolCalls: 60, MaxCostUSD: "2.00"},
		},
		Task:         "Fix the issue",
		Instructions: "Work only in the allowed scope",
		Agent: v1alpha1.AgentSpec{
			Runtime:            v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@sha256:" + strings.Repeat("b", 64)},
			Instructions:       v1alpha1.InstructionsSpec{Inline: &inlineInstructions},
			ContextStrategyRef: "context-one",
			ToolSetRef:         "tools-one", ModelRouteRef: "models-one",
			Skills: []v1alpha1.SkillRef{{Name: "codebase-design", Ref: "https://skills.example/codebase-design", Digest: "sha256:" + strings.Repeat("c", 64)}},
		},
		Gate: v1alpha1.GateSpec{Verify: v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + strings.Repeat("d", 64)}},
		ToolSet: v1alpha1.ToolSetSpec{
			Servers: []v1alpha1.ToolServer{{Name: "github", Ref: "https://mcp.example.com/mcp", CredentialsRef: "mcp-credential", Tools: []v1alpha1.ToolDefinition{{Name: "get_issue", Effect: v1alpha1.EffectRead}}}},
			Profiles: []v1alpha1.ToolProfile{
				{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_issue"}}},
				{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_issue"}}},
				{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_issue"}}},
			},
			MaxToolsPerPhase: v1alpha1.MaxToolsPerPhase,
		},
		ModelRoute: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "openrouter", Kind: "openrouter-responses", Model: "nvidia/nemotron-free", Family: "nvidia", CredentialRef: "model-credential", Priority: 1}}, Budget: v1alpha1.ModelBudget{MaxCostUSD: "2.00"}},
		ContextStrategy: v1alpha1.ContextStrategySpec{
			RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 8000},
			Budget:  v1alpha1.ContextBudgetSpec{TotalTokens: 40000, MaxBytes: 8 << 20},
		},
		References: resolved.References{
			Agent:           resolved.ObjectVersion{Name: "agent-one", UID: "agent-uid-one", ResourceVersion: "11", Generation: 1},
			Gate:            resolved.ObjectVersion{Name: "gate-one", UID: "gate-uid-one", ResourceVersion: "12", Generation: 1},
			ToolSet:         resolved.ObjectVersion{Name: "tools-one", UID: "tools-uid-one", ResourceVersion: "13", Generation: 1},
			ModelRoute:      resolved.ObjectVersion{Name: "models-one", UID: "models-uid-one", ResourceVersion: "14", Generation: 1},
			ContextStrategy: resolved.ObjectVersion{Name: "context-one", UID: "context-uid-one", ResourceVersion: "15", Generation: 1},
		},
	}
	digest := digestPlan(snapshot)
	run.Status.SpecDigest = digest
	return run, snapshot
}

func digestPlan(snapshot resolved.Snapshot) string {
	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		panic(err)
	}
	return digest
}

func planPEM(t *testing.T) []byte {
	t.Helper()
	planKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			planKeyErr = err
			return
		}
		planKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	})
	if planKeyErr != nil {
		t.Fatal(planKeyErr)
	}
	return append([]byte(nil), planKeyPEM...)
}

func planAppSecret(t *testing.T, namespace string) *corev1.Secret {
	t.Helper()
	immutable := true
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: namespace}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{
		GitHubAppIDKey:          []byte("123"),
		GitHubInstallationIDKey: []byte("42"),
		GitHubPrivateKeyKey:     planPEM(t),
	}}
}

func planSourceObjects(t *testing.T, namespace string) []client.Object {
	return []client.Object{
		planAppSecret(t, namespace),
		planCredentialSecret("mcp-credential", namespace, "mcp-secret"),
		planCredentialSecret("model-credential", namespace, "model-secret"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "skills-provider", Namespace: namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"api-key": []byte("skills-secret")}},
	}
}

func planCredentialSecret(name, namespace, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{runsecret.SourceTokenKey: []byte(value)}}
}

type planClient struct {
	client.Client
	gets    []client.ObjectKey
	creates int
}

func newPlanClient(t *testing.T, objects ...client.Object) *planClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &planClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

func (c *planClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	c.gets = append(c.gets, key)
	return c.Client.Get(ctx, key, object, options...)
}

func (c *planClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.creates++
	return c.Client.Create(ctx, object, options...)
}

type errorPlanClient struct{ message string }

func (c errorPlanClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New(c.message)
}

func (c errorPlanClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errors.New("create should not be called")
}

func getPlanSecret(t *testing.T, c SecretReader, namespace, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

func countGet(keys []client.ObjectKey, namespace, name string) int {
	count := 0
	for _, key := range keys {
		if key.Namespace == namespace && key.Name == name {
			count++
		}
	}
	return count
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for index, container := range containers {
		names[index] = container.Name
	}
	return names
}

func containsSecretMount(container corev1.Container) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == workload.CloneSecretVolumeName || mount.Name == workload.SkillsSecretVolumeName || mount.Name == workload.BrokerSecretVolumeName {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
