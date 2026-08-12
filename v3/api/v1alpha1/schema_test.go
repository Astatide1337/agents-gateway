package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestGateRequirementsOptionalFieldsStayOmittable(t *testing.T) {
	body, err := json.Marshal(GateRequirements{
		ScopeRespected:  true,
		TestStrength:    TestStrengthNone,
		MaxFilesChanged: 1,
		MaxDiffLines:    1,
		NoBinaryFiles:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(body)
	for _, field := range []string{`"adapter"`, `"baseTestCommand"`, `"coverageDelta"`} {
		if strings.Contains(encoded, field) {
			t.Fatalf("optional field %s was emitted when omitted: %s", field, encoded)
		}
	}
}

func TestGeneratedGateCRDGuardsOmittedOptionalFields(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "agents.astatide.com_gates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apix.CustomResourceDefinition
	if err := yaml.Unmarshal(body, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatal("generated Gate CRD has no schema")
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	requirements, ok := schema.Properties["spec"].Properties["require"]
	if !ok {
		t.Fatal("generated Gate CRD has no spec.require schema")
	}
	want := map[string]bool{
		"(self.testStrength == 'none' && !has(self.baseTestCommand)) || (self.testStrength == 'newTestsMustFailOnBase' && has(self.adapter) && self.adapter == 'go' && has(self.baseTestCommand))": false,
		"!has(self.coverageDelta) || size(self.coverageDelta) == 0 || (has(self.adapter) && self.adapter == 'go')":                                                                                 false,
	}
	for _, validation := range requirements.XValidations {
		if _, exists := want[validation.Rule]; exists {
			want[validation.Rule] = true
		}
	}
	for rule, found := range want {
		if !found {
			t.Errorf("generated Gate CRD is missing optional-field-safe CEL rule %q", rule)
		}
	}
	for _, required := range requirements.Required {
		if required == "adapter" || required == "baseTestCommand" || required == "coverageDelta" {
			t.Errorf("optional Gate requirement field became required: %q", required)
		}
	}
}

func TestGeneratedGateCRDUsesFixedPointCriticSignalsWithoutMutation(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "agents.astatide.com_gates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apix.CustomResourceDefinition
	if err := yaml.Unmarshal(body, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	signals, ok := spec.Properties["signals"]
	if !ok {
		t.Fatal("generated Gate CRD has no spec.signals schema")
	}
	if signals.Properties["executionWeightBasisPoints"].Maximum == nil || *signals.Properties["executionWeightBasisPoints"].Maximum != float64(GateScoreScale) {
		t.Fatalf("execution weight schema=%#v", signals.Properties["executionWeightBasisPoints"])
	}
	if signals.Properties["minScoreBasisPoints"].Maximum == nil || *signals.Properties["minScoreBasisPoints"].Maximum != float64(GateScoreScale) {
		t.Fatalf("minimum score schema=%#v", signals.Properties["minScoreBasisPoints"])
	}
	critic, criticOK := signals.Properties["critic"]
	if !criticOK || critic.Properties["modelRouteRef"].Pattern == "" || critic.Properties["maxFindings"].Maximum == nil || *critic.Properties["maxFindings"].Maximum != float64(MaxCriticFindings) {
		t.Fatalf("critic signal schema=%#v", critic)
	}
	if _, ok := signals.Properties["mutation"]; ok {
		t.Fatal("mutation signal unexpectedly appeared in the Gate schema")
	}
}

func TestGeneratedToolSetCRDGuardsPhaseProfilesAndLimits(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "agents.astatide.com_toolsets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apix.CustomResourceDefinition
	if err := yaml.Unmarshal(body, &crd); err != nil {
		t.Fatal(err)
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec, ok := schema.Properties["spec"]
	if !ok {
		t.Fatal("generated ToolSet CRD has no spec schema")
	}
	if !containsString(spec.Required, "profiles") || !containsString(spec.Required, "maxToolsPerPhase") {
		t.Fatalf("ToolSet spec required fields=%v, want profiles and maxToolsPerPhase", spec.Required)
	}
	profiles := spec.Properties["profiles"]
	if profiles.MinItems == nil || *profiles.MinItems != 1 || profiles.MaxItems == nil || *profiles.MaxItems != 3 || profiles.XListType == nil || *profiles.XListType != "map" || len(profiles.XListMapKeys) != 1 || profiles.XListMapKeys[0] != "name" {
		t.Fatalf("profiles schema=%#v, want bounded unique map", profiles)
	}
	wantValidations := map[string]bool{
		"size(self.profiles) == 3 && self.profiles.exists(p, p.name == 'explore') && self.profiles.exists(p, p.name == 'edit') && self.profiles.exists(p, p.name == 'verify')": false,
		"self.profiles.all(p, size(p.tools) <= self.maxToolsPerPhase)": false,
	}
	for _, validation := range spec.XValidations {
		for rule := range wantValidations {
			if validation.Rule == rule {
				wantValidations[rule] = true
			}
		}
	}
	for rule, found := range wantValidations {
		if !found {
			t.Errorf("generated ToolSet CRD is missing CEL rule %q", rule)
		}
	}
	maxTools := spec.Properties["maxToolsPerPhase"]
	if maxTools.Minimum == nil || *maxTools.Minimum != 1 || maxTools.Maximum == nil || *maxTools.Maximum != float64(MaxToolsPerPhase) {
		t.Fatalf("maxToolsPerPhase schema=%#v, want 1..%d", maxTools, MaxToolsPerPhase)
	}
	if profiles.Items == nil || profiles.Items.Schema == nil {
		t.Fatal("profile item schema is missing")
	}
	profile := profiles.Items.Schema
	name := profile.Properties["name"]
	if len(name.Enum) != 3 {
		t.Fatalf("profile name enum=%v, want explore/edit/verify", name.Enum)
	}
	tools := profile.Properties["tools"]
	if tools.MinItems == nil || *tools.MinItems != 1 || tools.MaxItems == nil || *tools.MaxItems != int64(MaxToolsPerPhase) || tools.XListType == nil || *tools.XListType != "map" || len(tools.XListMapKeys) != 2 || tools.XListMapKeys[0] != "server" || tools.XListMapKeys[1] != "tool" {
		t.Fatalf("profile tools schema=%#v, want bounded unique server/tool map", tools)
	}
}

func TestAgentRunChildPlanFingerprintSchemaAndDeepCopy(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "agents.astatide.com_agentruns.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apix.CustomResourceDefinition
	if err := yaml.Unmarshal(body, &crd); err != nil {
		t.Fatal(err)
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	statusSchema := schema.Properties["status"]
	for _, field := range []string{"workSandboxRef", "verifySandboxRef"} {
		childSchema := statusSchema.Properties[field]
		fingerprint, ok := childSchema.Properties["planFingerprint"]
		if !ok {
			t.Fatalf("generated AgentRun CRD %s has no planFingerprint", field)
		}
		if fingerprint.MaxLength == nil || *fingerprint.MaxLength != 71 || fingerprint.Pattern != `^$|^sha256:[a-f0-9]{64}$` {
			t.Fatalf("%s planFingerprint schema=%#v, want bounded lowercase sha256", field, fingerprint)
		}
	}

	fingerprint := "sha256:" + strings.Repeat("f", 64)
	run := &AgentRun{Status: AgentRunStatus{WorkSandboxRef: &ChildRef{Name: "work", Kind: "Sandbox", PlanFingerprint: fingerprint}}}
	copy := run.DeepCopy()
	copy.Status.WorkSandboxRef.PlanFingerprint = "sha256:" + strings.Repeat("e", 64)
	if run.Status.WorkSandboxRef.PlanFingerprint != fingerprint {
		t.Fatal("AgentRun DeepCopy shared ChildRef planFingerprint storage")
	}
}

func TestToolSetDeepCopyDoesNotShareProfilesOrExactArguments(t *testing.T) {
	toolSet := &ToolSet{Spec: ToolSetSpec{
		Servers: []ToolServer{{
			Name:  "github",
			Ref:   "https://mcp.example.test/mcp",
			Tools: []ToolDefinition{{Name: "get_me", Effect: EffectRead, ExactArguments: map[string]apix.JSON{"owner": {Raw: []byte(`"astatide"`)}}}},
		}},
		Profiles: []ToolProfile{
			{Name: ToolProfileExplore, Tools: []ToolRef{{Server: "github", Tool: "get_me"}}},
			{Name: ToolProfileEdit, Tools: []ToolRef{{Server: "github", Tool: "get_me"}}},
			{Name: ToolProfileVerify, Tools: []ToolRef{{Server: "github", Tool: "get_me"}}},
		},
		MaxToolsPerPhase: MaxToolsPerPhase,
	}}
	copy := toolSet.DeepCopy()
	copy.Spec.Profiles[0].Tools[0].Server = "changed"
	copy.Spec.Servers[0].Tools[0].ExactArguments["owner"] = apix.JSON{Raw: []byte(`"other"`)}
	if toolSet.Spec.Profiles[0].Tools[0].Server != "github" {
		t.Fatal("ToolSet DeepCopy shared profile reference storage")
	}
	if string(toolSet.Spec.Servers[0].Tools[0].ExactArguments["owner"].Raw) != `"astatide"` {
		t.Fatal("ToolSet DeepCopy shared exact argument storage")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
