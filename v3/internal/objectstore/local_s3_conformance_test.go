package objectstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// localS3 is deliberately a test-only, loopback S3 subset. It is not a
// production object store and does not attempt to validate SigV4. Its purpose
// is to exercise the real AWS SDK request/response path without credentials,
// a registry, Docker, or an external service.
type localS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	dropOnce map[string]bool
	server   *httptest.Server
}

func newLocalS3(t *testing.T) *localS3 {
	t.Helper()
	fixture := &localS3{
		objects:  make(map[string][]byte),
		dropOnce: make(map[string]bool),
	}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *localS3) handle(writer http.ResponseWriter, request *http.Request) {
	bucket, key, ok := localS3Path(request.URL.Path)
	if !ok || bucket != "agw-test-bucket" {
		s.writeError(writer, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}

	switch request.Method {
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
		if err != nil {
			s.writeError(writer, http.StatusBadRequest, "InvalidBody", "request body could not be read")
			return
		}
		if request.ContentLength >= 0 && int64(len(body)) != request.ContentLength {
			s.writeError(writer, http.StatusBadRequest, "InvalidBody", "request body length mismatch")
			return
		}

		s.mu.Lock()
		_, exists := s.objects[key]
		if request.Header.Get("If-None-Match") == "*" && exists {
			s.mu.Unlock()
			s.writeError(writer, http.StatusPreconditionFailed, "PreconditionFailed", "object already exists")
			return
		}
		s.objects[key] = append([]byte(nil), body...)
		dropResponse := s.dropOnce[key]
		if dropResponse {
			delete(s.dropOnce, key)
		}
		s.mu.Unlock()

		if dropResponse {
			// Persist the bytes, then make the client observe an ambiguous
			// transport failure rather than a successful response.
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				return
			}
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		writer.Header().Set("ETag", fmt.Sprintf("%q", md5Hex(body)))
		writer.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if key == "" && request.URL.Query().Get("list-type") == "2" {
			s.writeList(writer, request.URL.Query().Get("prefix"), request.URL.Query().Get("max-keys"))
			return
		}
		s.mu.Lock()
		body, exists := s.objects[key]
		body = append([]byte(nil), body...)
		s.mu.Unlock()
		if !exists {
			s.writeError(writer, http.StatusNotFound, "NoSuchKey", "object not found")
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(body)))
		writer.Header().Set("ETag", fmt.Sprintf("%q", md5Hex(body)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
	case http.MethodDelete:
		s.mu.Lock()
		body, exists := s.objects[key]
		if !exists {
			s.mu.Unlock()
			s.writeError(writer, http.StatusNotFound, "NoSuchKey", "object not found")
			return
		}
		if want := request.Header.Get("If-Match"); want != "" && want != fmt.Sprintf("%q", md5Hex(body)) {
			s.mu.Unlock()
			s.writeError(writer, http.StatusPreconditionFailed, "PreconditionFailed", "ETag fence did not match")
			return
		}
		delete(s.objects, key)
		s.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	default:
		s.writeError(writer, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not supported")
	}
}

type localS3ListResult struct {
	XMLName     xml.Name            `xml:"ListBucketResult"`
	Name        string              `xml:"Name"`
	Prefix      string              `xml:"Prefix"`
	KeyCount    int                 `xml:"KeyCount"`
	MaxKeys     int                 `xml:"MaxKeys"`
	IsTruncated bool                `xml:"IsTruncated"`
	Contents    []localS3ListObject `xml:"Contents"`
}

type localS3ListObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

func (s *localS3) writeList(writer http.ResponseWriter, prefix, rawMaxKeys string) {
	maxKeys := 1000
	if rawMaxKeys != "" {
		if parsed, err := strconv.Atoi(rawMaxKeys); err == nil && parsed > 0 {
			maxKeys = parsed
		}
	}
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxKeys {
		keys = keys[:maxKeys]
	}
	contents := make([]localS3ListObject, 0, len(keys))
	for _, key := range keys {
		body := s.objects[key]
		contents = append(contents, localS3ListObject{
			Key: key, LastModified: time.Now().UTC().Format(time.RFC3339),
			ETag: fmt.Sprintf("%q", md5Hex(body)), Size: int64(len(body)),
		})
	}
	s.mu.Unlock()
	result := localS3ListResult{
		Name: "agw-test-bucket", Prefix: prefix, KeyCount: len(contents),
		MaxKeys: maxKeys, IsTruncated: false, Contents: contents,
	}
	body, err := xml.Marshal(result)
	if err != nil {
		s.writeError(writer, http.StatusInternalServerError, "InternalError", "could not encode list response")
		return
	}
	writer.Header().Set("Content-Type", "application/xml")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func (s *localS3) writeError(writer http.ResponseWriter, status int, code, message string) {
	body := fmt.Sprintf("<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
	writer.Header().Set("Content-Type", "application/xml")
	writer.Header().Set("Content-Length", fmt.Sprint(len(body)))
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

func localS3Path(path string) (bucket, key string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		return "", "", false
	}
	return parts[0], strings.Join(parts[1:], "/"), true
}

func md5Hex(body []byte) string {
	// S3 ETags are not used as the content identity by AGW. This is only a
	// realistic response header for the test fixture.
	sum := md5.Sum(body) //nolint:gosec // S3-compatible ETag fixture only.
	return hex.EncodeToString(sum[:])
}

func newLocalSDKStore(t *testing.T, fixture *localS3) *Store {
	t.Helper()
	client := newLocalSDKClient(t, fixture)
	store, err := NewWithClient(Config{
		Bucket:         "agw-test-bucket",
		Region:         "us-east-1",
		Prefix:         "runs",
		Endpoint:       fixture.server.URL,
		ForcePathStyle: true,
		MaxObjectBytes: 2 << 20,
	}, client)
	if err != nil {
		t.Fatalf("NewWithClient() = %v", err)
	}
	return store
}

func newLocalSDKClient(t *testing.T, fixture *localS3) *s3.Client {
	t.Helper()
	config := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("local-test", "local-test-secret", ""),
		HTTPClient:  fixture.server.Client(),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	client := s3.NewFromConfig(config, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(fixture.server.URL)
		options.UsePathStyle = true
		options.ContinueHeaderThresholdBytes = -1
	})
	return client
}

func TestLocalSDKStoreConditionalArtifactRestartAndAmbiguity(t *testing.T) {
	fixture := newLocalS3(t)
	first := newLocalSDKStore(t, fixture)
	ctx := context.Background()

	body, err := canonical.CanonicalizeResolvedSpec(map[string]any{
		"run":     "local-s3-conformance",
		"version": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := artifacts.NewWriter(first)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := writer.SaveResolvedSpec(ctx, "run-local-s3", digest, body)
	if err != nil {
		t.Fatalf("SaveResolvedSpec() = %v", err)
	}
	if ref.Digest != digest || ref.SizeBytes != int64(len(body)) || !strings.HasPrefix(ref.URI, "s3://agw-test-bucket/") {
		t.Fatalf("unexpected content-addressed ref: %#v", ref)
	}

	// A new client/store models a process restart. The persistent fixture is
	// unchanged, and the writer must reconstruct the same immutable key.
	restarted := newLocalSDKStore(t, fixture)
	restartedWriter, err := artifacts.NewWriter(restarted)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restartedWriter.LoadResolvedSpec(ctx, "run-local-s3", digest)
	if err != nil || !bytes.Equal(loaded, body) {
		t.Fatalf("restart read = (%q, %v), want exact canonical bytes", loaded, err)
	}

	// A later conditional write cannot overwrite the content-addressed object.
	created, _, err := restarted.Put(ctx, "runs/run-local-s3/resolved/immutable.bin", body, "application/octet-stream")
	if err != nil || !created {
		t.Fatalf("first immutable write = (%t, %v), want created", created, err)
	}
	created, _, err = restarted.Put(ctx, "runs/run-local-s3/resolved/immutable.bin", []byte("different"), "application/octet-stream")
	if err != nil || created {
		t.Fatalf("conflicting immutable write = (%t, %v), want (false, nil)", created, err)
	}
	unchanged, err := restarted.Get(ctx, "runs/run-local-s3/resolved/immutable.bin")
	if err != nil || !bytes.Equal(unchanged, body) {
		t.Fatalf("conflicting write changed bytes = (%q, %v)", unchanged, err)
	}

	// The server persists before dropping the response. Store.Put must return
	// an error (unknown outcome), never success; a restarted reader can later
	// reconcile the exact bytes that may have been committed.
	ambiguousKey := "runs/run-local-s3/ambiguous/" + strings.TrimPrefix(digest, canonical.DigestPrefix) + ".bin"
	fixture.mu.Lock()
	fixture.dropOnce["runs/"+ambiguousKey] = true
	fixture.mu.Unlock()
	created, uri, err := restarted.Put(ctx, ambiguousKey, []byte("ambiguous-payload"), "application/octet-stream")
	if err == nil || created || uri != "" {
		t.Fatalf("ambiguous write = (%t, %q, %v), want error with no success/URI", created, uri, err)
	}
	reconciled, err := newLocalSDKStore(t, fixture).Get(ctx, ambiguousKey)
	if err != nil || !bytes.Equal(reconciled, []byte("ambiguous-payload")) {
		t.Fatalf("ambiguous restart reconciliation = (%q, %v)", reconciled, err)
	}
}

func TestLocalSDKStoreConditionalCreateHasOneWinner(t *testing.T) {
	fixture := newLocalS3(t)
	store := newLocalSDKStore(t, fixture)
	ctx := context.Background()
	key := "runs/local-s3-conformance/concurrent.bin"

	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 4)
	for i := 0; i < 4; i++ {
		go func(index int) {
			results <- func() result {
				created, err := store.Create(ctx, key, []byte(fmt.Sprintf("writer-%d", index)), "application/octet-stream")
				return result{created: created, err: err}
			}()
		}(i)
	}

	winners := 0
	for i := 0; i < 4; i++ {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("conditional writer error = %v", outcome.err)
		}
		if outcome.created {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("conditional winners = %d, want exactly one", winners)
	}
	if _, err := store.Get(ctx, key); err != nil {
		t.Fatalf("winner object was not readable: %v", err)
	}
}

func TestLocalSDKStoreFixtureDoesNotMaskAmbiguousError(t *testing.T) {
	fixture := newLocalS3(t)
	store := newLocalSDKStore(t, fixture)
	key := "runs/local-s3-conformance/ambiguous-error.bin"
	fixture.mu.Lock()
	fixture.dropOnce["runs/"+key] = true
	fixture.mu.Unlock()
	created, err := store.Create(context.Background(), key, []byte("payload"), "application/octet-stream")
	if err == nil || created {
		t.Fatalf("Create() = (%t, %v), want ambiguous error", created, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected context cancellation classification: %v", err)
	}
}
