package artifactcatalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validManifest() Manifest {
	return Manifest{
		SchemaVersion: CurrentSchemaVersion,
		ArtifactID:    "artifact-1",
		Title:         "Example document",
		Description:   "A safe, self-contained artifact.",
		ContentKind:   ContentKindDocument,
		MediaType:     "text/plain",
		Renderer:      Renderer{Kind: RendererPlainText, SourceView: true, Download: true},
		Security:      DefaultSecurityPolicy(),
		Preview:       PreviewMetadata{AltText: "Example", Summary: "A document"},
	}
}

func validRef(id, mediaType string) ArtifactRef {
	return ArtifactRef{ID: id, URI: "artifact://local/" + id, Digest: ContentDigest([]byte(id)), SizeBytes: int64(len(id)), MediaType: mediaType}
}

func validVersion() Version {
	manifest := validManifest()
	return Version{
		SchemaVersion: CurrentSchemaVersion,
		VersionID:     "version-1",
		ArtifactID:    manifest.ArtifactID,
		CreatedAt:     time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC),
		Manifest:      manifest,
		Content:       validRef("content-1", "text/plain"),
		Source:        validRef("source-1", "text/plain"),
	}
}

func TestDefaultPolicyDeniesAllIncludingExternalCalls(t *testing.T) {
	policy := DefaultSecurityPolicy()
	if len(policy.Allow) != 0 || policy.Allows(CapabilityNetwork) || policy.Allows(CapabilityExternalCalls) || policy.Allows(CapabilitySandboxedScripts) {
		t.Fatalf("default policy is not deny-by-default: %#v", policy)
	}
	if err := policy.Validate(ContentKindDocument); err != nil {
		t.Fatal(err)
	}
}

func TestContentKindsAndRendererMatrix(t *testing.T) {
	cases := []struct {
		kind     ContentKind
		renderer RendererKind
	}{
		{ContentKindDocument, RendererPlainText},
		{ContentKindDocument, RendererMarkdown},
		{ContentKindCode, RendererCode},
		{ContentKindSinglePage, RendererSafeHTML},
		{ContentKindSVG, RendererSafeSVG},
		{ContentKindDiagram, RendererDiagram},
		{ContentKindInteractive, RendererSandboxedComponent},
	}
	for _, tc := range cases {
		if err := (Renderer{Kind: tc.renderer, SourceView: true, Download: true}).Validate(tc.kind); err != nil {
			t.Errorf("%s/%s rejected: %v", tc.kind, tc.renderer, err)
		}
	}
	if err := (Renderer{Kind: RendererSafeHTML, SourceView: true, Download: true}).Validate(ContentKindCode); err == nil {
		t.Fatal("incompatible renderer accepted")
	}
	if err := (Renderer{Kind: RendererPlainText, SourceView: true}).Validate(ContentKindDocument); err == nil {
		t.Fatal("renderer without download accepted")
	}
}

func TestSecurityPolicyRequiresExplicitAndCompatibleCapabilities(t *testing.T) {
	for _, policy := range []SecurityPolicy{
		{Allow: []Capability{CapabilityExternalCalls}},
		{Allow: []Capability{CapabilityNetwork, CapabilityNetwork}},
		{Allow: []Capability{"trusted_execution"}},
		{Allow: []Capability{CapabilitySandboxedScripts}},
	} {
		if err := policy.Validate(ContentKindDocument); err == nil {
			t.Fatalf("unsafe policy accepted: %#v", policy)
		}
	}
	if err := (SecurityPolicy{Allow: []Capability{CapabilityNetwork, CapabilityExternalCalls}}).Validate(ContentKindDocument); err != nil {
		t.Fatal(err)
	}
	if err := (SecurityPolicy{Allow: []Capability{CapabilitySandboxedScripts}}).Validate(ContentKindInteractive); err != nil {
		t.Fatal(err)
	}
}

func TestManifestValidation(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Manifest){
		func(m *Manifest) { m.SchemaVersion = 2 },
		func(m *Manifest) { m.ArtifactID = "../escape" },
		func(m *Manifest) { m.Title = "" },
		func(m *Manifest) { m.Title = "bad\npreview" },
		func(m *Manifest) { m.ContentKind = "unknown" },
		func(m *Manifest) { m.MediaType = "TEXT/PLAIN" },
		func(m *Manifest) { m.Renderer.Kind = RendererSafeHTML },
		func(m *Manifest) { m.Preview.Summary = "<script>alert(1)</script>" },
	}
	for i, mutate := range cases {
		m := validManifest()
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("invalid manifest case %d accepted", i)
		}
	}
}

func TestArtifactRefValidationRejectsExecutableAndLocalURIs(t *testing.T) {
	base := validRef("ref-1", "text/plain")
	for _, uri := range []string{"file:///tmp/x", "data:text/html,x", "javascript://host/x", "http://insecure/x", "artifact://host/x#fragment", "artifact://host/../x"} {
		ref := base
		ref.URI = uri
		if err := ref.Validate(); err == nil {
			t.Errorf("unsafe URI accepted: %q", uri)
		}
	}
	if err := (ArtifactRef{ID: "ref", URI: "https://example.test/artifact", Digest: "sha256:" + strings.Repeat("a", 64), MediaType: "text/plain"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVersionLineageAndImmutabilityContract(t *testing.T) {
	version := validVersion()
	if err := version.Validate(); err != nil {
		t.Fatal(err)
	}
	version.Lineage = Lineage{ParentVersionID: "version-0", ForkedFrom: &VersionRef{ArtifactID: "other-artifact", VersionID: "version-9"}}
	if err := version.Validate(); err != nil {
		t.Fatal(err)
	}
	version.Lineage.ParentVersionID = version.VersionID
	if err := version.Validate(); err == nil {
		t.Fatal("self-parent version accepted")
	}
	version = validVersion()
	version.Manifest.ArtifactID = "other-artifact"
	if err := version.Validate(); err == nil {
		t.Fatal("manifest/artifact identity mismatch accepted")
	}
	version = validVersion()
	version.Content.MediaType = "application/json"
	if err := version.Validate(); err == nil {
		t.Fatal("content/manifest media mismatch accepted")
	}
	version = validVersion()
	version.CreatedAt = time.Now().In(time.FixedZone("EST", -5*60*60))
	if err := version.Validate(); err == nil {
		t.Fatal("non-UTC timestamp accepted")
	}
}

func TestStrictJSONRoundTripAndUnknownFields(t *testing.T) {
	version := validVersion()
	data, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Version
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.VersionID != version.VersionID || roundTrip.Content.Digest != version.Content.Digest {
		t.Fatalf("round trip changed immutable fields: %#v", roundTrip)
	}
	for _, input := range []string{
		`{"schema_version":1,"version_id":"v","artifact_id":"a","created_at":"2026-08-09T12:00:00Z","lineage":{},"manifest":{},"content":{},"source":{},"unknown":true}`,
		`{"id":"ref","uri":"artifact://local/ref","digest":"sha256:` + strings.Repeat("a", 64) + `","size_bytes":0,"media_type":"text/plain","unknown":true}`,
		`{"allow":[],"unknown":true}`,
		`{"kind":"plain_text","source_view":true,"download":true,"unknown":true}`,
		`{"alt_text":"ok","unknown":true}`,
		`{"parent_version_id":"v1","unknown":true}`,
	} {
		var target any
		switch {
		case strings.Contains(input, `"version_id"`):
			target = new(Version)
		case strings.Contains(input, `"size_bytes"`):
			target = new(ArtifactRef)
		case strings.Contains(input, `"kind"`):
			target = new(Renderer)
		case strings.Contains(input, `"alt_text"`):
			target = new(PreviewMetadata)
		case strings.Contains(input, `"parent_version_id"`):
			target = new(Lineage)
		default:
			target = new(SecurityPolicy)
		}
		if err := json.Unmarshal([]byte(input), target); err == nil {
			t.Errorf("unknown JSON field accepted: %s", input)
		}
	}
	if err := json.Unmarshal(append(data, []byte(` {}`)...), &roundTrip); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
}

func TestSecurityJSONIsCanonicalAndSafe(t *testing.T) {
	data, err := json.Marshal(SecurityPolicy{Allow: []Capability{CapabilityExternalCalls, CapabilityNetwork}})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"allow":["external_calls","network"]}` {
		t.Fatalf("unexpected canonical policy: %s", data)
	}
	var decoded SecurityPolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
}
