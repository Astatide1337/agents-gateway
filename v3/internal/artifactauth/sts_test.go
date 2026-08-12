package artifactauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

type recordingSTSClient struct {
	mu       sync.Mutex
	input    *sts.AssumeRoleInput
	output   *sts.AssumeRoleOutput
	err      error
	requests int
}

func (c *recordingSTSClient) AssumeRole(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.input = input
	c.requests++
	return c.output, c.err
}

func (c *recordingSTSClient) snapshot() (*sts.AssumeRoleInput, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.input, c.requests
}

func TestSTSSessionPolicyUsesExactObjectPrefixAndPermissions(t *testing.T) {
	cases := []struct {
		name    string
		perms   Permissions
		actions []string
	}{
		{name: "read", perms: Permissions{Read: true}, actions: []string{"s3:GetObject"}},
		{name: "write", perms: Permissions{Write: true}, actions: []string{"s3:PutObject"}},
		{name: "read-write", perms: Permissions{Read: true, Write: true}, actions: []string{"s3:GetObject", "s3:PutObject"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := stsMintRequest(tc.perms)
			client := &recordingSTSClient{output: validSTSOutput(testNow.Add(request.TTL))}
			minter := newTestSTSMinter(t, client)
			lease, err := minter.Mint(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lease.Scope, request.Scope) {
				t.Fatalf("returned scope = %#v, want exact %#v", lease.Scope, request.Scope)
			}

			input, _ := client.snapshot()
			if input == nil || input.Policy == nil {
				t.Fatal("AssumeRole policy was not sent")
			}
			var policy struct {
				Version   string `json:"Version"`
				Statement []struct {
					Effect   string   `json:"Effect"`
					Action   []string `json:"Action"`
					Resource []string `json:"Resource"`
				} `json:"Statement"`
			}
			if err := json.Unmarshal([]byte(aws.ToString(input.Policy)), &policy); err != nil {
				t.Fatal(err)
			}
			if policy.Version != "2012-10-17" || len(policy.Statement) != 1 {
				t.Fatalf("policy envelope = %#v", policy)
			}
			statement := policy.Statement[0]
			if statement.Effect != "Allow" || !reflect.DeepEqual(statement.Action, tc.actions) {
				t.Fatalf("policy statement = %#v, want actions %#v", statement, tc.actions)
			}
			wantResource := "arn:aws:s3:::agw-artifacts/" + request.Scope.Prefix + "/*"
			if !reflect.DeepEqual(statement.Resource, []string{wantResource}) {
				t.Fatalf("resource = %#v, want %#v", statement.Resource, []string{wantResource})
			}
			encoded := aws.ToString(input.Policy)
			for _, forbidden := range []string{"s3:ListBucket", "s3:DeleteObject", "s3:*", "arn:aws:s3:::agw-artifacts\"}"} {
				if strings.Contains(encoded, forbidden) {
					t.Fatalf("policy contains forbidden permission/resource fragment %q: %s", forbidden, encoded)
				}
			}
			if strings.Contains(wantResource, request.Scope.Prefix+"-evil") || strings.HasSuffix(wantResource, request.Scope.Prefix+"*") {
				t.Fatalf("object resource loses the prefix boundary: %s", wantResource)
			}
		})
	}
}

func TestSTSMinterRejectsInvalidConfiguration(t *testing.T) {
	base := STSConfig{RoleARN: "arn:aws:iam::123456789012:role/agw-artifacts", Region: "us-east-1"}
	cases := []struct {
		name   string
		mutate func(*STSConfig)
	}{
		{name: "missing role", mutate: func(config *STSConfig) { config.RoleARN = "" }},
		{name: "wrong account width", mutate: func(config *STSConfig) { config.RoleARN = "arn:aws:iam::12345678901:role/agw-artifacts" }},
		{name: "wildcard role", mutate: func(config *STSConfig) { config.RoleARN = "arn:aws:iam::123456789012:role/agw-*" }},
		{name: "http endpoint", mutate: func(config *STSConfig) { config.Endpoint = "http://sts.example.com" }},
		{name: "endpoint credentials", mutate: func(config *STSConfig) { config.Endpoint = "https://user:secret@sts.example.com" }},
		{name: "endpoint query", mutate: func(config *STSConfig) { config.Endpoint = "https://sts.example.com/?x=1" }},
		{name: "invalid external id", mutate: func(config *STSConfig) { config.ExternalID = "external id" }},
		{name: "oversized external id", mutate: func(config *STSConfig) { config.ExternalID = strings.Repeat("x", MaxSTSExternalIDBytes+1) }},
		{name: "invalid session prefix", mutate: func(config *STSConfig) { config.SessionNamePrefix = "agw session" }},
		{name: "oversized session prefix", mutate: func(config *STSConfig) { config.SessionNamePrefix = strings.Repeat("x", MaxSTSSessionPrefixBytes+1) }},
		{name: "missing region", mutate: func(config *STSConfig) { config.Region = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := base
			tc.mutate(&config)
			if _, err := NewSTSMinterWithClient(config, &recordingSTSClient{}); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("constructor error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestSTSMinterRejectsUnsafeRequestTTLAndIdentity(t *testing.T) {
	client := &recordingSTSClient{output: validSTSOutput(testNow.Add(20 * time.Minute))}
	minter := newTestSTSMinter(t, client)
	cases := []struct {
		name   string
		mutate func(*MintRequest)
		want   error
	}{
		{name: "below STS minimum", mutate: func(request *MintRequest) { request.TTL = MinSTSLeaseLifetime - time.Second }, want: ErrInvalidRequest},
		{name: "above contract maximum", mutate: func(request *MintRequest) { request.TTL = MaxLeaseLifetime + time.Second }, want: ErrInvalidRequest},
		{name: "missing idempotency key", mutate: func(request *MintRequest) { request.IdempotencyKey = "" }, want: ErrInvalidRequest},
		{name: "scope endpoint not normalized", mutate: func(request *MintRequest) { request.Scope.Endpoint += "/" }, want: ErrInvalidScope},
		{name: "no permissions", mutate: func(request *MintRequest) { request.Scope.Permissions = Permissions{} }, want: ErrInvalidScope},
		{name: "scope belongs to another run", mutate: func(request *MintRequest) { request.Scope.Prefix = "runs/other-run" }, want: ErrInvalidScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := stsMintRequest(Permissions{Read: true, Write: true})
			tc.mutate(&request)
			if _, err := minter.Mint(t.Context(), request); !errors.Is(err, tc.want) {
				t.Fatalf("Mint() error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, requests := client.snapshot(); requests != 0 {
		t.Fatalf("unsafe requests reached STS: %d", requests)
	}
}

func TestSTSMinterValidatesReturnedCredentialsAndExpiry(t *testing.T) {
	cases := []struct {
		name   string
		output *sts.AssumeRoleOutput
		want   error
	}{
		{name: "missing output", output: nil, want: ErrInvalidCredentials},
		{name: "missing token", output: &sts.AssumeRoleOutput{Credentials: stsCredentialsWithoutToken()}, want: ErrInvalidCredentials},
		{name: "too soon", output: validSTSOutput(testNow.Add(MinLeaseLifetime - time.Nanosecond)), want: ErrInvalidExpiry},
		{name: "too late", output: validSTSOutput(testNow.Add(20*time.Minute + time.Nanosecond)), want: ErrInvalidExpiry},
		{name: "missing expiry", output: &sts.AssumeRoleOutput{Credentials: stsCredentialsWithoutExpiry()}, want: ErrInvalidExpiry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingSTSClient{output: tc.output}
			minter := newTestSTSMinter(t, client)
			if _, err := minter.Mint(t.Context(), stsMintRequest(Permissions{Read: true})); !errors.Is(err, tc.want) {
				t.Fatalf("Mint() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSTSMinterSanitizesProviderFailuresAndNamesSessionsDeterministically(t *testing.T) {
	providerErr := errors.New("AccessDenied: AKIAEXAMPLE secret-value response-body=private")
	client := &recordingSTSClient{err: providerErr}
	minter := newTestSTSMinter(t, client)
	request := stsMintRequest(Permissions{Read: true})
	_, err := minter.Mint(t.Context(), request)
	if !errors.Is(err, ErrMintFailed) || strings.Contains(err.Error(), "AKIAEXAMPLE") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("sanitized error = %v", err)
	}

	client.err = nil
	client.output = validSTSOutput(testNow.Add(request.TTL))
	if _, err := minter.Mint(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	first, _ := client.snapshot()
	firstName := aws.ToString(first.RoleSessionName)
	if len(firstName) > MaxSTSRoleSessionNameBytes || !validSTSSessionName(firstName) {
		t.Fatalf("unsafe role session name %q", firstName)
	}
	if _, err := minter.Mint(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	second, _ := client.snapshot()
	if firstName != aws.ToString(second.RoleSessionName) {
		t.Fatalf("same idempotency key changed session name: %q -> %q", firstName, aws.ToString(second.RoleSessionName))
	}
	if firstName == request.Run.UID || strings.Contains(firstName, request.Run.Name) {
		t.Fatalf("role session name contains raw run identity: %q", firstName)
	}
}

func TestNewSTSMinterUsesAWSSTSWireAndHTTPSCustomEndpoint(t *testing.T) {
	const response = `<?xml version="1.0" encoding="UTF-8"?><AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>ASIAEXAMPLE</AccessKeyId><SecretAccessKey>secret-example</SecretAccessKey><SessionToken>token-example</SessionToken><Expiration>2026-08-11T15:20:00Z</Expiration></Credentials></AssumeRoleResult><ResponseMetadata><RequestId>request-id</RequestId></ResponseMetadata></AssumeRoleResponse>`
	var received url.Values
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if err := request.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		received = request.Form
		writer.Header().Set("Content-Type", "text/xml")
		_, _ = writer.Write([]byte(response))
	}))
	defer server.Close()

	config := STSConfig{
		RoleARN:    "arn:aws:iam::123456789012:role/agw-artifacts",
		Region:     "us-east-1",
		Endpoint:   server.URL,
		Clock:      func() time.Time { return testNow },
		HTTPClient: &tlsHTTPClient{client: server.Client()},
		CredentialsProvider: staticAWSProvider{
			credentials: aws.Credentials{AccessKeyID: "caller", SecretAccessKey: "caller-secret", Source: "test"},
		},
	}
	minter, err := NewSTSMinter(config)
	if err != nil {
		t.Fatal(err)
	}
	request := stsMintRequest(Permissions{Read: true})
	lease, err := minter.Mint(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credentials.AccessKeyID != "ASIAEXAMPLE" || lease.ExpiresAt != testNow.Add(request.TTL) {
		t.Fatalf("lease = %#v", lease)
	}
	if received.Get("RoleArn") != config.RoleARN || received.Get("DurationSeconds") != "1200" || received.Get("ExternalId") != "" {
		t.Fatalf("STS form = %#v", received)
	}
	if received.Get("Policy") == "" || !strings.Contains(received.Get("Policy"), "runs/1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15/*") {
		t.Fatalf("STS policy form = %q", received.Get("Policy"))
	}
}

type tlsHTTPClient struct {
	client *http.Client
}

func (c *tlsHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request)
}

type staticAWSProvider struct {
	credentials aws.Credentials
}

func (p staticAWSProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return p.credentials, nil
}

func newTestSTSMinter(t *testing.T, client STSAssumeRoleClient) *STSMinter {
	t.Helper()
	minter, err := NewSTSMinterWithClient(STSConfig{
		RoleARN:    "arn:aws:iam::123456789012:role/agw-artifacts",
		Region:     "us-east-1",
		ExternalID: "astatide-external",
		Clock: func() time.Time {
			return testNow
		},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	return minter
}

func stsMintRequest(permissions Permissions) MintRequest {
	request := Request{
		Run: RunIdentity{Namespace: "agw-runs", Name: "repair-427", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15"},
		Scope: Scope{
			Region:      "us-east-1",
			Bucket:      "agw-artifacts",
			Prefix:      "runs/1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15",
			Permissions: permissions,
		},
		Purpose: "work",
		TTL:     20 * time.Minute,
	}
	return MintRequest{
		Run:            request.Run,
		Scope:          request.Scope,
		Purpose:        request.Purpose,
		TTL:            request.TTL,
		IdempotencyKey: idempotencyKey(request),
	}
}

func validSTSOutput(expiration time.Time) *sts.AssumeRoleOutput {
	return &sts.AssumeRoleOutput{Credentials: &types.Credentials{
		AccessKeyId:     aws.String("ASIAEXAMPLE"),
		SecretAccessKey: aws.String("secret-example"),
		SessionToken:    aws.String("token-example"),
		Expiration:      aws.Time(expiration),
	}}
}

func stsCredentialsWithoutToken() *types.Credentials {
	return &types.Credentials{
		AccessKeyId:     aws.String("ASIAEXAMPLE"),
		SecretAccessKey: aws.String("secret-example"),
		Expiration:      aws.Time(testNow.Add(20 * time.Minute)),
	}
}

func stsCredentialsWithoutExpiry() *types.Credentials {
	return &types.Credentials{
		AccessKeyId:     aws.String("ASIAEXAMPLE"),
		SecretAccessKey: aws.String("secret-example"),
		SessionToken:    aws.String("token-example"),
	}
}

func validSTSSessionName(value string) bool {
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("+=,.@-_", character)) {
			return false
		}
	}
	return value != ""
}
