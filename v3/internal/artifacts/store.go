// Package artifacts defines the immutable object-storage boundary used by the
// controller. Provider credentials and S3 implementation details stay behind
// this interface; controllers only publish content-addressed evidence.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

var (
	ErrInvalidArtifact = errors.New("invalid artifact")
	ErrObjectConflict  = errors.New("immutable object conflicts with existing content")
)

// Store implements atomic create-if-absent and exact retrieval. Put returning
// created=false MUST mean the object already existed and was not overwritten.
type Store interface {
	Put(context.Context, string, []byte, string) (created bool, uri string, err error)
	Get(context.Context, string) ([]byte, error)
}

type Writer struct {
	store Store
}

func NewWriter(store Store) (*Writer, error) {
	if store == nil {
		return nil, ErrInvalidArtifact
	}
	return &Writer{store: store}, nil
}

// SaveResolvedSpec persists the exact canonical snapshot. A racing writer is
// accepted only when the stored bytes are identical; ambiguity fails closed.
func (w *Writer) SaveResolvedSpec(ctx context.Context, runUID, digest string, body []byte) (v1alpha1.ArtifactRef, error) {
	if w == nil || w.store == nil || !safeSegment(runUID) || !canonical.ValidDigest(digest) || len(body) == 0 || len(body) > 1<<20 {
		return v1alpha1.ArtifactRef{}, ErrInvalidArtifact
	}
	computed, err := canonical.ResolvedSpecDigest(body)
	if err != nil || computed != digest {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: body does not match digest", ErrInvalidArtifact)
	}
	key := "runs/" + runUID + "/resolved/" + strings.TrimPrefix(digest, canonical.DigestPrefix) + ".json"
	created, uri, err := w.store.Put(ctx, key, append([]byte(nil), body...), "application/json")
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("put resolved spec: %w", err)
	}
	if !created {
		existing, getErr := w.store.Get(ctx, key)
		if getErr != nil {
			return v1alpha1.ArtifactRef{}, fmt.Errorf("verify existing resolved spec: %w", getErr)
		}
		if !bytes.Equal(existing, body) {
			return v1alpha1.ArtifactRef{}, ErrObjectConflict
		}
	}
	if !safeURI(uri) {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: store returned an unsafe URI", ErrInvalidArtifact)
	}
	return v1alpha1.ArtifactRef{URI: uri, Digest: digest, Kind: "resolved-spec", Name: "resolved-spec.json", MediaType: "application/json", SizeBytes: int64(len(body))}, nil
}

// LoadResolvedSpec retrieves the content-addressed snapshot by its immutable
// run/digest key and verifies that storage returned the expected bytes.
func (w *Writer) LoadResolvedSpec(ctx context.Context, runUID, digest string) ([]byte, error) {
	if w == nil || w.store == nil || !safeSegment(runUID) || !canonical.ValidDigest(digest) {
		return nil, ErrInvalidArtifact
	}
	key := "runs/" + runUID + "/resolved/" + strings.TrimPrefix(digest, canonical.DigestPrefix) + ".json"
	body, err := w.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get resolved spec: %w", err)
	}
	if len(body) == 0 || len(body) > 1<<20 {
		return nil, ErrInvalidArtifact
	}
	computed, err := canonical.ResolvedSpecDigest(body)
	if err != nil || computed != digest {
		return nil, ErrObjectConflict
	}
	return append([]byte(nil), body...), nil
}

// SaveArgoLifecycleOutput persists the controller-validated Argo handoff as a
// separate immutable object. The Workflow status parameter is transport only;
// AGW-owned verification and publication consumers receive the content-
// addressed object recorded in AgentRun status. A false Put result is safe
// only after the existing bytes have been compared exactly.
func (w *Writer) SaveArgoLifecycleOutput(ctx context.Context, runUID, specDigest string, body []byte) (v1alpha1.ArtifactRef, error) {
	if w == nil || w.store == nil || !safeSegment(runUID) || !canonical.ValidDigest(specDigest) || len(body) == 0 || len(body) > 1<<20 {
		return v1alpha1.ArtifactRef{}, ErrInvalidArtifact
	}
	sum := sha256.Sum256(body)
	digest := canonical.DigestPrefix + hex.EncodeToString(sum[:])
	key := "runs/" + runUID + "/orchestration/" + hex.EncodeToString(sum[:]) + ".json"
	created, uri, err := w.store.Put(ctx, key, append([]byte(nil), body...), "application/json")
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("put Argo lifecycle output: %w", err)
	}
	if !created {
		existing, getErr := w.store.Get(ctx, key)
		if getErr != nil {
			return v1alpha1.ArtifactRef{}, fmt.Errorf("verify existing Argo lifecycle output: %w", getErr)
		}
		if !bytes.Equal(existing, body) {
			return v1alpha1.ArtifactRef{}, ErrObjectConflict
		}
	}
	if !safeURI(uri) {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: store returned an unsafe URI", ErrInvalidArtifact)
	}
	return v1alpha1.ArtifactRef{
		URI: uri, Digest: digest, Kind: "argo-lifecycle-output", Name: "lifecycle-output.json",
		MediaType: "application/json", SizeBytes: int64(len(body)),
	}, nil
}

// LoadArgoLifecycleOutput retrieves the exact object addressed by the
// controller's persisted lifecycle-output reference and checks its digest.
func (w *Writer) LoadArgoLifecycleOutput(ctx context.Context, runUID, specDigest, digest string) ([]byte, error) {
	if w == nil || w.store == nil || !safeSegment(runUID) || !canonical.ValidDigest(specDigest) || !canonical.ValidDigest(digest) {
		return nil, ErrInvalidArtifact
	}
	key := "runs/" + runUID + "/orchestration/" + strings.TrimPrefix(digest, canonical.DigestPrefix) + ".json"
	body, err := w.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get Argo lifecycle output: %w", err)
	}
	if len(body) == 0 || len(body) > 1<<20 {
		return nil, ErrInvalidArtifact
	}
	sum := sha256.Sum256(body)
	if got := canonical.DigestPrefix + hex.EncodeToString(sum[:]); got != digest {
		return nil, ErrObjectConflict
	}
	return append([]byte(nil), body...), nil
}

// LoadContextPack loads the deterministic context evidence object without
// decoding it. The controller performs the independent contextartifact.Decode
// after this store read. URI reconstruction is required so a restart cannot
// project a guessed or logical status reference.
func (w *Writer) LoadContextPack(ctx context.Context, runUID, specDigest, baseSHA string) ([]byte, string, error) {
	if w == nil || w.store == nil {
		return nil, "", ErrInvalidArtifact
	}
	if !resolved.ValidBaseSHA(baseSHA) {
		return nil, "", ErrInvalidArtifact
	}
	body, uri, err := contextartifact.Load(ctx, w.store, runUID, specDigest)
	if err != nil {
		return nil, "", err
	}
	// Keep baseSHA in the signature so controller callers cannot accidentally
	// omit the identity they are about to validate. The body remains raw here;
	// contextartifact.Decode is the authority for its contents.
	return body, uri, nil
}

func safeSegment(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func safeURI(value string) bool {
	return (strings.HasPrefix(value, "s3://") || strings.HasPrefix(value, "https://")) &&
		!strings.ContainsAny(value, "\x00\r\n") && !strings.Contains(value, "@") && !strings.Contains(value, "?") && !strings.Contains(value, "#")
}
