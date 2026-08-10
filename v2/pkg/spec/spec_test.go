package spec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDecodeAllDispatchesMultiDocumentStream(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
---
apiVersion: agents.astatide.com/v1alpha1
kind: Project
metadata:
  name: agents
spec:
  organizationRef: acme
`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(resources) != 2 || ResourceKind(resources[0]) != KindOrganization || ResourceKind(resources[1]) != KindProject {
		t.Fatalf("unexpected resources: %#v", resources)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
  typo: rejected
`))
	if err == nil || !strings.Contains(err.Error(), "field typo not found") {
		t.Fatalf("expected strict unknown-field error, got %v", err)
	}
}

func TestValidateProductionRequiresDigests(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Agent
metadata:
  name: fixer
spec:
  runtime:
    harness: codex
    image: ghcr.io/example/agent:latest
  instructions:
    inline: fix it
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAll(resources, ValidationOptions{Production: true}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected digest validation error, got %v", err)
	}
}

func TestToolGrantArgumentsDecodeAsOptionalJSONObject(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: ToolSet
metadata:
  name: github-tools
spec:
  servers:
    - name: github
      ref: https://mcp.example.test/mcp
      tools:
        - name: create_branch
          arguments:
            owner: acme
            repo: gateway
            nested:
              enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	toolSet := resources[0].(*ToolSet)
	if len(toolSet.Spec.Servers[0].Tools) != 1 || toolSet.Spec.Servers[0].Tools[0].Arguments == nil {
		t.Fatalf("exact arguments constraint was not decoded: %#v", toolSet)
	}
	if !json.Valid(toolSet.Spec.Servers[0].Tools[0].Arguments.Raw()) {
		t.Fatal("YAML arguments did not become a valid JSON object")
	}
	if err := ValidateAll(resources, ValidationOptions{}); err != nil {
		t.Fatal(err)
	}
	yamlRoundTrip, err := yaml.Marshal(toolSet)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := Decode(yamlRoundTrip); err != nil || decoded[0].(*ToolSet).Spec.Servers[0].Tools[0].Arguments == nil {
		t.Fatalf("ToolSet arguments did not survive YAML encoding: %v", err)
	}
	normalized, err := NormalizedJSON(toolSet)
	if err != nil || !strings.Contains(string(normalized), `"arguments"`) {
		t.Fatalf("normalized ToolSet omitted arguments constraint: %s (err=%v)", normalized, err)
	}
}

func TestToolGrantArgumentsPreserveArbitraryJSONNumberLexemes(t *testing.T) {
	source := []byte(`{"apiVersion":"agents.astatide.com/v1alpha1","kind":"ToolSet","metadata":{"name":"numeric-tools"},"spec":{"servers":[{"name":"gateway","ref":"https://mcp.example.test/mcp","tools":[{"name":"calculate","arguments":{"huge":90071992547409931234567890.1234500,"tiny":1e-4096,"nested":{"value":10e4095}}}]}]}}`)
	var toolSet ToolSet
	if err := json.Unmarshal(source, &toolSet); err != nil {
		t.Fatal(err)
	}
	arguments := toolSet.Spec.Servers[0].Tools[0].Arguments
	for _, lexeme := range []string{"90071992547409931234567890.1234500", "1e-4096", "10e4095"} {
		if !strings.Contains(string(arguments.Raw()), lexeme) {
			t.Fatalf("JSON number lexeme %q was not preserved: %s", lexeme, arguments.Raw())
		}
	}
	normalized, err := AsJSON(&toolSet)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := Decode(normalized)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(roundTripped[0].(*ToolSet).Spec.Servers[0].Tools[0].Arguments.Raw())
	for _, lexeme := range []string{"90071992547409931234567890.1234500", "1e-4096", "10e4095"} {
		if !strings.Contains(raw, lexeme) {
			t.Fatalf("persisted JSON number lexeme %q was not preserved: %s", lexeme, raw)
		}
	}
}

func TestToolGrantArgumentsRequireUniqueJSONObject(t *testing.T) {
	var toolSet ToolSet
	err := json.Unmarshal([]byte(`{"apiVersion":"agents.astatide.com/v1alpha1","kind":"ToolSet","metadata":{"name":"duplicate-tools"},"spec":{"servers":[{"name":"gateway","ref":"https://mcp.example.test/mcp","tools":[{"name":"call","arguments":{"nested":{"secret":"first","secret":"second"}}}]}]}}`), &toolSet)
	if err == nil || strings.Contains(err.Error(), "first") || strings.Contains(err.Error(), "second") {
		t.Fatalf("duplicate-key decoding was not bounded and fail-closed: %v", err)
	}
	invalid := &ToolSet{
		ResourceMeta: ResourceMeta{TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindToolSet}, Metadata: ObjectMeta{Name: "invalid-tools"}},
		Spec:         ToolSetSpec{Servers: []MCPServer{{Name: "gateway", Ref: "https://mcp.example.test/mcp", Tools: []ToolGrant{{Name: "call", Arguments: &JSONArguments{}}}}}},
	}
	if err := ValidateAll([]Resource{invalid}, ValidationOptions{}); err == nil || !strings.Contains(err.Error(), ".arguments") {
		t.Fatalf("invalid programmatic arguments were not rejected: %v", err)
	}
	if _, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: ToolSet
metadata:
  name: invalid-tools
spec:
  servers:
    - name: gateway
      ref: https://mcp.example.test/mcp
      tools:
        - name: call
          arguments: [not, an, object]
`)); err == nil {
		t.Fatal("non-object YAML arguments were accepted")
	}
}

func TestToolGrantArgumentsRejectExplicitNullAndPreserveOmission(t *testing.T) {
	jsonOmitted := []byte(`{"apiVersion":"agents.astatide.com/v1alpha1","kind":"ToolSet","metadata":{"name":"omitted-tools"},"spec":{"servers":[{"name":"gateway","ref":"https://mcp.example.test/mcp","tools":[{"name":"get_me","effect":"read"}]}]}}`)
	var omitted ToolSet
	if err := json.Unmarshal(jsonOmitted, &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.Spec.Servers[0].Tools[0].Arguments != nil {
		t.Fatal("omitted JSON arguments became a constraint")
	}
	digest, err := RevisionDigest(&omitted)
	if err != nil {
		t.Fatal(err)
	}
	readback, err := AsJSON(&omitted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readback), `"arguments"`) {
		t.Fatalf("omitempty emitted an omitted arguments field: %s", readback)
	}
	decoded, err := Decode(readback)
	if err != nil {
		t.Fatal(err)
	}
	if decoded[0].(*ToolSet).Spec.Servers[0].Tools[0].Arguments != nil {
		t.Fatal("persisted omitted arguments became a constraint")
	}
	readbackDigest, err := RevisionDigest(decoded[0])
	if err != nil || readbackDigest != digest {
		t.Fatalf("omitted arguments digest changed across readback: %q != %q (err=%v)", readbackDigest, digest, err)
	}

	jsonNull := bytes.Replace(jsonOmitted, []byte(`"effect":"read"`), []byte(`"effect":"read","arguments":null`), 1)
	if err := json.Unmarshal(jsonNull, &ToolSet{}); err == nil {
		t.Fatal("explicit JSON arguments null was accepted")
	}

	yamlOmitted := []byte(`apiVersion: agents.astatide.com/v1alpha1
kind: ToolSet
metadata:
  name: omitted-tools
spec:
  servers:
    - name: gateway
      ref: https://mcp.example.test/mcp
      tools:
        - name: get_me
          effect: read
`)
	yamlResources, err := Decode(yamlOmitted)
	if err != nil {
		t.Fatal(err)
	}
	if yamlResources[0].(*ToolSet).Spec.Servers[0].Tools[0].Arguments != nil {
		t.Fatal("omitted YAML arguments became a constraint")
	}
	for _, explicitNull := range []string{"null", "~", ""} {
		document := append(append([]byte(nil), yamlOmitted...), []byte("          arguments: "+explicitNull+"\n")...)
		if _, err := Decode(document); err == nil {
			t.Fatalf("explicit YAML arguments null %q was accepted", explicitNull)
		}
	}
}

func TestToolGrantArgumentsRemainOpaqueDuringNormalization(t *testing.T) {
	var toolSet ToolSet
	source := []byte(`{"apiVersion":"agents.astatide.com/v1alpha1","kind":"ToolSet","metadata":{"name":"opaque-tools"},"spec":{"servers":[{"name":"gateway","ref":"https://mcp.example.test/mcp","tools":[{"name":"call","arguments":{"nullMember":null,"tags":["b","a"],"allowedHosts":["z","a"],"nested":{"routes":["second","first"]}}}]}]}}`)
	if err := json.Unmarshal(source, &toolSet); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizedJSON(&toolSet)
	if err != nil {
		t.Fatal(err)
	}
	text := string(normalized)
	for _, fragment := range []string{`"nullMember":null`, `"tags":["b","a"]`, `"allowedHosts":["z","a"]`, `"routes":["second","first"]`} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("opaque argument meaning changed: %s", text)
		}
	}
}

func TestToolGrantArgumentsRejectAliasesAndNonJSONYAMLForms(t *testing.T) {
	for _, source := range []string{
		`apiVersion: agents.astatide.com/v1alpha1
kind: ToolSet
metadata: {name: aliases}
spec:
  servers:
    - name: gateway
      ref: https://mcp.example.test/mcp
      tools:
        - name: call
          arguments: &base {owner: acme}
        - name: other
          arguments: *base
`,
		`apiVersion: agents.astatide.com/v1alpha1
kind: ToolSet
metadata: {name: timestamp}
spec:
  servers:
    - name: gateway
      ref: https://mcp.example.test/mcp
      tools:
        - name: call
          arguments: {when: 2026-08-09}
`,
		`apiVersion: agents.astatide.com/v1alpha1
kind: ToolSet
metadata: {name: yaml-number}
spec:
  servers:
    - name: gateway
      ref: https://mcp.example.test/mcp
      tools:
        - name: call
          arguments: {hex: 0x10}
`,
	} {
		if _, err := Decode([]byte(source)); err == nil {
			t.Fatalf("non-JSON YAML argument form was accepted: %s", source)
		}
	}
}

func TestValidateWorkflowRejectsCyclesAndUnknownDependencies(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Workflow
metadata:
  name: cycle
spec:
  steps:
    - id: first
      agent: one
      needs: [second]
    - id: second
      agent: two
      needs: [first, missing]
`))
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateAll(resources, ValidationOptions{})
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") || !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("expected cycle and dependency errors, got %v", err)
	}
}

func TestRevisionDigestIsStableForMapAndUnorderedFieldOrder(t *testing.T) {
	first, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: SandboxProfile
metadata:
  name: coding
  labels:
    z: last
    a: first
spec:
  backend: podman
  image: ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  filesystem:
    root: read-only
  network:
    mode: brokered
    routes: [tools, models]
`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: SandboxProfile
metadata:
  name: coding
  labels:
    a: first
    z: last
spec:
  backend: podman
  image: ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  filesystem:
    root: read-only
  network:
    mode: brokered
    routes: [models, tools]
`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := RevisionDigest(first[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := RevisionDigest(second[0])
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("digest changed for normalized equivalent resources: %s != %s", a, b)
	}
}

func TestArtifactDigestMustBeSha256(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Artifact
metadata:
  name: report
spec:
  runRef: run
  uri: s3://bucket/report
  digest: md5:bad
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAll(resources, ValidationOptions{}); err == nil {
		t.Fatal("expected artifact digest validation error")
	}
}
