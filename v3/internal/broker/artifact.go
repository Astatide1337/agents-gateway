package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

// UploadArtifact writes one immutable content-addressed object and verifies a
// pre-existing object byte-for-byte before returning its reference. It is the
// only artifact upload operation exposed by the broker core.
func (b *Broker) UploadArtifact(ctx context.Context, upload ArtifactUpload) (ArtifactReference, error) {
	var zero ArtifactReference
	if b == nil || ctx == nil || b.artifacts == nil || len(upload.Data) == 0 || int64(len(upload.Data)) > b.limits.maxArtifact {
		return zero, ErrArtifactUnavailable
	}
	mediaType, err := canonicalMediaType(upload.MediaType)
	if err != nil {
		return zero, ErrInvalidRequest
	}
	if upload.Kind != "" && !safeName(upload.Kind, 64) {
		return zero, ErrInvalidRequest
	}
	if upload.Name != "" && !safeName(upload.Name, 128) {
		return zero, ErrInvalidRequest
	}
	digest := digestBytes(upload.Data)
	key := "runs/" + b.runUID + "/broker-artifacts/" + strings.TrimPrefix(digest, "sha256:")
	created, uri, err := b.artifacts.Put(ctx, key, append([]byte(nil), upload.Data...), mediaType)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		return zero, ErrArtifactUnavailable
	}
	if !created {
		existing, getErr := b.artifacts.Get(ctx, key)
		if getErr != nil {
			return zero, ErrArtifactUnavailable
		}
		if !bytes.Equal(existing, upload.Data) {
			return zero, ErrArtifactConflict
		}
	}
	reference := ArtifactReference{URI: uri, Digest: digest, SizeBytes: int64(len(upload.Data)), MediaType: mediaType, Kind: upload.Kind, Name: upload.Name}
	if err := validateArtifactReference(reference, len(upload.Data)); err != nil {
		return zero, err
	}
	return reference, nil
}

func canonicalMediaType(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return "", ErrInvalidRequest
	}
	parsed, params, err := mime.ParseMediaType(value)
	if err != nil || len(params) != 0 || parsed == "" {
		return "", ErrInvalidRequest
	}
	return strings.ToLower(parsed), nil
}

// ArtifactHandler implements the two loopback endpoints consumed by the v3
// Codex adapter. It is intentionally a thin protocol adapter over
// UploadArtifact; storage authority remains in the injected ArtifactStore.
type ArtifactHandler struct{ broker *Broker }

func NewArtifactHandler(b *Broker) (*ArtifactHandler, error) {
	if b == nil || b.artifacts == nil {
		return nil, ErrInvalidConfig
	}
	return &ArtifactHandler{broker: b}, nil
}

func (h *ArtifactHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.broker == nil || r == nil || !isLoopbackRequest(r) || r.URL.RawQuery != "" {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/v1/artifacts/output":
		h.handleOutput(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/artifacts/create":
		h.handleCreate(w, r)
	default:
		writeBrokerHTTPError(w, http.StatusNotFound)
	}
}

func (h *ArtifactHandler) handleOutput(w http.ResponseWriter, r *http.Request) {
	mediaType, err := canonicalMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/vnd.agw.run-output+json" {
		writeBrokerHTTPError(w, http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBounded(r.Body, h.broker.limits.maxArtifact)
	if err != nil || strictjson.ValidateObject(body) != nil {
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	var envelope struct {
		Schema string          `json:"schema"`
		Result json.RawMessage `json:"result"`
	}
	if decodeStrictObject(body, &envelope) != nil || envelope.Schema != "agents-gateway.run-output.v1" || len(envelope.Result) == 0 || strictjson.Validate(envelope.Result) != nil {
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}
	reference, err := h.broker.UploadArtifact(r.Context(), ArtifactUpload{Data: body, MediaType: mediaType, Kind: "run-output", Name: "run-output.json"})
	if err != nil {
		writeBrokerHTTPError(w, artifactHTTPStatus(err))
		return
	}
	h.writeArtifactReference(w, reference)
}

type authoredDescriptor struct {
	Schema       string                       `json:"schema"`
	Title        string                       `json:"title"`
	Description  string                       `json:"description,omitempty"`
	ContentKind  artifactcatalog.ContentKind  `json:"content_kind"`
	Capabilities []artifactcatalog.Capability `json:"capabilities,omitempty"`
}

func (h *ArtifactHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	mediaType, err := canonicalMediaType(r.Header.Get("Content-Type"))
	if err != nil || !utf8.ValidString(mediaType) {
		writeBrokerHTTPError(w, http.StatusUnsupportedMediaType)
		return
	}
	metadata := r.Header.Get("X-AGW-Artifact-Metadata")
	if metadata == "" || len(metadata) > 12<<10 {
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}
	rawMetadata, err := base64.RawURLEncoding.DecodeString(metadata)
	if err != nil || len(rawMetadata) > 8<<10 || strictjson.ValidateObject(rawMetadata) != nil {
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}
	var descriptor authoredDescriptor
	if decodeStrictObject(rawMetadata, &descriptor) != nil || descriptor.Schema != "agents-gateway.artifact.v1" || !utf8.ValidString(descriptor.Title) || !utf8.ValidString(descriptor.Description) {
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}
	if _, err := artifactcatalog.NewRunOutput(artifactcatalog.PublishInput{
		ArtifactID: "agw-validation", VersionID: "agw-validation-version", Title: descriptor.Title,
		Description: descriptor.Description, URI: "artifact://validation/object", Digest: "sha256:" + strings.Repeat("0", 64),
		MediaType: mediaType, SizeBytes: 0, ContentKind: descriptor.ContentKind,
		CreatedAt: time.Now().UTC(), Security: artifactcatalog.SecurityPolicy{Allow: descriptor.Capabilities},
	}); err != nil {
		writeBrokerHTTPError(w, http.StatusBadRequest)
		return
	}
	body, err := readBounded(r.Body, h.broker.limits.maxArtifact)
	if err != nil || !utf8.Valid(body) {
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	// The human title is carried by the descriptor. The storage-facing name is
	// deliberately fixed so titles cannot become object-key or path syntax.
	reference, err := h.broker.UploadArtifact(r.Context(), ArtifactUpload{Data: body, MediaType: mediaType, Kind: string(descriptor.ContentKind), Name: "artifact"})
	if err != nil {
		writeBrokerHTTPError(w, artifactHTTPStatus(err))
		return
	}
	h.writeArtifactReference(w, reference)
}

func (h *ArtifactHandler) writeArtifactReference(w http.ResponseWriter, reference ArtifactReference) {
	hexDigest := strings.TrimPrefix(reference.Digest, "sha256:")
	// artifact:// is the stable broker-facing logical URI. The injected store
	// URI remains private to the host-side integration.
	logicalURI := "artifact://agw/" + hexDigest
	response := struct {
		ID        string `json:"id"`
		URI       string `json:"uri"`
		Digest    string `json:"digest"`
		SizeBytes int64  `json:"size_bytes"`
		MediaType string `json:"media_type"`
	}{ID: "agw-" + hexDigest[:32], URI: logicalURI, Digest: reference.Digest, SizeBytes: reference.SizeBytes, MediaType: reference.MediaType}
	body, err := json.Marshal(response)
	if err != nil || len(body) > 64<<10 {
		writeBrokerHTTPError(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

func artifactHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrArtifactConflict):
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}
