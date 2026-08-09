package codexadapter

import (
	"bytes"
	"context"
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
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerbridge"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"golang.org/x/sys/unix"
)

const (
	generatedArtifactDirectory       = ".agw/artifacts"
	maxGeneratedArtifacts            = 16
	maxDescriptorBytes         int64 = 64 << 10
	maxGeneratedBytes          int64 = 8 << 20
)

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
	descriptor artifact.PublishDescriptor
	mediaType  string
	body       []byte
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
			return nil, errors.New("read authored artifact descriptor")
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
		if err != nil || !utf8.Valid(body) {
			return nil, errors.New("read authored artifact source")
		}
		total += int64(len(body))
		if total > maxGeneratedBytes {
			return nil, errors.New("authored artifacts exceed the aggregate size limit")
		}
		publish := artifact.PublishDescriptor{
			Schema: "agents-gateway.artifact.v1", Title: descriptor.Title, Description: descriptor.Description,
			ContentKind: descriptor.ContentKind, Capabilities: append([]artifactcatalog.Capability(nil), descriptor.Capabilities...),
		}
		if _, err := artifact.EncodePublishDescriptor(publish); err != nil {
			return nil, errors.New("invalid authored artifact metadata")
		}
		result = append(result, authoredArtifact{descriptor: publish, mediaType: mediaType, body: body})
	}
	return result, nil
}

func uploadAuthoredArtifact(ctx context.Context, endpoint string, authored authoredArtifact) (workflow.ArtifactRef, error) {
	if _, err := validateLoopbackEndpoint(endpoint, brokerbridge.ArtifactCreatePath); err != nil {
		return workflow.ArtifactRef{}, err
	}
	metadata, err := artifact.EncodePublishDescriptor(authored.descriptor)
	if err != nil {
		return workflow.ArtifactRef{}, errors.New("encode authored artifact metadata")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(authored.body))
	if err != nil {
		return workflow.ArtifactRef{}, errors.New("create authored artifact request")
	}
	request.Header.Set("Content-Type", authored.mediaType)
	request.Header.Set(artifact.PublishMetadataHeader, metadata)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return workflow.ArtifactRef{}, errors.New("upload authored artifact")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return workflow.ArtifactRef{}, fmt.Errorf("authored artifact upload returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var reference workflow.ArtifactRef
	if err := decoder.Decode(&reference); err != nil || ensureArtifactJSONEOF(decoder) != nil || reference.Validate() != nil {
		return workflow.ArtifactRef{}, errors.New("invalid authored artifact reference")
	}
	return reference, nil
}

func authoredArtifactEndpoint(outputEndpoint string) (string, error) {
	if _, err := validateLoopbackEndpoint(outputEndpoint, brokerbridge.ArtifactPath); err != nil {
		return "", err
	}
	parsed, _ := url.Parse(outputEndpoint)
	parsed.Path = brokerbridge.ArtifactCreatePath
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
	fd, err := unix.Openat2(rootFD, filepath.FromSlash(relative), &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS),
	})
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
		return nil, errors.New("artifact source is not a bounded regular file")
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

func safeRelativeSource(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsAny(value, "\\\x00\r\n") && !path.IsAbs(value) && path.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func safeDescriptorName(value string) bool {
	return value != "" && len(value) <= 128 && path.Base(value) == value && !strings.ContainsAny(value, "\\\x00\r\n")
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
