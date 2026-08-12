// Package runtimeevents implements the runtime-to-controller completion
// boundary for Agents Gateway v3.
//
// The collector keeps only counters and a bounded projection in memory. Each
// accepted JSONL frame is stored as an immutable, content-addressed object and
// referenced by a small sequence index. A final manifest identifies the whole
// stream by the SHA-256 of its canonical JSONL bytes. This lets a reader replay
// the stream in order without requiring the collector to buffer it.
package runtimeevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	schemaVersion = 1

	// MaxIndexBytes, MaxManifestBytes, and MaxCompletionBytes are intentionally
	// much smaller than a frame. They prevent a corrupt store object from
	// turning a read into an unbounded allocation.
	MaxIndexBytes      = 16 << 10
	MaxManifestBytes   = 16 << 10
	MaxCompletionBytes = 32 << 10

	// MaxStreamBytes is a storage-side safety ceiling. It is large enough for
	// normal agent runs while ensuring counters and ArtifactRef sizes remain
	// representable. It is not an in-memory buffer.
	MaxStreamBytes = uint64(1 << 40)

	maxRunUIDBytes    = 256
	maxSummaryBytes   = 128
	maxArtifactURI    = 1024
	maxArtifactKind   = 64
	maxArtifactName   = 128
	maxMediaType      = 128
	maxStreamFrames   = uint64(1 << 32)
	maxStoreKeyBytes  = 512
	maxIdentifierSize = 256
)

var (
	ErrInvalid            = errors.New("invalid runtime event boundary input")
	ErrInvalidFrame       = errors.New("invalid runtime event frame")
	ErrWrongRun           = errors.New("runtime event belongs to a different run")
	ErrSequence           = errors.New("runtime event sequence is not contiguous")
	ErrRejected           = errors.New("runtime event stream was rejected")
	ErrStreamClosed       = errors.New("runtime event stream is closed")
	ErrNoTerminal         = errors.New("process exited without a terminal runtime event")
	ErrProcessExitUnknown = errors.New("process exit outcome is unknown")
	ErrProcessExitFailed  = errors.New("process exit was non-zero for a completed run")
	ErrNotReady           = errors.New("runtime completion is not ready")
	ErrMissing            = errors.New("runtime event object is missing")
	ErrCorrupt            = errors.New("runtime event object is corrupt")
	ErrConflict           = errors.New("immutable runtime event object conflicts with existing data")
	ErrAmbiguous          = errors.New("runtime event store outcome is ambiguous")
	ErrStoreUnavailable   = errors.New("runtime event store is unavailable")
	ErrStoreInvalid       = errors.New("runtime event store returned invalid data")
	ErrNonCanonical       = errors.New("runtime event object is not canonical JSON")
	ErrStreamTooLarge     = errors.New("runtime event stream exceeds its storage limit")
)

// Store is the only persistence dependency. Put must implement create-if-
// absent semantics: created=false means an existing object was observed, not
// that a timeout was guessed to be a conflict. Implementations should return a
// stable URI for either result.
type Store interface {
	Put(context.Context, string, []byte, string) (created bool, uri string, err error)
	Get(context.Context, string) ([]byte, error)
}

// NotFoundError is optional. A Store can return ErrNotFound directly or an
// error implementing NotFound() bool. The repository preserves the distinction
// between a missing/not-ready record and a corrupt record when it is available.
var ErrNotFound = errors.New("runtime event store object not found")

type notFound interface {
	NotFound() bool
}

// ArtifactRef identifies the immutable event-stream manifest. Digest is the
// digest of the reconstructed canonical JSONL stream, not the manifest JSON.
// URI is opaque and is validated only for embedded credential material.
type ArtifactRef struct {
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// TerminalType is the only terminal vocabulary accepted by runtimeproto.
type TerminalType string

const (
	TerminalCompleted     TerminalType = "run.completed"
	TerminalFailed        TerminalType = "run.failed"
	TerminalCancelled     TerminalType = "run.cancelled"
	TerminalUnknownEffect TerminalType = "run.unknown_effect"
)

// TerminalSummary is deliberately not a copy of terminal event data. It
// contains only bounded, non-secret fields useful to a controller. In
// particular, error messages, cancellation reasons, results, and effect
// explanations are never persisted in the completion record.
type TerminalSummary struct {
	ErrorCode     string `json:"errorCode,omitempty"`
	EffectID      string `json:"effectId,omitempty"`
	OutputPresent bool   `json:"outputPresent,omitempty"`
}

// CompletionRecord is the small immutable hand-off a controller can load and
// verify. Its absence is not success; it means the process has not produced a
// durable terminal boundary (or the record is missing from storage).
type CompletionRecord struct {
	SchemaVersion int             `json:"schemaVersion"`
	RunUID        string          `json:"runUID"`
	SpecDigest    string          `json:"specDigest"`
	BaseSHA       string          `json:"baseSHA"`
	TerminalType  TerminalType    `json:"terminalType"`
	TerminalSeq   uint64          `json:"terminalSeq"`
	EventStream   ArtifactRef     `json:"eventStream"`
	Summary       TerminalSummary `json:"summary"`
}

func validBaseSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// StreamInfo is a bounded description returned after replaying an event
// stream. It contains no event payloads.
type StreamInfo struct {
	RunUID       string
	SpecDigest   string
	FrameCount   uint64
	SizeBytes    uint64
	Digest       string
	TerminalType TerminalType
	TerminalSeq  uint64
}

// Repository reads completion records and replays immutable event streams.
type Repository struct {
	store Store
}

func NewRepository(store Store) (*Repository, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	return &Repository{store: store}, nil
}

// CompletionKey returns the deterministic key used for a run/spec completion
// record. Every object written for a run is kept below its exact run prefix so
// a short-lived object-store policy can fence one broker to one run.
func CompletionKey(runUID, specDigest string) (string, error) {
	if !validRunUID(runUID) || !canonical.ValidDigest(specDigest) {
		return "", ErrInvalid
	}
	return runPrefix(runUID) + "/runtime-events/completions/" + identityDigestHex(runUID, specDigest) + ".json", nil
}

// LoadCompletion returns ErrMissing when the store can prove that no record
// exists, ErrStoreUnavailable/ErrAmbiguous for storage failures, and
// ErrCorrupt when bytes exist but fail the strict canonical record contract.
func (r *Repository) LoadCompletion(ctx context.Context, runUID, specDigest string) (CompletionRecord, error) {
	var zero CompletionRecord
	if r == nil || r.store == nil || !validRunUID(runUID) || !canonical.ValidDigest(specDigest) {
		return zero, ErrInvalid
	}
	key, err := CompletionKey(runUID, specDigest)
	if err != nil {
		return zero, err
	}
	body, err := r.get(ctx, key)
	if err != nil {
		return zero, err
	}
	var record CompletionRecord
	if err := decodeCanonicalObject(body, MaxCompletionBytes, &record); err != nil {
		return zero, fmt.Errorf("%w: %w: completion record", ErrCorrupt, err)
	}
	if err := validateCompletion(record); err != nil || record.RunUID != runUID || record.SpecDigest != specDigest {
		return zero, fmt.Errorf("%w: completion record fields", ErrCorrupt)
	}
	return record, nil
}

// WriteJSONL verifies and reconstructs the immutable stream in order. It
// holds at most one frame and one index in memory at a time. A caller that
// needs bytes can pass a bytes.Buffer; a controller should generally stream to
// a verifier instead.
func (r *Repository) WriteJSONL(ctx context.Context, runUID string, ref ArtifactRef, output io.Writer) (StreamInfo, error) {
	var zero StreamInfo
	if r == nil || r.store == nil || output == nil || !validRunUID(runUID) {
		return zero, ErrInvalid
	}
	if err := validateArtifactRef(ref); err != nil {
		return zero, fmt.Errorf("%w: event stream reference", ErrCorrupt)
	}
	body, err := r.get(ctx, streamKey(runUID, ref.Digest))
	if err != nil {
		return zero, err
	}
	var manifest streamManifest
	if err := decodeCanonicalObject(body, MaxManifestBytes, &manifest); err != nil {
		return zero, fmt.Errorf("%w: stream manifest", ErrCorrupt)
	}
	if err := validateManifest(manifest); err != nil || manifest.RunUID != runUID || manifest.StreamDigest != ref.Digest || manifest.SizeBytes != uint64(ref.SizeBytes) {
		return zero, fmt.Errorf("%w: stream manifest fields", ErrCorrupt)
	}

	streamHash := sha256.New()
	var written uint64
	var terminalSeen TerminalType
	var terminalSeq uint64
	for seq := uint64(1); seq <= manifest.FrameCount; seq++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		indexBody, err := r.get(ctx, indexKey(runUID, manifest.IdentityDigest, seq))
		if err != nil {
			return zero, err
		}
		var index frameIndex
		if err := decodeCanonicalObject(indexBody, MaxIndexBytes, &index); err != nil {
			return zero, fmt.Errorf("%w: frame index", ErrCorrupt)
		}
		if err := validateFrameIndex(index); err != nil || index.IdentityDigest != manifest.IdentityDigest || index.Seq != seq {
			return zero, fmt.Errorf("%w: frame index fields", ErrCorrupt)
		}
		frameBody, err := r.get(ctx, frameKey(runUID, index.LineDigest))
		if err != nil {
			return zero, err
		}
		if len(frameBody) == 0 || len(frameBody) > maxFrameStorageBytes() || !bytes.HasSuffix(frameBody, []byte{'\n'}) {
			return zero, fmt.Errorf("%w: frame bytes", ErrCorrupt)
		}
		if digestBytes(frameBody) != index.LineDigest {
			return zero, fmt.Errorf("%w: frame digest", ErrCorrupt)
		}
		frame, err := parseStoredLine(frameBody)
		if err != nil || frame.RunID != manifest.RunUID || frame.Seq != seq || frame.Kind != "event" {
			return zero, fmt.Errorf("%w: frame envelope", ErrCorrupt)
		}
		canonicalLine, err := canonicalLine(frame)
		if err != nil || !bytes.Equal(canonicalLine, frameBody) || uint64(len(frameBody)) != index.SizeBytes {
			return zero, fmt.Errorf("%w: non-canonical frame", ErrCorrupt)
		}
		if terminalSeen != "" {
			return zero, fmt.Errorf("%w: data after terminal", ErrCorrupt)
		}
		if frame.Terminal {
			terminalSeen = TerminalType(frame.Type)
			terminalSeq = seq
			if seq != manifest.FrameCount {
				return zero, fmt.Errorf("%w: terminal is not final frame", ErrCorrupt)
			}
		}
		if _, err := streamHash.Write(frameBody); err != nil {
			return zero, fmt.Errorf("%w: hash stream", ErrCorrupt)
		}
		written += uint64(len(frameBody))
		if err := writeAll(output, frameBody); err != nil {
			return zero, err
		}
	}
	if terminalSeen == "" || terminalSeq != manifest.TerminalSeq || terminalSeen != manifest.TerminalType || written != manifest.SizeBytes || formatDigest(streamHash.Sum(nil)) != manifest.StreamDigest {
		return zero, fmt.Errorf("%w: stream summary", ErrCorrupt)
	}
	return StreamInfo{RunUID: manifest.RunUID, SpecDigest: manifest.SpecDigest, FrameCount: manifest.FrameCount, SizeBytes: written, Digest: manifest.StreamDigest, TerminalType: terminalSeen, TerminalSeq: terminalSeq}, nil
}

func (r *Repository) get(ctx context.Context, key string) ([]byte, error) {
	if err := validateGeneratedKey(key); err != nil {
		return nil, ErrInvalid
	}
	body, err := r.store.Get(ctx, key)
	if err == nil {
		return append([]byte(nil), body...), nil
	}
	if isNotFound(err) {
		return nil, ErrMissing
	}
	if errors.Is(err, ErrAmbiguous) {
		return nil, ErrAmbiguous
	}
	return nil, ErrStoreUnavailable
}

func putImmutable(ctx context.Context, store Store, key string, body []byte, contentType string, maxBytes int) (string, error) {
	if store == nil || validateGeneratedKey(key) != nil || len(body) == 0 || len(body) > maxBytes {
		return "", ErrInvalid
	}
	created, uri, err := store.Put(ctx, key, append([]byte(nil), body...), contentType)
	if err != nil {
		// A Put error cannot prove whether the object was created. Never retry
		// an externally visible completion action from this state.
		return "", ErrAmbiguous
	}
	if !safeURI(uri) {
		return "", ErrStoreInvalid
	}
	if created {
		return uri, nil
	}
	existing, err := store.Get(ctx, key)
	if err != nil {
		return "", ErrAmbiguous
	}
	if !bytes.Equal(existing, body) {
		return "", ErrConflict
	}
	return uri, nil
}

func decodeCanonicalObject(body []byte, maxBytes int, into any) error {
	if len(body) == 0 || len(body) > maxBytes || strictjson.ValidateObject(body) != nil {
		return ErrCorrupt
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) {
		return ErrNonCanonical
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return ErrCorrupt
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrCorrupt
	}
	return nil
}

func canonicalBytes(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalid
	}
	return strictjson.Normalize(raw)
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return formatDigest(sum[:])
}

func formatDigest(sum []byte) string {
	return canonical.DigestPrefix + hex.EncodeToString(sum)
}

func digestHex(digest string) string {
	return strings.TrimPrefix(digest, canonical.DigestPrefix)
}

func identityDigestHex(runUID, specDigest string) string {
	sum := sha256.Sum256([]byte(runUID + "\x00" + specDigest))
	return hex.EncodeToString(sum[:])
}

func runPrefix(runUID string) string {
	return "runs/" + runUID
}

func streamKey(runUID, streamDigest string) string {
	return runPrefix(runUID) + "/runtime-events/streams/" + digestHex(streamDigest) + ".json"
}

func frameKey(runUID, lineDigest string) string {
	return runPrefix(runUID) + "/runtime-events/frames/" + digestHex(lineDigest) + ".jsonl"
}

func indexKey(runUID, identityDigest string, seq uint64) string {
	return fmt.Sprintf("%s/runtime-events/index/%s/%020d.json", runPrefix(runUID), identityDigest, seq)
}

func safeURI(value string) bool {
	if value == "" || len(value) > maxArtifactURI || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n@?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "s3" || parsed.Scheme == "https"
}

func validRunUID(value string) bool {
	if value == "" || len(value) > maxRunUIDBytes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "..") || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func validateGeneratedKey(key string) error {
	if key == "" || len(key) > maxStoreKeyBytes || strings.ContainsAny(key, "\x00\r\n") || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return ErrInvalid
	}
	return nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	if marker, ok := err.(notFound); ok {
		return marker.NotFound()
	}
	return false
}

func writeAll(output io.Writer, body []byte) error {
	for len(body) > 0 {
		written, err := output.Write(body)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(body) {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}
