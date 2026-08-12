package resolved

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolvePinsAllReferencedContentAndPromptText(t *testing.T) {
	run, objects := fixture(t)
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	result, err := Resolve(context.Background(), reader, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest == "" || len(result.Canonical) == 0 {
		t.Fatal("resolved result has no content address")
	}
	if result.Snapshot.Task != "fix nil dereference" || result.Snapshot.Instructions != "work carefully" {
		t.Fatalf("resolved text = %q / %q", result.Snapshot.Task, result.Snapshot.Instructions)
	}
	if result.Snapshot.Spec.Output == nil || result.Snapshot.Spec.Output.Mode != v1alpha1.OutputPatch {
		t.Fatalf("omitted output was not canonicalized to patch: %#v", result.Snapshot.Spec.Output)
	}
	if len(result.Snapshot.Policies) != 1 || result.Snapshot.Policies[0].Reference.Name != "default-quality" {
		t.Fatalf("resolved policies = %#v", result.Snapshot.Policies)
	}
	if strings.Contains(string(result.Canonical), "credential-value") {
		t.Fatal("resolved snapshot contains credential material")
	}
	pinned, err := WithBaseSHA(result, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(pinned.Canonical, pinned.Digest)
	if err != nil || decoded.Run.UID != string(run.UID) {
		t.Fatalf("decode immutable snapshot: %#v %v", decoded, err)
	}
	second, err := Resolve(context.Background(), reader, run)
	if err != nil || second.Digest != result.Digest || string(second.Canonical) != string(result.Canonical) {
		t.Fatalf("resolution is not deterministic: %q / %v", second.Digest, err)
	}
}

func TestResolveChangesDigestWhenReferencedConfigChanges(t *testing.T) {
	run, objects := fixture(t)
	scheme := testScheme(t)
	firstReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	first, err := Resolve(context.Background(), firstReader, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if configMap, ok := object.(*corev1.ConfigMap); ok && configMap.Name == "task" {
			configMap.Data[TaskConfigMapKey] = "fix a different bug"
		}
	}
	secondReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	second, err := Resolve(context.Background(), secondReader, run)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("changed referenced prompt retained one digest")
	}
}

func TestResolveFailsClosedForMutableImageAndMissingConfigKey(t *testing.T) {
	run, objects := fixture(t)
	objects[0].(*v1alpha1.Agent).Spec.Runtime.Image = "ghcr.io/astatide/runtime:latest"
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	if _, err := Resolve(context.Background(), reader, run); err == nil {
		t.Fatal("mutable runtime image was accepted")
	}

	run, objects = fixture(t)
	for _, object := range objects {
		if configMap, ok := object.(*corev1.ConfigMap); ok && configMap.Name == "task" {
			delete(configMap.Data, TaskConfigMapKey)
		}
	}
	reader = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	if _, err := Resolve(context.Background(), reader, run); err == nil {
		t.Fatal("missing prompt key was accepted")
	}
}

func TestResolveRevalidatesMutableToolSetSecurityBoundary(t *testing.T) {
	run, objects := fixture(t)
	toolSet := objects[2].(*v1alpha1.ToolSet)
	toolSet.Spec.Servers[0].Ref = "https://127.0.0.1/mcp"
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	if _, err := Resolve(context.Background(), reader, run); err == nil {
		t.Fatal("unsafe ToolSet endpoint was accepted after admission-time mutation")
	}

	run, objects = fixture(t)
	toolSet = objects[2].(*v1alpha1.ToolSet)
	toolSet.Spec.Servers[0].Tools[0].ExactArguments = map[string]apiextensionsv1.JSON{
		"authorization": {Raw: []byte(`"should-not-be-here"`)},
	}
	reader = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	if _, err := Resolve(context.Background(), reader, run); err == nil {
		t.Fatal("credential-like exact argument was accepted after admission-time mutation")
	}
}

func TestResolveRejectsOversizedConfigMapInstructions(t *testing.T) {
	run, objects := fixture(t)
	for _, object := range objects {
		if configMap, ok := object.(*corev1.ConfigMap); ok && configMap.Name == "instructions" {
			configMap.Data[InstructionsMapKey] = strings.Repeat("x", v1alpha1.MaxTaskLength+1)
		}
	}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	if _, err := Resolve(context.Background(), reader, run); err == nil {
		t.Fatal("oversized ConfigMap instructions were accepted")
	}
}

func TestResolveRejectsPlaceholderImageDigest(t *testing.T) {
	run, objects := fixture(t)
	objects[0].(*v1alpha1.Agent).Spec.Runtime.Image = "ghcr.io/astatide/runtime@sha256:" + strings.Repeat("0", 64)
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	if _, err := Resolve(context.Background(), reader, run); err == nil {
		t.Fatal("all-zero placeholder image digest was accepted")
	}
}

func TestResolveBindsCriticRouteAndProvesFamilySeparation(t *testing.T) {
	run, objects := fixture(t)
	gate := objects[1].(*v1alpha1.Gate)
	gate.Spec.Signals = &v1alpha1.GateSignalsSpec{
		ExecutionWeightBasisPoints: 7000,
		Critic: &v1alpha1.GateCriticSignalSpec{
			WeightBasisPoints: 3000,
			ModelRouteRef:     "critic-models",
			MaxFindings:       16,
		},
		MinScoreBasisPoints: 8500,
	}
	critic := &v1alpha1.ModelRoute{
		ObjectMeta: meta("critic-models", run.Namespace, "critic-models-uid", "18"),
		Spec: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{
			Name: "critic", Kind: "anthropic-messages", Model: "claude-sonnet", Family: "anthropic", CredentialRef: "critic", Priority: 1,
		}}},
	}
	objects = append(objects, critic)
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	result, err := Resolve(context.Background(), reader, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.CriticModelRoute == nil || result.Snapshot.References.CriticModelRoute == nil {
		t.Fatalf("critic route was not bound: %#v", result.Snapshot)
	}
	if result.Snapshot.Gate.Signals == nil || result.Snapshot.Gate.Signals.Critic.ModelRouteRef != "critic-models" {
		t.Fatalf("resolved signals=%#v", result.Snapshot.Gate.Signals)
	}
	pinned, err := WithBaseSHA(result, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(pinned.Canonical, pinned.Digest)
	if err != nil || decoded.CriticModelRoute == nil || decoded.References.CriticModelRoute == nil {
		t.Fatalf("decoded critic route=%#v err=%v", decoded.CriticModelRoute, err)
	}
}

func TestResolveRejectsUnprovableCriticRouteSeparation(t *testing.T) {
	tests := []struct {
		name   string
		family string
		model  string
		kind   string
	}{
		{name: "same family", family: "nvidia", model: "different-model", kind: "openrouter-responses"},
		{name: "same provider and model", family: "anthropic", model: "nvidia/nemotron-free", kind: "openrouter-responses"},
		{name: "unknown family", family: "", model: "different-model", kind: "openrouter-responses"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run, objects := fixture(t)
			gate := objects[1].(*v1alpha1.Gate)
			gate.Spec.Signals = &v1alpha1.GateSignalsSpec{
				ExecutionWeightBasisPoints: 7000,
				Critic:                     &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 3000, ModelRouteRef: "critic-models", MaxFindings: 8},
				MinScoreBasisPoints:        8500,
			}
			critic := &v1alpha1.ModelRoute{
				ObjectMeta: meta("critic-models", run.Namespace, "critic-models-uid", "18"),
				Spec: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{
					Name: "critic", Kind: test.kind, Model: test.model, Family: test.family, CredentialRef: "critic", Priority: 1,
				}}},
			}
			reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(objects, critic)...).Build()
			if _, err := Resolve(context.Background(), reader, run); err == nil {
				t.Fatal("unprovably distinct critic route was accepted")
			}
		})
	}
}

func TestResolveNormalizesPolicySetAndBindsExpandedQualityContent(t *testing.T) {
	run, objects := fixture(t)
	agent := objects[0].(*v1alpha1.Agent)
	gate := objects[1].(*v1alpha1.Gate)
	gate.Spec.PolicyRefs = []string{"default-quality"}
	agent.Spec.PolicyRefs = []string{"default-quality"}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	result, err := Resolve(context.Background(), reader, run)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Snapshot.Agent.PolicyRefs; len(got) != 1 || got[0] != "default-quality" {
		t.Fatalf("normalized Agent policy refs = %v", got)
	}
	if result.Snapshot.ContextStrategy.RepoMap == nil || result.Snapshot.Policies[0].Spec.Rules[0].ID != "no-raw-sql" {
		t.Fatalf("expanded quality content missing: %#v", result.Snapshot)
	}

	policy := policyObject(objects)
	policy.Spec.Rules[0].Context = "changed policy context"
	changedPolicyReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	changed, err := Resolve(context.Background(), changedPolicyReader, run)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == result.Digest {
		t.Fatal("policy content change did not change resolved digest")
	}
}

func TestResolveFailsClosedForQualityReferenceMismatchMissingAndDeletion(t *testing.T) {
	tests := []struct {
		name string
		edit func([]client.Object)
	}{
		{
			name: "policy mismatch",
			edit: func(objects []client.Object) {
				objects[0].(*v1alpha1.Agent).Spec.PolicyRefs = []string{"other-quality"}
			},
		},
		{
			name: "policy missing",
			edit: func(objects []client.Object) {
				objects[0].(*v1alpha1.Agent).Spec.PolicyRefs = []string{"missing-policy"}
				objects[1].(*v1alpha1.Gate).Spec.PolicyRefs = []string{"missing-policy"}
			},
		},
		{
			name: "policy terminating",
			edit: func(objects []client.Object) {
				objects[0].(*v1alpha1.Agent).Spec.PolicyRefs = []string{"default-quality"}
				objects[1].(*v1alpha1.Gate).Spec.PolicyRefs = []string{"default-quality"}
				policy := policyObject(objects)
				policy.Finalizers = []string{"test.finalizer"}
				policy.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run, objects := fixture(t)
			test.edit(objects)
			reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			if _, err := Resolve(context.Background(), reader, run); err == nil {
				t.Fatal("unsafe quality reference was accepted")
			}
		})
	}
}

func TestResolveCanonicalizesQualityAndExistingListMapPermutations(t *testing.T) {
	configure := func(objects []client.Object, reverse bool) {
		agent := objects[0].(*v1alpha1.Agent)
		toolSet := objects[2].(*v1alpha1.ToolSet)
		policy := policyObject(objects)
		contextStrategy := objects[4].(*v1alpha1.ContextStrategy)
		agent.Spec.PolicyRefs = []string{"default-quality", "second-quality"}
		gate := objects[1].(*v1alpha1.Gate)
		gate.Spec.PolicyRefs = []string{"default-quality", "second-quality"}
		if reverse {
			agent.Spec.PolicyRefs = []string{"second-quality", "default-quality"}
			gate.Spec.PolicyRefs = []string{"second-quality", "default-quality"}
		}
		agent.Spec.Skills = []v1alpha1.SkillRef{
			{Name: "zeta", Ref: "https://skills.example/zeta", Digest: "sha256:" + strings.Repeat("b", 64)},
			{Name: "alpha", Ref: "https://skills.example/alpha", Digest: "sha256:" + strings.Repeat("c", 64)},
		}
		toolSet.Spec.Servers = []v1alpha1.ToolServer{
			{Name: "zeta", Ref: "https://zeta.example/mcp", Tools: []v1alpha1.ToolDefinition{{Name: "zeta-tool", Effect: v1alpha1.EffectRead}}},
			{Name: "alpha", Ref: "https://alpha.example/mcp", Tools: []v1alpha1.ToolDefinition{{Name: "alpha-tool", Effect: v1alpha1.EffectRead}}},
		}
		toolSet.Spec.Profiles = []v1alpha1.ToolProfile{
			{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "zeta", Tool: "zeta-tool"}, {Server: "alpha", Tool: "alpha-tool"}}},
			{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "alpha", Tool: "alpha-tool"}}},
			{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "zeta", Tool: "zeta-tool"}}},
		}
		policy.Spec.Rules = []v1alpha1.PolicyRule{
			{ID: "z-rule", Severity: v1alpha1.PolicySeverityAdvisory, Context: "z"},
			{ID: "a-rule", Severity: v1alpha1.PolicySeverityBlocking, Context: "a", Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/a.sh", Expect: "exit0"}},
		}
		contextStrategy.Spec.Symbols = &v1alpha1.ContextSymbolsSpec{Kind: "lsp-serena", Languages: []string{"typescript", "go"}}
		if !reverse {
			sort.Slice(agent.Spec.Skills, func(i, j int) bool { return agent.Spec.Skills[i].Name < agent.Spec.Skills[j].Name })
			sort.Slice(toolSet.Spec.Servers, func(i, j int) bool { return toolSet.Spec.Servers[i].Name < toolSet.Spec.Servers[j].Name })
			sort.Slice(toolSet.Spec.Profiles, func(i, j int) bool { return toolSet.Spec.Profiles[i].Name < toolSet.Spec.Profiles[j].Name })
			sort.Slice(policy.Spec.Rules, func(i, j int) bool { return policy.Spec.Rules[i].ID < policy.Spec.Rules[j].ID })
			contextStrategy.Spec.Symbols.Languages = []string{"go", "typescript"}
		} else {
			sort.Slice(toolSet.Spec.Profiles, func(i, j int) bool { return toolSet.Spec.Profiles[i].Name > toolSet.Spec.Profiles[j].Name })
			for index := range toolSet.Spec.Profiles {
				sort.Slice(toolSet.Spec.Profiles[index].Tools, func(i, j int) bool {
					if toolSet.Spec.Profiles[index].Tools[i].Server != toolSet.Spec.Profiles[index].Tools[j].Server {
						return toolSet.Spec.Profiles[index].Tools[i].Server > toolSet.Spec.Profiles[index].Tools[j].Server
					}
					return toolSet.Spec.Profiles[index].Tools[i].Tool > toolSet.Spec.Profiles[index].Tools[j].Tool
				})
			}
		}
	}

	firstRun, firstObjects := fixture(t)
	configure(firstObjects, false)
	secondRun, secondObjects := fixture(t)
	configure(secondObjects, true)
	firstReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(firstObjects...).Build()
	secondReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secondObjects...).Build()
	first, err := Resolve(context.Background(), firstReader, firstRun)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(context.Background(), secondReader, secondRun)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || string(first.Canonical) != string(second.Canonical) {
		t.Fatalf("permuted list fields changed canonical resolution: %s/%s", first.Digest, second.Digest)
	}
}

func TestNormalizeToolSetSpecSortsProfilesAndKeepsInputImmutable(t *testing.T) {
	original := v1alpha1.ToolSetSpec{
		Servers: []v1alpha1.ToolServer{
			{Name: "zeta", Ref: "https://zeta.example/mcp", Tools: []v1alpha1.ToolDefinition{{Name: "zeta-tool", Effect: v1alpha1.EffectRead}}},
			{Name: "alpha", Ref: "https://alpha.example/mcp", Tools: []v1alpha1.ToolDefinition{{Name: "alpha-tool", Effect: v1alpha1.EffectRead}}},
		},
		Profiles: []v1alpha1.ToolProfile{
			{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "zeta", Tool: "zeta-tool"}}},
			{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "zeta", Tool: "zeta-tool"}, {Server: "alpha", Tool: "alpha-tool"}}},
		},
		MaxToolsPerPhase: v1alpha1.MaxToolsPerPhase,
	}
	normalized := NormalizeToolSetSpec(original)
	if original.Servers[0].Name != "zeta" || original.Profiles[0].Name != v1alpha1.ToolProfileVerify || original.Profiles[1].Tools[0].Server != "zeta" {
		t.Fatal("ToolSet normalization mutated its input")
	}
	if normalized.Servers[0].Name != "alpha" || normalized.Profiles[0].Name != v1alpha1.ToolProfileExplore || normalized.Profiles[0].Tools[0].Server != "alpha" {
		t.Fatalf("ToolSet normalization order=%#v", normalized)
	}
	normalized.Profiles[0].Tools[0].Tool = "changed"
	if original.Profiles[1].Tools[1].Tool != "alpha-tool" {
		t.Fatal("normalized ToolSet shares profile reference storage with input")
	}
}

func TestValidateToolSetSpecRejectsUnsafeOrUnresolvedProfiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.ToolSetSpec)
	}{
		{name: "missing max", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.MaxToolsPerPhase = 0 }},
		{name: "empty profiles", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Profiles = nil }},
		{name: "missing verify profile", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Profiles = spec.Profiles[:2] }},
		{name: "empty active profile", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Profiles[0].Tools = nil }},
		{name: "duplicate profile ref", mutate: func(spec *v1alpha1.ToolSetSpec) {
			spec.Profiles[0].Tools = append(spec.Profiles[0].Tools, spec.Profiles[0].Tools[0])
		}},
		{name: "unknown server", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Profiles[0].Tools[0].Server = "missing" }},
		{name: "unknown tool", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Profiles[0].Tools[0].Tool = "missing" }},
		{name: "profile over max", mutate: func(spec *v1alpha1.ToolSetSpec) {
			spec.MaxToolsPerPhase = 1
			spec.Profiles[0].Tools = append(spec.Profiles[0].Tools, v1alpha1.ToolRef{Server: "github", Tool: "list_issues"})
		}},
		{name: "unsafe server name", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Servers[0].Name = "../server" }},
		{name: "unsafe tool name", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Servers[0].Tools[0].Name = "tool/name" }},
		{name: "duplicate server", mutate: func(spec *v1alpha1.ToolSetSpec) { spec.Servers = append(spec.Servers, *spec.Servers[0].DeepCopy()) }},
		{name: "duplicate tool definition", mutate: func(spec *v1alpha1.ToolSetSpec) {
			spec.Servers[0].Tools = append(spec.Servers[0].Tools, *spec.Servers[0].Tools[0].DeepCopy())
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validToolSetSpec()
			test.mutate(&spec)
			if err := ValidateToolSetSpec(spec); err == nil {
				t.Fatal("invalid ToolSet was accepted")
			}
		})
	}
}

func TestResolveChangesDigestWhenToolProfileChanges(t *testing.T) {
	run, objects := fixture(t)
	firstReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	first, err := Resolve(context.Background(), firstReader, run)
	if err != nil {
		t.Fatal(err)
	}
	toolSet := objects[2].(*v1alpha1.ToolSet)
	toolSet.Spec.Profiles[0].Tools = []v1alpha1.ToolRef{{Server: "github", Tool: "list_issues"}}
	secondReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	second, err := Resolve(context.Background(), secondReader, run)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("changed ToolSet profile retained the same resolved digest")
	}
}

func TestDecodeRejectsPreviousSnapshotSchemaVersion(t *testing.T) {
	run, objects := fixture(t)
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	result, err := Resolve(context.Background(), reader, run)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := WithBaseSHA(result, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(pinned.Canonical, &value); err != nil {
		t.Fatal(err)
	}
	value["schemaVersion"] = 1
	body, err := canonical.CanonicalizeResolvedSpec(value)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(body, digest); err == nil {
		t.Fatal("previous resolved snapshot schema version was accepted")
	}
}

func fixture(t *testing.T) (*v1alpha1.AgentRun, []client.Object) {
	t.Helper()
	ns := "agw-runs"
	taskRef := "task"
	instructionRef := "instructions"
	digest := strings.Repeat("a", 64)
	run := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: ns, UID: types.UID("run-uid"), Generation: 3}, Spec: v1alpha1.AgentRunSpec{AgentRef: "agent", GateRef: "gate", Task: v1alpha1.TaskSpec{ConfigMapRef: &taskRef}}}
	agent := &v1alpha1.Agent{ObjectMeta: meta("agent", ns, "agent-uid", "11"), Spec: v1alpha1.AgentSpec{Runtime: v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/runtime@sha256:" + digest}, Instructions: v1alpha1.InstructionsSpec{ConfigMapRef: &instructionRef}, ToolSetRef: "tools", ModelRouteRef: "models", ContextStrategyRef: "context"}}
	gate := &v1alpha1.Gate{ObjectMeta: meta("gate", ns, "gate-uid", "12"), Spec: v1alpha1.GateSpec{Verify: v1alpha1.VerifySpec{Image: "ghcr.io/astatide/verify@sha256:" + digest}, PolicyRefs: []string{"default-quality"}}}
	agent.Spec.PolicyRefs = []string{"default-quality"}
	tools := &v1alpha1.ToolSet{ObjectMeta: meta("tools", ns, "tools-uid", "13"), Spec: v1alpha1.ToolSetSpec{
		Servers: []v1alpha1.ToolServer{{
			Name: "github", Ref: "https://mcp.example.test/mcp", CredentialsRef: "github-app",
			Tools: []v1alpha1.ToolDefinition{{Name: "get_me", Effect: v1alpha1.EffectRead}, {Name: "list_issues", Effect: v1alpha1.EffectRead}},
		}},
		Profiles: []v1alpha1.ToolProfile{
			{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}, {Server: "github", Tool: "list_issues"}}},
			{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "list_issues"}}},
			{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}}},
		},
		MaxToolsPerPhase: v1alpha1.MaxToolsPerPhase,
	}}
	models := &v1alpha1.ModelRoute{ObjectMeta: meta("models", ns, "models-uid", "14"), Spec: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "openrouter", Kind: "openrouter-responses", Model: "nvidia/nemotron-free", Family: "nvidia", CredentialRef: "openrouter", Priority: 1}}}}
	contextStrategy := &v1alpha1.ContextStrategy{ObjectMeta: meta("context", ns, "context-uid", "15"), Spec: v1alpha1.ContextStrategySpec{RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 8000}, Budget: v1alpha1.ContextBudgetSpec{TotalTokens: 40000, MaxBytes: 8 << 20}}}
	policy := &v1alpha1.Policy{ObjectMeta: meta("default-quality", ns, "policy-uid", "16"), Spec: v1alpha1.PolicySpec{Rules: []v1alpha1.PolicyRule{{ID: "no-raw-sql", Severity: v1alpha1.PolicySeverityBlocking, Context: "use the repository abstraction", Check: &v1alpha1.PolicyCheck{Kind: "script", Script: "policies/no-raw-sql.sh", Expect: "exit0"}}}}}
	secondPolicy := &v1alpha1.Policy{ObjectMeta: meta("second-quality", ns, "second-policy-uid", "17"), Spec: v1alpha1.PolicySpec{Rules: []v1alpha1.PolicyRule{{ID: "second-rule", Severity: v1alpha1.PolicySeverityAdvisory, Context: "second quality rule"}}}}
	return run, []client.Object{agent, gate, tools, models, contextStrategy, policy, secondPolicy,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: ns}, Data: map[string]string{TaskConfigMapKey: "fix nil dereference"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "instructions", Namespace: ns}, Data: map[string]string{InstructionsMapKey: "work carefully", "credential": "credential-value"}},
	}
}

func validToolSetSpec() v1alpha1.ToolSetSpec {
	return v1alpha1.ToolSetSpec{
		Servers: []v1alpha1.ToolServer{{
			Name: "github", Ref: "https://mcp.example.test/mcp",
			Tools: []v1alpha1.ToolDefinition{{Name: "get_me", Effect: v1alpha1.EffectRead}, {Name: "list_issues", Effect: v1alpha1.EffectRead}},
		}},
		Profiles: []v1alpha1.ToolProfile{
			{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}}},
			{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "list_issues"}}},
			{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}}},
		},
		MaxToolsPerPhase: v1alpha1.MaxToolsPerPhase,
	}
}

func meta(name, namespace, uid, rv string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid), ResourceVersion: rv, Generation: 1}
}

func policyObject(objects []client.Object) *v1alpha1.Policy {
	for _, object := range objects {
		if policy, ok := object.(*v1alpha1.Policy); ok {
			return policy
		}
	}
	panic("fixture has no Policy")
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestValidateOutputSpecAllowsBothToDeriveItsPullRequestTarget(t *testing.T) {
	if err := validateOutputSpec(&v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputBoth}); err != nil {
		t.Fatalf("both output without a pre-existing target was rejected: %v", err)
	}
	if err := validateOutputSpec(&v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings}); err == nil {
		t.Fatal("findings-only output without a pre-existing target was accepted")
	}
	if err := validateOutputPublication(&v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputBoth}, v1alpha1.PublishNone); err == nil {
		t.Fatal("findings output with publish.mode=none was accepted")
	}
}
