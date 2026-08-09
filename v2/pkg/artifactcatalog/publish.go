package artifactcatalog

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// PublishInput contains trusted metadata produced after immutable object
// storage commits. It deliberately has no raw artifact bytes.
type PublishInput struct {
	ArtifactID, VersionID, Title, Description string
	URI, Digest, MediaType                    string
	SizeBytes                                 int64
	CreatedAt                                 time.Time
	ContentKind                               ContentKind
	Security                                  SecurityPolicy
}

// NewRunOutput publishes the first version of a run artifact. Later edits use
// the same ArtifactID, a new VersionID, and explicit Lineage rather than
// changing this immutable value.
func NewRunOutput(input PublishInput) (Version, error) {
	if input.ArtifactID == "" || input.VersionID == "" {
		return Version{}, errors.New("artifact and version ids are required")
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = "Run output"
	}
	description := strings.TrimSpace(input.Description)
	if description == "" {
		description = "Immutable output produced by an agent run."
	}
	contentKind, renderer := inferPresentation(input.MediaType)
	if input.ContentKind != "" {
		contentKind = input.ContentKind
		var ok bool
		renderer, ok = defaultRenderer(contentKind, input.MediaType)
		if !ok {
			return Version{}, errors.New("content kind is incompatible with media type")
		}
	}
	reference := ArtifactRef{
		ID: input.VersionID, URI: input.URI, Digest: input.Digest,
		SizeBytes: input.SizeBytes, MediaType: strings.ToLower(input.MediaType),
	}
	version := Version{
		SchemaVersion: CurrentSchemaVersion,
		ArtifactID:    input.ArtifactID,
		VersionID:     input.VersionID,
		CreatedAt:     input.CreatedAt.UTC(),
		Manifest: Manifest{
			SchemaVersion: CurrentSchemaVersion,
			ArtifactID:    input.ArtifactID,
			Title:         title,
			Description:   description,
			ContentKind:   contentKind,
			MediaType:     strings.ToLower(input.MediaType),
			Renderer:      Renderer{Kind: renderer, SourceView: true, Download: true},
			Security:      input.Security,
			Preview:       PreviewMetadata{Summary: description},
		},
		Content: reference,
		Source:  reference,
	}
	if err := version.Validate(); err != nil {
		return Version{}, fmt.Errorf("publish artifact version: %w", err)
	}
	return version, nil
}

func defaultRenderer(kind ContentKind, mediaType string) (RendererKind, bool) {
	mediaType = strings.ToLower(mediaType)
	switch kind {
	case ContentKindDocument:
		if mediaType == "text/markdown" {
			return RendererMarkdown, true
		}
		return RendererPlainText, strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "json")
	case ContentKindCode:
		return RendererCode, strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "json") || strings.Contains(mediaType, "javascript") || strings.Contains(mediaType, "typescript")
	case ContentKindSinglePage:
		return RendererSafeHTML, mediaType == "text/html"
	case ContentKindSVG:
		return RendererSafeSVG, mediaType == "image/svg+xml"
	case ContentKindDiagram:
		if mediaType == "image/svg+xml" {
			return RendererSafeSVG, true
		}
		return RendererDiagram, strings.HasPrefix(mediaType, "text/")
	case ContentKindInteractive:
		return RendererSandboxedComponent, mediaType == "text/html"
	default:
		return "", false
	}
}

func inferPresentation(mediaType string) (ContentKind, RendererKind) {
	switch strings.ToLower(mediaType) {
	case "text/markdown":
		return ContentKindDocument, RendererMarkdown
	case "text/html":
		return ContentKindSinglePage, RendererSafeHTML
	case "image/svg+xml":
		return ContentKindSVG, RendererSafeSVG
	case "application/javascript", "application/json", "application/typescript", "text/css", "text/javascript", "text/x-diff", "text/x-go", "text/x-python":
		return ContentKindCode, RendererCode
	default:
		return ContentKindDocument, RendererPlainText
	}
}
