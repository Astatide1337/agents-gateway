// Package objectstore provides the S3-compatible storage adapter used by the
// v3 effect ledger and artifact writer.
//
// The adapter deliberately exposes only immutable, create-if-absent writes and
// bounded reads.  It does not log, inspect, or otherwise handle credentials;
// the AWS SDK resolves those through its normal credential provider chain.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const (
	// GeneralMaxObjectBytes is the shared maximum for general runtime objects
	// such as artifacts, effects, and runtime events. Zero-valued configuration
	// selects this bound, and callers must not accept a larger value.
	GeneralMaxObjectBytes int64 = 64 << 20

	maxKeyBytes    = 1024
	maxBucketBytes = 63
	maxRegionBytes = 64
	readChunkBytes = 32 << 10
)

var (
	ErrInvalidConfig      = errors.New("invalid object-store configuration")
	ErrInvalidKey         = errors.New("invalid object-store key")
	ErrInvalidBucket      = errors.New("invalid object-store bucket")
	ErrInvalidEndpoint    = errors.New("invalid object-store endpoint")
	ErrInvalidContentType = errors.New("invalid object-store content type")
	ErrObjectTooLarge     = errors.New("object exceeds configured size limit")
	ErrInvalidResponse    = errors.New("invalid object-store response")
	ErrBodyRead           = errors.New("object-store response body read failed")
	ErrBodyClose          = errors.New("object-store response body close failed")
	ErrBodyLength         = errors.New("object-store response body length mismatch")
)

// Client is the narrow part of the AWS S3 client needed by Store.  Keeping it
// here makes the adapter testable without a live object-store service while
// still allowing *s3.Client to be used in production.
type Client interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Config configures an S3-compatible bucket.  Region may be omitted when the
// AWS SDK can resolve it from its normal environment/profile/role chain; if no
// region is available, us-east-1 is used, which is the conventional default
// for S3-compatible services such as MinIO.
//
// Endpoint is optional.  When set it must be an HTTPS origin without a path,
// credentials, query string, or fragment.  ForcePathStyle is useful for
// MinIO and other S3-compatible services whose virtual-hosted bucket endpoint
// is not available.
type Config struct {
	Bucket         string
	Region         string
	Prefix         string
	Endpoint       string
	ForcePathStyle bool

	// MaxObjectBytes bounds both writes and reads. Zero selects
	// GeneralMaxObjectBytes. Values above GeneralMaxObjectBytes are rejected.
	// A negative value is rejected.
	MaxObjectBytes int64
}

// Store is an immutable, bounded S3 object store.
type Store struct {
	client Client
	bucket string
	prefix string
	max    int64
}

// New constructs a Store using the AWS SDK's default credential and region
// providers.  It never prints or logs configuration values or credentials.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidConfig)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}

	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if normalized.region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(normalized.region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if awsCfg.Region == "" {
		awsCfg.Region = "us-east-1"
	}

	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = normalized.forcePathStyle
		if normalized.endpoint != "" {
			options.BaseEndpoint = aws.String(normalized.endpoint)
		}
	})
	return newStore(normalized, client)
}

// NewWithClient constructs a Store around an injected S3 client.  It is
// intended for unit tests and for callers that own SDK client construction;
// endpoint and credential configuration still undergoes the same validation.
func NewWithClient(cfg Config, client Client) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: client is required", ErrInvalidConfig)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newStore(normalized, client)
}

func newStore(cfg normalizedConfig, client Client) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: client is required", ErrInvalidConfig)
	}
	return &Store{
		client: client,
		bucket: cfg.bucket,
		prefix: cfg.prefix,
		max:    cfg.maxObjectBytes,
	}, nil
}

// Create atomically creates key if it does not already exist.  A false,
// nil result means the service returned a definitive precondition/already
// exists response.  Network failures, timeouts, 409 conflicts, and all other
// ambiguous failures are returned as errors and never translated to false,
// nil.
func (s *Store) Create(ctx context.Context, key string, body []byte, contentType string) (bool, error) {
	_, created, err := s.put(ctx, key, body, contentType)
	return created, err
}

// Put implements the immutable artifact Store contract.  It returns the
// stable s3:// URI even when the object already existed.
func (s *Store) Put(ctx context.Context, key string, body []byte, contentType string) (bool, string, error) {
	fullKey, created, err := s.put(ctx, key, body, contentType)
	if err != nil {
		return false, "", err
	}
	return created, objectURI(s.bucket, fullKey), nil
}

// URI returns the stable provider URI for a validated object key without
// performing a network request. Controllers use it when projecting an object
// that was created by a separate per-run process.
func (s *Store) URI(key string) (string, error) {
	fullKey, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	return objectURI(s.bucket, fullKey), nil
}

func (s *Store) put(ctx context.Context, key string, body []byte, contentType string) (string, bool, error) {
	fullKey, err := s.objectKey(key)
	if err != nil {
		return "", false, err
	}
	if ctx == nil {
		return "", false, fmt.Errorf("%w: context is required", ErrInvalidConfig)
	}
	if int64(len(body)) > s.max {
		return "", false, fmt.Errorf("%w: %d bytes exceeds %d-byte limit", ErrObjectTooLarge, len(body), s.max)
	}
	if err := validateContentType(contentType); err != nil {
		return "", false, err
	}

	input := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(fullKey),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		IfNoneMatch:   aws.String("*"),
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		if definitiveAlreadyExists(err) {
			return fullKey, false, nil
		}
		return "", false, fmt.Errorf("put object %q: %w", fullKey, err)
	}
	return fullKey, true, nil
}

// Get retrieves one object and verifies that the response body contains
// exactly the advertised Content-Length bytes.  The body is always closed;
// close failures are surfaced instead of being silently discarded.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	fullKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidConfig)
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		if output != nil && output.Body != nil {
			closeErr := output.Body.Close()
			if closeErr != nil {
				return nil, errors.Join(
					fmt.Errorf("get object %q: %w", fullKey, err),
					ErrBodyClose,
					closeErr,
				)
			}
		}
		return nil, fmt.Errorf("get object %q: %w", fullKey, err)
	}
	if output == nil || output.Body == nil || output.ContentLength == nil {
		if output != nil && output.Body != nil {
			closeErr := output.Body.Close()
			if closeErr != nil {
				return nil, errors.Join(
					fmt.Errorf("%w: response for %q lacks a body or Content-Length", ErrInvalidResponse, fullKey),
					ErrBodyClose,
					closeErr,
				)
			}
		}
		return nil, fmt.Errorf("%w: response for %q lacks a body or Content-Length", ErrInvalidResponse, fullKey)
	}
	return readExact(output.Body, *output.ContentLength, s.max)
}

func (s *Store) objectKey(key string) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("%w: store is not initialized", ErrInvalidConfig)
	}
	if err := validateKey(key, false); err != nil {
		return "", err
	}
	fullKey := key
	if s.prefix != "" {
		fullKey = s.prefix + "/" + key
	}
	if err := validateKey(fullKey, false); err != nil {
		return "", fmt.Errorf("%w: prefixed key is invalid", err)
	}
	return fullKey, nil
}

type normalizedConfig struct {
	bucket         string
	region         string
	prefix         string
	endpoint       string
	forcePathStyle bool
	maxObjectBytes int64
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	if err := validateBucket(cfg.Bucket); err != nil {
		return normalizedConfig{}, err
	}
	region := strings.TrimSpace(cfg.Region)
	if err := validateRegion(region); err != nil {
		return normalizedConfig{}, err
	}
	prefix := cfg.Prefix
	if prefix != strings.Trim(prefix, "/") {
		return normalizedConfig{}, fmt.Errorf("%w: prefix must not have leading or trailing slashes", ErrInvalidConfig)
	}
	if prefix != "" {
		if err := validateKey(prefix, false); err != nil {
			return normalizedConfig{}, fmt.Errorf("%w: prefix: %v", ErrInvalidConfig, err)
		}
	}
	endpoint, err := validateEndpoint(cfg.Endpoint)
	if err != nil {
		return normalizedConfig{}, err
	}
	maxObjectBytes := cfg.MaxObjectBytes
	if maxObjectBytes == 0 {
		maxObjectBytes = GeneralMaxObjectBytes
	}
	if maxObjectBytes < 0 {
		return normalizedConfig{}, fmt.Errorf("%w: MaxObjectBytes must not be negative", ErrInvalidConfig)
	}
	if maxObjectBytes > GeneralMaxObjectBytes {
		return normalizedConfig{}, fmt.Errorf("%w: MaxObjectBytes must not exceed %d bytes", ErrInvalidConfig, GeneralMaxObjectBytes)
	}
	return normalizedConfig{
		bucket:         cfg.Bucket,
		region:         region,
		prefix:         prefix,
		endpoint:       endpoint,
		forcePathStyle: cfg.ForcePathStyle,
		maxObjectBytes: maxObjectBytes,
	}, nil
}

func validateBucket(bucket string) error {
	if len(bucket) < 3 || len(bucket) > maxBucketBytes || bucket[0] == '-' || bucket[len(bucket)-1] == '-' || bucket[0] == '.' || bucket[len(bucket)-1] == '.' {
		return fmt.Errorf("%w: bucket must be 3-63 characters and start/end with a letter or number", ErrInvalidBucket)
	}
	if bucket != strings.ToLower(bucket) || strings.Contains(bucket, "..") || net.ParseIP(bucket) != nil || hasReservedBucketSuffix(bucket) {
		return fmt.Errorf("%w: bucket must be lowercase, non-IP, and contain no adjacent periods", ErrInvalidBucket)
	}
	for i := 0; i < len(bucket); i++ {
		c := bucket[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
			return fmt.Errorf("%w: bucket contains unsupported characters", ErrInvalidBucket)
		}
	}
	return nil
}

func hasReservedBucketSuffix(bucket string) bool {
	for _, suffix := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(bucket, suffix) {
			return true
		}
	}
	return strings.HasPrefix(bucket, "xn--")
}

func validateRegion(region string) error {
	if region == "" {
		return nil
	}
	if len(region) > maxRegionBytes {
		return fmt.Errorf("%w: region is too long", ErrInvalidConfig)
	}
	for i := 0; i < len(region); i++ {
		c := region[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("%w: region contains unsupported characters", ErrInvalidConfig)
		}
	}
	return nil
}

func validateEndpoint(endpoint string) (string, error) {
	if endpoint == "" {
		return "", nil
	}
	if strings.TrimSpace(endpoint) != endpoint || hasControl(endpoint) {
		return "", fmt.Errorf("%w: endpoint contains whitespace or control characters", ErrInvalidEndpoint)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%w: endpoint must be an absolute URL with a host", ErrInvalidEndpoint)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("%w: custom endpoints must use HTTPS", ErrInvalidEndpoint)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: endpoint must not contain credentials, query parameters, or fragments", ErrInvalidEndpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%w: endpoint paths are not supported", ErrInvalidEndpoint)
	}
	host := u.Hostname()
	if host == "" || strings.HasSuffix(host, ".") || hasControl(host) {
		return "", fmt.Errorf("%w: endpoint host is invalid", ErrInvalidEndpoint)
	}
	if port := u.Port(); port != "" {
		value, parseErr := strconv.Atoi(port)
		if parseErr != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("%w: endpoint port is invalid", ErrInvalidEndpoint)
		}
	}
	if net.ParseIP(host) == nil {
		for i := 0; i < len(host); i++ {
			c := host[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
				return "", fmt.Errorf("%w: endpoint host contains unsupported characters", ErrInvalidEndpoint)
			}
		}
	}
	return strings.TrimSuffix(endpoint, "/"), nil
}

func validateKey(key string, allowEmpty bool) error {
	if key == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%w: key is required", ErrInvalidKey)
	}
	if len(key) > maxKeyBytes || !utf8.ValidString(key) || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.Contains(key, "//") {
		return fmt.Errorf("%w: key must be a relative path of at most %d bytes", ErrInvalidKey, maxKeyBytes)
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: key contains an empty or traversal segment", ErrInvalidKey)
		}
		for i := 0; i < len(segment); i++ {
			c := segment[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
				return fmt.Errorf("%w: key contains unsupported characters", ErrInvalidKey)
			}
		}
	}
	return nil
}

func validateContentType(contentType string) error {
	if len(contentType) > 1024 || hasControl(contentType) {
		return fmt.Errorf("%w: content type is too long or contains control characters", ErrInvalidContentType)
	}
	return nil
}

func hasControl(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}

func objectURI(bucket, key string) string {
	return "s3://" + bucket + "/" + key
}

func definitiveAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) {
		switch statusErr.HTTPStatusCode() {
		case http.StatusPreconditionFailed:
			return true
		default:
			// A known HTTP status that is not 412 is not a definitive
			// conditional-create result. In particular, 409 is explicitly
			// ambiguous and must remain an error.
			return false
		}
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(apiErr.ErrorCode())) {
	case "preconditionfailed", "alreadyexists", "objectalreadyexists", "entityalreadyexists":
		return true
	default:
		return false
	}
}

func readExact(body io.ReadCloser, length, max int64) ([]byte, error) {
	if body == nil {
		return nil, fmt.Errorf("%w: response body is nil", ErrInvalidResponse)
	}
	if max <= 0 || max > GeneralMaxObjectBytes {
		return closeAfterResponseError(body, fmt.Errorf("%w: read limit is outside the hard ceiling", ErrInvalidConfig))
	}
	if length < 0 {
		return closeAfterResponseError(body, fmt.Errorf("%w: negative Content-Length", ErrInvalidResponse))
	}
	if length > max || length > GeneralMaxObjectBytes || uint64(length) > uint64(maxInt()) {
		return closeAfterResponseError(body, fmt.Errorf("%w: Content-Length %d exceeds %d-byte limit", ErrObjectTooLarge, length, max))
	}

	// Do not preallocate from Content-Length. Although the header is bounded,
	// it is response-controlled and may describe a body that never arrives.
	// Reading through fixed-size chunks keeps allocation proportional to bytes
	// actually received and ensures an invalid response cannot trigger one
	// header-sized allocation before validation.
	var data bytes.Buffer
	chunk := make([]byte, readChunkBytes)
	remaining := length
	var readErr error
	for remaining > 0 {
		readSize := int64(len(chunk))
		if readSize > remaining {
			readSize = remaining
		}
		n, err := body.Read(chunk[:int(readSize)])
		if n < 0 || n > int(readSize) {
			readErr = fmt.Errorf("invalid read count %d", n)
			break
		}
		if n > 0 {
			_, _ = data.Write(chunk[:n])
			remaining -= int64(n)
		}
		if remaining == 0 {
			// Match io.ReadFull: a read that fills the requested bytes succeeds
			// even if the reader also reports an error. The trailing-byte read
			// below still validates the complete response.
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if data.Len() == 0 {
					readErr = io.EOF
				} else {
					readErr = io.ErrUnexpectedEOF
				}
			} else {
				readErr = err
			}
			break
		}
		if n == 0 {
			readErr = io.ErrNoProgress
			break
		}
	}
	if readErr == nil {
		var extra [1]byte
		n, err := body.Read(extra[:])
		switch {
		case n > 0:
			readErr = ErrBodyLength
		case err == nil:
			readErr = io.ErrNoProgress
		case !errors.Is(err, io.EOF):
			readErr = err
		}
	}
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		var combined error
		if readErr != nil {
			if errors.Is(readErr, ErrBodyLength) {
				combined = ErrBodyLength
			} else {
				combined = errors.Join(ErrBodyRead, readErr)
			}
		}
		if closeErr != nil {
			combined = errors.Join(combined, ErrBodyClose, closeErr)
		}
		return nil, combined
	}
	return data.Bytes(), nil
}

func closeAfterResponseError(body io.ReadCloser, err error) ([]byte, error) {
	if closeErr := body.Close(); closeErr != nil {
		return nil, errors.Join(err, ErrBodyClose, closeErr)
	}
	return nil, err
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
