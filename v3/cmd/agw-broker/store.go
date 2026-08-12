package main

import (
	"context"
	"errors"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/broker"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type storageDependencies struct {
	ArtifactStore broker.ArtifactStore
	Effects       broker.EffectLedger
	RuntimeStore  runtimeevents.Store
}

func newStorageDependencies(ctx context.Context, config objectStoreConfig, effectPrefix string) (storageDependencies, error) {
	store, err := newObjectStore(ctx, config)
	if err != nil {
		return storageDependencies{}, err
	}
	effectsLedger, err := effects.New(effectObjectStore{store: store}, effectPrefix, nil)
	if err != nil {
		return storageDependencies{}, configError{code: "effect_ledger_initialization_failed"}
	}
	return storageDependencies{
		ArtifactStore: artifactObjectStore{store: store},
		Effects:       effectsLedger,
		RuntimeStore:  runtimeObjectStore{store: store},
	}, nil
}

func newObjectStore(ctx context.Context, config objectStoreConfig) (*objectstore.Store, error) {
	if contextStillLive(ctx) != nil {
		return nil, configError{code: "object_store_context_invalid"}
	}
	accessKey, err := readBoundedFile(config.AccessKeyFile, 256, true)
	if err != nil {
		return nil, configError{code: "object_store_access_key_file_unreadable"}
	}
	defer wipeBytes(accessKey)
	secretKey, err := readBoundedFile(config.SecretKeyFile, 512, true)
	if err != nil {
		return nil, configError{code: "object_store_secret_key_file_unreadable"}
	}
	defer wipeBytes(secretKey)
	var sessionToken []byte
	if config.SessionTokenFile != "" {
		sessionToken, err = readBoundedFile(config.SessionTokenFile, 4096, true)
		if err != nil {
			return nil, configError{code: "object_store_session_token_file_unreadable"}
		}
		defer wipeBytes(sessionToken)
	}

	// The SDK receives an explicit static provider. It never receives the
	// process environment/default credential chain, metadata credentials, or a
	// web-identity token. The provider necessarily retains a process-local copy
	// for request signing; the file buffers are wiped immediately after client
	// construction.
	sdkConfig := aws.Config{
		Region:      config.Region,
		Credentials: credentials.NewStaticCredentialsProvider(string(accessKey), string(secretKey), string(sessionToken)),
	}
	s3Client := s3.NewFromConfig(sdkConfig, func(options *s3.Options) {
		options.UsePathStyle = config.ForcePathStyle
		if config.Endpoint != "" {
			options.BaseEndpoint = aws.String(config.Endpoint)
		}
	})
	store, err := objectstore.NewWithClient(objectstore.Config{
		Bucket: config.Bucket, Region: config.Region, Prefix: config.Prefix,
		Endpoint: config.Endpoint, ForcePathStyle: config.ForcePathStyle,
		MaxObjectBytes: config.MaxObjectBytes,
	}, s3Client)
	if err != nil {
		return nil, configError{code: "object_store_configuration_invalid"}
	}
	return store, nil
}

type artifactObjectStore struct{ store *objectstore.Store }

func (s artifactObjectStore) Put(ctx context.Context, key string, body []byte, contentType string) (bool, string, error) {
	if s.store == nil {
		return false, "", errors.New("artifact store unavailable")
	}
	return s.store.Put(ctx, key, body, contentType)
}

func (s artifactObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.store == nil {
		return nil, errors.New("artifact store unavailable")
	}
	return s.store.Get(ctx, key)
}

type effectObjectStore struct{ store *objectstore.Store }

func (s effectObjectStore) Create(ctx context.Context, key string, body []byte, contentType string) (bool, error) {
	if s.store == nil {
		return false, errors.New("effect store unavailable")
	}
	return s.store.Create(ctx, key, body, contentType)
}

func (s effectObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.store == nil {
		return nil, effects.ErrNotFound
	}
	body, err := s.store.Get(ctx, key)
	if err != nil && isObjectNotFound(err) {
		return nil, effects.ErrNotFound
	}
	return body, err
}

type runtimeObjectStore struct{ store *objectstore.Store }

func (s runtimeObjectStore) Put(ctx context.Context, key string, body []byte, contentType string) (bool, string, error) {
	if s.store == nil {
		return false, "", runtimeevents.ErrStoreUnavailable
	}
	return s.store.Put(ctx, key, body, contentType)
}

func (s runtimeObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.store == nil {
		return nil, runtimeevents.ErrStoreUnavailable
	}
	body, err := s.store.Get(ctx, key)
	if err != nil && isObjectNotFound(err) {
		return nil, runtimeevents.ErrNotFound
	}
	return body, err
}

func isObjectNotFound(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(apiErr.ErrorCode()))
	return code == "nosuchkey" || code == "notfound" || code == "nosuchobject"
}
