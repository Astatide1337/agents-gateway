package artifact

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Options struct {
	Region, Endpoint, AccessKeyID, SecretAccessKey, SessionToken string
	PathStyle                                                    bool
}
type S3Client struct {
	client  *s3.Client
	presign *s3.PresignClient
}

func NewS3Client(ctx context.Context, options S3Options) (*S3Client, error) {
	if options.Region == "" {
		return nil, errors.New("S3 region is required")
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(options.Region)}
	if options.AccessKeyID != "" || options.SecretAccessKey != "" {
		if options.AccessKeyID == "" || options.SecretAccessKey == "" {
			return nil, errors.New("both S3 access key ID and secret are required")
		}
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(options.AccessKeyID, options.SecretAccessKey, options.SessionToken)))
	}
	config, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, err
	}
	var endpoint *string
	if options.Endpoint != "" {
		parsed, err := url.Parse(options.Endpoint)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("invalid S3 endpoint")
		}
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
			return nil, errors.New("S3 endpoint must use HTTPS (HTTP loopback is allowed for tests)")
		}
		normalized := strings.TrimRight(parsed.String(), "/")
		endpoint = &normalized
	}
	client := s3.NewFromConfig(config, func(o *s3.Options) { o.UsePathStyle = options.PathStyle; o.BaseEndpoint = endpoint })
	return &S3Client{client: client, presign: s3.NewPresignClient(client)}, nil
}

func (c *S3Client) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, mediaType, digest string) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(mediaType), Metadata: map[string]string{"agw-sha256": digest}})
	return err
}

func (c *S3Client) Delete(ctx context.Context, bucket, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	return err
}

func (c *S3Client) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	result, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}, func(options *s3.PresignOptions) { options.Expires = ttl })
	if err != nil {
		return "", err
	}
	return result.URL, nil
}

func (c *S3Client) Open(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	result, err := c.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, 0, err
	}
	if result.Body == nil || result.ContentLength == nil || *result.ContentLength < 0 {
		if result.Body != nil {
			_ = result.Body.Close()
		}
		return nil, 0, errors.New("S3 returned invalid artifact metadata")
	}
	return result.Body, *result.ContentLength, nil
}
