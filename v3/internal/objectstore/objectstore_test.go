package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var (
	_ effects.ObjectStore = (*Store)(nil)
	_ artifacts.Store     = (*Store)(nil)
)

type fakeClient struct {
	put func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	get func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error)
}

func (f *fakeClient) PutObject(ctx context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.put == nil {
		return &s3.PutObjectOutput{}, nil
	}
	return f.put(ctx, input)
}

func (f *fakeClient) GetObject(ctx context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.get == nil {
		return nil, errors.New("fake GetObject not configured")
	}
	return f.get(ctx, input)
}

func testStore(t *testing.T, client Client, max int64) *Store {
	t.Helper()
	store, err := NewWithClient(Config{
		Bucket:         "agw-test-bucket",
		Region:         "us-east-1",
		Prefix:         "runs",
		MaxObjectBytes: max,
	}, client)
	if err != nil {
		t.Fatalf("NewWithClient() error = %v", err)
	}
	return store
}

func TestStoreImplementsExistingContracts(t *testing.T) {
	var _ effects.ObjectStore = (*Store)(nil)
	var _ artifacts.Store = (*Store)(nil)
}

func TestCreateUsesConditionalCreateAndPrefix(t *testing.T) {
	var gotInput *s3.PutObjectInput
	store := testStore(t, &fakeClient{
		put: func(_ context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			gotInput = input
			return &s3.PutObjectOutput{}, nil
		},
	}, 1024)

	created, err := store.Create(context.Background(), "patch.diff", []byte("patch"), "text/x-diff")
	if err != nil || !created {
		t.Fatalf("Create() = (%v, %v), want (true, nil)", created, err)
	}
	if got := aws.ToString(gotInput.Bucket); got != "agw-test-bucket" {
		t.Errorf("bucket = %q", got)
	}
	if got := aws.ToString(gotInput.Key); got != "runs/patch.diff" {
		t.Errorf("key = %q", got)
	}
	if got := aws.ToString(gotInput.IfNoneMatch); got != "*" {
		t.Errorf("IfNoneMatch = %q, want *", got)
	}
	if got := aws.ToString(gotInput.ContentType); got != "text/x-diff" {
		t.Errorf("ContentType = %q", got)
	}
	if got := aws.ToInt64(gotInput.ContentLength); got != 5 {
		t.Errorf("ContentLength = %d, want 5", got)
	}
	body, err := io.ReadAll(gotInput.Body)
	if err != nil || string(body) != "patch" {
		t.Fatalf("request body = %q, read error = %v", body, err)
	}
}

func TestCreateMapsOnlyDefinitiveAlreadyExists(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantExist bool
		wantErr   bool
	}{
		{name: "412 response", err: responseError(http.StatusPreconditionFailed, "PreconditionFailed"), wantExist: true},
		{name: "already exists code", err: &smithy.GenericAPIError{Code: "AlreadyExists", Message: "object exists"}, wantExist: true},
		{name: "object already exists code", err: &smithy.GenericAPIError{Code: "ObjectAlreadyExists", Message: "object exists"}, wantExist: true},
		{name: "409 conflict", err: responseError(http.StatusConflict, "ConditionalRequestConflict"), wantErr: true},
		{name: "409 already exists is still ambiguous", err: responseError(http.StatusConflict, "AlreadyExists"), wantErr: true},
		{name: "timeout", err: context.DeadlineExceeded, wantErr: true},
		{name: "plain error", err: errors.New("connection reset"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := testStore(t, &fakeClient{
				put: func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
					return nil, tt.err
				},
			}, 1024)
			created, err := store.Create(context.Background(), "object", []byte("body"), "")
			if tt.wantExist {
				if created || err != nil {
					t.Fatalf("Create() = (%v, %v), want (false, nil)", created, err)
				}
				return
			}
			if !tt.wantErr {
				t.Fatal("test case must request existence or error")
			}
			if err == nil {
				t.Fatal("Create() error = nil, want error")
			}
			if errors.Is(err, context.DeadlineExceeded) != (tt.name == "timeout") {
				t.Errorf("wrapped timeout identity = %v", errors.Is(err, context.DeadlineExceeded))
			}
		})
	}
}

func TestPutReturnsStableURIWhenObjectAlreadyExists(t *testing.T) {
	store := testStore(t, &fakeClient{
		put: func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			return nil, responseError(http.StatusPreconditionFailed, "PreconditionFailed")
		},
	}, 1024)

	created, uri, err := store.Put(context.Background(), "reports/report.json", []byte("{}"), "application/json")
	if err != nil || created || uri != "s3://agw-test-bucket/runs/reports/report.json" {
		t.Fatalf("Put() = (%v, %q, %v), want (false, stable URI, nil)", created, uri, err)
	}
}

func TestGetReadsExactLengthAndClosesBody(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader("payload")}
	store := testStore(t, &fakeClient{
		get: func(_ context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			if aws.ToString(input.Bucket) != "agw-test-bucket" || aws.ToString(input.Key) != "runs/object" {
				t.Errorf("request target = %q/%q", aws.ToString(input.Bucket), aws.ToString(input.Key))
			}
			return &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(7)}, nil
		},
	}, 1024)

	got, err := store.Get(context.Background(), "object")
	if err != nil || string(got) != "payload" {
		t.Fatalf("Get() = (%q, %v), want payload, nil", got, err)
	}
	if !body.Closed {
		t.Error("response body was not closed")
	}
}

func TestGetRejectsShortBodyAndReportsCloseError(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	body := &trackingBody{Reader: errorReader{err: readErr}, CloseErr: closeErr}
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(4)}, nil
		},
	}, 1024)

	got, err := store.Get(context.Background(), "object")
	if got != nil {
		t.Fatalf("Get() bytes = %q, want nil", got)
	}
	if !errors.Is(err, ErrBodyRead) || !errors.Is(err, readErr) || !errors.Is(err, ErrBodyClose) || !errors.Is(err, closeErr) {
		t.Fatalf("Get() error = %v, want read and close errors", err)
	}
	if !body.Closed {
		t.Error("response body was not closed after read failure")
	}
}

func TestGetRejectsLongBodyAndClosesIt(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader("too-long")}
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(8)}, nil
		},
	}, 4)

	got, err := store.Get(context.Background(), "object")
	if got != nil || !errors.Is(err, ErrObjectTooLarge) || !body.Closed {
		t.Fatalf("Get() = (%q, %v), want bounded error and closed body", got, err)
	}
}

func TestGetRejectsOversizedContentLengthBeforeReadingBody(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader("body")}
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(GeneralMaxObjectBytes + 1)}, nil
		},
	}, GeneralMaxObjectBytes)

	got, err := store.Get(context.Background(), "object")
	if got != nil || !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("Get() = (%q, %v), want oversized-object error", got, err)
	}
	if body.ReadCalls != 0 {
		t.Fatalf("oversized response body was read %d times", body.ReadCalls)
	}
	if !body.Closed {
		t.Fatal("oversized response body was not closed")
	}
}

func TestGetRejectsUnknownLengthBeforeReadingBody(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader("body")}
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body}, nil
		},
	}, GeneralMaxObjectBytes)

	got, err := store.Get(context.Background(), "object")
	if got != nil || !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Get() = (%q, %v), want invalid-response error", got, err)
	}
	if body.ReadCalls != 0 {
		t.Fatalf("unknown-length response body was read %d times", body.ReadCalls)
	}
	if !body.Closed {
		t.Fatal("unknown-length response body was not closed")
	}
}

func TestReadExactUsesBoundedStreamingChunks(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader(strings.Repeat("x", readChunkBytes+17))}
	got, err := readExact(body, int64(readChunkBytes+17), GeneralMaxObjectBytes)
	if err != nil || len(got) != readChunkBytes+17 {
		t.Fatalf("readExact() = (%d bytes, %v), want exact payload", len(got), err)
	}
	if body.MaxReadSize > readChunkBytes {
		t.Fatalf("readExact() requested %d bytes in one read, hard chunk bound is %d", body.MaxReadSize, readChunkBytes)
	}
}

func TestGetDetectsTrailingBytesAndCloseFailures(t *testing.T) {
	closeErr := errors.New("close failed")
	body := &trackingBody{Reader: strings.NewReader("payload-extra"), CloseErr: closeErr}
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(7)}, nil
		},
	}, 1024)

	got, err := store.Get(context.Background(), "object")
	if got != nil || !errors.Is(err, ErrBodyLength) || !errors.Is(err, ErrBodyClose) || !errors.Is(err, closeErr) {
		t.Fatalf("Get() = (%q, %v), want length and close errors", got, err)
	}
}

func TestGetRejectsMissingContentLengthAndSurfacesCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	body := &trackingBody{Reader: strings.NewReader("payload"), CloseErr: closeErr}
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body}, nil
		},
	}, 1024)

	got, err := store.Get(context.Background(), "object")
	if got != nil || !errors.Is(err, ErrInvalidResponse) || !errors.Is(err, ErrBodyClose) || !errors.Is(err, closeErr) {
		t.Fatalf("Get() = (%q, %v), want invalid response and close errors", got, err)
	}
}

func TestGetClosesBodyWhenClientReturnsAnError(t *testing.T) {
	closeErr := errors.New("close failed")
	body := &trackingBody{Reader: strings.NewReader("payload"), CloseErr: closeErr}
	clientErr := errors.New("request failed")
	store := testStore(t, &fakeClient{
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: body}, clientErr
		},
	}, 1024)

	got, err := store.Get(context.Background(), "object")
	if got != nil || !errors.Is(err, clientErr) || !errors.Is(err, ErrBodyClose) || !errors.Is(err, closeErr) {
		t.Fatalf("Get() = (%q, %v), want client and close errors", got, err)
	}
	if !body.Closed {
		t.Error("response body was not closed when GetObject returned an error")
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{name: "empty bucket", cfg: Config{}, want: ErrInvalidBucket},
		{name: "uppercase bucket", cfg: Config{Bucket: "BadBucket"}, want: ErrInvalidBucket},
		{name: "ip bucket", cfg: Config{Bucket: "192.0.2.1"}, want: ErrInvalidBucket},
		{name: "reserved bucket prefix", cfg: Config{Bucket: "xn--reserved"}, want: ErrInvalidBucket},
		{name: "traversal prefix", cfg: Config{Bucket: "valid-bucket", Prefix: "runs/../x"}, want: ErrInvalidConfig},
		{name: "leading prefix slash", cfg: Config{Bucket: "valid-bucket", Prefix: "/runs"}, want: ErrInvalidConfig},
		{name: "trailing prefix slash", cfg: Config{Bucket: "valid-bucket", Prefix: "runs/"}, want: ErrInvalidConfig},
		{name: "http endpoint", cfg: Config{Bucket: "valid-bucket", Endpoint: "http://minio.example"}, want: ErrInvalidEndpoint},
		{name: "endpoint credentials", cfg: Config{Bucket: "valid-bucket", Endpoint: "https://user:pass@minio.example"}, want: ErrInvalidEndpoint},
		{name: "endpoint query", cfg: Config{Bucket: "valid-bucket", Endpoint: "https://minio.example/?x=1"}, want: ErrInvalidEndpoint},
		{name: "endpoint path", cfg: Config{Bucket: "valid-bucket", Endpoint: "https://minio.example/minio"}, want: ErrInvalidEndpoint},
		{name: "negative limit", cfg: Config{Bucket: "valid-bucket", MaxObjectBytes: -1}, want: ErrInvalidConfig},
		{name: "limit above general ceiling", cfg: Config{Bucket: "valid-bucket", MaxObjectBytes: GeneralMaxObjectBytes + 1}, want: ErrInvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWithClient(tt.cfg, &fakeClient{})
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewWithClient() error = %v, want errors.Is(..., %v)", err, tt.want)
			}
		})
	}

	store := testStore(t, &fakeClient{}, 4)
	for _, key := range []string{"", "/leading", "trailing/", "a//b", "../x", "a/./b", "a\\b", "a?b", "a#b", "a\x00b"} {
		if _, err := store.Get(context.Background(), key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Get(%q) error = %v, want ErrInvalidKey", key, err)
		}
	}

	if _, err := NewWithClient(Config{Bucket: "valid-bucket", MaxObjectBytes: GeneralMaxObjectBytes}, &fakeClient{}); err != nil {
		t.Fatalf("general limit should be accepted by object-store construction: %v", err)
	}
}

func TestNewAcceptsHTTPSMinIOEndpointAndDefaultsRegion(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	store, err := New(context.Background(), Config{
		Bucket:         "minio-bucket",
		Endpoint:       "https://minio.example.test:9443/",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if store.bucket != "minio-bucket" || store.max != GeneralMaxObjectBytes {
		t.Fatalf("normalized store = %#v", store)
	}
}

func TestDefinitiveAlreadyExistsDoesNotClassifyAmbiguousErrors(t *testing.T) {
	if definitiveAlreadyExists(context.DeadlineExceeded) {
		t.Fatal("deadline exceeded was classified as already exists")
	}
	if definitiveAlreadyExists(responseError(http.StatusConflict, "AlreadyExists")) {
		t.Fatal("409 was classified as already exists")
	}
	if definitiveAlreadyExists(responseError(http.StatusInternalServerError, "AlreadyExists")) {
		t.Fatal("500 was classified as already exists")
	}
	if definitiveAlreadyExists(errors.Join(context.DeadlineExceeded, &smithy.GenericAPIError{Code: "AlreadyExists"})) {
		t.Fatal("timeout wrapper was classified as already exists")
	}
	if !definitiveAlreadyExists(responseError(http.StatusPreconditionFailed, "Unknown")) {
		t.Fatal("412 was not classified as definitive precondition failure")
	}
}

func responseError(status int, code string) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      &smithy.GenericAPIError{Code: code, Message: "test error"},
	}
}

type trackingBody struct {
	io.Reader
	CloseErr    error
	Closed      bool
	ReadCalls   int
	MaxReadSize int
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.ReadCalls++
	if len(p) > b.MaxReadSize {
		b.MaxReadSize = len(p)
	}
	return b.Reader.Read(p)
}

func (b *trackingBody) Close() error {
	b.Closed = true
	return b.CloseErr
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestValidationErrorDoesNotEchoCredentialLikeEndpoint(t *testing.T) {
	secret := "do-not-echo-this-secret"
	_, err := NewWithClient(Config{Bucket: "valid-bucket", Endpoint: fmt.Sprintf("https://user:%s@example.test", secret)}, &fakeClient{})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked endpoint credentials: %v", err)
	}
}

func TestReadExactDoesNotMutateReturnedBody(t *testing.T) {
	body := &trackingBody{Reader: bytes.NewReader([]byte("data"))}
	got, err := readExact(body, 4, 4)
	if err != nil || !reflect.DeepEqual(got, []byte("data")) {
		t.Fatalf("readExact() = (%q, %v)", got, err)
	}
}
