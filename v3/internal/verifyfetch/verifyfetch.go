// Package verifyfetch contains the non-Kubernetes parts of the verify fetch
// init contract. It deliberately has no dependency on the AgentRun API. The
// command uses it to validate an S3-compatible object location, construct a
// static-credential client, and read one bounded object exactly once.
package verifyfetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	// PatchFetchMaxObjectBytes is the deliberately separate ceiling for the
	// controller-authored patch fetched by the independent verify Sandbox. It
	// is larger than the general runtime object bound because a patch is a
	// distinct, bounded verification input rather than a broker/effect object.
	PatchFetchMaxObjectBytes int64 = 1 << 30
	MaxEndpointBytes               = 512
	MaxRegionBytes                 = 64
	MaxBucketBytes                 = 63
	MaxObjectKeyBytes              = 1024
	MaxAccessKeyBytes              = 256
	MaxSecretKeyBytes              = 512
	MaxSessionTokenBytes           = 4096

	defaultHTTPTimeout = 2 * time.Minute
)

var (
	ErrInvalidConfig   = errors.New("verifyfetch: invalid object-store configuration")
	ErrInvalidLocation = errors.New("verifyfetch: invalid object location")
	ErrInvalidKey      = errors.New("verifyfetch: invalid object key")
	ErrInvalidSecret   = errors.New("verifyfetch: invalid object-store credential")
	ErrTooLarge        = errors.New("verifyfetch: object exceeds byte bound")
	ErrLengthMismatch  = errors.New("verifyfetch: object length mismatch")
	ErrUnsafeEndpoint  = errors.New("verifyfetch: unsafe object-store endpoint")
	ErrResponse        = errors.New("verifyfetch: invalid object-store response")
)

// StoreConfig is non-secret S3-compatible connection information. Endpoint
// may be empty to use the AWS S3 endpoint selected by Region. A custom
// endpoint is always HTTPS and is never allowed to carry credentials, query
// parameters, or a path that could hide a different service.
type StoreConfig struct {
	Endpoint       string
	Region         string
	Bucket         string
	ForcePathStyle bool
	// MaxObjectBytes bounds the fetched verification object. Zero selects the
	// shared general object limit; an explicit value may use the larger
	// PatchFetchMaxObjectBytes ceiling for a controller-authored patch.
	MaxObjectBytes int64
}

// Credentials is held only in the fetch process. It is never serialised,
// included in an error, or copied into a child environment.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Location is an exact S3 object key. HTTPS and presigned URLs are not a
// supported input to this package.
type Location struct {
	Bucket string
	Key    string
}

// ObjectGetter is the narrow fetch seam. It makes the digest/length contract
// testable without a live S3 service while S3Getter supplies the production
// implementation.
type ObjectGetter interface {
	Get(context.Context, string, int64) ([]byte, error)
}

// S3Getter uses explicit static credentials and a no-redirect HTTP client.
// The static provider is intentional: the fetch init container must not fall
// through to environment, shared files, web identity, or instance metadata
// credentials.
type S3Getter struct {
	client *s3.Client
	bucket string
	max    int64
}

// ParseS3URI accepts only s3://bucket/key. In particular it rejects HTTPS
// objects, query strings, fragments, userinfo, encoded paths, and traversal.
func ParseS3URI(raw string) (Location, error) {
	if raw == "" || len(raw) > MaxEndpointBytes+MaxObjectKeyBytes || strings.ContainsAny(raw, "\x00\r\n") {
		return Location{}, ErrInvalidLocation
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "s3") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return Location{}, ErrInvalidLocation
	}
	if strings.Contains(u.Host, ":") || strings.ContainsAny(u.Host, "/\\% \t") {
		return Location{}, ErrInvalidLocation
	}
	key := strings.TrimPrefix(u.EscapedPath(), "/")
	if key == "" || key != strings.TrimPrefix(u.Path, "/") || strings.Contains(key, "%") {
		return Location{}, ErrInvalidLocation
	}
	if err := validateBucket(u.Host); err != nil {
		return Location{}, err
	}
	if err := ValidateObjectKey(key); err != nil {
		return Location{}, err
	}
	return Location{Bucket: u.Host, Key: key}, nil
}

// ValidateObjectKey enforces one canonical, relative object-key grammar. It
// is intentionally narrower than S3's full key grammar so a URI cannot hide
// a second object through escaping, traversal, or ambiguous separators.
func ValidateObjectKey(key string) error {
	if key == "" || len(key) > MaxObjectKeyBytes || !utf8.ValidString(key) || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.Contains(key, "//") {
		return ErrInvalidKey
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ErrInvalidKey
		}
		for i := 0; i < len(segment); i++ {
			c := segment[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
				return ErrInvalidKey
			}
		}
	}
	return nil
}

// ValidateStoreConfig validates only non-secret values. It also rejects
// private, loopback, link-local, multicast, unspecified, and metadata hosts.
// Runtime DNS results are checked again by the transport before every dial.
func ValidateStoreConfig(config StoreConfig) error {
	if config.Bucket == "" {
		return fmt.Errorf("%w: bucket is required", ErrInvalidConfig)
	}
	if err := validateBucket(config.Bucket); err != nil {
		return err
	}
	if len(config.Region) > MaxRegionBytes || strings.TrimSpace(config.Region) != config.Region || strings.ContainsAny(config.Region, "\x00\r\n") {
		return fmt.Errorf("%w: region is invalid", ErrInvalidConfig)
	}
	for _, c := range config.Region {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("%w: region is invalid", ErrInvalidConfig)
		}
	}
	if config.MaxObjectBytes == 0 {
		config.MaxObjectBytes = objectstore.GeneralMaxObjectBytes
	}
	if config.MaxObjectBytes <= 0 || config.MaxObjectBytes > PatchFetchMaxObjectBytes {
		return fmt.Errorf("%w: object byte bound is outside the hard limit", ErrInvalidConfig)
	}
	if config.Endpoint == "" {
		return nil
	}
	if len(config.Endpoint) > MaxEndpointBytes || strings.TrimSpace(config.Endpoint) != config.Endpoint || strings.ContainsAny(config.Endpoint, "\x00\r\n") {
		return fmt.Errorf("%w: endpoint is invalid", ErrInvalidConfig)
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return fmt.Errorf("%w: custom endpoint must be an HTTPS origin without credentials, query, or path", ErrInvalidConfig)
	}
	if unsafeHost(u.Hostname()) {
		return ErrUnsafeEndpoint
	}
	if port := u.Port(); port != "" {
		value, parseErr := strconv.Atoi(port)
		if parseErr != nil || value < 1 || value > 65535 {
			return fmt.Errorf("%w: endpoint port is invalid", ErrInvalidConfig)
		}
	}
	return nil
}

func ValidateCredentials(creds Credentials) error {
	if !validSecret(creds.AccessKeyID, MaxAccessKeyBytes) || !validSecret(creds.SecretAccessKey, MaxSecretKeyBytes) || (creds.SessionToken != "" && !validSecret(creds.SessionToken, MaxSessionTokenBytes)) {
		return ErrInvalidSecret
	}
	return nil
}

func validSecret(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// NewS3Getter constructs the production getter. It never uses proxy
// environment variables and never follows redirects. The AWS SDK still signs
// the exact bucket/key request; no presigned URL is constructed.
func NewS3Getter(ctx context.Context, config StoreConfig, creds Credentials) (*S3Getter, error) {
	return newS3Getter(ctx, config, creds, nil)
}

func newS3Getter(ctx context.Context, config StoreConfig, creds Credentials, httpClient *http.Client) (*S3Getter, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ValidateStoreConfig(config); err != nil {
		return nil, err
	}
	if err := ValidateCredentials(creds); err != nil {
		return nil, err
	}
	max := config.MaxObjectBytes
	if max == 0 {
		max = objectstore.GeneralMaxObjectBytes
	}
	if httpClient == nil {
		httpClient = safeHTTPClient()
	} else if httpClient.CheckRedirect == nil {
		return nil, fmt.Errorf("%w: test or injected clients must reject redirects", ErrInvalidConfig)
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)),
		awsconfig.WithHTTPClient(httpClient),
	}
	if config.Region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(config.Region))
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("%w: SDK configuration unavailable", ErrInvalidConfig)
	}
	if awsConfig.Region == "" {
		awsConfig.Region = "us-east-1"
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.UsePathStyle = config.ForcePathStyle
		if config.Endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimSuffix(config.Endpoint, "/"))
		}
	})
	return &S3Getter{client: client, bucket: config.Bucket, max: max}, nil
}

// Get reads exactly one object. A Content-Length mismatch, a body larger than
// the configured bound, a short body, or trailing bytes is a hard failure.
func (g *S3Getter) Get(ctx context.Context, key string, expectedSize int64) ([]byte, error) {
	if g == nil || g.client == nil || ctx == nil || expectedSize < 0 || expectedSize > g.max {
		return nil, ErrInvalidConfig
	}
	if err := ValidateObjectKey(key); err != nil {
		return nil, err
	}
	output, err := g.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(g.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, ErrResponse
	}
	return readResponse(output, expectedSize, g.max)
}

func readResponse(output *s3.GetObjectOutput, expectedSize, max int64) ([]byte, error) {
	if output == nil || output.Body == nil || output.ContentLength == nil || *output.ContentLength < 0 || *output.ContentLength != expectedSize || *output.ContentLength > max || uint64(expectedSize) > uint64(maxInt()) {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		return nil, ErrLengthMismatch
	}
	body := output.Body
	data := make([]byte, int(expectedSize))
	if _, err := io.ReadFull(body, data); err != nil {
		_ = body.Close()
		return nil, ErrLengthMismatch
	}
	var extra [1]byte
	n, err := body.Read(extra[:])
	if n != 0 || err == nil || !errors.Is(err, io.EOF) {
		_ = body.Close()
		return nil, ErrLengthMismatch
	}
	if err := body.Close(); err != nil {
		return nil, ErrResponse
	}
	return data, nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func safeHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           safeDialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: transport, Timeout: defaultHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrUnsafeEndpoint
	}
	if ip := net.ParseIP(host); ip != nil {
		if unsafeIP(ip) {
			return nil, ErrUnsafeEndpoint
		}
		return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, ErrUnsafeEndpoint
	}
	for _, ip := range ips {
		if unsafeIP(ip) {
			return nil, ErrUnsafeEndpoint
		}
	}
	var last error
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
	}
	if last != nil {
		return nil, ErrUnsafeEndpoint
	}
	return nil, ErrUnsafeEndpoint
}

func validateBucket(bucket string) error {
	if len(bucket) < 3 || len(bucket) > MaxBucketBytes || bucket[0] == '-' || bucket[len(bucket)-1] == '-' || bucket[0] == '.' || bucket[len(bucket)-1] == '.' || bucket != strings.ToLower(bucket) || strings.Contains(bucket, "..") || net.ParseIP(bucket) != nil {
		return fmt.Errorf("%w: bucket is invalid", ErrInvalidConfig)
	}
	for i := 0; i < len(bucket); i++ {
		c := bucket[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
			return fmt.Errorf("%w: bucket is invalid", ErrInvalidConfig)
		}
	}
	return nil
}

func unsafeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return unsafeIP(ip)
	}
	return false
}

func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}
