// Package artifactcatalog defines the portable, declarative artifact contract.
//
// The contract is intentionally separate from storage and HTTP handlers. It
// describes what an artifact is and how it may be previewed; it does not grant
// access to the artifact bytes, execute content, or perform network requests.
package artifactcatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const CurrentSchemaVersion = 1

type ContentKind string

const (
	ContentKindDocument    ContentKind = "document"
	ContentKindCode        ContentKind = "code"
	ContentKindSinglePage  ContentKind = "single_page_html"
	ContentKindSVG         ContentKind = "svg"
	ContentKindDiagram     ContentKind = "diagram"
	ContentKindInteractive ContentKind = "interactive_component"
)

func (k ContentKind) valid() bool {
	switch k {
	case ContentKindDocument, ContentKindCode, ContentKindSinglePage, ContentKindSVG, ContentKindDiagram, ContentKindInteractive:
		return true
	default:
		return false
	}
}

func (k ContentKind) MarshalJSON() ([]byte, error) {
	if !k.valid() {
		return nil, fmt.Errorf("content kind %q is not supported", k)
	}
	return json.Marshal(string(k))
}

func (k *ContentKind) UnmarshalJSON(data []byte) error {
	var value string
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := ContentKind(value)
	if !decoded.valid() {
		return fmt.Errorf("content kind %q is not supported", decoded)
	}
	*k = decoded
	return nil
}

type RendererKind string

const (
	RendererPlainText          RendererKind = "plain_text"
	RendererMarkdown           RendererKind = "markdown"
	RendererCode               RendererKind = "code"
	RendererSafeHTML           RendererKind = "safe_html"
	RendererSafeSVG            RendererKind = "safe_svg"
	RendererDiagram            RendererKind = "diagram"
	RendererSandboxedComponent RendererKind = "sandboxed_component"
)

func (r RendererKind) valid() bool {
	switch r {
	case RendererPlainText, RendererMarkdown, RendererCode, RendererSafeHTML, RendererSafeSVG, RendererDiagram, RendererSandboxedComponent:
		return true
	default:
		return false
	}
}

// Renderer declares a safe presentation mode. Source and download remain
// first-class even when a consumer elects not to show a preview.
type Renderer struct {
	Kind       RendererKind `json:"kind"`
	SourceView bool         `json:"source_view"`
	Download   bool         `json:"download"`
}

func (r Renderer) Validate(kind ContentKind) error {
	if err := r.validateShape(); err != nil {
		return err
	}
	allowed := map[ContentKind]map[RendererKind]bool{
		ContentKindDocument:    {RendererPlainText: true, RendererMarkdown: true},
		ContentKindCode:        {RendererPlainText: true, RendererCode: true},
		ContentKindSinglePage:  {RendererSafeHTML: true},
		ContentKindSVG:         {RendererSafeSVG: true},
		ContentKindDiagram:     {RendererDiagram: true, RendererSafeSVG: true},
		ContentKindInteractive: {RendererSandboxedComponent: true},
	}
	if !kind.valid() || !allowed[kind][r.Kind] {
		return fmt.Errorf("renderer %q is incompatible with content kind %q", r.Kind, kind)
	}
	return nil
}

func (r Renderer) validateShape() error {
	if !r.Kind.valid() {
		return fmt.Errorf("renderer kind %q is not supported", r.Kind)
	}
	if !r.SourceView || !r.Download {
		return errors.New("renderer must enable source_view and download")
	}
	return nil
}

func (r Renderer) MarshalJSON() ([]byte, error) {
	if err := r.validateShape(); err != nil {
		return nil, err
	}
	type plain Renderer
	return json.Marshal(plain(r))
}

func (r *Renderer) UnmarshalJSON(data []byte) error {
	type plain Renderer
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := Renderer(value)
	if err := decoded.validateShape(); err != nil {
		return err
	}
	*r = decoded
	return nil
}

type Capability string

const (
	CapabilitySandboxedScripts Capability = "sandboxed_scripts"
	CapabilityNetwork          Capability = "network"
	CapabilityExternalCalls    Capability = "external_calls"
	CapabilityClipboardWrite   Capability = "clipboard_write"
)

func (c Capability) valid() bool {
	switch c {
	case CapabilitySandboxedScripts, CapabilityNetwork, CapabilityExternalCalls, CapabilityClipboardWrite:
		return true
	default:
		return false
	}
}

// SecurityPolicy is an allowlist. The zero value is valid and denies every
// capability, including network and external calls. There is deliberately no
// trusted-execution capability in this contract.
type SecurityPolicy struct {
	Allow []Capability `json:"allow,omitempty"`
}

func DefaultSecurityPolicy() SecurityPolicy { return SecurityPolicy{} }

func (p SecurityPolicy) Validate(kind ContentKind) error {
	seen := make(map[Capability]struct{}, len(p.Allow))
	for _, capability := range p.Allow {
		if !capability.valid() {
			return fmt.Errorf("security capability %q is not supported", capability)
		}
		if _, ok := seen[capability]; ok {
			return fmt.Errorf("security capability %q is duplicated", capability)
		}
		seen[capability] = struct{}{}
		if capability == CapabilitySandboxedScripts && kind != ContentKindInteractive {
			return errors.New("sandboxed_scripts is only valid for interactive_component artifacts")
		}
	}
	if _, external := seen[CapabilityExternalCalls]; external {
		if _, network := seen[CapabilityNetwork]; !network {
			return errors.New("external_calls requires the network capability")
		}
	}
	return nil
}

func (p SecurityPolicy) Allows(capability Capability) bool {
	for _, allowed := range p.Allow {
		if allowed == capability {
			return true
		}
	}
	return false
}

// PreviewMetadata contains display metadata only. It has no HTML, script,
// URL, executable source, or instructions for a renderer to perform.
type PreviewMetadata struct {
	AltText  string `json:"alt_text,omitempty"`
	Summary  string `json:"summary,omitempty"`
	WidthPx  uint32 `json:"width_px,omitempty"`
	HeightPx uint32 `json:"height_px,omitempty"`
}

func (p PreviewMetadata) Validate() error {
	if err := safeText("preview alt_text", p.AltText, 500); err != nil {
		return err
	}
	if err := safeText("preview summary", p.Summary, 2000); err != nil {
		return err
	}
	if strings.ContainsAny(p.AltText+p.Summary, "<>") {
		return errors.New("preview metadata must not contain markup")
	}
	if p.WidthPx > 10000 || p.HeightPx > 10000 {
		return errors.New("preview dimensions must be at most 10000 pixels")
	}
	return nil
}

func (p PreviewMetadata) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	type plain PreviewMetadata
	return json.Marshal(plain(p))
}

func (p *PreviewMetadata) UnmarshalJSON(data []byte) error {
	type plain PreviewMetadata
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := PreviewMetadata(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*p = decoded
	return nil
}

// Manifest is the immutable descriptive contract shared by versions of an
// artifact. Artifact bytes are referenced by Version.Content and Version.Source.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	ArtifactID    string          `json:"artifact_id"`
	Title         string          `json:"title"`
	Description   string          `json:"description,omitempty"`
	ContentKind   ContentKind     `json:"content_kind"`
	MediaType     string          `json:"media_type"`
	Renderer      Renderer        `json:"renderer"`
	Security      SecurityPolicy  `json:"security"`
	Preview       PreviewMetadata `json:"preview"`
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("schema_version must be %d", CurrentSchemaVersion)
	}
	if err := validateID("artifact_id", m.ArtifactID); err != nil {
		return err
	}
	if err := safeText("title", m.Title, 200); err != nil || m.Title == "" {
		if err != nil {
			return err
		}
		return errors.New("title is required")
	}
	if err := safeText("description", m.Description, 4000); err != nil {
		return err
	}
	if !m.ContentKind.valid() {
		return fmt.Errorf("content_kind %q is not supported", m.ContentKind)
	}
	canonical, err := canonicalMediaType(m.MediaType)
	if err != nil {
		return fmt.Errorf("manifest media_type: %w", err)
	}
	if canonical != m.MediaType {
		return errors.New("manifest media_type must be canonical lowercase media type")
	}
	if err := m.Renderer.Validate(m.ContentKind); err != nil {
		return err
	}
	if err := m.Security.Validate(m.ContentKind); err != nil {
		return err
	}
	return m.Preview.Validate()
}

// ArtifactRef is an immutable content reference. It is a reference, not an
// instruction to fetch or execute anything.
type ArtifactRef struct {
	ID        string `json:"id"`
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

func (r ArtifactRef) Validate() error {
	if err := validateID("artifact ref id", r.ID); err != nil {
		return err
	}
	if err := validateURI(r.URI); err != nil {
		return fmt.Errorf("artifact ref uri: %w", err)
	}
	if !validDigest(r.Digest) {
		return errors.New("artifact ref digest must be a lowercase sha256 digest")
	}
	if r.SizeBytes < 0 {
		return errors.New("artifact ref size_bytes cannot be negative")
	}
	canonical, err := canonicalMediaType(r.MediaType)
	if err != nil {
		return fmt.Errorf("artifact ref media_type: %w", err)
	}
	if canonical != r.MediaType {
		return errors.New("artifact ref media_type must be canonical lowercase media type")
	}
	return nil
}

type VersionRef struct {
	ArtifactID string `json:"artifact_id"`
	VersionID  string `json:"version_id"`
}

func (r VersionRef) Validate() error {
	if err := validateID("lineage artifact_id", r.ArtifactID); err != nil {
		return err
	}
	return validateID("lineage version_id", r.VersionID)
}

type Lineage struct {
	ParentVersionID string      `json:"parent_version_id,omitempty"`
	ForkedFrom      *VersionRef `json:"forked_from,omitempty"`
}

func (l Lineage) MarshalJSON() ([]byte, error) {
	if err := l.Validate("", ""); err != nil {
		return nil, err
	}
	type plain Lineage
	return json.Marshal(plain(l))
}

func (l *Lineage) UnmarshalJSON(data []byte) error {
	type plain Lineage
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := Lineage(value)
	if err := decoded.Validate("", ""); err != nil {
		return err
	}
	*l = decoded
	return nil
}

func (l Lineage) Validate(currentArtifactID, currentVersionID string) error {
	if l.ParentVersionID != "" {
		if err := validateID("parent_version_id", l.ParentVersionID); err != nil {
			return err
		}
		if l.ParentVersionID == currentVersionID {
			return errors.New("parent_version_id cannot equal version_id")
		}
	}
	if l.ForkedFrom != nil {
		if err := l.ForkedFrom.Validate(); err != nil {
			return err
		}
		if l.ForkedFrom.ArtifactID == currentArtifactID && l.ForkedFrom.VersionID == currentVersionID {
			return errors.New("forked_from cannot point to the current version")
		}
	}
	return nil
}

// Version is immutable once published. A new edit is represented by another
// Version with a parent and/or fork lineage, never by mutating this value.
type Version struct {
	SchemaVersion int         `json:"schema_version"`
	VersionID     string      `json:"version_id"`
	ArtifactID    string      `json:"artifact_id"`
	CreatedAt     time.Time   `json:"created_at"`
	Lineage       Lineage     `json:"lineage"`
	Manifest      Manifest    `json:"manifest"`
	Content       ArtifactRef `json:"content"`
	Source        ArtifactRef `json:"source"`
}

func (v Version) Validate() error {
	if v.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("schema_version must be %d", CurrentSchemaVersion)
	}
	if err := validateID("version_id", v.VersionID); err != nil {
		return err
	}
	if err := validateID("artifact_id", v.ArtifactID); err != nil {
		return err
	}
	if v.CreatedAt.IsZero() || v.CreatedAt.Location() != time.UTC {
		return errors.New("created_at must be a non-zero UTC timestamp")
	}
	if err := v.Manifest.Validate(); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if v.Manifest.ArtifactID != v.ArtifactID {
		return errors.New("manifest artifact_id must match version artifact_id")
	}
	if err := v.Lineage.Validate(v.ArtifactID, v.VersionID); err != nil {
		return err
	}
	if err := v.Content.Validate(); err != nil {
		return fmt.Errorf("content: %w", err)
	}
	if err := v.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if v.Content.MediaType != v.Manifest.MediaType {
		return errors.New("content media_type must match manifest media_type")
	}
	return nil
}

// ContentDigest computes the digest used by tests and catalog clients when a
// canonical byte sequence is available. The returned format matches ArtifactRef.
func ContentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func safeText(label, value string, max int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", label)
	}
	if len([]rune(value)) > max {
		return fmt.Errorf("%s must be at most %d characters", label, max)
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return fmt.Errorf("%s contains unsafe control characters", label)
		}
	}
	return nil
}

func validateID(label, value string) error {
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s is required and must be at most 128 characters", label)
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (i > 0 && (r == '-' || r == '_' || r == '.')) {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", label)
	}
	return nil
}

func canonicalMediaType(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("must be a media type")
	}
	parsed, params, err := mime.ParseMediaType(value)
	if err != nil || parsed == "" {
		return "", errors.New("must be a valid media type")
	}
	if len(params) != 0 {
		return "", errors.New("parameters are not allowed")
	}
	return strings.ToLower(parsed), nil
}

func validateURI(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00#") {
		return errors.New("must be a safe absolute URI")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Fragment != "" {
		return errors.New("must be a safe absolute URI")
	}
	if parsed.Path == "" || path.Clean(parsed.Path) != parsed.Path || strings.Contains(parsed.Path, "/../") || strings.HasSuffix(parsed.Path, "/..") {
		return errors.New("must be a normalized URI path")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "artifact", "https":
		return nil
	default:
		return errors.New("URI scheme is not allowed")
	}
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range value[len("sha256:"):] {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (m Manifest) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	type plain Manifest
	return json.Marshal(plain(m))
}

func (m *Manifest) UnmarshalJSON(data []byte) error {
	type plain Manifest
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := Manifest(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*m = decoded
	return nil
}

func (r ArtifactRef) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	type plain ArtifactRef
	return json.Marshal(plain(r))
}

func (r *ArtifactRef) UnmarshalJSON(data []byte) error {
	type plain ArtifactRef
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := ArtifactRef(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*r = decoded
	return nil
}

func (r VersionRef) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	type plain VersionRef
	return json.Marshal(plain(r))
}

func (r *VersionRef) UnmarshalJSON(data []byte) error {
	type plain VersionRef
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := VersionRef(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*r = decoded
	return nil
}

func (v Version) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	type plain Version
	return json.Marshal(plain(v))
}

func (v *Version) UnmarshalJSON(data []byte) error {
	type plain Version
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := Version(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}

func (p SecurityPolicy) MarshalJSON() ([]byte, error) {
	if err := p.Validate(ContentKindInteractive); err != nil {
		return nil, err
	}
	copyPolicy := p
	copyPolicy.Allow = append([]Capability(nil), p.Allow...)
	sort.Slice(copyPolicy.Allow, func(i, j int) bool { return copyPolicy.Allow[i] < copyPolicy.Allow[j] })
	type plain SecurityPolicy
	return json.Marshal(plain(copyPolicy))
}

func (p *SecurityPolicy) UnmarshalJSON(data []byte) error {
	type plain SecurityPolicy
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	decoded := SecurityPolicy(value)
	if err := decoded.Validate(ContentKindInteractive); err != nil {
		return err
	}
	*p = decoded
	return nil
}
