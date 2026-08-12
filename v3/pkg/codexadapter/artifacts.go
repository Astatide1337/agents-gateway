package codexadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/pkg/artifactcatalog"
	"golang.org/x/sys/unix"
)

const (
	generatedArtifactDirectory       = ".agw/artifacts"
	maxGeneratedArtifacts            = 16
	maxDescriptorBytes         int64 = 64 << 10
	maxGeneratedBytes          int64 = 8 << 20
)

var errArtifactFileBounds = errors.New("artifact source is not a bounded regular file")

type authoredArtifactDescriptor struct {
	Schema       string                       `json:"schema"`
	Title        string                       `json:"title"`
	Description  string                       `json:"description,omitempty"`
	ContentKind  artifactcatalog.ContentKind  `json:"content_kind"`
	MediaType    string                       `json:"media_type"`
	Source       string                       `json:"source"`
	Capabilities []artifactcatalog.Capability `json:"capabilities,omitempty"`
}

type authoredArtifact struct {
	descriptor publishDescriptor
	mediaType  string
	body       []byte
}

// RuntimeArtifactUpload is the common artifact handoff used by all v3
// runtimes. Metadata is the bounded, raw-URL-base64 descriptor expected by
// the broker's create endpoint; provider credentials never appear in it.
type RuntimeArtifactUpload struct {
	MediaType string
	Metadata  string
	Body      []byte
}

// CollectAuthoredArtifactUploads validates the model-authored .agw/artifacts
// catalog and returns bounded immutable upload payloads. Keeping this parser
// here makes the Codex and Claude runtimes use exactly the same artifact
// contract and path-safety checks.
func CollectAuthoredArtifactUploads(workspace string) ([]RuntimeArtifactUpload, error) {
	authored, err := collectAuthoredArtifacts(workspace)
	if err != nil {
		return nil, err
	}
	result := make([]RuntimeArtifactUpload, 0, len(authored))
	for _, item := range authored {
		metadata, err := encodePublishDescriptor(item.descriptor, item.mediaType)
		if err != nil {
			return nil, errors.New("encode authored artifact metadata")
		}
		result = append(result, RuntimeArtifactUpload{
			MediaType: item.mediaType,
			Metadata:  metadata,
			Body:      append([]byte(nil), item.body...),
		})
	}
	return result, nil
}

// UploadAuthoredArtifact uploads one item returned by
// CollectAuthoredArtifactUploads to a validated loopback broker endpoint.
func UploadAuthoredArtifact(ctx context.Context, endpoint string, upload RuntimeArtifactUpload) (artifactcatalog.ArtifactRef, error) {
	if _, err := validateLoopbackEndpoint(endpoint, ArtifactCreatePath); err != nil {
		return artifactcatalog.ArtifactRef{}, err
	}
	if upload.MediaType == "" || upload.Metadata == "" || len(upload.Body) == 0 {
		return artifactcatalog.ArtifactRef{}, errors.New("authored artifact upload is incomplete")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(upload.Body))
	if err != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("create authored artifact request")
	}
	request.Header.Set("Content-Type", upload.MediaType)
	request.Header.Set(PublishMetadataHeader, upload.Metadata)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("upload authored artifact")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return artifactcatalog.ArtifactRef{}, fmt.Errorf("authored artifact upload returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var reference artifactcatalog.ArtifactRef
	if err := decoder.Decode(&reference); err != nil || ensureArtifactJSONEOF(decoder) != nil || reference.Validate() != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("invalid authored artifact reference")
	}
	return reference, nil
}

// UploadRunOutput publishes the bounded runtime summary through the common
// v3 run-output endpoint.
func UploadRunOutput(ctx context.Context, endpoint string, result map[string]any) (artifactcatalog.ArtifactRef, error) {
	return uploadRunOutput(ctx, endpoint, result)
}

func collectAuthoredArtifacts(workspace string) ([]authoredArtifact, error) {
	directory := filepath.Join(workspace, filepath.FromSlash(generatedArtifactDirectory))
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("read authored artifact descriptors")
	}
	if len(entries) > maxGeneratedArtifacts {
		return nil, fmt.Errorf("authored artifact count exceeds %d", maxGeneratedArtifacts)
	}
	var result []authoredArtifact
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".json" || !safeDescriptorName(entry.Name()) {
			return nil, errors.New("artifact descriptor directory contains an unsupported entry")
		}
		descriptorPath := generatedArtifactDirectory + "/" + entry.Name()
		raw, err := readWorkspaceFile(workspace, descriptorPath, maxDescriptorBytes)
		if err != nil {
			return nil, fmt.Errorf("read authored artifact descriptor: %w", classifyWorkspaceReadError(err))
		}
		var descriptor authoredArtifactDescriptor
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&descriptor); err != nil || ensureArtifactJSONEOF(decoder) != nil {
			return nil, errors.New("decode authored artifact descriptor")
		}
		if descriptor.Schema != "agents-gateway.artifact.v1" || !safeRelativeSource(descriptor.Source) {
			return nil, errors.New("invalid authored artifact descriptor")
		}
		mediaType, params, err := mime.ParseMediaType(descriptor.MediaType)
		if err != nil || len(params) != 0 || mediaType != strings.ToLower(mediaType) {
			return nil, errors.New("invalid authored artifact media type")
		}
		body, err := readWorkspaceFile(workspace, descriptor.Source, maxGeneratedBytes)
		if err != nil {
			return nil, fmt.Errorf("read authored artifact source: %w", classifyWorkspaceReadError(err))
		}
		if !utf8.Valid(body) {
			return nil, errors.New("read authored artifact source: content is not valid UTF-8")
		}
		total += int64(len(body))
		if total > maxGeneratedBytes {
			return nil, errors.New("authored artifacts exceed the aggregate size limit")
		}
		publish := publishDescriptor{
			Schema: "agents-gateway.artifact.v1", Title: descriptor.Title, Description: descriptor.Description,
			ContentKind: descriptor.ContentKind, Capabilities: append([]artifactcatalog.Capability(nil), descriptor.Capabilities...),
		}
		if _, err := encodePublishDescriptor(publish, mediaType); err != nil {
			return nil, errors.New("invalid authored artifact metadata")
		}
		result = append(result, authoredArtifact{descriptor: publish, mediaType: mediaType, body: body})
	}
	return result, nil
}

func uploadAuthoredArtifact(ctx context.Context, endpoint string, authored authoredArtifact) (artifactcatalog.ArtifactRef, error) {
	if _, err := validateLoopbackEndpoint(endpoint, ArtifactCreatePath); err != nil {
		return artifactcatalog.ArtifactRef{}, err
	}
	metadata, err := encodePublishDescriptor(authored.descriptor, authored.mediaType)
	if err != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("encode authored artifact metadata")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(authored.body))
	if err != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("create authored artifact request")
	}
	request.Header.Set("Content-Type", authored.mediaType)
	request.Header.Set(PublishMetadataHeader, metadata)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("upload authored artifact")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return artifactcatalog.ArtifactRef{}, fmt.Errorf("authored artifact upload returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var reference artifactcatalog.ArtifactRef
	if err := decoder.Decode(&reference); err != nil || ensureArtifactJSONEOF(decoder) != nil || reference.Validate() != nil {
		return artifactcatalog.ArtifactRef{}, errors.New("invalid authored artifact reference")
	}
	return reference, nil
}

func authoredArtifactEndpoint(outputEndpoint string) (string, error) {
	if _, err := validateLoopbackEndpoint(outputEndpoint, ArtifactPath); err != nil {
		return "", err
	}
	parsed, _ := url.Parse(outputEndpoint)
	parsed.Path = ArtifactCreatePath
	return parsed.String(), nil
}

func readWorkspaceFile(workspace, relative string, limit int64) ([]byte, error) {
	if !safeRelativeSource(relative) || limit <= 0 {
		return nil, errors.New("invalid workspace artifact path")
	}
	rootFD, err := unix.Open(workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := openWorkspaceFile(rootFD, filepath.FromSlash(relative), unix.Openat2)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), relative)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("invalid artifact file descriptor")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, errArtifactFileBounds
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("artifact source grew beyond its size limit")
	}
	return content, nil
}

type openat2Func func(int, string, *unix.OpenHow) (int, error)

func openWorkspaceFile(rootFD int, relative string, openat2 openat2Func) (int, error) {
	fd, err := openat2(rootFD, relative, &unix.OpenHow{
		// O_NONBLOCK prevents a model-authored FIFO from blocking the adapter
		// before the regular-file check below can reject it.
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK),
		Resolve: uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS),
	})
	if err == nil || (!errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL)) {
		return fd, err
	}
	return openWorkspaceFileByComponents(rootFD, relative)
}

// openWorkspaceFileByComponents is the fail-closed compatibility path for
// kernels or container policies that reject openat2. Every component is opened
// relative to an already-open directory descriptor with O_NOFOLLOW, so a
// concurrent rename cannot redirect resolution through a symlink or outside
// the workspace.
func openWorkspaceFileByComponents(rootFD int, relative string) (int, error) {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	currentFD := rootFD
	ownedCurrent := false
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			if ownedCurrent {
				_ = unix.Close(currentFD)
			}
			return -1, errors.New("invalid workspace artifact path")
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		nextFD, err := unix.Openat(currentFD, part, flags, 0)
		if ownedCurrent {
			_ = unix.Close(currentFD)
		}
		if err != nil {
			return -1, err
		}
		currentFD = nextFD
		ownedCurrent = true
	}
	return currentFD, nil
}

// classifyWorkspaceReadError returns only a bounded, path-free reason. The
// underlying syscall error may contain model-controlled path material and must
// not cross the runtime protocol boundary.
func classifyWorkspaceReadError(err error) error {
	switch {
	case errors.Is(err, errArtifactFileBounds):
		return errArtifactFileBounds
	case errors.Is(err, os.ErrNotExist):
		return errors.New("artifact file does not exist")
	case errors.Is(err, os.ErrPermission):
		return errors.New("artifact file permission denied")
	case errors.Is(err, unix.ELOOP):
		return errors.New("artifact symlink rejected")
	case errors.Is(err, unix.EXDEV):
		return errors.New("artifact path escaped the workspace")
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL):
		return errors.New("secure artifact path resolution is unavailable")
	default:
		return errors.New("artifact file is unavailable")
	}
}

func safeRelativeSource(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsAny(value, "\\\x00\r\n") && !path.IsAbs(value) && path.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func safeDescriptorName(value string) bool {
	return value != "" && len(value) <= 128 && path.Base(value) == value && !strings.ContainsAny(value, "\\\x00\r\n")
}

const PublishMetadataHeader = "X-AGW-Artifact-Metadata"

type publishDescriptor struct {
	Schema       string                       `json:"schema"`
	Title        string                       `json:"title"`
	Description  string                       `json:"description,omitempty"`
	ContentKind  artifactcatalog.ContentKind  `json:"content_kind"`
	Capabilities []artifactcatalog.Capability `json:"capabilities,omitempty"`
}

// encodePublishDescriptor is the v3-local equivalent of v2/pkg/artifact's
// metadata encoder. The HTTP broker contract remains unchanged: metadata is a
// bounded raw-URL base64 JSON descriptor, while artifact bytes travel in the
// request body.
func encodePublishDescriptor(descriptor publishDescriptor, mediaType string) (string, error) {
	if err := validatePublishDescriptor(descriptor, mediaType); err != nil {
		return "", err
	}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	if len(raw) > 8<<10 {
		return "", errors.New("artifact metadata exceeds 8 KiB")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validatePublishDescriptor(descriptor publishDescriptor, mediaType string) error {
	if descriptor.Schema != "agents-gateway.artifact.v1" {
		return errors.New("unsupported artifact descriptor schema")
	}
	if err := boundedPublishText(descriptor.Title, 200, false); err != nil {
		return errors.New("invalid artifact title")
	}
	if err := boundedPublishText(descriptor.Description, 4000, true); err != nil {
		return errors.New("invalid artifact description")
	}
	policy := artifactcatalog.SecurityPolicy{Allow: append([]artifactcatalog.Capability(nil), descriptor.Capabilities...)}
	if err := policy.Validate(descriptor.ContentKind); err != nil {
		return err
	}
	_, err := artifactcatalog.NewRunOutput(artifactcatalog.PublishInput{
		ArtifactID: strings.Repeat("a", 32), VersionID: strings.Repeat("b", 32),
		Title: descriptor.Title, Description: descriptor.Description, URI: "artifact://catalog/preview/preview",
		Digest: "sha256:" + strings.Repeat("0", 64), MediaType: strings.ToLower(mediaType),
		CreatedAt: time.Unix(1, 0).UTC(), ContentKind: descriptor.ContentKind, Security: policy,
	})
	return err
}

func boundedPublishText(value string, limit int, optional bool) error {
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

func ensureArtifactJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
