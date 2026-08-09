package artifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
)

const PublishMetadataHeader = "X-AGW-Artifact-Metadata"

type PublishDescriptor struct {
	Schema       string                       `json:"schema"`
	Title        string                       `json:"title"`
	Description  string                       `json:"description,omitempty"`
	ContentKind  artifactcatalog.ContentKind  `json:"content_kind"`
	Capabilities []artifactcatalog.Capability `json:"capabilities,omitempty"`
}

func (d PublishDescriptor) validate(mediaType string) error {
	if d.Schema != "agents-gateway.artifact.v1" {
		return errors.New("unsupported artifact descriptor schema")
	}
	if err := boundedText(d.Title, 200, false); err != nil {
		return errors.New("invalid artifact title")
	}
	if err := boundedText(d.Description, 4000, true); err != nil {
		return errors.New("invalid artifact description")
	}
	policy := artifactcatalog.SecurityPolicy{Allow: append([]artifactcatalog.Capability(nil), d.Capabilities...)}
	if err := policy.Validate(d.ContentKind); err != nil {
		return err
	}
	_, err := artifactcatalog.NewRunOutput(artifactcatalog.PublishInput{
		ArtifactID: strings.Repeat("a", 32), VersionID: strings.Repeat("b", 32),
		Title: d.Title, Description: d.Description, URI: "artifact://catalog/preview/preview",
		Digest: "sha256:" + strings.Repeat("0", 64), MediaType: mediaType,
		CreatedAt: time.Unix(1, 0).UTC(), ContentKind: d.ContentKind, Security: policy,
	})
	return err
}

func EncodePublishDescriptor(descriptor PublishDescriptor) (string, error) {
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	if len(raw) > 8<<10 {
		return "", errors.New("artifact metadata exceeds 8 KiB")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

type PublishHandlerConfig struct {
	OrganizationID, ProjectID, RunID string
	Path                             string
	MaxBytes                         int64
	OnStored                         func(context.Context, Metadata, string, PublishDescriptor) error
}

type PublishHandler struct {
	store                            *Store
	organizationID, projectID, runID string
	path                             string
	maxBytes                         int64
	onStored                         func(context.Context, Metadata, string, PublishDescriptor) error
}

func NewPublishHandler(store *Store, config PublishHandlerConfig) (*PublishHandler, error) {
	if store == nil {
		return nil, errors.New("artifact store is required")
	}
	for _, value := range []string{config.OrganizationID, config.ProjectID, config.RunID} {
		if !segmentPattern.MatchString(value) {
			return nil, errors.New("invalid artifact publish scope")
		}
	}
	if config.Path == "" || !strings.HasPrefix(config.Path, "/") || strings.Contains(config.Path, "..") || strings.ContainsAny(config.Path, "?#") {
		return nil, errors.New("invalid artifact publish path")
	}
	maxBytes := config.MaxBytes
	if maxBytes <= 0 || maxBytes > store.config.MaxBytes {
		maxBytes = store.config.MaxBytes
	}
	return &PublishHandler{store: store, organizationID: config.OrganizationID, projectID: config.ProjectID, runID: config.RunID, path: config.Path, maxBytes: maxBytes, onStored: config.OnStored}, nil
}

func (h *PublishHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL == nil || r.URL.Path != h.path || r.URL.RawQuery != "" {
		publishError(w, http.StatusNotFound)
		return
	}
	if len(r.Header.Values(PublishMetadataHeader)) != 1 || len(r.Header.Values("Content-Type")) != 1 {
		publishError(w, http.StatusBadRequest)
		return
	}
	metadata, err := base64.RawURLEncoding.DecodeString(r.Header.Get(PublishMetadataHeader))
	if err != nil || len(metadata) == 0 || len(metadata) > 8<<10 {
		publishError(w, http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	var descriptor PublishDescriptor
	if err := decoder.Decode(&descriptor); err != nil || ensurePublishEOF(decoder) != nil {
		publishError(w, http.StatusBadRequest)
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || len(params) != 0 {
		publishError(w, http.StatusUnsupportedMediaType)
		return
	}
	mediaType = strings.ToLower(mediaType)
	if err := descriptor.validate(mediaType); err != nil {
		publishError(w, http.StatusBadRequest)
		return
	}
	if r.ContentLength > h.maxBytes {
		publishError(w, http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes+1)
	stored, err := h.store.Put(r.Context(), PutRequest{
		OrganizationID: h.organizationID, ProjectID: h.projectID, RunID: h.runID,
		Name: publishObjectName(mediaType), MediaType: mediaType, Body: r.Body,
	})
	if err != nil {
		if strings.Contains(err.Error(), "exceeds size limit") {
			publishError(w, http.StatusRequestEntityTooLarge)
		} else {
			publishError(w, http.StatusBadRequest)
		}
		return
	}
	uri := catalogURI(stored.ID)
	if h.onStored != nil {
		if err := h.onStored(r.Context(), stored, uri, descriptor); err != nil {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			_ = h.store.Delete(cleanupContext, h.organizationID, h.projectID, h.runID, stored)
			cancel()
			publishError(w, http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(uploadResponse{ID: stored.ID, URI: uri, Digest: stored.Digest, SizeBytes: stored.SizeBytes, MediaType: stored.MediaType})
}

func publishObjectName(mediaType string) string {
	switch mediaType {
	case "text/html":
		return "artifact.html"
	case "text/markdown":
		return "artifact.md"
	case "image/svg+xml":
		return "artifact.svg"
	case "application/json":
		return "artifact.json"
	default:
		return "artifact.txt"
	}
}

func boundedText(value string, limit int, optional bool) error {
	if !utf8.ValidString(value) || len([]rune(value)) > limit || (!optional && strings.TrimSpace(value) == "") {
		return errors.New("invalid text")
	}
	for _, character := range value {
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' {
			return errors.New("invalid text")
		}
	}
	return nil
}

func ensurePublishEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func publishError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status)})
}
