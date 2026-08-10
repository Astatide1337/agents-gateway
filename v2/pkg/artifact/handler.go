package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
)

// UploadHandlerConfig fixes every scope component and accepts one output at
// one exact URL. The request cannot choose a tenant, run, object name, or
// destination through URL parameters or headers.
type UploadHandlerConfig struct {
	OrganizationID    string
	ProjectID         string
	RunID             string
	Name              string
	Path              string
	Method            string
	MaxBytes          int64
	AllowedMediaTypes []string
	OnStored          func(context.Context, Metadata, string) error
}

// UploadHandler is safe to mount behind a per-run broker. Its response is a
// workflow.ArtifactRef-compatible JSON object with an opaque catalog URI.
type UploadHandler struct {
	store             *Store
	organizationID    string
	projectID         string
	runID             string
	name              string
	path              string
	method            string
	maxBytes          int64
	allowedMediaTypes map[string]struct{}
	onStored          func(context.Context, Metadata, string) error
}

func NewUploadHandler(store *Store, config UploadHandlerConfig) (*UploadHandler, error) {
	if store == nil {
		return nil, errors.New("artifact store is required")
	}
	for label, value := range map[string]string{"organization": config.OrganizationID, "project": config.ProjectID, "run": config.RunID, "name": config.Name} {
		if !segmentPattern.MatchString(value) {
			return nil, fmt.Errorf("invalid %s identifier", label)
		}
	}
	if config.Path == "" || !strings.HasPrefix(config.Path, "/") || strings.ContainsAny(config.Path, "?#") || strings.Contains(config.Path, "//") || strings.Contains(config.Path, "..") {
		return nil, errors.New("upload path must be one absolute path without query or traversal")
	}
	method := config.Method
	if method == "" {
		method = http.MethodPut
	}
	if method != http.MethodPut && method != http.MethodPost {
		return nil, errors.New("upload method must be PUT or POST")
	}
	maxBytes := config.MaxBytes
	if maxBytes <= 0 || (store.config.MaxBytes > 0 && maxBytes > store.config.MaxBytes) {
		maxBytes = store.config.MaxBytes
	}
	if maxBytes <= 0 {
		return nil, errors.New("upload size limit must be positive")
	}
	if len(config.AllowedMediaTypes) == 0 {
		return nil, errors.New("at least one allowed media type is required")
	}
	allowed := make(map[string]struct{}, len(config.AllowedMediaTypes))
	for _, value := range config.AllowedMediaTypes {
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil || mediaType == "" || strings.ContainsAny(mediaType, "\r\n") {
			return nil, errors.New("invalid allowed media type")
		}
		allowed[strings.ToLower(mediaType)] = struct{}{}
	}
	return &UploadHandler{store: store, organizationID: config.OrganizationID, projectID: config.ProjectID, runID: config.RunID, name: config.Name, path: config.Path, method: method, maxBytes: maxBytes, allowedMediaTypes: allowed, onStored: config.OnStored}, nil
}

func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != h.method || r.URL == nil || r.URL.Path != h.path || r.URL.RawQuery != "" || r.URL.Fragment != "" {
		h.writeError(w, http.StatusNotFound)
		return
	}
	if len(r.Header.Values("Content-Type")) != 1 {
		h.writeError(w, http.StatusUnsupportedMediaType)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		h.writeError(w, http.StatusUnsupportedMediaType)
		return
	}
	mediaType = strings.ToLower(mediaType)
	if _, ok := h.allowedMediaTypes[mediaType]; !ok {
		h.writeError(w, http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > h.maxBytes {
		h.writeError(w, http.StatusRequestEntityTooLarge)
		return
	}
	// Store also bounds the body. The extra byte makes an unknown-length body
	// fail closed without buffering it in the handler.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes+1)
	metadata, err := h.store.Put(r.Context(), PutRequest{OrganizationID: h.organizationID, ProjectID: h.projectID, RunID: h.runID, Name: h.name, MediaType: mediaType, Body: r.Body})
	if err != nil {
		if strings.Contains(err.Error(), "exceeds size limit") {
			h.writeError(w, http.StatusRequestEntityTooLarge)
			return
		}
		h.writeError(w, http.StatusBadRequest)
		return
	}
	uri := catalogURI(metadata.ID)
	if h.onStored != nil {
		if err := h.onStored(r.Context(), metadata, uri); err != nil {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			_ = h.store.Delete(cleanupContext, h.organizationID, h.projectID, h.runID, metadata)
			cancel()
			h.writeError(w, http.StatusServiceUnavailable)
			return
		}
	}
	response := uploadResponse{ID: metadata.ID, URI: uri, Digest: metadata.Digest, SizeBytes: metadata.SizeBytes, MediaType: metadata.MediaType}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

type uploadResponse struct {
	ID        string `json:"id"`
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

func catalogURI(id string) string {
	return "artifact://catalog/" + id + "/" + id
}

func (h *UploadHandler) writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status)})
}
