package verifyworkload

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBuildCreatesIndependentVerifyTopologyAndPassesBackendValidation(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Gate.Require.Adapter = v1alpha1.GateAdapterGo
	snapshot.Gate.Require.TestStrength = v1alpha1.TestStrengthNewTestsFailOnBase
	snapshot.Gate.Require.BaseTestCommand = &v1alpha1.VerifyCommand{Argv: []string{"go", "test", "./..."}}
	snapshot.Gate.Require.CoverageDelta = ">= 0"
	patch := testPatch()
	options := testOptions(snapshot)
	plan, err := Build(snapshot, patch, testBaseSHA(), options)
	if err != nil {
		t.Fatal(err)
	}
	manifest := plan.Sandbox
	if manifest == nil {
		t.Fatal("Build returned a nil Sandbox")
	}
	if plan.Role != sandbox.RoleVerify || plan.SpecDigest == "" || plan.Owner == nil {
		t.Fatalf("invalid plan identity: %#v", plan)
	}

	wantName, err := sandbox.ChildName(types.UID(snapshot.Run.UID), sandbox.RoleVerify)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != wantName || manifest.Namespace != snapshot.Run.Namespace {
		t.Fatalf("child identity=%s/%s, want %s/%s", manifest.Namespace, manifest.Name, snapshot.Run.Namespace, wantName)
	}
	if len(manifest.OwnerReferences) != 1 || manifest.OwnerReferences[0].UID != types.UID(snapshot.Run.UID) || manifest.OwnerReferences[0].Kind != "AgentRun" || manifest.OwnerReferences[0].Controller == nil || !*manifest.OwnerReferences[0].Controller {
		t.Fatalf("owner reference is not deterministic and controller-owned: %#v", manifest.OwnerReferences)
	}
	if manifest.Annotations[sandbox.SpecDigestAnnotationKey] != plan.SpecDigest || manifest.Labels[sandbox.SpecDigestLabelKey] != strings.TrimPrefix(plan.SpecDigest, "sha256:")[:63] {
		t.Fatalf("spec identity mismatch: labels=%v annotations=%v plan=%q", manifest.Labels, manifest.Annotations, plan.SpecDigest)
	}
	if manifest.Annotations[VerifyInputDigestAnnotationKey] == "" {
		t.Fatal("verify input digest is missing")
	}

	pod := manifest.Spec.PodTemplate.Spec
	if pod.HostUsers == nil || *pod.HostUsers || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("hostUsers/service-account token posture is not explicit and disabled")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("verify Sandbox enables a host namespace or restart")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("verify Sandbox does not use RuntimeDefault seccomp")
	}
	if pod.NodeSelector[workload.NodeLabelKey] != workload.NodeLabelValue || len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != workload.NodeLabelKey || pod.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("unexpected agent-node placement: selector=%v tolerations=%v", pod.NodeSelector, pod.Tolerations)
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks || pod.ShareProcessNamespace != nil && *pod.ShareProcessNamespace {
		t.Fatal("verify pod has service links or a shared process namespace")
	}

	if got := containerNames(pod.InitContainers); !reflect.DeepEqual(got, []string{"fetch", "apply", "lockdown"}) {
		t.Fatalf("init topology=%v, want fetch -> apply -> lockdown", got)
	}
	if got := containerNames(pod.Containers); !reflect.DeepEqual(got, []string{"verify"}) {
		t.Fatalf("regular topology=%v, want one verify container", got)
	}
	fetch := pod.InitContainers[0]
	if fetch.SecurityContext == nil || fetch.SecurityContext.RunAsUser == nil || *fetch.SecurityContext.RunAsUser != 0 || fetch.SecurityContext.RunAsNonRoot == nil || *fetch.SecurityContext.RunAsNonRoot {
		t.Fatalf("fetch must be namespace-root for the immutable base tree: %#v", fetch.SecurityContext)
	}
	if fetch.SecurityContext.Capabilities == nil || len(fetch.SecurityContext.Capabilities.Add) != 0 || !reflect.DeepEqual(fetch.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatalf("fetch must not add capabilities and must drop all: %#v", fetch.SecurityContext.Capabilities)
	}
	if strings.Contains(envValue(fetch, "AGW_FETCH_SPEC_JSON"), "artifact-token") || strings.Contains(envValue(fetch, "AGW_FETCH_SPEC_JSON"), "presign") {
		t.Fatal("fetch contract contains the legacy artifact token or presigned URL")
	}
	apply := pod.InitContainers[1]
	if hasSecretMount(apply, FetchSecretVolumeName) || hasSecretEnvRef(apply) {
		t.Fatal("apply initContainer received fetch credentials")
	}
	verify := pod.Containers[0]
	if verify.SecurityContext == nil || verify.SecurityContext.RunAsUser == nil || *verify.SecurityContext.RunAsUser != 1000 || verify.SecurityContext.RunAsGroup == nil || *verify.SecurityContext.RunAsGroup != 1000 {
		t.Fatalf("verify identity=%#v, want UID/GID 1000", verify.SecurityContext)
	}
	assertSecureContainer(t, verify)
	if envValue(verify, "AGW_VERIFY_NETWORK") != "disabled" || envValue(verify, "AGW_BROKER") != "" {
		t.Fatal("verify container has an execution egress/broker contract")
	}
	if envValue(verify, "AGW_PATCH_DIGEST") != patch.Digest {
		t.Fatal("verifier is not bound to the immutable patch digest")
	}
	if envValue(verify, "AGW_VERIFY_MAX_OUTPUT_BYTES") != "65536" {
		t.Fatalf("verifier output bound=%q, want safe default", envValue(verify, "AGW_VERIFY_MAX_OUTPUT_BYTES"))
	}
	commandsJSON := envValue(verify, "AGW_VERIFY_COMMANDS_JSON")
	var commandContract commandSpec
	if err := json.Unmarshal([]byte(commandsJSON), &commandContract); err != nil || commandContract.Requirements == nil || commandContract.Requirements.Version != 2 || commandContract.Requirements.Adapter != v1alpha1.GateAdapterGo || commandContract.Requirements.TestStrength != v1alpha1.TestStrengthNewTestsFailOnBase || commandContract.Requirements.CoverageDelta != ">= 0" || commandContract.Requirements.BaseTestCommand == nil || len(commandContract.Requirements.BaseTestCommand.Argv) != 3 {
		t.Fatalf("verifier requirements are missing from its immutable command contract: %s", commandsJSON)
	}
	if hasSecretMount(verify, FetchSecretVolumeName) || hasSecretEnvRef(verify) {
		t.Fatal("verify container received credentials")
	}

	lockdown := pod.InitContainers[2]
	if lockdown.Command[0] != lockdownShell || len(lockdown.Args) != 2 || lockdown.Args[0] != lockdownShellArgs || lockdown.Args[1] != lockdownScript {
		t.Fatalf("lockdown command is not the fixed script: command=%v args=%v", lockdown.Command, lockdown.Args)
	}
	for _, required := range []string{"iptables-restore --wait --noflush", "ip6tables-restore --wait --noflush", "-P OUTPUT DROP", "-F OUTPUT"} {
		if !strings.Contains(lockdownScript, required) {
			t.Fatalf("lockdown script is missing %q", required)
		}
	}
	for _, forbidden := range []string{"-A OUTPUT -o lo -j ACCEPT", "-A OUTPUT -m owner --uid-owner 1000 -j ACCEPT"} {
		if strings.Contains(lockdownScript, forbidden) {
			t.Fatalf("networkless verifier lockdown contains broad allow rule %q", forbidden)
		}
	}
	if strings.Contains(lockdownScript, "--uid-owner") || strings.Contains(lockdownScript, "ACCEPT -p") {
		t.Fatal("verify lockdown contains a broker or non-loopback exception")
	}
	if lockdown.SecurityContext == nil || lockdown.SecurityContext.RunAsUser == nil || *lockdown.SecurityContext.RunAsUser != 0 || lockdown.SecurityContext.Capabilities == nil || len(lockdown.SecurityContext.Capabilities.Add) != 1 || lockdown.SecurityContext.Capabilities.Add[0] != corev1.Capability("NET_ADMIN") {
		t.Fatalf("lockdown security=%#v, want namespaced NET_ADMIN only", lockdown.SecurityContext)
	}

	if len(manifest.Spec.VolumeClaimTemplates) != 0 {
		t.Fatalf("verify Sandbox unexpectedly has PVC templates: %#v", manifest.Spec.VolumeClaimTemplates)
	}
	if manifest.Spec.Service == nil || *manifest.Spec.Service {
		t.Fatal("verify Sandbox unexpectedly creates a Service")
	}
	if len(pod.Volumes) != 2 || volumeByName(pod.Volumes, WorkspaceVolumeName).EmptyDir == nil || volumeByName(pod.Volumes, FetchSecretVolumeName).Projected == nil {
		t.Fatalf("verify volumes are not one emptyDir plus one projected Secret: %#v", pod.Volumes)
	}
	if volumeByName(pod.Volumes, WorkspaceVolumeName).PersistentVolumeClaim != nil || strings.Contains(string(mustJSON(t, manifest)), "work-secret") {
		t.Fatal("verify manifest exposes a work PVC or work Secret")
	}
	projected := volumeByName(pod.Volumes, FetchSecretVolumeName).Projected
	if len(projected.Sources) != 3 {
		t.Fatalf("fetch Secret projection sources=%d, want clone, required artifact, required session", len(projected.Sources))
	}
	if got := projected.Sources[0].Secret.Items[0].Path; got != "clone-token" || projected.Sources[0].Secret.Items[0].Key != options.CloneSecretKey {
		t.Fatalf("clone projection=%#v", projected.Sources[0])
	}
	artifactKeys := map[string]struct{}{}
	for _, item := range projected.Sources[1].Secret.Items {
		artifactKeys[item.Key] = struct{}{}
	}
	for _, key := range []string{ArtifactAccessKeyIDSecretKey, ArtifactSecretAccessKeySecretKey} {
		if _, ok := artifactKeys[key]; !ok {
			t.Fatalf("fixed artifact key %q is not projected", key)
		}
	}
	if projected.Sources[2].Secret.Optional != nil || projected.Sources[2].Secret.Items[0].Key != ArtifactSessionTokenSecretKey {
		t.Fatalf("session projection is not required and fixed: %#v", projected.Sources[2])
	}
	if hasSecretMount(pod.InitContainers[0], FetchSecretVolumeName) == false || hasSecretMount(pod.InitContainers[1], FetchSecretVolumeName) || hasSecretMount(lockdown, FetchSecretVolumeName) {
		t.Fatal("fetch Secret projection is not isolated to fetch")
	}
	wantFetchMounts := map[string]string{
		"clone-token":                CloneTokenFile,
		"artifact-access-key-id":     ArtifactAccessKeyIDFile,
		"artifact-secret-access-key": ArtifactSecretAccessKeyFile,
		"artifact-session-token":     ArtifactSessionTokenFile,
	}
	if !hasExactDirectSecretItemMounts(pod.InitContainers[0], FetchSecretVolumeName, wantFetchMounts) {
		t.Fatal("fetch credentials are not mounted as exact direct-file projections")
	}

	scheme := runtime.NewScheme()
	if err := sandboxv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	backend, err := sandbox.NewAgentSandboxBackend(kube, sandbox.BackendOptions{Now: func() time.Time { return options.Now }})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatalf("generated verify Sandbox failed backend security validation: %v", err)
	}
	if ref.Role != sandbox.RoleVerify || ref.Name != manifest.Name || ref.OwnerUID != plan.Owner.UID {
		t.Fatalf("unexpected backend reference: %#v", ref)
	}
}

func TestBuildIsDeterministicAndPatchIdentityIsContentBound(t *testing.T) {
	snapshot := testSnapshot()
	patch := testPatch()
	options := testOptions(snapshot)
	first, err := Build(snapshot, patch, testBaseSHA(), options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(snapshot, patch, testBaseSHA(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical inputs produced different plans")
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
		t.Fatal("identical inputs produced different serialized plans")
	}

	patch.Digest = "sha256:" + strings.Repeat("d", 64)
	changed, err := Build(snapshot, patch, testBaseSHA(), options)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SpecDigest != first.SpecDigest {
		t.Fatal("resolved run digest changed when only captured patch evidence changed")
	}
	if changed.Sandbox.Annotations[VerifyInputDigestAnnotationKey] == first.Sandbox.Annotations[VerifyInputDigestAnnotationKey] {
		t.Fatal("verify input digest did not change with patch identity")
	}
}

func TestBuildUsesFixedArtifactCredentialKeys(t *testing.T) {
	snapshot := testSnapshot()
	options := testOptions(snapshot)
	plan, err := Build(snapshot, testPatch(), testBaseSHA(), options)
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(mustJSON(t, plan.Sandbox))
	for _, key := range []string{ArtifactAccessKeyIDSecretKey, ArtifactSecretAccessKeySecretKey, ArtifactSessionTokenSecretKey} {
		if !strings.Contains(manifest, key) {
			t.Fatalf("fixed artifact key %q is absent from the verify plan", key)
		}
	}
}

func TestBuildKeepsUserDataOutOfCommandsAndUsesBoundedJSONContracts(t *testing.T) {
	snapshot := testSnapshot()
	task := "$(touch /tmp/task) && echo task"
	instructions := "$(touch /tmp/instructions)"
	snapshot.Task = task
	snapshot.Instructions = instructions
	shell := "go test ./...; touch /tmp/repository-compromised"
	snapshot.Gate.Verify.Commands = []v1alpha1.VerifyCommand{{Shell: &shell}}
	plan, err := Build(snapshot, testPatch(), testBaseSHA(), testOptions(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	pod := plan.Sandbox.Spec.PodTemplate.Spec
	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, value := range append(append([]string{}, container.Command...), container.Args...) {
			if strings.Contains(value, task) || strings.Contains(value, instructions) || strings.Contains(value, shell) {
				t.Fatalf("user data was interpolated into container command/args: container=%q value=%q", container.Name, value)
			}
		}
	}
	if pod.InitContainers[0].Command[0] != fetchEntrypoint || len(pod.InitContainers[0].Args) != 0 || pod.InitContainers[1].Command[0] != applyEntrypoint || len(pod.InitContainers[1].Args) != 0 {
		t.Fatal("fetch/apply do not use trusted fixed entrypoints")
	}
	verifyJSON := envValue(pod.Containers[0], "AGW_VERIFY_COMMANDS_JSON")
	if !strings.Contains(verifyJSON, shell) || !strings.Contains(envValue(pod.InitContainers[0], "AGW_FETCH_SPEC_JSON"), testBaseSHA()) {
		t.Fatal("trusted binaries did not receive user data as bounded data")
	}
	if len(verifyJSON) > defaultMaxCommandBytes {
		t.Fatalf("verify command JSON is not bounded: %d", len(verifyJSON))
	}
}

func TestBuildRejectsInvalidOrUnpinnedInputs(t *testing.T) {
	baseSnapshot := testSnapshot()
	basePatch := testPatch()
	baseOptions := testOptions(baseSnapshot)
	cases := []struct {
		name   string
		mutate func(*resolved.Snapshot, *v1alpha1.ArtifactRef, *string, *Options)
	}{
		{name: "fetch image tag", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, o *Options) {
			o.FetchImage = "ghcr.io/astatide/fetch:latest"
		}},
		{name: "apply zero placeholder digest", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, o *Options) {
			o.ApplyImage = "ghcr.io/astatide/apply@sha256:" + strings.Repeat("0", 64)
		}},
		{name: "lockdown malformed image", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, o *Options) {
			o.LockdownImage = "ghcr.io/astatide/lockdown;echo@sha256:" + strings.Repeat("a", 64)
		}},
		{name: "verifier tag", mutate: func(s *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, _ *Options) {
			s.Gate.Verify.Image = "ghcr.io/astatide/verify:latest"
		}},
		{name: "patch digest", mutate: func(_ *resolved.Snapshot, p *v1alpha1.ArtifactRef, _ *string, _ *Options) {
			p.Digest = "sha256:not-a-digest"
		}},
		{name: "patch query credentials", mutate: func(_ *resolved.Snapshot, p *v1alpha1.ArtifactRef, _ *string, _ *Options) {
			p.URI = "https://objects.example/patch?token=secret"
		}},
		{name: "private patch host", mutate: func(_ *resolved.Snapshot, p *v1alpha1.ArtifactRef, _ *string, _ *Options) {
			p.URI = "https://127.0.0.1/patch.diff"
		}},
		{name: "base SHA injection", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, b *string, _ *Options) { *b = "$(touch /tmp/base)" }},
		{name: "not clean checkout", mutate: func(s *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, _ *Options) {
			s.Gate.Verify.FromCleanCheckout = false
		}},
		{name: "wrong secret", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, o *Options) {
			o.SecretName = "some-other-secret"
		}},
		{name: "past shutdown", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, o *Options) {
			o.ShutdownTime = o.Now.Add(-time.Second)
		}},
		{name: "oversized verifier output", mutate: func(_ *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, o *Options) {
			o.MaxOutputBytes = (256 << 10) + 1
		}},
		{name: "oversized command JSON", mutate: func(s *resolved.Snapshot, _ *v1alpha1.ArtifactRef, _ *string, _ *Options) {
			value := strings.Repeat("x", defaultMaxCommandBytes)
			s.Gate.Verify.Commands = []v1alpha1.VerifyCommand{{Shell: &value}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := baseSnapshot
			patch := basePatch
			baseSHA := testBaseSHA()
			options := baseOptions
			tc.mutate(&snapshot, &patch, &baseSHA, &options)
			if _, err := Build(snapshot, patch, baseSHA, options); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Build error=%v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestBuildPlacementRequiresExactOperatorAllowlists(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Agent.Runtime.RuntimeClassName = "gvisor"
	snapshot.Spec.Workspace.StorageClassName = "fast-local"
	patch := testPatch()
	options := testOptions(snapshot)
	if _, err := Build(snapshot, patch, testBaseSHA(), options); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("caller-selected placement without allowlists error=%v, want ErrInvalidInput", err)
	}
	options.AllowedRuntimeClasses = []string{"gvisor"}
	options.AllowedStorageClasses = []string{"fast-local"}
	if _, err := Build(snapshot, patch, testBaseSHA(), options); err != nil {
		t.Fatalf("allowlisted placement was rejected: %v", err)
	}
}

func testSnapshot() resolved.Snapshot {
	digest := "sha256:" + strings.Repeat("a", 64)
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "repair-427", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15", Generation: 3},
		BaseSHA:       testBaseSHA(),
		Spec:          v1alpha1.AgentRunSpec{Source: v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1}},
		Agent:         v1alpha1.AgentSpec{Runtime: v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@" + digest}},
		Gate: v1alpha1.GateSpec{Verify: v1alpha1.VerifySpec{
			Image: "ghcr.io/astatide/agw-verify@" + digest, FromCleanCheckout: true,
			Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}}, Timeout: "20m",
		}},
		Task: "fix the issue", Instructions: "keep the change focused",
	}
}

func testPatch() v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{
		URI: "s3://agw-artifacts/runs/run/patch.diff", Digest: "sha256:" + strings.Repeat("b", 64),
		Kind: "patch", Name: "patch.diff", MediaType: "text/x-diff", SizeBytes: 1024,
	}
}

func testOptions(snapshot resolved.Snapshot) Options {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("c", 64)
	return Options{
		FetchImage: "ghcr.io/astatide/agw-fetch@" + digest, ApplyImage: "ghcr.io/astatide/agw-apply@" + digest, LockdownImage: "ghcr.io/astatide/agw-lockdown@" + digest,
		SecretName: workload.VerifySecretName(snapshot.Run.UID), CloneSecretKey: "clone-token",
		ArtifactStoreBucket: "agw-artifacts", ArtifactStoreRegion: "us-east-1", MaxPatchBytes: 8 << 20,
		ArtifactCredentialTTL: 45 * time.Minute,
		Now:                   now, ShutdownTime: now.Add(20 * time.Minute), MaxShutdownDuration: time.Hour,
	}
}

func testBaseSHA() string { return strings.Repeat("d", 40) }

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
}

func hasSecretMount(container corev1.Container, name string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func hasExactDirectSecretItemMounts(container corev1.Container, name string, want map[string]string) bool {
	seen := make(map[string]struct{}, len(want))
	for _, mount := range container.VolumeMounts {
		if mount.Name != name {
			continue
		}
		if !mount.ReadOnly || want[mount.SubPath] != mount.MountPath {
			return false
		}
		seen[mount.SubPath] = struct{}{}
	}
	return len(seen) == len(want)
}

func hasSecretEnvRef(container corev1.Container) bool {
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			return true
		}
	}
	return len(container.EnvFrom) != 0
}

func volumeByName(volumes []corev1.Volume, name string) corev1.Volume {
	for _, volume := range volumes {
		if volume.Name == name {
			return volume
		}
	}
	return corev1.Volume{}
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for index, container := range containers {
		names[index] = container.Name
	}
	return names
}

func envValue(container corev1.Container, name string) string {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
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
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
