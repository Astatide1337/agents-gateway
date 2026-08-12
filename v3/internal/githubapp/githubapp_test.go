package githubapp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyErr  error
)

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		testKey, testKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if testKeyErr != nil {
		t.Fatal(testKeyErr)
	}
	return testKey
}

func testPEM(t *testing.T, pkcs8 bool) []byte {
	t.Helper()
	key := testRSAKey(t)
	der := x509.MarshalPKCS1PrivateKey(key)
	label := "RSA PRIVATE KEY"
	if pkcs8 {
		var err error
		der, err = x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		label = "PRIVATE KEY"
	}
	return pem.EncodeToMemory(&pem.Block{Type: label, Bytes: der})
}

func fixedNow() time.Time {
	return time.Date(2026, time.August, 11, 15, 4, 5, 987654321, time.FixedZone("EDT", -4*60*60))
}

func testConfig(t *testing.T, baseURL string, client *http.Client) Config {
	t.Helper()
	return Config{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPEM:  testPEM(t, false),
		BaseURL:        baseURL,
		HTTPClient:     client,
		Clock:          fixedNow,
		RequestTimeout: 2 * time.Second,
	}
}

func newTestMinter(t *testing.T, server *httptest.Server, configMutator func(*Config)) *Minter {
	t.Helper()
	cfg := testConfig(t, server.URL, server.Client())
	if configMutator != nil {
		configMutator(&cfg)
	}
	minter, err := NewForTest(cfg)
	if err != nil {
		t.Fatalf("NewForTest: %v", err)
	}
	return minter
}

func validRepository() Repository {
	return Repository{Owner: "Astatide1337", Name: "agents-gateway"}
}

func validResponse(profile PermissionProfile, repo Repository, expiresAt time.Time) []byte {
	permissions, _ := permissionsFor(profile)
	permissionBody := map[string]string{"contents": permissions.Contents}
	if permissions.PullRequests != "" {
		permissionBody["pull_requests"] = permissions.PullRequests
	}
	response := tokenResponse{
		Token:               "ghs_test_installation_token",
		ExpiresAt:           expiresAt.UTC().Format(time.RFC3339),
		Permissions:         permissionBody,
		RepositorySelection: "selected",
		Repositories: []tokenResponseRepository{{
			FullName: repo.FullName(),
		}},
	}
	body, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	return body
}

func TestNewParsesPKCS1AndPKCS8(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		t.Run(map[bool]string{false: "pkcs1", true: "pkcs8"}[pkcs8], func(t *testing.T) {
			cfg := testConfig(t, "", nil)
			cfg.PrivateKeyPEM = testPEM(t, pkcs8)
			minter, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if minter.privateKey == nil || minter.privateKey.N.Cmp(testRSAKey(t).N) != 0 {
				t.Fatal("parsed key does not match input")
			}
		})
	}
}

func TestNewRejectsMalformedOrUnsafeKeysWithoutRedactionLeaks(t *testing.T) {
	keyMaterial := "-----BEGIN RSA PRIVATE KEY-----\nprivate-secret-material\n-----END RSA PRIVATE KEY-----"
	malformedPKCS8, err := x509.MarshalPKCS8PrivateKey(mustECDSAKey(t))
	if err != nil {
		t.Fatal(err)
	}
	tooSmall, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pem  []byte
	}{
		{name: "empty", pem: nil},
		{name: "not PEM", pem: []byte(keyMaterial)},
		{name: "trailing second block", pem: append(testPEM(t, false), testPEM(t, true)...)},
		{name: "wrong label", pem: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("bad")})},
		{name: "PKCS8 non RSA", pem: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: malformedPKCS8})},
		{name: "encrypted header", pem: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: []byte("bad")})},
		{name: "1024 bit RSA", pem: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(tooSmall)})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(t, "", nil)
			cfg.PrivateKeyPEM = test.pem
			_, err := New(cfg)
			if err == nil {
				t.Fatal("malformed key was accepted")
			}
			if strings.Contains(err.Error(), "private-secret-material") || strings.Contains(err.Error(), "BEGIN") {
				t.Fatalf("key material leaked in error: %q", err)
			}
		})
	}

	cfg := testConfig(t, "", nil)
	cfg.PrivateKeyPEM = bytesOfSize(maxPrivateKeyPEMBytes + 1)
	_, err = New(cfg)
	if !errors.Is(err, ErrPrivateKeyTooLarge) || strings.Contains(err.Error(), "private") && strings.Contains(err.Error(), keyMaterial) {
		t.Fatalf("unexpected oversized-key result: %v", err)
	}
}

func mustECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func bytesOfSize(size int) []byte {
	return []byte(strings.Repeat("x", size))
}

func TestAppJWTIsRS256BoundedAndUsesSkew(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	minter := newTestMinter(t, server, func(cfg *Config) {
		cfg.JWTClockSkew = 45 * time.Second
		cfg.JWTLifetime = 9 * time.Minute
	})
	now := fixedNow().UTC()
	jwt, err := minter.appJWT(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT segments=%d, want 3", len(parts))
	}
	decode := func(value string) []byte {
		t.Helper()
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header.Algorithm != "RS256" || header.Type != "JWT" {
		t.Fatalf("header=%#v", header)
	}
	var claims struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != 12345 || claims.IssuedAt != now.Add(-45*time.Second).Unix() || claims.ExpiresAt != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims=%#v", claims)
	}
	if claims.ExpiresAt-now.Unix() > int64(MaxJWTLifetime/time.Second) {
		t.Fatalf("JWT expiry exceeds the nine-minute bound: %#v", claims)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature := decode(parts[2])
	if err := rsa.VerifyPKCS1v15(&testRSAKey(t).PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("JWT signature did not verify: %v", err)
	}
}

func TestMintRequestsExactlyOneRepositoryAndClosedPermissionProfiles(t *testing.T) {
	repo := validRepository()
	for _, profile := range []PermissionProfile{PermissionCloneRead, PermissionPublish} {
		t.Run(string(profile), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v3/app/installations/67890/access_tokens" {
					t.Errorf("request=%s %s", r.Method, r.URL.String())
				}
				if r.URL.RawQuery != "" || r.URL.Fragment != "" {
					t.Errorf("request has query or fragment: %s", r.URL.String())
				}
				if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
					t.Errorf("Accept=%q", got)
				}
				if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
					t.Errorf("X-GitHub-Api-Version=%q", got)
				}
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Errorf("missing Bearer authorization")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
					return
				}
				if len(body) != 2 {
					t.Errorf("body keys=%v, want exactly repositories and permissions", body)
				}
				var repositories []string
				if err := json.Unmarshal(body["repositories"], &repositories); err != nil || len(repositories) != 1 || repositories[0] != repo.FullName() {
					t.Errorf("repositories=%#v err=%v", repositories, err)
				}
				var permissions map[string]string
				if err := json.Unmarshal(body["permissions"], &permissions); err != nil {
					t.Errorf("permissions: %v", err)
					return
				}
				if profile == PermissionCloneRead {
					if fmt.Sprint(permissions) != "map[contents:read]" {
						t.Errorf("clone permissions=%v", permissions)
					}
				} else if fmt.Sprint(permissions) != "map[contents:write pull_requests:write]" {
					t.Errorf("publish permissions=%v", permissions)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(validResponse(profile, repo, fixedNow().Add(time.Hour)))
			}))
			defer server.Close()
			minter := newTestMinter(t, server, func(cfg *Config) {
				cfg.BaseURL = server.URL + "/api/v3/"
			})
			token, err := minter.Mint(context.Background(), repo, profile)
			if err != nil {
				t.Fatal(err)
			}
			wantExpiry := fixedNow().Add(time.Hour).UTC().Truncate(time.Second)
			if token.Value() != "ghs_test_installation_token" || !token.ExpiresAt().Equal(wantExpiry) {
				t.Fatalf("token metadata=%q %s", token.Value(), token.ExpiresAt())
			}
			if token.Repository() != repo || token.PermissionProfile() != profile || token.IsZero() {
				t.Fatalf("token metadata=%#v", token)
			}
			if requests.Load() != 1 {
				t.Fatalf("request count=%d, want 1", requests.Load())
			}
		})
	}
}

func TestInstallationTokenFormattingAlwaysRedacts(t *testing.T) {
	secret := "ghs_super_secret_value"
	token := InstallationToken{value: secret}
	for _, formatted := range []string{
		token.String(),
		fmt.Sprintf("%s", token),
		fmt.Sprintf("%v", token),
		fmt.Sprintf("%+v", token),
		fmt.Sprintf("%#v", token),
	} {
		if strings.Contains(formatted, secret) || !strings.Contains(formatted, "REDACTED") {
			t.Fatalf("formatted token leaked or was not redacted: %q", formatted)
		}
	}
}

func TestMintRejectsUnknownPermissionProfileBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	minter := newTestMinter(t, server, nil)
	if _, err := minter.Mint(context.Background(), validRepository(), PermissionProfile("admin")); !errors.Is(err, ErrInvalidPermission) {
		t.Fatalf("error=%v, want invalid permission", err)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid permission profile reached the network")
	}
}

func TestMintRejectsRedirectsWithoutFollowingThem(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			redirected.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(validResponse(PermissionCloneRead, validRepository(), fixedNow().Add(time.Hour)))
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer server.Close()
	minter := newTestMinter(t, server, nil)
	_, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead)
	if !errors.Is(err, ErrRedirect) || redirected.Load() != 0 {
		t.Fatalf("redirect result=%v target requests=%d", err, redirected.Load())
	}
}

func TestResolveCommitClassifiesPermanentAndTransientHTTPFailuresWithoutLeaks(t *testing.T) {
	const responseSecret = "github-response-secret"
	tests := []struct {
		name      string
		status    int
		class     ResolutionClass
		permanent bool
		retryable bool
		classErr  error
	}{
		{name: "not found", status: http.StatusNotFound, class: ClassNotFound, permanent: true, classErr: ErrSourceNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized, class: ClassUnauthorized, permanent: true, classErr: ErrSourceUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, class: ClassUnauthorized, permanent: true, classErr: ErrSourceUnauthorized},
		{name: "rate limited", status: http.StatusTooManyRequests, class: ClassRateLimited, retryable: true, classErr: ErrSourceRateLimited},
		{name: "server error", status: http.StatusInternalServerError, class: ClassServer, retryable: true, classErr: ErrSourceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"message":"`+responseSecret+`"}`)
			}))
			defer server.Close()
			minter := newTestMinter(t, server, nil)
			token := InstallationToken{
				value:      "ghs_commit_secret",
				expiresAt:  fixedNow().Add(time.Hour),
				repository: validRepository(),
				profile:    PermissionCloneRead,
			}

			_, err := minter.ResolveCommit(context.Background(), token, validRepository(), "main")
			if err == nil {
				t.Fatal("source resolution unexpectedly succeeded")
			}
			if got, ok := ClassOf(err); !ok || got != test.class {
				t.Fatalf("class=%q ok=%t, want %q: %v", got, ok, test.class, err)
			}
			if IsPermanentClass(test.class) != test.permanent || IsRetryableClass(test.class) != test.retryable {
				t.Fatalf("permanent/retryable=%t/%t, want %t/%t", IsPermanentClass(test.class), IsRetryableClass(test.class), test.permanent, test.retryable)
			}
			if !errors.Is(err, test.classErr) || !errors.Is(err, ErrUnexpectedStatus) {
				t.Fatalf("error=%v, want class sentinel %v and legacy status sentinel", err, test.classErr)
			}
			if strings.Contains(err.Error(), responseSecret) || strings.Contains(err.Error(), "ghs_commit_secret") {
				t.Fatalf("response or token data leaked in error: %q", err)
			}
		})
	}
}

func TestResolveCommitRejectsInvalidReferenceBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	minter := newTestMinter(t, server, nil)
	token := InstallationToken{
		value:      "ghs_commit_secret",
		expiresAt:  fixedNow().Add(time.Hour),
		repository: validRepository(),
		profile:    PermissionCloneRead,
	}
	_, err := minter.ResolveCommit(context.Background(), token, validRepository(), "main?"+"secret")
	if !errors.Is(err, ErrInvalidReference) || !IsPermanentClass(ClassInvalidReference) {
		t.Fatalf("invalid reference error=%v, want permanent ErrInvalidReference", err)
	}
	if got, ok := ClassOf(err); !ok || got != ClassInvalidReference {
		t.Fatalf("class=%q ok=%t, want %q", got, ok, ClassInvalidReference)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid reference reached the network")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "ghs_commit_secret") {
		t.Fatalf("invalid reference or token data leaked in error: %q", err)
	}
}

func TestResolveCommitClassifiesTimeoutAndTransportWithoutLeaks(t *testing.T) {
	const secret = "transport-secret"
	tests := []struct {
		name  string
		err   error
		class ResolutionClass
	}{
		{name: "timeout", err: testNetworkError{message: "timeout contains " + secret, timeout: true}, class: ClassTimeout},
		{name: "network", err: fmt.Errorf("network failure contains %s", secret), class: ClassTransport},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.NotFoundHandler())
			defer server.Close()
			minter := newTestMinter(t, server, func(cfg *Config) {
				cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, test.err
				})}
			})
			token := InstallationToken{
				value:      "ghs_commit_secret",
				expiresAt:  fixedNow().Add(time.Hour),
				repository: validRepository(),
				profile:    PermissionCloneRead,
			}
			_, err := minter.ResolveCommit(context.Background(), token, validRepository(), "main")
			if got, ok := ClassOf(err); !ok || got != test.class || !IsRetryableClass(test.class) || IsPermanentClass(test.class) {
				t.Fatalf("class/retry/permanent=%q/%t/%t, want %q/true/false: %v", got, ok, IsPermanentClass(test.class), test.class, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "ghs_commit_secret") {
				t.Fatalf("transport data leaked in error: %q", err)
			}
		})
	}
}

func TestMintClassifiesHTTPStatusWithoutLeaks(t *testing.T) {
	const responseSecret = "mint-response-secret"
	tests := []struct {
		name   string
		status int
		class  ResolutionClass
	}{
		{name: "not found", status: http.StatusNotFound, class: ClassNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized, class: ClassUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, class: ClassUnauthorized},
		{name: "rate limited", status: http.StatusTooManyRequests, class: ClassRateLimited},
		{name: "server error", status: http.StatusBadGateway, class: ClassServer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"message":"`+responseSecret+`"}`)
			}))
			defer server.Close()
			minter := newTestMinter(t, server, nil)
			_, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead)
			if err == nil {
				t.Fatal("token mint unexpectedly succeeded")
			}
			if got, ok := ClassOf(err); !ok || got != test.class || !errors.Is(err, ErrUnexpectedStatus) {
				t.Fatalf("class=%q ok=%t error=%v, want %q and legacy status sentinel", got, ok, err, test.class)
			}
			if IsPermanentClass(test.class) != (test.class == ClassNotFound || test.class == ClassUnauthorized) || IsRetryableClass(test.class) != (test.class == ClassRateLimited || test.class == ClassServer) {
				t.Fatalf("permanent/retryable=%t/%t for %q", IsPermanentClass(test.class), IsRetryableClass(test.class), test.class)
			}
			if strings.Contains(err.Error(), responseSecret) || strings.Contains(err.Error(), "ghs_test_installation_token") {
				t.Fatalf("response or token data leaked in error: %q", err)
			}
		})
	}
}

type testNetworkError struct {
	message string
	timeout bool
}

func (e testNetworkError) Error() string   { return e.message }
func (e testNetworkError) Timeout() bool   { return e.timeout }
func (e testNetworkError) Temporary() bool { return true }

func TestMintRejectsStatusMalformedResponseAndOversizedBodyWithoutResponseLeak(t *testing.T) {
	secret := "response-secret-must-not-appear"
	tests := []struct {
		name      string
		status    int
		body      func() []byte
		maxBody   int64
		want      error
		wantInErr string
	}{
		{name: "noncreated status", status: http.StatusOK, body: func() []byte { return []byte(`{"token":"` + secret + `"}`) }, want: ErrUnexpectedStatus, wantInErr: secret},
		{name: "empty", status: http.StatusCreated, body: func() []byte { return nil }, want: ErrInvalidResponse},
		{name: "malformed JSON", status: http.StatusCreated, body: func() []byte { return []byte(`{"token":`) }, want: ErrInvalidResponse},
		{name: "missing token", status: http.StatusCreated, body: func() []byte {
			return []byte(`{"expires_at":"2026-08-11T16:04:05Z","permissions":{"contents":"read"}}`)
		}, want: errInvalidTokenContents},
		{name: "bad expiry", status: http.StatusCreated, body: func() []byte {
			return []byte(`{"token":"ghs_test","expires_at":"tomorrow","permissions":{"contents":"read"}}`)
		}, want: ErrInvalidResponse},
		{name: "expired", status: http.StatusCreated, body: func() []byte {
			return validResponseWithToken(PermissionCloneRead, validRepository(), fixedNow().Add(-time.Second), "ghs_test")
		}, want: ErrTokenExpired},
		{name: "missing permissions", status: http.StatusCreated, body: func() []byte { return validResponseWithTokenAndPermissions(nil, fixedNow().Add(time.Hour), "ghs_test") }, want: ErrTokenPermission},
		{name: "wrong permissions", status: http.StatusCreated, body: func() []byte {
			return validResponseWithTokenAndPermissions(map[string]string{"contents": "write"}, fixedNow().Add(time.Hour), "ghs_test")
		}, want: ErrTokenPermission},
		{name: "invalid selection", status: http.StatusCreated, body: func() []byte {
			return invalidSelectionResponse()
		}, want: ErrInvalidResponse},
		{name: "oversized", status: http.StatusCreated, body: func() []byte {
			return append(bytesOfSize(128), []byte(secret)...)
		}, maxBody: 64, want: ErrResponseTooLarge, wantInErr: secret},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body())
			}))
			defer server.Close()
			minter := newTestMinter(t, server, func(cfg *Config) {
				if test.maxBody != 0 {
					cfg.MaxResponseSize = test.maxBody
				}
			})
			_, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want errors.Is %v", err, test.want)
			}
			if test.wantInErr != "" && strings.Contains(err.Error(), test.wantInErr) {
				t.Fatalf("error leaked response material %q: %v", test.wantInErr, err)
			}
		})
	}
}

func validResponseWithToken(profile PermissionProfile, repo Repository, expiresAt time.Time, token string) []byte {
	body := validResponse(profile, repo, expiresAt)
	var response tokenResponse
	if err := json.Unmarshal(body, &response); err != nil {
		panic(err)
	}
	response.Token = token
	body, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	return body
}

func validResponseWithTokenAndPermissions(permissions map[string]string, expiresAt time.Time, token string) []byte {
	response := tokenResponse{Token: token, ExpiresAt: expiresAt.UTC().Format(time.RFC3339), Permissions: permissions}
	body, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	return body
}

func invalidSelectionResponse() []byte {
	body := validResponse(PermissionCloneRead, validRepository(), fixedNow().Add(time.Hour))
	var response tokenResponse
	if err := json.Unmarshal(body, &response); err != nil {
		panic(err)
	}
	response.RepositorySelection = "all"
	body, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	return body
}

func TestMintHonorsCancellationAndTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.CloseClientConnections()
		server.Close()
	})
	minter := newTestMinter(t, server, func(cfg *Config) {
		cfg.RequestTimeout = 5 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := minter.Mint(ctx, validRepository(), PermissionCloneRead)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach test server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Mint did not honor cancellation")
	}

	deadlineRelease := make(chan struct{})
	deadlineServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-deadlineRelease:
		}
	}))
	t.Cleanup(func() {
		close(deadlineRelease)
		deadlineServer.CloseClientConnections()
		deadlineServer.Close()
	})
	deadlineMinter := newTestMinter(t, deadlineServer, func(cfg *Config) {
		cfg.RequestTimeout = 20 * time.Millisecond
	})
	_, err := deadlineMinter.Mint(context.Background(), validRepository(), PermissionCloneRead)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v, want deadline exceeded", err)
	}

	canceled, cancelImmediately := context.WithCancel(context.Background())
	cancelImmediately()
	if _, err := minter.Mint(canceled, validRepository(), PermissionCloneRead); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error=%v", err)
	}
}

func TestMintDoesNotUseProxyFromInjectedStandardTransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(validResponse(PermissionCloneRead, validRepository(), fixedNow().Add(time.Hour)))
	}))
	defer server.Close()
	baseClient := server.Client()
	transport := baseClient.Transport.(*http.Transport).Clone()
	transport.Proxy = func(*http.Request) (*url.URL, error) {
		return nil, errors.New("proxy must not be used")
	}
	client := &http.Client{Transport: transport}
	minter := newTestMinter(t, server, func(cfg *Config) {
		cfg.HTTPClient = client
	})
	if _, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead); err != nil {
		t.Fatalf("proxy was used or request failed: %v", err)
	}
}

func TestMintSanitizesTransportErrorsContainingSecrets(t *testing.T) {
	privateMaterial := string(testPEM(t, false))
	secret := "ghs_transport_secret"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("private=%s token=%s body=%s", privateMaterial, secret, secret)
	})}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	minter := newTestMinter(t, server, func(cfg *Config) {
		cfg.HTTPClient = client
	})
	_, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead)
	if !errors.Is(err, ErrRequest) || strings.Contains(err.Error(), privateMaterial) || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error was not sanitized: %v", err)
	}
}

func TestMintRejectsNonASCIIAndWhitespaceInstallationTokens(t *testing.T) {
	for _, token := range []string{" ghs_test", "ghs_test ", "ghs\n test", "ghs-☃"} {
		t.Run(fmt.Sprintf("%q", token), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response := validResponseWithToken(PermissionCloneRead, validRepository(), fixedNow().Add(time.Hour), token)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(response)
			}))
			defer server.Close()
			minter := newTestMinter(t, server, nil)
			if _, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead); !errors.Is(err, errInvalidTokenContents) {
				t.Fatalf("token=%q error=%v", token, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestNewValidatesIDsOriginsAndBounds(t *testing.T) {
	base := testConfig(t, "", nil)
	tests := []struct {
		name string
		edit func(*Config)
		want error
	}{
		{name: "zero app", edit: func(cfg *Config) { cfg.AppID = 0 }, want: ErrInvalidConfig},
		{name: "negative installation", edit: func(cfg *Config) { cfg.InstallationID = -1 }, want: ErrInvalidConfig},
		{name: "http origin", edit: func(cfg *Config) { cfg.BaseURL = "http://api.github.com" }, want: ErrInvalidAPIOrigin},
		{name: "userinfo", edit: func(cfg *Config) { cfg.BaseURL = "https://user:pass@example.com" }, want: ErrInvalidAPIOrigin},
		{name: "query", edit: func(cfg *Config) { cfg.BaseURL = "https://example.com/api?token=secret" }, want: ErrInvalidAPIOrigin},
		{name: "fragment", edit: func(cfg *Config) { cfg.BaseURL = "https://example.com/api#fragment" }, want: ErrInvalidAPIOrigin},
		{name: "dot path", edit: func(cfg *Config) { cfg.BaseURL = "https://example.com/api/../v3" }, want: errInvalidBaseURLPath},
		{name: "loopback production", edit: func(cfg *Config) { cfg.BaseURL = "https://127.0.0.1:8443" }, want: ErrPrivateTestOnly},
		{name: "private production", edit: func(cfg *Config) { cfg.BaseURL = "https://10.0.0.5" }, want: ErrPrivateTestOnly},
		{name: "request timeout negative", edit: func(cfg *Config) { cfg.RequestTimeout = -time.Second }, want: ErrInvalidConfig},
		{name: "request timeout too long", edit: func(cfg *Config) { cfg.RequestTimeout = MaxRequestTimeout + time.Nanosecond }, want: ErrInvalidConfig},
		{name: "skew negative", edit: func(cfg *Config) { cfg.JWTClockSkew = -time.Second }, want: ErrInvalidConfig},
		{name: "skew too long", edit: func(cfg *Config) { cfg.JWTClockSkew = MaxJWTClockSkew + time.Nanosecond }, want: ErrInvalidConfig},
		{name: "lifetime too short", edit: func(cfg *Config) { cfg.JWTLifetime = 500 * time.Millisecond }, want: ErrInvalidConfig},
		{name: "lifetime too long", edit: func(cfg *Config) { cfg.JWTLifetime = MaxJWTLifetime + time.Nanosecond }, want: ErrInvalidConfig},
		{name: "response negative", edit: func(cfg *Config) { cfg.MaxResponseSize = -1 }, want: ErrInvalidConfig},
		{name: "response too long", edit: func(cfg *Config) { cfg.MaxResponseSize = MaxResponseBytes + 1 }, want: ErrInvalidConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.PrivateKeyPEM = append([]byte(nil), base.PrivateKeyPEM...)
			test.edit(&cfg)
			_, err := New(cfg)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want errors.Is %v", err, test.want)
			}
		})
	}

	if _, err := NewForTest(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: testPEM(t, false), BaseURL: "http://127.0.0.1"}); !errors.Is(err, ErrInvalidAPIOrigin) {
		t.Fatalf("test seam accepted non-HTTPS origin: %v", err)
	}
	if _, err := New(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: testPEM(t, false), BaseURL: "https://example.com"}); err != nil {
		t.Fatalf("public HTTPS origin rejected: %v", err)
	}
}

func TestRepositoryValidationRequiresExactlyOneSafeOwnerAndName(t *testing.T) {
	for _, repo := range []Repository{
		{},
		{Owner: "owner/repo", Name: "repo"},
		{Owner: "owner", Name: "repo/other"},
		{Owner: "owner", Name: "../repo"},
		{Owner: "owner", Name: "repo?token=x"},
		{Owner: "owner", Name: "repo%2Fother"},
		{Owner: "owner", Name: "repo name"},
		{Owner: "é", Name: "repo"},
		{Owner: strings.Repeat("a", maxRepositoryPart+1), Name: "repo"},
	} {
		if err := repo.Validate(); !errors.Is(err, ErrInvalidRepository) {
			t.Fatalf("repository %#v was accepted: %v", repo, err)
		}
	}
	if err := (Repository{Owner: "owner", Name: "repo_name.v3-1"}).Validate(); err != nil {
		t.Fatalf("valid repository rejected: %v", err)
	}
}

func TestValidateTokenResponseRejectsUnexpectedRepositoryList(t *testing.T) {
	repo := validRepository()
	response := tokenResponse{
		Token:        "ghs_test",
		ExpiresAt:    fixedNow().Add(time.Hour).Format(time.RFC3339),
		Permissions:  map[string]string{"contents": "read"},
		Repositories: []tokenResponseRepository{{FullName: "other/repo"}},
	}
	if err := validateTokenResponse(response, repo, PermissionCloneRead, fixedNow().UTC()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("unexpected repository list accepted: %v", err)
	}
}

func TestReadBoundedHandlesReaderErrorsWithoutLeak(t *testing.T) {
	secret := "body-secret"
	_, err := readBounded(errorReader{err: fmt.Errorf("body contains %s", secret)}, 64)
	if !errors.Is(err, ErrRequest) || strings.Contains(err.Error(), secret) {
		t.Fatalf("reader error not sanitized: %v", err)
	}
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

func TestInstallationEndpointEscapesOnlyFixedNumericIdentity(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	minter := newTestMinter(t, server, func(cfg *Config) {
		cfg.BaseURL = server.URL + "/api/v3"
	})
	if got, want := minter.installationEndpoint(), server.URL+"/api/v3/app/installations/67890/access_tokens"; got != want {
		t.Fatalf("endpoint=%q, want %q", got, want)
	}
}

func TestResponseTimeComparisonUsesInjectedClock(t *testing.T) {
	now := fixedNow().UTC()
	response := tokenResponse{
		Token:       "ghs_test",
		ExpiresAt:   now.Add(MaxInstallationTokenLifetime + time.Nanosecond).Format(time.RFC3339Nano),
		Permissions: map[string]string{"contents": "read"},
	}
	if err := validateTokenResponse(response, validRepository(), PermissionCloneRead, now); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("overlong token lifetime accepted: %v", err)
	}
}

func TestMinterDoesNotExposePrivateKeyThroughConstructionErrors(t *testing.T) {
	secret := string(testPEM(t, false))
	cfg := testConfig(t, "https://127.0.0.1:8443", nil)
	_, err := New(cfg)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "BEGIN RSA") {
		t.Fatalf("construction error leaked key material: %v", err)
	}
}

func TestMintRejectsNonContextNil(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	minter := newTestMinter(t, server, nil)
	if _, err := minter.Mint(nil, validRepository(), PermissionCloneRead); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil context error=%v", err)
	}
}

func TestResponseBodyReadIsBoundedBeforeJSONParsing(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"ghs_test","expires_at":"2026-08-11T16:04:05Z","permissions":{"contents":"read"}}`+strings.Repeat("x", 1000))
	}))
	defer server.Close()
	minter := newTestMinter(t, server, func(cfg *Config) {
		cfg.MaxResponseSize = 128
	})
	_, err := minter.Mint(context.Background(), validRepository(), PermissionCloneRead)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error=%v, want bounded response error", err)
	}
}
