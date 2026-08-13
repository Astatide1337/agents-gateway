package workload

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/airlock"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBuildIsDeterministicAndUsesResolvedRuntimeImage(t *testing.T) {
	snapshot := testSnapshot()
	options := testOptions(snapshot)
	first, err := Build(snapshot, options)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := Build(snapshot, options)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical inputs produced different Sandbox objects")
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("identical inputs produced different serialized manifests")
	}
	if first.Spec.PodTemplate.Spec.Containers[0].Image != snapshot.Agent.Runtime.Image {
		t.Fatalf("agent image=%q, want resolved runtime image %q", first.Spec.PodTemplate.Spec.Containers[0].Image, snapshot.Agent.Runtime.Image)
	}
	if first.Annotations[sandbox.SpecDigestAnnotationKey] == "" || first.Labels["agents.astatide.com/spec-digest-short"] == "" {
		t.Fatalf("deterministic spec identity is missing: labels=%v annotations=%v", first.Labels, first.Annotations)
	}
	if got := first.Annotations[TrustedPhaseSupervisorAnnotationKey]; got != TrustedPhaseSupervisorDisabled {
		t.Fatalf("trusted phase supervisor annotation=%q, want %q", got, TrustedPhaseSupervisorDisabled)
	}
}

func TestBuildDefaultAgentGatewaySidecarIsDisabled(t *testing.T) {
	snapshot := testSnapshot()
	options := testOptions(snapshot)
	if options.AgentGateway != (AgentGatewaySidecarOptions{}) {
		t.Fatalf("test options unexpectedly enable agentgateway: %#v", options.AgentGateway)
	}
	manifest, err := Build(snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	containers := manifest.Spec.PodTemplate.Spec.Containers
	if len(containers) != 2 || containers[0].Name != "agent" || containers[1].Name != "broker" {
		t.Fatalf("default regular containers=%v, want exactly agent -> broker", containerNames(containers))
	}
	if lockdown := manifest.Spec.PodTemplate.Spec.InitContainers[len(manifest.Spec.PodTemplate.Spec.InitContainers)-1]; lockdown.Name != "lockdown" || lockdown.Args[1] != lockdownScript {
		t.Fatalf("default lockdown contract changed: %#v", lockdown)
	}
}

func TestBuildAgentGatewaySidecarConfigurationFailsClosed(t *testing.T) {
	snapshot := testSnapshot()
	base := testOptions(snapshot)
	digest := "sha256:" + strings.Repeat("c", 64)
	valid := AgentGatewaySidecarOptions{
		Enabled:            true,
		Image:              AgentGatewayImagePrefix + strings.Repeat("d", 64),
		ConfigMapName:      "agw-run-agentgateway-config",
		CapabilityVersion:  AgentGatewaySidecarVersion,
		CapabilityVerified: true,
		EvidenceDigest:     digest,
		ConfigDigest:       digest,
	}
	cases := []struct {
		name      string
		configure func(*Options)
		want      error
	}{
		{
			name: "disabled with image is rejected",
			configure: func(options *Options) {
				options.AgentGateway.Image = valid.Image
			},
			want: ErrInvalidInput,
		},
		{
			name: "enabled without required fields is rejected",
			configure: func(options *Options) {
				options.AgentGateway.Enabled = true
			},
			want: ErrInvalidInput,
		},
		{
			name: "enabled tag is rejected",
			configure: func(options *Options) {
				options.AgentGateway = valid
				options.AgentGateway.Image = "cr.agentgateway.dev/agentgateway:v1.4.1"
			},
			want: ErrInvalidInput,
		},
		{
			name: "enabled without capability evidence is rejected",
			configure: func(options *Options) {
				options.AgentGateway = valid
				options.AgentGateway.CapabilityVerified = false
			},
			want: ErrInvalidInput,
		},
		{
			name: "fully specified enablement remains rejected until adapter proof",
			configure: func(options *Options) {
				options.AgentGateway = valid
			},
			want: ErrAgentGatewaySidecarUnsupported,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := base
			tc.configure(&options)
			if _, err := Build(snapshot, options); !errors.Is(err, tc.want) {
				t.Fatalf("Build error=%v, want errors.Is(..., %v)", err, tc.want)
			}
		})
	}
}

func TestBuildProducesADR006ADR007WorkTopology(t *testing.T) {
	snapshot := testSnapshot()
	manifest, err := Build(snapshot, testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	pod := manifest.Spec.PodTemplate.Spec

	if manifest.APIVersion != sandboxv1beta1.GroupVersion.String() || manifest.Kind != sandboxv1beta1.SandboxKind {
		t.Fatalf("unexpected upstream identity: %s/%s", manifest.APIVersion, manifest.Kind)
	}
	wantName, err := sandbox.ChildName(types.UID(snapshot.Run.UID), sandbox.RoleWork)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != wantName || manifest.Namespace != snapshot.Run.Namespace {
		t.Fatalf("child identity=%s/%s, want %s/%s", manifest.Namespace, manifest.Name, snapshot.Run.Namespace, wantName)
	}
	if pod.HostUsers == nil || *pod.HostUsers {
		t.Fatal("hostUsers is not explicitly false")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("automountServiceAccountToken is not explicitly false")
	}
	if pod.ShareProcessNamespace == nil || *pod.ShareProcessNamespace {
		t.Fatal("shareProcessNamespace must be explicitly false until a trusted phase supervisor contract exists")
	}
	if got := manifest.Annotations[TrustedPhaseSupervisorAnnotationKey]; got != TrustedPhaseSupervisorDisabled {
		t.Fatalf("trusted phase supervisor annotation=%q, want %q", got, TrustedPhaseSupervisorDisabled)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("pod does not use RuntimeDefault seccomp")
	}
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 1000 {
		t.Fatalf("pod fsGroup=%v, want 1000 for UID-1000 writable volumes", pod.SecurityContext.FSGroup)
	}
	if pod.SecurityContext.FSGroupChangePolicy == nil || *pod.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("pod fsGroupChangePolicy=%v, want OnRootMismatch", pod.SecurityContext.FSGroupChangePolicy)
	}
	if pod.NodeSelector[NodeLabelKey] != NodeLabelValue || len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != NodeLabelKey || pod.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("unexpected agent node placement: selector=%v tolerations=%v", pod.NodeSelector, pod.Tolerations)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Fatal("work pod lifecycle defaults are not fail-closed")
	}

	if len(pod.InitContainers) != 4 || pod.InitContainers[0].Name != "clone" || pod.InitContainers[1].Name != "skills" || pod.InitContainers[2].Name != "context" || pod.InitContainers[3].Name != "lockdown" {
		t.Fatalf("init order=%v, want clone -> skills -> context -> lockdown", containerNames(pod.InitContainers))
	}
	if len(pod.Containers) != 2 || pod.Containers[0].Name != "agent" || pod.Containers[1].Name != "broker" {
		t.Fatalf("regular containers=%v, want agent -> broker", containerNames(pod.Containers))
	}
	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, env := range container.Env {
			if env.Name == "AGW_TRUSTED_PHASE_SOCKET" || strings.HasPrefix(env.Name, "AGW_PHASE_") || strings.Contains(strings.ToLower(env.Name), "phase_transition_token") {
				t.Fatalf("unwired trusted phase credential reached container %q through env %q", container.Name, env.Name)
			}
		}
	}
	if got := pod.InitContainers[3].Args; len(got) != 2 || got[0] != lockdownShellArgs || got[1] != lockdownScript || pod.InitContainers[3].Command[0] != lockdownShell {
		t.Fatalf("lockdown command is not the fixed owner-rule script: command=%v args=%v", pod.InitContainers[3].Command, got)
	}
	for _, fragment := range []string{
		"iptables-restore --wait",
		"ip6tables-restore --wait",
		"--uid-owner 1337",
		"-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8081 -j ACCEPT",
		"-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d ::1 --dport 8081 -j ACCEPT",
	} {
		if !strings.Contains(lockdownScript, fragment) {
			t.Fatalf("lockdown script is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"-o lo -j ACCEPT",
		"--uid-owner 1000 -j ACCEPT",
		"--uid-owner 1000 -p udp",
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8080",
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8082",
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8083",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8080",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8082",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8083",
	} {
		if strings.Contains(lockdownScript, forbidden) {
			t.Fatalf("lockdown script contains forbidden rule %q", forbidden)
		}
	}
	if got, err := airlock.Render(airlock.DirectPolicy()); err != nil || got != lockdownScript {
		t.Fatalf("workload script does not match the direct airlock renderer: err=%v", err)
	}

	if len(manifest.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("PVC templates=%d, want one workspace template", len(manifest.Spec.VolumeClaimTemplates))
	}
	claim := manifest.Spec.VolumeClaimTemplates[0]
	if claim.Name != WorkspaceVolumeName || len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("unexpected workspace PVC: %#v", claim)
	}
	requestedStorage := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	limitedStorage := claim.Spec.Resources.Limits[corev1.ResourceStorage]
	if requestedStorage.String() != "8Gi" || limitedStorage.String() != "8Gi" {
		t.Fatalf("workspace PVC resources=%v, want 8Gi request and limit", claim.Spec.Resources)
	}
	if volumeByName(pod.Volumes, SkillsVolumeName).EmptyDir == nil || volumeByName(pod.Volumes, ContextVolumeName).EmptyDir == nil {
		t.Fatal("skills and context are not emptyDir volumes")
	}

	secretByName := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			secretByName[volume.Name] = volume
			if len(volume.Secret.Items) == 0 {
				t.Fatalf("Secret volume %q exposes the whole Secret", volume.Name)
			}
		}
	}
	if len(secretByName[CloneSecretVolumeName].Secret.Items) != 1 || secretByName[CloneSecretVolumeName].Secret.Items[0].Key != "github-token" {
		t.Fatalf("clone Secret projection=%#v", secretByName[CloneSecretVolumeName].Secret.Items)
	}
	if len(secretByName[SkillsSecretVolumeName].Secret.Items) != 1 || secretByName[SkillsSecretVolumeName].Secret.Items[0].Key != "skills-token" {
		t.Fatalf("skills Secret projection=%#v", secretByName[SkillsSecretVolumeName].Secret.Items)
	}
	if len(secretByName[BrokerSecretVolumeName].Secret.Items) != 2 || secretByName[BrokerSecretVolumeName].Secret.Items[0].Key != "mcp-token" || secretByName[BrokerSecretVolumeName].Secret.Items[1].Key != "model-token" {
		t.Fatalf("broker Secret projection=%#v", secretByName[BrokerSecretVolumeName].Secret.Items)
	}
	if items := secretByName[ArtifactSecretVolumeName].Secret.Items; len(items) != 3 || items[0].Key != artifactauth.AccessKeyIDKey || items[1].Key != artifactauth.SecretAccessKeyKey || items[2].Key != artifactauth.SessionTokenKey {
		t.Fatalf("artifact Secret projection=%#v", items)
	}
	clone := containerByName(pod.InitContainers, "clone")
	if clone.SecurityContext.RunAsUser == nil || *clone.SecurityContext.RunAsUser != 0 || clone.SecurityContext.Privileged == nil || *clone.SecurityContext.Privileged {
		t.Fatalf("clone must be unprivileged namespace-root: %#v", clone.SecurityContext)
	}
	if len(clone.SecurityContext.Capabilities.Add) != 1 || clone.SecurityContext.Capabilities.Add[0] != corev1.Capability("CHOWN") {
		t.Fatalf("clone capabilities=%v, want only CHOWN", clone.SecurityContext.Capabilities)
	}
	if !hasExactDirectSecretItemMount(clone, CloneSecretVolumeName, CloneTokenFile, "token") {
		t.Fatal("clone is missing its direct-file credential projection")
	}
	skills := containerByName(pod.InitContainers, "skills")
	if !hasExactDirectSecretItemMount(skills, SkillsSecretVolumeName, SkillsTokenFile, "token") {
		t.Fatal("skills is missing its direct-file credential projection")
	}

	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		assertSecureContainer(t, container)
	}
	contextInit := containerByName(pod.InitContainers, "context")
	if contextInit.SecurityContext == nil || contextInit.SecurityContext.RunAsUser == nil || *contextInit.SecurityContext.RunAsUser != 0 {
		t.Fatalf("context init must run as namespaced root to seal the output: %#v", contextInit.SecurityContext)
	}
	if hasSecretMount(contextInit, secretByName) || hasSecretEnvRef(contextInit) {
		t.Fatal("context init can see a Secret")
	}
	if mount := namedMount(contextInit, ContextVolumeName, ContextMountPath); mount == nil || mount.ReadOnly {
		t.Fatalf("context output mount=%#v, want writable", mount)
	}
	if mount := namedSubPathMount(contextInit, WorkspaceVolumeName, WorkspaceMountPath+"/"+WorkspaceBaseSubPath, WorkspaceBaseSubPath); mount == nil || !mount.ReadOnly {
		t.Fatalf("context base mount=%#v, want read-only", mount)
	}
	lockdownSecurity := pod.InitContainers[3].SecurityContext
	if lockdownSecurity.RunAsUser == nil || *lockdownSecurity.RunAsUser != 0 || lockdownSecurity.RunAsGroup == nil || *lockdownSecurity.RunAsGroup != 0 {
		t.Fatalf("lockdown identity=%v, want UID/GID 0", lockdownSecurity)
	}
	if len(lockdownSecurity.Capabilities.Add) != 1 || lockdownSecurity.Capabilities.Add[0] != corev1.Capability("NET_ADMIN") {
		t.Fatalf("lockdown capabilities=%v, want only NET_ADMIN", lockdownSecurity.Capabilities)
	}

	agent := containerByName(pod.Containers, "agent")
	if agent.SecurityContext.RunAsUser == nil || *agent.SecurityContext.RunAsUser != 1000 || agent.SecurityContext.RunAsGroup == nil || *agent.SecurityContext.RunAsGroup != 1000 {
		t.Fatalf("agent identity=%v, want UID/GID 1000", agent.SecurityContext)
	}
	if hasSecretMount(agent, secretByName) || hasSecretEnvRef(agent) {
		t.Fatal("agent can see a Secret volume or SecretKeyRef")
	}
	if mount := namedMount(agent, WorkspaceVolumeName, WorkspaceMountPath); mount != nil {
		t.Fatal("agent must not receive the entire workspace mount")
	}
	if mount := namedSubPathMount(agent, WorkspaceVolumeName, WorkspaceMountPath+"/"+WorkspaceRepoSubPath, WorkspaceRepoSubPath); mount == nil || mount.ReadOnly {
		t.Fatalf("agent repo mount=%#v, want writable repo subPath", mount)
	}
	if mount := namedSubPathMount(agent, WorkspaceVolumeName, WorkspaceMountPath+"/"+WorkspaceBaseSubPath, WorkspaceBaseSubPath); mount == nil || !mount.ReadOnly {
		t.Fatalf("agent base mount=%#v, want read-only base subPath", mount)
	}
	workspaceMounts := 0
	for _, mount := range agent.VolumeMounts {
		if mount.Name == WorkspaceVolumeName {
			workspaceMounts++
		}
	}
	if workspaceMounts != 2 {
		t.Fatalf("agent workspace mount count=%d, want exactly repo and base subPath mounts", workspaceMounts)
	}
	if mount := namedMount(agent, SkillsVolumeName, SkillsMountPath); mount == nil || !mount.ReadOnly {
		t.Fatalf("agent skills mount=%#v, want read-only", mount)
	}
	for subPath, mountPath := range map[string]string{
		"AGENTS.md":                     WorkspaceMountPath + "/AGENTS.md",
		".agents":                       WorkspaceMountPath + "/.agents",
		".agw/context":                  WorkspaceMountPath + "/.agw/context",
		contextmaterializer.RefFileName: WorkspaceMountPath + "/" + contextmaterializer.RefFileName,
	} {
		if mount := namedSubPathMount(agent, ContextVolumeName, mountPath, subPath); mount == nil || !mount.ReadOnly {
			t.Fatalf("agent context mount=%#v for %q, want read-only subPath", mount, subPath)
		}
	}
	if envValue(agent, "AGW_CONTEXT_PACK_REF_FILE") != WorkspaceMountPath+"/"+contextmaterializer.RefFileName {
		t.Fatalf("context ref path=%q", envValue(agent, "AGW_CONTEXT_PACK_REF_FILE"))
	}
	if mount := namedMount(containerByName(pod.InitContainers, "skills"), SkillsVolumeName, SkillsMountPath); mount == nil || mount.ReadOnly {
		t.Fatalf("skills init mount=%#v, want writable for UID 1000", mount)
	}
	if strings.Contains(string(mustJSON(t, agent)), testOptions(snapshot).SecretName) || strings.Contains(string(mustJSON(t, agent)), "github-token") || strings.Contains(string(mustJSON(t, agent)), "skills-token") {
		t.Fatal("agent manifest contains a credential Secret name or key")
	}
	if got := envValue(agent, "AGW_CODEX_MODEL"); got != "nvidia/nemotron-3-ultra-550b-a55b:free" {
		t.Fatalf("AGW_CODEX_MODEL=%q, want deterministic primary provider model", got)
	}
	if got := envValue(agent, "AGW_CODEX_MAX_RUNTIME"); got != snapshot.Spec.Limits.Timeout {
		t.Fatalf("AGW_CODEX_MAX_RUNTIME=%q, want %q", got, snapshot.Spec.Limits.Timeout)
	}
	if got := envValue(agent, "AGW_CODEX_WORKSPACE"); got != WorkspaceMountPath+"/repo" {
		t.Fatalf("AGW_CODEX_WORKSPACE=%q", got)
	}
	if got := envValue(agent, "AGW_BASE_SHA"); got != snapshot.BaseSHA {
		t.Fatalf("AGW_BASE_SHA=%q, want immutable base %q", got, snapshot.BaseSHA)
	}
	if got := envValue(agent, "AGW_AGENT_REF"); got != snapshot.Spec.AgentRef {
		t.Fatalf("AGW_AGENT_REF=%q, want %q", got, snapshot.Spec.AgentRef)
	}
	broker := containerByName(pod.Containers, "broker")
	if !hasDirectSecretItemMounts(broker, BrokerSecretVolumeName, BrokerSecretMountPath, len(testOptions(snapshot).BrokerSecretKeys)) {
		t.Fatal("broker is missing its selected direct-file Secret projections")
	}
	if !hasDirectArtifactMounts(broker) {
		t.Fatal("broker is missing direct-file artifact credential projections")
	}
	if envValue(broker, "AGW_OBJECT_STORE_PREFIX") != "agents-gateway/v3" || envValue(broker, "AGW_EFFECTS_PREFIX") != "runs/"+snapshot.Run.UID+"/effects" {
		t.Fatal("broker object-store paths are not fenced to the configured root and run")
	}
	if envValue(broker, "AGW_BASE_SHA") != snapshot.BaseSHA {
		t.Fatalf("broker base SHA=%q, want immutable base %q", envValue(broker, "AGW_BASE_SHA"), snapshot.BaseSHA)
	}
	if got := envValue(broker, "AGW_PRICING_JSON"); got != "" {
		t.Fatalf("unpriced test route unexpectedly emitted pricing=%q", got)
	}
	if envValue(broker, "AGW_MAX_TOOL_CALLS") != "60" || envValue(broker, "AGW_MAX_COST_USD") != "2.00" || envValue(broker, "AGW_MAX_MODEL_TOKENS") != "0" {
		t.Fatalf("broker run limits are not copied from the immutable snapshot: toolCalls=%q cost=%q tokens=%q", envValue(broker, "AGW_MAX_TOOL_CALLS"), envValue(broker, "AGW_MAX_COST_USD"), envValue(broker, "AGW_MAX_MODEL_TOKENS"))
	}
	if hasSecretEnvRef(broker) {
		t.Fatal("broker unexpectedly uses an unbounded SecretKeyRef")
	}
	if !hasNamedMount(broker, WorkspaceVolumeName, WorkspaceMountPath) {
		t.Fatal("broker must see the workspace read-only")
	}
	if mount := namedMount(broker, ContextVolumeName, ContextBrokerMountPath); mount == nil || !mount.ReadOnly {
		t.Fatalf("broker context mount=%#v, want dedicated read-only context volume", mount)
	}
	if envValue(broker, "AGW_CONTEXT_PACK_DIR") != ContextBrokerMountPath {
		t.Fatalf("broker context path=%q, want %q", envValue(broker, "AGW_CONTEXT_PACK_DIR"), ContextBrokerMountPath)
	}
	if envValue(broker, "AGW_BROKER_SCRATCH") != "" {
		t.Fatal("broker should use the content-addressed object store for tool-result storage")
	}
}

func TestBuildEmitsDeclarativeModelPricingForBroker(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.ModelRoute.Providers[0].Pricing = &v1alpha1.ModelPricing{InputMicrosPerToken: 3, OutputMicrosPerToken: 7}
	manifest, err := Build(snapshot, testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	broker := containerByName(manifest.Spec.PodTemplate.Spec.Containers, "broker")
	if got := envValue(broker, "AGW_PRICING_JSON"); got != `{"openrouter":{"inputMicrosPerToken":3,"outputMicrosPerToken":7}}` {
		t.Fatalf("pricing JSON=%q", got)
	}
}

func TestBuildClaudeCodeUsesClaudeContractAndRejectsKnownImageMismatch(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Agent.Runtime.Harness = v1alpha1.HarnessClaudeCode
	snapshot.Agent.Runtime.Image = "ghcr.io/astatide/agw-runtime-claude@sha256:" + strings.Repeat("a", 64)
	snapshot.ModelRoute.Providers[0].Kind = "openrouter-anthropic-messages"
	manifest, err := Build(snapshot, testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	agent := containerByName(manifest.Spec.PodTemplate.Spec.Containers, "agent")
	if envValue(agent, "AGW_CLAUDE_MODEL") != "nvidia/nemotron-3-ultra-550b-a55b:free" || envValue(agent, "AGW_CLAUDE_WORKSPACE") != WorkspaceMountPath+"/repo" {
		t.Fatalf("Claude env=%v", agent.Env)
	}
	if envValue(agent, "AGW_CLAUDE_ANTHROPIC_BASE_URL") != "http://127.0.0.1:8081/api" || envValue(agent, "AGW_CODEX_MODEL") != "" {
		t.Fatalf("Claude broker env=%v", agent.Env)
	}

	snapshot.Agent.Runtime.Harness = v1alpha1.HarnessCodex
	if _, err := Build(snapshot, testOptions(snapshot)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("known Claude image mismatch error=%v", err)
	}
}

func TestWorkspaceClaimNameEncapsulatesUpstreamContract(t *testing.T) {
	got, err := WorkspaceClaimName("agw-work-0123456789abcdef0123")
	if err != nil || got != "workspace-agw-work-0123456789abcdef0123" {
		t.Fatalf("WorkspaceClaimName=%q, %v", got, err)
	}
	if _, err := WorkspaceClaimName("../foreign"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe Sandbox name error=%v", err)
	}
}

func TestBuildSelectsPrimaryModelDeterministically(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.ModelRoute.Providers = []v1alpha1.ModelProvider{
		{Name: "zeta", Kind: "openai-responses", Model: "model-z", Priority: 2},
		{Name: "beta", Kind: "openai-responses", Model: "model-b", Priority: 1},
		{Name: "alpha", Kind: "openai-responses", Model: "model-a", Priority: 1},
	}
	manifest, err := Build(snapshot, testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(containerByName(manifest.Spec.PodTemplate.Spec.Containers, "agent"), "AGW_CODEX_MODEL"); got != "model-a" {
		t.Fatalf("AGW_CODEX_MODEL=%q, want model-a", got)
	}
}

func TestBuildRejectsMissingModelProvider(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.ModelRoute.Providers = nil
	if _, err := Build(snapshot, testOptions(snapshot)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Build error=%v, want ErrInvalidInput", err)
	}
}

func TestBuildSupportsCredentialFreeBrokerAndNoSkills(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Agent.Skills = nil
	snapshot.ToolSet.Servers[0].CredentialsRef = ""
	snapshot.ModelRoute.Providers[0].CredentialRef = ""
	options := testOptions(snapshot)
	options.SkillsImage = ""
	options.SkillsEndpoint = ""
	options.SkillsSecretKey = ""
	options.BrokerSecretKeys = nil

	manifest, err := Build(snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	pod := manifest.Spec.PodTemplate.Spec
	if got := containerNames(pod.InitContainers); !reflect.DeepEqual(got, []string{"clone", "context", "lockdown"}) {
		t.Fatalf("credential-free init order=%v, want clone -> context -> lockdown", got)
	}
	for _, volume := range pod.Volumes {
		if volume.Name == SkillsSecretVolumeName || volume.Name == BrokerSecretVolumeName {
			t.Fatalf("unused Secret volume %q was materialized", volume.Name)
		}
	}
	broker := containerByName(pod.Containers, "broker")
	if hasNamedMount(broker, BrokerSecretVolumeName, BrokerSecretMountPath) {
		t.Fatal("credential-free broker received a Secret mount")
	}
	if envValue(broker, "AGW_BROKER_SECRET_FILES") != "[]" {
		t.Fatalf("unexpected broker file map %q", envValue(broker, "AGW_BROKER_SECRET_FILES"))
	}
}

func TestBuildSupportsPublicSkillsWithoutSkillsCredential(t *testing.T) {
	snapshot := testSnapshot()
	options := testOptions(snapshot)
	options.SkillsSecretKey = ""
	manifest, err := Build(snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	skills := containerByName(manifest.Spec.PodTemplate.Spec.InitContainers, "skills")
	if envValue(skills, "AGW_SKILLS_TOKEN_FILE") != "" || hasNamedMount(skills, SkillsSecretVolumeName, SkillsSecretMountPath) {
		t.Fatal("public skills init received a credential path or Secret mount")
	}
}

func TestBuildPassesSandboxBackendSecurityValidation(t *testing.T) {
	snapshot := testSnapshot()
	manifest, err := Build(snapshot, testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := sandboxv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	clientForTest := fake.NewClientBuilder().WithScheme(scheme).Build()
	backend, err := sandbox.NewAgentSandboxBackend(clientForTest, sandbox.BackendOptions{Now: func() time.Time { return testOptions(snapshot).Now }})
	if err != nil {
		t.Fatal(err)
	}
	owner := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: snapshot.Run.Name, Namespace: snapshot.Run.Namespace, UID: types.UID(snapshot.Run.UID)}}
	ref, err := backend.Ensure(t.Context(), sandbox.SandboxPlan{Owner: owner, Role: sandbox.RoleWork, SpecDigest: manifest.Annotations[sandbox.SpecDigestAnnotationKey], Sandbox: manifest})
	if err != nil {
		t.Fatalf("backend rejected generated work Sandbox: %v", err)
	}
	if ref.Name != manifest.Name || ref.Role != sandbox.RoleWork {
		t.Fatalf("unexpected backend ref=%#v", ref)
	}
}

func TestBuildDoesNotInterpolateUserDataIntoCommands(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Spec.Source.Repo = "github.com/example/repo"
	snapshot.Spec.Source.BaseRef = "feature/$(touch /tmp/base)"
	snapshot.Task = "$(touch /tmp/task) && echo compromised"
	snapshot.Instructions = "$(touch /tmp/instructions)"
	snapshot.Agent.Skills[0].Ref = "https://skills.example/$(touch /tmp/skill)"
	manifest, err := Build(snapshot, testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	userData := []string{snapshot.Spec.Source.Repo, snapshot.Spec.Source.BaseRef, snapshot.Task, snapshot.Instructions, snapshot.Agent.Skills[0].Ref}
	for _, container := range append(append([]corev1.Container{}, manifest.Spec.PodTemplate.Spec.InitContainers...), manifest.Spec.PodTemplate.Spec.Containers...) {
		for _, value := range append(append([]string{}, container.Command...), container.Args...) {
			for _, input := range userData {
				if strings.Contains(value, input) {
					t.Fatalf("container %q interpolated user data into command/args: %q", container.Name, value)
				}
			}
		}
	}
	if !strings.Contains(envValue(containerByName(manifest.Spec.PodTemplate.Spec.Containers, "agent"), "AGW_TASK"), snapshot.Task) {
		t.Fatal("task was not passed as environment data")
	}
	if !strings.Contains(envValue(containerByName(manifest.Spec.PodTemplate.Spec.InitContainers, "clone"), "AGW_REPO"), snapshot.Spec.Source.Repo) {
		t.Fatal("source repo was not passed as environment data")
	}
}

func TestBuildRejectsMalformedOrUnpinnedInputs(t *testing.T) {
	baseSnapshot := testSnapshot()
	baseOptions := testOptions(baseSnapshot)
	cases := []struct {
		name   string
		mutate func(*resolved.Snapshot, *Options)
	}{
		{name: "clone tag", mutate: func(_ *resolved.Snapshot, options *Options) { options.CloneImage = "ghcr.io/astatide/clone:latest" }},
		{name: "skills zero digest", mutate: func(_ *resolved.Snapshot, options *Options) {
			options.SkillsImage = "ghcr.io/astatide/skills@sha256:" + strings.Repeat("0", 64)
		}},
		{name: "lockdown missing", mutate: func(_ *resolved.Snapshot, options *Options) { options.LockdownImage = "" }},
		{name: "broker tag", mutate: func(_ *resolved.Snapshot, options *Options) { options.BrokerImage = "ghcr.io/astatide/broker:v1" }},
		{name: "malformed pinned image", mutate: func(_ *resolved.Snapshot, options *Options) {
			options.CloneImage = "ghcr.io/astatide/clone;echo@sha256:" + strings.Repeat("b", 64)
		}},
		{name: "agent tag", mutate: func(snapshot *resolved.Snapshot, _ *Options) {
			snapshot.Agent.Runtime.Image = "ghcr.io/astatide/agent:latest"
		}},
		{name: "wrong Secret name", mutate: func(_ *resolved.Snapshot, options *Options) { options.SecretName = "some-other-secret" }},
		{name: "whole Secret clone projection", mutate: func(_ *resolved.Snapshot, options *Options) { options.CloneSecretKey = "" }},
		{name: "duplicate credential projection", mutate: func(_ *resolved.Snapshot, options *Options) { options.BrokerSecretKeys = []string{"github-token"} }},
		{name: "missing Now", mutate: func(_ *resolved.Snapshot, options *Options) { options.Now = time.Time{} }},
		{name: "past shutdown", mutate: func(_ *resolved.Snapshot, options *Options) { options.ShutdownTime = options.Now.Add(-time.Second) }},
		{name: "shutdown too far", mutate: func(_ *resolved.Snapshot, options *Options) {
			options.ShutdownTime = options.Now.Add(options.MaxShutdownDuration + time.Second)
		}},
		{name: "unbounded shutdown option", mutate: func(_ *resolved.Snapshot, options *Options) { options.MaxShutdownDuration = 0 }},
		{name: "invalid skill digest", mutate: func(snapshot *resolved.Snapshot, _ *Options) { snapshot.Agent.Skills[0].Digest = "sha256:not-a-digest" }},
		{name: "invalid workspace", mutate: func(snapshot *resolved.Snapshot, _ *Options) { snapshot.Spec.Workspace.Size = "unlimited" }},
		{name: "invalid source repo", mutate: func(snapshot *resolved.Snapshot, _ *Options) {
			snapshot.Spec.Source.Repo = "https://github.com/example/repo.git"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := baseSnapshot
			options := baseOptions
			tc.mutate(&snapshot, &options)
			if _, err := Build(snapshot, options); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Build error=%v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestBuildRejectsArtifactStoreLimitAboveGeneralCeiling(t *testing.T) {
	snapshot := testSnapshot()
	options := testOptions(snapshot)
	options.ArtifactStoreMaxObjectBytes = objectstore.GeneralMaxObjectBytes + 1

	_, err := Build(snapshot, options)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Build error=%v, want ErrInvalidInput", err)
	}
	if strings.Contains(err.Error(), strconv.FormatInt(objectstore.GeneralMaxObjectBytes+1, 10)) {
		t.Fatalf("Build error exposed configured size: %v", err)
	}
}

func TestBuildAcceptsGeneralArtifactStoreLimitAtBoundary(t *testing.T) {
	snapshot := testSnapshot()
	options := testOptions(snapshot)
	options.ArtifactStoreMaxObjectBytes = objectstore.GeneralMaxObjectBytes

	if _, err := Build(snapshot, options); err != nil {
		t.Fatalf("Build rejected the shared object-store boundary: %v", err)
	}
}

func TestBuildPlacementRequiresExactOperatorAllowlists(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Agent.Runtime.RuntimeClassName = "gvisor"
	snapshot.Spec.Workspace.StorageClassName = "fast-local"
	options := testOptions(snapshot)
	if _, err := Build(snapshot, options); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("caller-selected placement without allowlists error=%v, want ErrInvalidInput", err)
	}

	options.AllowedRuntimeClasses = []string{"gvisor"}
	options.AllowedStorageClasses = []string{"fast-local"}
	manifest, err := Build(snapshot, options)
	if err != nil {
		t.Fatalf("allowlisted placement was rejected: %v", err)
	}
	pod := manifest.Spec.PodTemplate.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "gvisor" {
		t.Fatalf("runtime class=%v, want gvisor", pod.RuntimeClassName)
	}
	if len(manifest.Spec.VolumeClaimTemplates) != 1 || manifest.Spec.VolumeClaimTemplates[0].Spec.StorageClassName == nil || *manifest.Spec.VolumeClaimTemplates[0].Spec.StorageClassName != "fast-local" {
		t.Fatalf("storage class was not carried into the PVC template: %#v", manifest.Spec.VolumeClaimTemplates)
	}

	options.AllowedRuntimeClasses = []string{"other-runtime"}
	if _, err := Build(snapshot, options); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unallowlisted RuntimeClass error=%v, want ErrInvalidInput", err)
	}
	options.AllowedRuntimeClasses = []string{"gvisor"}
	options.AllowedStorageClasses = []string{"other-storage"}
	if _, err := Build(snapshot, options); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unallowlisted StorageClass error=%v, want ErrInvalidInput", err)
	}
}

func testSnapshot() resolved.Snapshot {
	digest := strings.Repeat("a", 64)
	inlineTask := "fix the nil dereference"
	inlineInstructions := "keep the change focused"
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		BaseSHA:       strings.Repeat("e", 40),
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "repair-427", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15", Generation: 3},
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "issue-fixer", GateRef: "go-default",
			Source:    v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1},
			Task:      v1alpha1.TaskSpec{Inline: &inlineTask},
			Workspace: v1alpha1.WorkspaceSpec{Size: "8Gi"},
			Publish:   v1alpha1.PublishSpec{Mode: v1alpha1.PublishNone},
			Limits:    v1alpha1.LimitsSpec{Timeout: "45m", MaxToolCalls: 60, MaxCostUSD: "2.00"},
		},
		Task:         inlineTask,
		Instructions: inlineInstructions,
		Agent: v1alpha1.AgentSpec{
			Runtime:            v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@sha256:" + digest},
			Instructions:       v1alpha1.InstructionsSpec{Inline: &inlineInstructions},
			ContextStrategyRef: "context-one",
			ToolSetRef:         "github-readonly",
			ModelRouteRef:      "default-codex",
			Skills:             []v1alpha1.SkillRef{{Name: "codebase-design", Ref: "https://skills.example/codebase-design", Digest: "sha256:" + digest}},
		},
		Gate: v1alpha1.GateSpec{Verify: v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + digest}},
		ToolSet: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{{
			Name: "github", Ref: "https://mcp.example.test/mcp", CredentialsRef: "github-app",
			Tools: []v1alpha1.ToolDefinition{{Name: "list_issues", Effect: v1alpha1.EffectRead}},
		}}},
		ModelRoute: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "openrouter", Kind: "openrouter-responses", Model: "nvidia/nemotron-3-ultra-550b-a55b:free", CredentialRef: "openrouter", Priority: 1}}, Budget: v1alpha1.ModelBudget{MaxCostUSD: "2.00"}},
		ContextStrategy: v1alpha1.ContextStrategySpec{
			RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 8000},
			Budget:  v1alpha1.ContextBudgetSpec{TotalTokens: 40000, MaxBytes: 8 << 20},
		},
	}
}

func testOptions(snapshot resolved.Snapshot) Options {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("b", 64)
	return Options{
		CloneImage:          "ghcr.io/astatide/agw-clone@sha256:" + digest,
		SkillsImage:         "ghcr.io/astatide/agw-skills@sha256:" + digest,
		ContextImage:        "ghcr.io/astatide/agw-context@sha256:" + digest,
		SkillsEndpoint:      "https://skills.example.test/mcp",
		LockdownImage:       "ghcr.io/astatide/agw-lockdown@sha256:" + digest,
		BrokerImage:         "ghcr.io/astatide/agw-broker@sha256:" + digest,
		SecretName:          WorkSecretName(snapshot.Run.UID),
		CloneSecretKey:      "github-token",
		SkillsSecretKey:     "skills-token",
		BrokerSecretKeys:    []string{"mcp-token", "model-token"},
		ArtifactStoreRegion: "us-east-1", ArtifactStoreBucket: "agw-artifacts", ArtifactStorePrefix: "agents-gateway/v3",
		ArtifactStoreMaxObjectBytes:      objectstore.GeneralMaxObjectBytes,
		ArtifactAccessKeyIDSecretKey:     artifactauth.AccessKeyIDKey,
		ArtifactSecretAccessKeySecretKey: artifactauth.SecretAccessKeyKey,
		ArtifactSessionTokenSecretKey:    artifactauth.SessionTokenKey,
		Now:                              now,
		ShutdownTime:                     now.Add(45 * time.Minute),
		MaxShutdownDuration:              time.Hour,
	}
}

func assertSecureContainer(t *testing.T, container corev1.Container) {
	t.Helper()
	security := container.SecurityContext
	if security == nil || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.Capabilities == nil || !containsCapability(security.Capabilities.Drop, "ALL") {
		t.Fatalf("container %q is not read-only/drop-all/no-escalation: %#v", container.Name, security)
	}
	if security.Privileged != nil && *security.Privileged {
		t.Fatalf("container %q is privileged", container.Name)
	}
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("container %q does not use RuntimeDefault seccomp", container.Name)
	}
	if container.Resources.Requests == nil || container.Resources.Limits == nil || len(container.Resources.Requests) == 0 || len(container.Resources.Limits) == 0 {
		t.Fatalf("container %q has no complete resource requests/limits", container.Name)
	}
}

func hasSecretMount(container corev1.Container, secrets map[string]corev1.Volume) bool {
	for _, mount := range container.VolumeMounts {
		if _, ok := secrets[mount.Name]; ok {
			return true
		}
	}
	return false
}

func hasNamedMount(container corev1.Container, name, path string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name && mount.MountPath == path && mount.ReadOnly {
			return true
		}
	}
	return false
}

func namedMount(container corev1.Container, name, path string) *corev1.VolumeMount {
	for index := range container.VolumeMounts {
		mount := &container.VolumeMounts[index]
		if mount.Name == name && mount.MountPath == path {
			return mount
		}
	}
	return nil
}

func namedSubPathMount(container corev1.Container, name, path, subPath string) *corev1.VolumeMount {
	for index := range container.VolumeMounts {
		mount := &container.VolumeMounts[index]
		if mount.Name == name && mount.MountPath == path && mount.SubPath == subPath {
			return mount
		}
	}
	return nil
}

func hasDirectSecretItemMounts(container corev1.Container, name, directory string, count int) bool {
	seen := make(map[string]struct{}, count)
	for _, mount := range container.VolumeMounts {
		if mount.Name != name {
			continue
		}
		if !mount.ReadOnly || mount.SubPath == "" || mount.MountPath != directory+"/"+mount.SubPath {
			return false
		}
		seen[mount.SubPath] = struct{}{}
	}
	if len(seen) != count {
		return false
	}
	for index := 0; index < count; index++ {
		if _, ok := seen["item-"+strconv.Itoa(index)]; !ok {
			return false
		}
	}
	return true
}

func hasDirectArtifactMounts(container corev1.Container) bool {
	want := map[string]string{
		"access-key-id":     ArtifactAccessKeyIDFile,
		"secret-access-key": ArtifactSecretAccessKeyFile,
		"session-token":     ArtifactSessionTokenFile,
	}
	seen := make(map[string]struct{}, len(want))
	for _, mount := range container.VolumeMounts {
		if mount.Name != ArtifactSecretVolumeName {
			continue
		}
		if !mount.ReadOnly || want[mount.SubPath] != mount.MountPath {
			return false
		}
		seen[mount.SubPath] = struct{}{}
	}
	return len(seen) == len(want)
}

func hasExactDirectSecretItemMount(container corev1.Container, name, path, subPath string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return mount.ReadOnly && mount.MountPath == path && mount.SubPath == subPath
		}
	}
	return false
}

func hasSecretEnvRef(container corev1.Container) bool {
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			return true
		}
	}
	return len(container.EnvFrom) > 0
}

func envValue(container corev1.Container, name string) string {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

func volumeByName(volumes []corev1.Volume, name string) corev1.Volume {
	for _, volume := range volumes {
		if volume.Name == name {
			return volume
		}
	}
	return corev1.Volume{}
}

func containerByName(containers []corev1.Container, name string) corev1.Container {
	for _, container := range containers {
		if container.Name == name {
			return container
		}
	}
	return corev1.Container{}
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for index, container := range containers {
		names[index] = container.Name
	}
	return names
}

func containsCapability(capabilities []corev1.Capability, wanted string) bool {
	for _, capability := range capabilities {
		if string(capability) == wanted {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
