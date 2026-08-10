// Package artifact stores immutable run outputs behind tenant-scoped object
// keys. Sandboxes never receive object-store credentials.
package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

const DefaultMaxBytes int64 = 256 << 20

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ObjectClient interface {
	Put(context.Context, string, string, io.Reader, int64, string, string) error
	Delete(context.Context, string, string) error
	PresignGet(context.Context, string, string, time.Duration) (string, error)
}

type ObjectReader interface {
	Open(context.Context, string, string) (io.ReadCloser, int64, error)
}

type OpenedObject struct {
	Body      io.ReadCloser
	SizeBytes int64
}

type Config struct {
	Bucket, Prefix string
	MaxBytes       int64
	DownloadTTL    time.Duration
}
type Store struct {
	client ObjectClient
	config Config
}

type PutRequest struct {
	OrganizationID, ProjectID, RunID, Name, MediaType string
	Body                                              io.Reader
}
type Metadata struct {
	ID, Name, MediaType, Digest, ObjectKey string
	SizeBytes                              int64
}

func New(client ObjectClient, config Config) (*Store, error) {
	if client == nil || config.Bucket == "" {
		return nil, errors.New("object client and bucket are required")
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = DefaultMaxBytes
	}
	if config.DownloadTTL <= 0 {
		config.DownloadTTL = 5 * time.Minute
	}
	config.Prefix = strings.Trim(config.Prefix, "/")
	return &Store{client: client, config: config}, nil
}

func (s *Store) Put(ctx context.Context, request PutRequest) (Metadata, error) {
	for label, value := range map[string]string{"organization": request.OrganizationID, "project": request.ProjectID, "run": request.RunID, "name": request.Name} {
		if !segmentPattern.MatchString(value) {
			return Metadata{}, fmt.Errorf("invalid %s identifier", label)
		}
	}
	if request.MediaType == "" || request.Body == nil {
		return Metadata{}, errors.New("media type and body are required")
	}
	temporary, err := os.CreateTemp("", "agw-artifact-*")
	if err != nil {
		return Metadata{}, fmt.Errorf("create artifact staging file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	defer temporary.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(request.Body, s.config.MaxBytes+1))
	if err != nil {
		return Metadata{}, fmt.Errorf("stage artifact: %w", err)
	}
	if written > s.config.MaxBytes {
		return Metadata{}, errors.New("artifact exceeds size limit")
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return Metadata{}, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return Metadata{}, err
	}
	id := hex.EncodeToString(idBytes)
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	key := strings.Join(nonempty(s.config.Prefix, request.OrganizationID, request.ProjectID, request.RunID, id+"-"+request.Name), "/")
	if err := s.client.Put(ctx, s.config.Bucket, key, temporary, written, request.MediaType, digest); err != nil {
		return Metadata{}, fmt.Errorf("store artifact: %w", err)
	}
	return Metadata{ID: id, Name: request.Name, MediaType: request.MediaType, Digest: digest, ObjectKey: key, SizeBytes: written}, nil
}

// Delete removes a just-written object when the catalog commit fails. It is
// intentionally scoped through trusted metadata produced by Put; callers
// cannot use it as a general object-store deletion primitive.
func (s *Store) Delete(ctx context.Context, organizationID, projectID, runID string, metadata Metadata) error {
	for _, value := range []string{organizationID, projectID, runID} {
		if !segmentPattern.MatchString(value) {
			return errors.New("invalid artifact scope")
		}
	}
	expected := strings.Join(nonempty(s.config.Prefix, organizationID, projectID, runID), "/") + "/"
	if metadata.ObjectKey == "" || !strings.HasPrefix(metadata.ObjectKey, expected) {
		return errors.New("artifact object is outside tenant/run scope")
	}
	if err := s.client.Delete(ctx, s.config.Bucket, metadata.ObjectKey); err != nil {
		return fmt.Errorf("delete artifact object: %w", err)
	}
	return nil
}

func (s *Store) DownloadURL(ctx context.Context, organizationID, projectID, runID, objectKey string) (string, error) {
	for _, value := range []string{organizationID, projectID, runID} {
		if !segmentPattern.MatchString(value) {
			return "", errors.New("invalid artifact scope")
		}
	}
	expected := strings.Join(nonempty(s.config.Prefix, organizationID, projectID, runID), "/") + "/"
	if !strings.HasPrefix(objectKey, expected) {
		return "", errors.New("artifact object is outside tenant/run scope")
	}
	return s.client.PresignGet(ctx, s.config.Bucket, objectKey, s.config.DownloadTTL)
}

// Open returns immutable bytes only after validating the trusted catalog key
// against the requested tenant and run. Browser-controlled paths never reach
// the object client.
func (s *Store) Open(ctx context.Context, organizationID, projectID, runID, objectKey string) (OpenedObject, error) {
	for _, value := range []string{organizationID, projectID, runID} {
		if !segmentPattern.MatchString(value) {
			return OpenedObject{}, errors.New("invalid artifact scope")
		}
	}
	expected := strings.Join(nonempty(s.config.Prefix, organizationID, projectID, runID), "/") + "/"
	if !strings.HasPrefix(objectKey, expected) {
		return OpenedObject{}, errors.New("artifact object is outside tenant/run scope")
	}
	reader, ok := s.client.(ObjectReader)
	if !ok {
		return OpenedObject{}, errors.New("artifact object reader is unavailable")
	}
	body, size, err := reader.Open(ctx, s.config.Bucket, objectKey)
	if err != nil {
		return OpenedObject{}, err
	}
	if body == nil || size < 0 {
		if body != nil {
			_ = body.Close()
		}
		return OpenedObject{}, errors.New("artifact object reader returned invalid metadata")
	}
	return OpenedObject{Body: body, SizeBytes: size}, nil
}

func nonempty(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
