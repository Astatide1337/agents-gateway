package criticworkload

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
)

func TestContractIsCanonicalAndBindsAllImmutableInputs(t *testing.T) {
	snapshot := testSnapshot()
	input, err := FromSnapshot(snapshot, testSpecDigest(t, snapshot), testPatchRef(), testContextRef())
	if err != nil {
		t.Fatal(err)
	}
	body, err := CanonicalInputBytes(input)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalInput(body)
	if err != nil || !reflect.DeepEqual(parsed, input) {
		t.Fatalf("round trip parsed=%#v err=%v input=%#v", parsed, err, input)
	}
	inputDigest, err := InputDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	bindingDigest, err := BindingDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	if inputDigest == bindingDigest || !canonical.ValidDigest(inputDigest) || !canonical.ValidDigest(bindingDigest) {
		t.Fatalf("input/binding digests=%q/%q", inputDigest, bindingDigest)
	}
	if input.Binding().RunGeneration != snapshot.Run.Generation || input.Binding().PatchDigest != input.Patch.Digest || input.Binding().ContextDigest != input.Context.Digest || input.Binding().CriticRoute.Selected.Family == input.Binding().WorkerRoute.Selected.Family {
		t.Fatalf("binding lost immutable identities: %#v", input.Binding())
	}

	unknown := append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"verdict":"Accepted"}`)...)
	if _, err := ParseCanonicalInput(unknown); err == nil {
		t.Fatal("unknown authority field was accepted")
	}
	duplicate := []byte(`{"schemaVersion":"agents.astatide.com/critic-workload/v1alpha1","schemaVersion":"agents.astatide.com/critic-workload/v1alpha1"}`)
	if _, err := ParseCanonicalInput(duplicate); err == nil {
		t.Fatal("duplicate contract field was accepted")
	}
}

func TestContractRejectsRouteFamilyCollisionAndBadContextOrPatch(t *testing.T) {
	snapshot := testSnapshot()
	specDigest := testSpecDigest(t, snapshot)
	collision := snapshot
	collision.CriticModelRoute = &v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "critic", Kind: "anthropic-messages", Model: "different", Family: "openai", Priority: 1}}}
	if _, err := FromSnapshot(collision, testSpecDigest(t, collision), testPatchRef(), testContextRef()); !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("family collision error=%v, want ErrRouteConflict", err)
	}
	badPatch := testPatchRef()
	badPatch.Kind = "context-pack"
	if _, err := FromSnapshot(snapshot, specDigest, badPatch, testContextRef()); err == nil {
		t.Fatal("patch/context kind mismatch was accepted")
	}
	badContext := testContextRef()
	badContext.Digest = "sha256:" + strings.Repeat("e", 64)
	if _, err := FromSnapshot(snapshot, specDigest, testPatchRef(), badContext); err != nil {
		// A different immutable context digest is valid as an identity. The
		// important property is that it is retained, not inferred from patch.
		t.Fatal(err)
	}
}

func TestVerifyAdapterRejectsBindingSubstitutionBeforeRunner(t *testing.T) {
	snapshot := testSnapshot()
	input := verifycontroller.CriticRunInput{
		Snapshot: snapshot, SpecDigest: testSpecDigest(t, snapshot), BaseSHA: snapshot.BaseSHA, Patch: testPatchRef(),
		Binding: verifycontroller.CriticEvidenceBinding{
			RunUID: "foreign-run", SpecDigest: testSpecDigest(t, snapshot), BaseSHA: snapshot.BaseSHA, PatchDigest: testPatchRef().Digest,
			GateUID: snapshot.References.Gate.UID, GateGeneration: snapshot.References.Gate.Generation,
			Route: gate.RouteEvidence{Name: "critic-route", UID: "critic-route-uid", Generation: 5, Provider: "critic", Family: "anthropic"},
		},
	}
	adapter := &VerifyAdapter{
		Runner: &Runner{},
		ResolveContext: func(context.Context, verifycontroller.CriticRunInput) (v1alpha1.ArtifactRef, error) {
			return testContextRef(), nil
		},
	}
	if _, err := adapter.Ensure(context.Background(), input); !errors.Is(err, verifycontroller.ErrCriticConflict) {
		t.Fatalf("substituted run binding err=%v, want ErrCriticConflict", err)
	}
}

func TestSourceRejectsEscapedOrTraversalOutputPaths(t *testing.T) {
	source := &Source{bucket: "artifacts", prefix: "agents/v3"}
	for _, uri := range []string{
		"s3://artifacts/agents/v3/runs/%2e%2e/critic/job/pod/input.json",
		"s3://artifacts/agents/v3/runs/run%2fuid/critic/job/pod/input.json",
		"s3://artifacts/agents/v3/runs/run/critic/../pod/input.json",
	} {
		if _, _, _, _, err := source.parseOutputURI(uri); err == nil {
			t.Fatalf("ambiguous output path was accepted: %s", uri)
		}
	}
}

func TestProductionBuildRequiresExplicitAgentGatewayCapabilityProbe(t *testing.T) {
	options := testBuildOptions()
	options.AgentGatewayCapability = AgentGatewayCapability{}
	if _, err := Build(testInput(t), options); !errors.Is(err, ErrAgentGatewayCapabilityRequired) {
		t.Fatalf("production critic Job build err=%v, want ErrAgentGatewayCapabilityRequired", err)
	}
	if _, err := NewRunner(RunnerOptions{Client: testClient(t), Logs: &fixedLogs{}, Store: newMemoryStore("s3://artifacts"), Auth: testAuth(t), Build: options}); !errors.Is(err, ErrAgentGatewayCapabilityRequired) {
		t.Fatalf("default runner construction err=%v, want ErrAgentGatewayCapabilityRequired", err)
	}
	if _, err := NewRunner(RunnerOptions{Client: testClient(t), Logs: &fixedLogs{}, Store: newMemoryStore("s3://artifacts"), Auth: testAuth(t), Build: testBuildOptions()}); err != nil {
		t.Fatalf("default runner construction with probed capability err=%v", err)
	}
}

func TestProductionBuildRequiresCentralGatewayJWTCapabilityProbe(t *testing.T) {
	options := testBuildOptions()
	options.Gateway.JWTCapability.Verified = false
	if _, err := Build(testInput(t), options); !errors.Is(err, ErrGatewayBinding) {
		t.Fatalf("production critic Job build err=%v, want ErrGatewayBinding", err)
	}
}

func TestProductionBuildRejectsJWTCapabilityEvidenceReplayedAcrossBindings(t *testing.T) {
	base := testBuildOptions()
	mutations := []struct {
		name   string
		mutate func(*BuildOptions)
	}{
		{name: "endpoint", mutate: func(options *BuildOptions) {
			options.Gateway.Endpoint = "http://agentgateway.agw-system.svc.cluster.local:8081"
		}},
		{name: "route", mutate: func(options *BuildOptions) { options.Gateway.RouteRef = "other-route" }},
		{name: "service account", mutate: func(options *BuildOptions) { options.Gateway.ServiceAccountName = "other-critic" }},
		{name: "token ttl", mutate: func(options *BuildOptions) { options.Gateway.TokenExpirationSeconds++ }},
		{name: "issuer", mutate: func(options *BuildOptions) { options.Gateway.JWTCapability.Issuer = "https://other.example" }},
		{name: "audience", mutate: func(options *BuildOptions) { options.Gateway.JWTCapability.Audience = "other-audience" }},
		{name: "policy", mutate: func(options *BuildOptions) { options.Gateway.JWTCapability.PolicyRef = "other-policy" }},
		{name: "gateway pod selector", mutate: func(options *BuildOptions) {
			options.Gateway.PodSelector = map[string]string{"app.kubernetes.io/name": "other-gateway"}
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			options := base
			options.Gateway.PodSelector = copyStringMap(base.Gateway.PodSelector)
			mutation.mutate(&options)
			if _, err := Build(testInput(t), options); !errors.Is(err, ErrGatewayBinding) {
				t.Fatalf("replayed JWT capability evidence err=%v, want ErrGatewayBinding", err)
			}
		})
	}
}

func TestProductionBuildCreatesImmutableCriticAndAgentGatewayWorkload(t *testing.T) {
	input := testInput(t)
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(input, testBuildOptions())
	if err != nil || !reflect.DeepEqual(plan, second) {
		t.Fatalf("production plan is not deterministic: err=%v", err)
	}
	pod := plan.Job.Spec.Template.Spec
	if pod.DNSPolicy != corev1.DNSClusterFirst || len(pod.InitContainers) != 3 || len(pod.Containers) != 2 || len(pod.Volumes) != 5 || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("production pod topology=%#v", pod)
	}
	if pod.ServiceAccountName != testBuildOptions().Gateway.ServiceAccountName {
		t.Fatalf("production pod ServiceAccount=%q", pod.ServiceAccountName)
	}
	if plan.ArtifactStoreSecretName != workload.VerifySecretName(input.Run.UID) {
		t.Fatalf("production plan artifact Secret=%q, want %q", plan.ArtifactStoreSecretName, workload.VerifySecretName(input.Run.UID))
	}
	otherInput := input
	otherInput.Run.UID = "run-uid-2"
	otherPlan, err := Build(otherInput, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if otherPlan.ArtifactStoreSecretName == plan.ArtifactStoreSecretName || otherPlan.ArtifactStoreSecretName != workload.VerifySecretName(otherInput.Run.UID) {
		t.Fatalf("per-run verify Secret derivation is not isolated: first=%q second=%q", plan.ArtifactStoreSecretName, otherPlan.ArtifactStoreSecretName)
	}
	if pod.InitContainers[0].Name != "input-materializer" || pod.InitContainers[1].Name != "gateway-config" || pod.InitContainers[2].Name != "airlock" || pod.Containers[0].Name != Role || pod.Containers[1].Name != "agentgateway" {
		t.Fatalf("production container order=%v/%v", pod.InitContainers, pod.Containers)
	}
	critic := pod.Containers[0]
	if len(critic.VolumeMounts) != 1 || critic.VolumeMounts[0].Name != InputVolumeName || !critic.VolumeMounts[0].ReadOnly || hasSecretEnv(critic) {
		t.Fatalf("critic mounts/env=%#v", critic)
	}
	gateway := pod.Containers[1]
	if len(gateway.Env) != 0 || len(gateway.VolumeMounts) != 3 || gateway.VolumeMounts[1].Name != GatewayIdentityVolumeName || !gateway.VolumeMounts[1].ReadOnly {
		t.Fatalf("gateway identity projection=%#v", gateway)
	}
	if len(pod.InitContainers[0].Env) == 0 || hasSecretRefNamed(gateway, "critic-provider") {
		t.Fatalf("provider credential leaked into critic workload")
	}
	secretVolumes := 0
	var tokenProjection *corev1.ServiceAccountTokenProjection
	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			secretVolumes++
		}
		if volume.Name == GatewayIdentityVolumeName && volume.Projected != nil && len(volume.Projected.Sources) == 1 {
			tokenProjection = volume.Projected.Sources[0].ServiceAccountToken
		}
		if volume.Name == ObjectStoreVolumeName && (volume.Secret == nil || volume.Secret.SecretName != workload.VerifySecretName(input.Run.UID)) {
			t.Fatalf("object-store Secret volume=%#v, want verify Secret", volume)
		}
	}
	if secretVolumes != 1 || tokenProjection == nil || tokenProjection.Audience != testBuildOptions().Gateway.JWTCapability.Audience || tokenProjection.ExpirationSeconds == nil || *tokenProjection.ExpirationSeconds != testBuildOptions().Gateway.TokenExpirationSeconds || tokenProjection.Path != "token" {
		t.Fatalf("projected gateway identity=%#v secretVolumes=%d", tokenProjection, secretVolumes)
	}
	if !hasSecretRefNamed(pod.InitContainers[0], workload.VerifySecretName(input.Run.UID)) {
		t.Fatalf("input materializer does not use the per-run verify Secret")
	}
	if !strings.Contains(pod.InitContainers[2].Args[0], "--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8082") || !strings.Contains(pod.InitContainers[2].Args[0], "--uid-owner 1338 -j ACCEPT") || strings.Contains(pod.InitContainers[2].Args[0], "--uid-owner 1000 -j ACCEPT") {
		t.Fatalf("critic UID airlock is not narrow: %s", pod.InitContainers[2].Args[0])
	}
	config, err := RenderAgentGatewayConfig(input.CriticRoute.Selected.Model, AgentGatewayClientBinding{Endpoint: testBuildOptions().Gateway.Endpoint, RouteRef: testBuildOptions().Gateway.RouteRef})
	if err != nil || !strings.Contains(string(config), `"pathPrefix":"/v1/messages"`) || !strings.Contains(string(config), `"attempts":1`) || !strings.Contains(string(config), `"file":"/var/run/secrets/agw-gateway/token"`) || !strings.Contains(string(config), `"name":"authorization"`) || strings.Contains(string(config), "ANTHROPIC_API_KEY") {
		t.Fatalf("rendered gateway config=%s err=%v", config, err)
	}
	policy := plan.NetworkPolicy
	if policy == nil || policy.Spec.PodSelector.MatchLabels["agents.astatide.com/role"] != Role || len(policy.Spec.Egress) != 3 || len(policy.Spec.Ingress) != 0 || len(policy.Spec.PolicyTypes) != 2 {
		t.Fatalf("critic egress policy=%#v", policy)
	}
	if policy.Spec.Egress[1].To[0].NamespaceSelector == nil || policy.Spec.Egress[1].To[0].PodSelector == nil || policy.Spec.Egress[1].Ports[0].Port.IntValue() != 8080 {
		t.Fatalf("central gateway egress rule=%#v", policy.Spec.Egress[1])
	}
}

func TestSourceAuthenticatesProductionBuildPodAndRejectsFullSpecTampering(t *testing.T) {
	input := testInput(t)
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Job.DeepCopy()
	job.UID = types.UID("production-job-uid")
	job.Status.Succeeded = 1
	pod := completedPod(job, "production-critic-pod", "production-pod-uid")
	// These are values Kubernetes may add while materializing/scheduling the
	// Pod; the security-bearing template remains unchanged.
	pod.Spec.NodeName = "node-agents-1"
	pod.Spec.SchedulerName = corev1.DefaultSchedulerName
	priority := int32(0)
	pod.Spec.Priority = &priority
	preemption := corev1.PreemptLowerPriority
	pod.Spec.PreemptionPolicy = &preemption
	defaultTolerationSeconds := int64(300)
	pod.Spec.Tolerations = append(pod.Spec.Tolerations,
		corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &defaultTolerationSeconds},
		corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &defaultTolerationSeconds},
	)
	for index := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[index].TerminationMessagePath = corev1.TerminationMessagePathDefault
		pod.Spec.InitContainers[index].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for index := range pod.Spec.Containers {
		pod.Spec.Containers[index].TerminationMessagePath = corev1.TerminationMessagePathDefault
		pod.Spec.Containers[index].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	kube := testClient(t, job, pod)
	store := newMemoryStore("s3://artifacts/agents/v3")
	auth := testAuth(t)
	runner, err := NewRunner(RunnerOptions{Client: kube, Logs: &fixedLogs{frame: mustFrame(t, emptyCorroborationInput(t))}, Store: store, Auth: auth, Build: testBuildOptions()})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Ensure(context.Background(), input)
	if err != nil || status.InputRef == nil {
		t.Fatalf("production runner status=%#v err=%v", status, err)
	}
	source, err := NewSource(SourceOptions{Reader: kube, Store: store, Auth: auth, Namespace: input.Run.Namespace, Bucket: "artifacts", Prefix: "agents/v3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(context.Background(), *status.InputRef, MaxOutputBytes); err != nil {
		t.Fatalf("production Build-derived Pod was not authenticated: %v", err)
	}

	// A change to the projected JWT binding is a full Pod-spec identity change,
	// even though the Job/Pod labels and completion state still look valid.
	for index := range pod.Spec.Volumes {
		if pod.Spec.Volumes[index].Name == GatewayIdentityVolumeName {
			pod.Spec.Volumes[index].Projected.Sources[0].ServiceAccountToken.Audience = "forged-audience"
		}
	}
	if err := kube.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(context.Background(), *status.InputRef, MaxOutputBytes); !errors.Is(err, ErrSourceUnauthenticated) {
		t.Fatalf("tampered production Pod err=%v, want ErrSourceUnauthenticated", err)
	}

	tamperedJob := job.DeepCopy()
	for index := range tamperedJob.Spec.Template.Spec.Volumes {
		if tamperedJob.Spec.Template.Spec.Volumes[index].Name == ObjectStoreVolumeName {
			tamperedJob.Spec.Template.Spec.Volumes[index].Secret.SecretName = "wrong-run-secret"
		}
	}
	if err := kube.Update(context.Background(), tamperedJob); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Ensure(context.Background(), input); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("tampered immutable verify Secret binding err=%v, want ErrJobConflict", err)
	}
}

func TestRunnerCreatesAndValidatesOwnedNetworkPolicyIdempotently(t *testing.T) {
	input := testInput(t)
	kube := testClient(t)
	runner, err := NewRunner(RunnerOptions{
		Client: kube, Logs: &fixedLogs{}, Store: newMemoryStore("s3://artifacts"), Auth: testAuth(t), Build: testBuildOptions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := runner.Ensure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runner.Ensure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("runner did not reuse immutable Job: first=%#v second=%#v", first, second)
	}
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: plan.NetworkPolicy.Namespace, Name: plan.NetworkPolicy.Name}, &policy); err != nil {
		t.Fatal(err)
	}
	if err := validateNetworkPolicyForPlan(&policy, plan.NetworkPolicy); err != nil {
		t.Fatalf("created policy failed its own validation: %v", err)
	}
	policy.Spec.Egress = nil
	if err := kube.Update(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Ensure(context.Background(), input); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("tampered policy err=%v, want ErrJobConflict", err)
	}
}

func TestBuildProducesBoundedNonRetryingHardenedJob(t *testing.T) {
	input := testInput(t)
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(input, testBuildOptions())
	if err != nil || !reflect.DeepEqual(plan, second) {
		t.Fatalf("identical input is not deterministic: err=%v", err)
	}
	job := plan.Job
	if job.Name != plan.JobName || job.Namespace != input.Run.Namespace || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Completions == nil || *job.Spec.Completions != 1 || job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.ManualSelector == nil || !*job.Spec.ManualSelector {
		t.Fatalf("job lifecycle is not bounded: %#v", job.Spec)
	}
	pod := job.Spec.Template.Spec
	if pod.HostUsers == nil || *pod.HostUsers || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.EnableServiceLinks == nil || *pod.EnableServiceLinks || pod.RestartPolicy != corev1.RestartPolicyNever || pod.DNSPolicy != corev1.DNSClusterFirst || len(pod.Containers) != 2 || len(pod.Volumes) != 5 || len(pod.InitContainers) != 3 {
		t.Fatalf("critic pod security/lifecycle=%#v", pod)
	}
	container := pod.Containers[0]
	if container.Name != "critic" || len(container.Command) != 1 || container.Command[0] != Entrypoint || container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.Capabilities == nil || !reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatalf("critic container security=%#v", container.SecurityContext)
	}
	for _, env := range container.Env {
		if env.ValueFrom != nil || strings.Contains(strings.ToLower(env.Name), "token") || strings.Contains(strings.ToLower(env.Name), "secret") {
			t.Fatalf("critic Job contains credential-shaped env=%#v", env)
		}
	}
	if job.OwnerReferences[0].UID != types.UID(input.Run.UID) || job.Annotations[ContextDigestAnnotationKey] != input.Context.Digest || job.Annotations[PatchDigestAnnotationKey] != input.Patch.Digest || job.Annotations[CriticRouteFamilyAnnotationKey] != input.CriticRoute.Selected.Family || !strings.Contains(job.Annotations[WorkerRouteFamilyAnnotationKey], input.WorkerRoute.Selected.Family) {
		t.Fatalf("job binding annotations=%v", job.Annotations)
	}
}

func TestFrameRejectsForgedAuthorityUnknownDuplicateAndOversizedOutput(t *testing.T) {
	empty := emptyCorroborationInput(t)
	frame, err := EncodeOutputFrame(empty)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOutputFrame(frame, MaxOutputBytes)
	if err != nil || !bytes.Equal(decoded, empty) {
		t.Fatalf("valid frame decoded=%q err=%v", decoded, err)
	}
	forged := []byte(`{"schemaVersion":"agents.astatide.com/finding-corroboration/v1alpha1","findings":[],"evidence":[],"verdict":"Accepted"}`)
	if _, err := DecodeOutputFrame(rawFrame(forged), MaxOutputBytes); !errors.Is(err, ErrOutputMalformed) {
		t.Fatalf("forged verdict err=%v, want ErrOutputMalformed", err)
	}
	duplicate := []byte(`{"schemaVersion":"agents.astatide.com/finding-corroboration/v1alpha1","schemaVersion":"agents.astatide.com/finding-corroboration/v1alpha1","findings":[],"evidence":[]}`)
	if _, err := DecodeOutputFrame(rawFrame(duplicate), MaxOutputBytes); !errors.Is(err, ErrOutputMalformed) {
		t.Fatalf("duplicate output field err=%v, want ErrOutputMalformed", err)
	}
	if _, err := DecodeOutputFrame(bytes.Repeat([]byte("x"), MaxFrameBytes+1), MaxOutputBytes); !errors.Is(err, ErrOutputOversized) {
		t.Fatalf("oversized frame err=%v, want ErrOutputOversized", err)
	}
}

func TestRunnerAndSourceAuthenticateImmutableOutputEndToEnd(t *testing.T) {
	input := testInput(t)
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Job.DeepCopy()
	job.UID = types.UID("job-uid-1")
	job.Status.Succeeded = 1
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: job.Namespace, Name: "critic-pod", UID: types.UID("pod-uid-1"),
		Labels:          mergeLabels(job.Spec.Template.Labels, map[string]string{jobControllerUIDLabel: string(job.UID)}),
		Annotations:     copyStringMap(job.Spec.Template.Annotations),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: job.UID, Controller: boolPtr(true)}},
	}, Spec: job.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	kube := testClient(t, job, pod)
	store := newMemoryStore("s3://artifacts/agents/v3")
	logs := &fixedLogs{frame: mustFrame(t, emptyCorroborationInput(t))}
	auth := testAuth(t)
	runner, err := NewRunner(RunnerOptions{Client: kube, Logs: logs, Store: store, Auth: auth, Build: testBuildOptions()})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Ensure(context.Background(), input)
	if err != nil || !status.Finished || status.Failed || status.InputRef == nil {
		t.Fatalf("runner status=%#v err=%v", status, err)
	}
	if status.InputRef.Kind != InputKind || status.InputRef.Digest != digestBytes(emptyCorroborationInput(t)) {
		t.Fatalf("input ref=%#v", status.InputRef)
	}
	source, err := NewSource(SourceOptions{Reader: kube, Store: store, Auth: auth, Namespace: input.Run.Namespace, Bucket: "artifacts", Prefix: "agents/v3"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := source.Read(context.Background(), *status.InputRef, MaxOutputBytes)
	if err != nil || !artifact.Authenticated || !bytes.Equal(artifact.Input, emptyCorroborationInput(t)) || artifact.Record.Job.UID != string(job.UID) || artifact.Record.Pod.UID != string(pod.UID) {
		t.Fatalf("source artifact=%#v err=%v", artifact, err)
	}
	if artifact.Binding.PatchDigest != input.Patch.Digest || artifact.Binding.ContextDigest != input.Context.Digest || artifact.Binding.RunGeneration != input.Run.Generation {
		t.Fatalf("source binding=%#v", artifact.Binding)
	}
	signatureKey := strings.TrimPrefix(status.InputRef.URI, "s3://artifacts/agents/v3/") + ".record.json.sig"
	originalSignature := append([]byte(nil), store.values[signatureKey]...)
	store.values[signatureKey] = bytes.Repeat([]byte{0}, len(originalSignature))
	if _, err := source.Read(context.Background(), *status.InputRef, MaxOutputBytes); !errors.Is(err, ErrSourceUnauthenticated) {
		t.Fatalf("forged record signature err=%v, want ErrSourceUnauthenticated", err)
	}
	store.values[signatureKey] = originalSignature
}

func TestSourceRejectsForgedOrMismatchedProducerBeforeDecoding(t *testing.T) {
	input := testInput(t)
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Job.DeepCopy()
	job.UID = types.UID("job-uid-2")
	job.Status.Succeeded = 1
	pod := completedPod(job, "critic-pod", "pod-uid-2")
	kube := testClient(t, job, pod)
	store := newMemoryStore("s3://artifacts/agents/v3")
	auth := testAuth(t)
	runner, err := NewRunner(RunnerOptions{Client: kube, Logs: &fixedLogs{frame: mustFrame(t, emptyCorroborationInput(t))}, Store: store, Auth: auth, Build: testBuildOptions()})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Ensure(context.Background(), input)
	if err != nil || status.InputRef == nil {
		t.Fatalf("seed runner status=%#v err=%v", status, err)
	}
	source, err := NewSource(SourceOptions{Reader: kube, Store: store, Auth: auth, Namespace: input.Run.Namespace, Bucket: "artifacts", Prefix: "agents/v3"})
	if err != nil {
		t.Fatal(err)
	}
	job.OwnerReferences[0].UID = types.UID("foreign-run")
	if err := kube.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	forged := []byte(`{"schemaVersion":"agents.astatide.com/finding-corroboration/v1alpha1","findings":[],"evidence":[],"verdict":"Accepted"}`)
	// The source must stop at producer authentication, before it can observe
	// or decode a forged result body.
	store.values[strings.TrimPrefix(status.InputRef.URI, "s3://artifacts/agents/v3/")] = forged
	if _, err := source.Read(context.Background(), *status.InputRef, MaxOutputBytes); !errors.Is(err, ErrSourceUnauthenticated) {
		t.Fatalf("foreign Job err=%v, want authentication failure", err)
	}
	job.OwnerReferences[0].UID = types.UID(input.Run.UID)
	if err := kube.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(context.Background(), *status.InputRef, MaxOutputBytes); err == nil {
		t.Fatal("forged output object was accepted")
	}
}

func TestRunnerAndSourceRejectDuplicatePodsAndMutatedJobs(t *testing.T) {
	input := testInput(t)
	plan, err := Build(input, testBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Job.DeepCopy()
	job.UID = types.UID("job-uid-3")
	job.Status.Succeeded = 1
	first := completedPod(job, "critic-pod-a", "pod-uid-a")
	second := completedPod(job, "critic-pod-b", "pod-uid-b")
	kube := testClient(t, job, first, second)
	store := newMemoryStore("s3://artifacts/agents/v3")
	runner, err := NewRunner(RunnerOptions{Client: kube, Logs: &fixedLogs{frame: mustFrame(t, emptyCorroborationInput(t))}, Store: store, Auth: testAuth(t), Build: testBuildOptions()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Ensure(context.Background(), input); !errors.Is(err, ErrDuplicateOutput) {
		t.Fatalf("duplicate pod err=%v, want ErrDuplicateOutput", err)
	}
	if err := kube.Delete(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	first.Spec.HostNetwork = true
	if err := kube.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Ensure(context.Background(), input); !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("forged Pod security err=%v, want ErrPodIdentity", err)
	}
	first.Spec.HostNetwork = false
	if err := kube.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	job.Spec.Template.Spec.Containers[0].Command = []string{"/bin/false"}
	if err := kube.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Ensure(context.Background(), input); !errors.Is(err, ErrJobConflict) && !errors.Is(err, ErrJobIdentity) {
		t.Fatalf("mutated Job err=%v, want conflict", err)
	}
}

type fixedLogs struct{ frame []byte }

func (f *fixedLogs) Read(_ context.Context, _, _, _ string, max int64) ([]byte, error) {
	if int64(len(f.frame)) > max {
		return nil, ErrOutputOversized
	}
	return append([]byte(nil), f.frame...), nil
}

type memoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
	uri    string
}

var _ artifacts.Store = (*memoryStore)(nil)

func newMemoryStore(uri string) *memoryStore {
	return &memoryStore{values: map[string][]byte{}, uri: strings.TrimSuffix(uri, "/")}
}

func testAuth(t *testing.T) *Ed25519Authenticator {
	t.Helper()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	auth, err := NewEd25519Authenticator(private, nil)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func (s *memoryStore) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.values[key]; ok {
		if old == nil {
			return false, s.uri + "/" + key, fmt.Errorf("nil existing object")
		}
		return false, s.uri + "/" + key, nil
	}
	s.values[key] = append([]byte(nil), body...)
	return true, s.uri + "/" + key, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.values[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), body...), nil
}

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func testInput(t *testing.T) Input {
	t.Helper()
	snapshot := testSnapshot()
	input, err := FromSnapshot(snapshot, testSpecDigest(t, snapshot), testPatchRef(), testContextRef())
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func testSnapshot() resolved.Snapshot {
	image := "ghcr.io/astatide/agw-runtime@sha256:" + strings.Repeat("a", 64)
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "critic-run", UID: "run-uid-1", Generation: 7},
		BaseSHA:       strings.Repeat("d", 40),
		Spec:          v1alpha1.AgentRunSpec{Source: v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1}, Scope: v1alpha1.ScopeSpec{Paths: []string{"src/**"}}},
		Agent:         v1alpha1.AgentSpec{Runtime: v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: image}},
		Gate: v1alpha1.GateSpec{
			Verify:  v1alpha1.VerifySpec{Image: image, FromCleanCheckout: true, Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}}, Timeout: "20m"},
			Require: v1alpha1.GateRequirements{ScopeRespected: true, MaxFilesChanged: 10, MaxDiffLines: 100, NoBinaryFiles: true},
			Signals: &v1alpha1.GateSignalsSpec{ExecutionWeightBasisPoints: 7000, MinScoreBasisPoints: 8500, Critic: &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 3000, ModelRouteRef: "critic-route", MaxFindings: 8}},
			OnFail:  "Rejected", Mode: v1alpha1.GateShadow,
		},
		ModelRoute:       v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "worker", Kind: "openai-responses", Model: "gpt-worker", Family: "openai", CredentialRef: "worker-credential", Priority: 1}}},
		CriticModelRoute: &v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "critic", Kind: "anthropic-messages", Model: "claude-critic", Family: "anthropic", CredentialRef: "critic-credential", Priority: 1}}},
		References: resolved.References{
			Gate:             resolved.ObjectVersion{Name: "go-default", UID: "gate-uid", Generation: 3},
			ModelRoute:       resolved.ObjectVersion{Name: "worker-route", UID: "worker-route-uid", Generation: 4},
			CriticModelRoute: &resolved.ObjectVersion{Name: "critic-route", UID: "critic-route-uid", Generation: 5},
		},
	}
}

func testSpecDigest(t *testing.T, snapshot resolved.Snapshot) string {
	t.Helper()
	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func testPatchRef() v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{URI: "s3://artifacts/agents/v3/runs/run-uid-1/patch.diff", Digest: "sha256:" + strings.Repeat("b", 64), Kind: "patch", Name: "patch.diff", MediaType: "text/x-diff", SizeBytes: 128}
}

func testContextRef() v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{URI: "s3://artifacts/agents/v3/runs/run-uid-1/context-pack.json", Digest: "sha256:" + strings.Repeat("c", 64), Kind: "context-pack", Name: "context-pack.json", MediaType: "application/vnd.agw.context-pack+json", SizeBytes: 256}
}

func testBuildOptions() BuildOptions {
	agentGatewayImage := "cr.agentgateway.dev/agentgateway@sha256:" + strings.Repeat("1", 64)
	gateway := GatewayClientOptions{Endpoint: "http://agentgateway.agw-system.svc.cluster.local:8080", RouteRef: "critic-anthropic", ServiceAccountName: "agw-critic", TokenExpirationSeconds: 900, JWTCapability: GatewayJWTCapability{Issuer: "https://kubernetes.default.svc", Audience: "agents-gateway-critic", PolicyRef: "critic-jwt", Verified: true}, PodSelector: map[string]string{"app.kubernetes.io/name": "agentgateway"}}
	evidenceDigest, err := GatewayJWTCapabilityEvidenceDigest(gateway)
	if err != nil {
		panic(err)
	}
	gateway.JWTCapability.EvidenceDigest = evidenceDigest
	return BuildOptions{
		CriticImage:            "ghcr.io/astatide1337/agw-critic@sha256:" + strings.Repeat("f", 64),
		AgentGatewayImage:      agentGatewayImage,
		LockdownImage:          "ghcr.io/astatide1337/agw-lockdown@sha256:" + strings.Repeat("2", 64),
		AgentGatewayCapability: AgentGatewayCapability{Image: agentGatewayImage, Version: AgentGatewayVersion, AnthropicMessages: true, RetryAttemptsOne: true, EvidenceDigest: "sha256:" + strings.Repeat("3", 64)},
		Gateway:                gateway,
		ArtifactStore:          ArtifactStoreOptions{Endpoint: "https://objects.example.com", Region: "us-east-1", Bucket: "artifacts", ForcePathStyle: true, EgressCIDRs: []string{"203.0.113.10/32"}},
		MaxOutputBytes:         MaxOutputBytes,
		Timeout:                20 * 60 * 1e9,
	}
}

func hasSecretRefNamed(container corev1.Container, name string) bool {
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == name {
			return true
		}
	}
	return false
}

func emptyCorroborationInput(t *testing.T) []byte {
	t.Helper()
	body, err := findingcorroboration.CanonicalInputBytes(findingcorroboration.CorroborationInput{SchemaVersion: findingcorroboration.SchemaVersion, Findings: []findingcorroboration.CriticFinding{}, Evidence: []findingcorroboration.Evidence{}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustFrame(t *testing.T, body []byte) []byte {
	t.Helper()
	frame, err := EncodeOutputFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func rawFrame(body []byte) []byte {
	return []byte(OutputProtocol + base64.RawStdEncoding.EncodeToString(body) + "\n")
}

func completedPod(job *batchv1.Job, name, uid string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: job.Namespace, Name: name, UID: types.UID(uid),
		Labels:          mergeLabels(job.Spec.Template.Labels, map[string]string{jobControllerUIDLabel: string(job.UID)}),
		Annotations:     copyStringMap(job.Spec.Template.Annotations),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: job.UID, Controller: boolPtr(true)}},
	}, Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
}

func mergeLabels(left, right map[string]string) map[string]string {
	output := copyStringMap(left)
	for key, value := range right {
		output[key] = value
	}
	return output
}
