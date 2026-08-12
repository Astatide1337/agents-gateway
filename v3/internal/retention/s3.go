package retention

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3API is the small SDK surface needed by the retention adapter. It makes
// ListObjectsV2 pagination and conditional DeleteObject behavior testable
// without a network or credentials.
type S3API interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

type S3Config struct {
	Bucket             string
	Prefix             string
	Region             string
	Endpoint           string
	ForcePathStyle     bool
	MaxLedgerBodyBytes int64
}

type S3Inventory struct {
	client             S3API
	bucket             string
	prefix             string
	maxLedgerBodyBytes int64
}

func NewS3Inventory(ctx context.Context, cfg S3Config) (*S3Inventory, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidInventoryConfig)
	}
	normalized, err := normalizeS3Config(cfg)
	if err != nil {
		return nil, err
	}
	options := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if normalized.Region != "" {
		options = append(options, awsconfig.WithRegion(normalized.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load retention object-store configuration: %w", err)
	}
	if awsCfg.Region == "" {
		awsCfg.Region = "us-east-1"
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = normalized.ForcePathStyle
		if normalized.Endpoint != "" {
			options.BaseEndpoint = aws.String(normalized.Endpoint)
		}
	})
	return NewS3InventoryWithClient(normalized, client)
}

func NewS3InventoryWithClient(cfg S3Config, client S3API) (*S3Inventory, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: object-store client is required", ErrInvalidInventoryConfig)
	}
	normalized, err := normalizeS3Config(cfg)
	if err != nil {
		return nil, err
	}
	return &S3Inventory{client: client, bucket: normalized.Bucket, prefix: normalized.Prefix, maxLedgerBodyBytes: normalized.MaxLedgerBodyBytes}, nil
}

func normalizeS3Config(cfg S3Config) (S3Config, error) {
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.MaxLedgerBodyBytes == 0 {
		cfg.MaxLedgerBodyBytes = effects.MaxLedgerBodyBytes
	}
	if cfg.Bucket == "" || cfg.Prefix == "" || !safeBucket(cfg.Bucket) {
		return S3Config{}, fmt.Errorf("%w: bucket and prefix are required", ErrInvalidInventoryConfig)
	}
	if err := validateObjectKey(cfg.Prefix, false); err != nil {
		return S3Config{}, fmt.Errorf("%w: object prefix: %v", ErrInvalidInventoryConfig, err)
	}
	if cfg.Region != "" && strings.ContainsAny(cfg.Region, "\x00\r\n /") {
		return S3Config{}, fmt.Errorf("%w: invalid object-store region", ErrInvalidInventoryConfig)
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return S3Config{}, fmt.Errorf("%w: endpoint must be an HTTPS origin", ErrInvalidInventoryConfig)
		}
	}
	if cfg.MaxLedgerBodyBytes <= 0 || cfg.MaxLedgerBodyBytes > effects.MaxLedgerBodyBytes {
		return S3Config{}, fmt.Errorf("%w: ledger body limit must be in [1,%d]", ErrInvalidInventoryConfig, effects.MaxLedgerBodyBytes)
	}
	return cfg, nil
}

func (s *S3Inventory) List(ctx context.Context, prefix string, limit int) ([]ObjectInfo, bool, error) {
	if s == nil || s.client == nil || ctx == nil || limit <= 0 {
		return nil, false, ErrInvalidInventoryConfig
	}
	wantPrefix := s.prefix + "/"
	if prefix != wantPrefix {
		return nil, false, fmt.Errorf("%w: list prefix must be %q", ErrInvalidInventoryConfig, wantPrefix)
	}
	maxKeys := int32(limit)
	if int64(maxKeys) != int64(limit) {
		return nil, false, fmt.Errorf("%w: list limit is too large", ErrInvalidInventoryConfig)
	}
	output, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(wantPrefix), MaxKeys: &maxKeys,
	})
	if err != nil {
		return nil, false, err
	}
	if output == nil {
		return nil, false, fmt.Errorf("%w: object-store returned an empty list response", ErrInventoryIncomplete)
	}
	if output.IsTruncated == nil {
		return nil, false, fmt.Errorf("%w: object-store omitted the truncation marker", ErrInventoryIncomplete)
	}
	if len(output.Contents) > limit {
		return nil, false, fmt.Errorf("%w: object-store exceeded the requested page limit", ErrInventoryIncomplete)
	}
	objects := make([]ObjectInfo, 0, len(output.Contents))
	for _, item := range output.Contents {
		if item.Key == nil || *item.Key == "" {
			return nil, false, fmt.Errorf("%w: object-store returned an object without a key", ErrInventoryIncomplete)
		}
		if validateObjectKey(*item.Key, false) != nil || !strings.HasPrefix(*item.Key, wantPrefix) {
			return nil, false, fmt.Errorf("%w: object-store returned a key outside the configured prefix", ErrInventoryIncomplete)
		}
		modified := timeFromPointer(item.LastModified)
		objects = append(objects, ObjectInfo{
			Key: *item.Key, ETag: aws.ToString(item.ETag), LastModified: modified,
			SizeBytes: aws.ToInt64(item.Size),
		})
	}
	complete := !aws.ToBool(output.IsTruncated) && len(objects) <= limit
	return objects, complete, nil
}

func (s *S3Inventory) Delete(ctx context.Context, key, etag string) error {
	if s == nil || s.client == nil || ctx == nil || etag == "" || !s.allowedKey(key) {
		return fmt.Errorf("%w: delete requires an exact prefixed key and ETag", ErrInvalidInventoryConfig)
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), IfMatch: aws.String(etag),
	})
	if err == nil {
		return nil
	}
	if isS3Status(err, http.StatusNotFound) || isS3Code(err, "NoSuchKey", "NoSuchObject", "NotFound") {
		return ErrObjectNotFound
	}
	if isS3Status(err, http.StatusPreconditionFailed) || isS3Code(err, "PreconditionFailed", "ConditionalRequestConflict") {
		return ErrFenceConflict
	}
	return err
}

// Get reads only bounded ledger bodies. The advertised length is checked, but
// the bytes are still read and length-verified; metadata alone is never used
// as evidence for a retention decision.
func (s *S3Inventory) Get(ctx context.Context, key string) ([]byte, error) {
	if s == nil || s.client == nil || ctx == nil || !s.allowedKey(key) {
		return nil, fmt.Errorf("%w: get requires an exact prefixed key", ErrInvalidInventoryConfig)
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		if isS3Status(err, http.StatusNotFound) || isS3Code(err, "NoSuchKey", "NoSuchObject", "NotFound") {
			return nil, ErrObjectNotFound
		}
		return nil, err
	}
	if output == nil || output.Body == nil || output.ContentLength == nil || *output.ContentLength < 0 || *output.ContentLength > s.maxLedgerBodyBytes {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		return nil, fmt.Errorf("%w: ledger response has an invalid bounded length", ErrInventoryIncomplete)
	}
	defer output.Body.Close()
	body, err := io.ReadAll(io.LimitReader(output.Body, s.maxLedgerBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != *output.ContentLength || int64(len(body)) > s.maxLedgerBodyBytes {
		return nil, fmt.Errorf("%w: ledger response body length mismatch", ErrInventoryIncomplete)
	}
	return body, nil
}

// Create is a conditional create used only for the permanent tombstone. A
// timeout or 409 remains an error because it cannot prove whether the fence
// exists; the pair must not be deleted in that case.
func (s *S3Inventory) Create(ctx context.Context, key string, body []byte, contentType string) (bool, error) {
	if s == nil || s.client == nil || ctx == nil || !s.allowedKey(key) || len(body) == 0 || int64(len(body)) > s.maxLedgerBodyBytes || contentType != "application/json" {
		return false, fmt.Errorf("%w: invalid bounded tombstone create", ErrInvalidInventoryConfig)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String(contentType), IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		return true, nil
	}
	if isS3Status(err, http.StatusPreconditionFailed) || isS3Code(err, "PreconditionFailed", "AlreadyExists", "ObjectAlreadyExists", "EntityAlreadyExists") {
		return false, nil
	}
	return false, err
}

func isS3Status(err error, want int) bool {
	var statusErr interface{ HTTPStatusCode() int }
	return errors.As(err, &statusErr) && statusErr.HTTPStatusCode() == want
}

func isS3Code(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	got := strings.ToLower(apiErr.ErrorCode())
	for _, code := range codes {
		if got == strings.ToLower(code) {
			return true
		}
	}
	return false
}

func (s *S3Inventory) allowedKey(key string) bool {
	return strings.HasPrefix(key, s.prefix+"/") && validateObjectKey(key, false) == nil
}

func safeBucket(value string) bool {
	if len(value) < 3 || len(value) > 63 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func timeFromPointer(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}
