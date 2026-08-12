package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureIsIdempotentAndStampsImmutableIdentity(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	plan := testPlan(run, RoleWork, digest('a'), now)

	first, err := backend.Ensure(context.Background(), plan)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := backend.Ensure(context.Background(), plan)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if first.Name != second.Name || first.OwnerUID != second.OwnerUID || first.SpecDigest != second.SpecDigest {
		t.Fatalf("idempotent refs differ: %#v %#v", first, second)
	}

	var children sandboxv1beta1.SandboxList
	if err := backend.client.List(context.Background(), &children, client.InNamespace(run.Namespace)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children.Items) != 1 {
		t.Fatalf("got %d children, want one", len(children.Items))
	}
	child := children.Items[0]
	if child.Name != first.Name {
		t.Fatalf("child name %q does not match ref %q", child.Name, first.Name)
	}
	if child.Labels[SpecDigestLabelKey] != digestLabelValue(plan.SpecDigest) || child.Annotations[SpecDigestAnnotationKey] != plan.SpecDigest {
		t.Fatalf("digest identity was not stamped: labels=%v annotations=%v", child.Labels, child.Annotations)
	}
	if child.Labels[RunUIDLabelKey] != string(run.UID) || child.Labels[RoleLabelKey] != string(RoleWork) {
		t.Fatalf("run identity was not stamped: %v", child.Labels)
	}
	if len(child.OwnerReferences) != 1 || child.OwnerReferences[0].UID != run.UID || child.OwnerReferences[0].Controller == nil || !*child.OwnerReferences[0].Controller {
		t.Fatalf("owner reference is not controller-owned: %#v", child.OwnerReferences)
	}
}

func TestEnsureRejectsDigestConflict(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	if _, err := backend.Ensure(context.Background(), testPlan(run, RoleWork, digest('a'), now)); err != nil {
		t.Fatalf("seed ensure: %v", err)
	}
	_, err := backend.Ensure(context.Background(), testPlan(run, RoleWork, digest('b'), now))
	if !errors.Is(err, ErrSpecDigestConflict) {
		t.Fatalf("got %v, want digest conflict", err)
	}
}

func TestEnsureRejectsExistingSandboxSpecMutationsEvenWhenFingerprintIsForged(t *testing.T) {
	now := fixedTime()
	run := testRun()
	cases := []struct {
		name   string
		mutate func(*sandboxv1beta1.Sandbox)
	}{
		{name: "image", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").Image = "ghcr.io/attacker/agent@sha256:" + strings.Repeat("b", 64)
		}},
		{name: "command", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").Command = []string{"/bin/attacker"}
		}},
		{name: "args", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").Args = []string{"--unsafe"}
		}},
		{name: "environment", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").Env = []corev1.EnvVar{{Name: "AGW_ATTACK", Value: "1"}}
		}},
		{name: "init ordering", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.PodTemplate.Spec.InitContainers[0], s.Spec.PodTemplate.Spec.InitContainers[1] = s.Spec.PodTemplate.Spec.InitContainers[1], s.Spec.PodTemplate.Spec.InitContainers[0]
		}},
		{name: "added setup container", mutate: func(s *sandboxv1beta1.Sandbox) {
			init := testContainer("untrusted-setup", 1000, true, nil)
			s.Spec.PodTemplate.Spec.InitContainers = append([]corev1.Container{init}, s.Spec.PodTemplate.Spec.InitContainers...)
		}},
		{name: "lockdown command", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "lockdown").Command = []string{"sh", "-c", "echo bypass"}
		}},
		{name: "mount", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").VolumeMounts = []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}}
		}},
		{name: "volume", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "attacker", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}},
		{name: "PVC template", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.VolumeClaimTemplates = []sandboxv1beta1.PersistentVolumeClaimTemplate{{
				EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "workspace"},
				Spec:                   corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
			}}
		}},
		{name: "service", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.Service = boolPtr(true)
		}},
		{name: "lifecycle policy", mutate: func(s *sandboxv1beta1.Sandbox) {
			policy := sandboxv1beta1.ShutdownPolicyRetain
			s.Spec.ShutdownPolicy = &policy
		}},
		{name: "lifecycle deadline", mutate: func(s *sandboxv1beta1.Sandbox) {
			deadline := s.Spec.ShutdownTime.DeepCopy()
			deadline.Time = deadline.Time.Add(time.Minute)
			s.Spec.ShutdownTime = deadline
		}},
		{name: "security posture", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.RunAsUser = int64Ptr(2000)
		}},
		{name: "metadata label", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Labels["agents.astatide.com/unexpected"] = "mutation"
		}},
		{name: "owner reference name", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.OwnerReferences[0].Name = "different-run"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := testBackend(t, now)
			plan := testPlan(run, RoleWork, digest('a'), now)
			if _, err := backend.Ensure(t.Context(), plan); err != nil {
				t.Fatalf("seed ensure: %v", err)
			}
			name, err := ChildName(run.UID, RoleWork)
			if err != nil {
				t.Fatal(err)
			}
			var current sandboxv1beta1.Sandbox
			if err := backend.client.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: name}, &current); err != nil {
				t.Fatalf("get seeded Sandbox: %v", err)
			}
			tc.mutate(&current)
			// A forged annotation must not turn a changed actual spec into an
			// apparently idempotent child.
			current.Annotations[SandboxSpecFingerprintAnnotationKey] = sandboxSpecFingerprint(current.Spec)
			if err := backend.client.Update(t.Context(), &current); err != nil {
				t.Fatalf("persist mutation: %v", err)
			}
			if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrSandboxSpecConflict) {
				t.Fatalf("Ensure mutation error=%v, want sandbox spec conflict", err)
			}
		})
	}
}

func TestEnsureRejectsForgedFingerprintForChangedSpec(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	plan := testPlan(run, RoleWork, digest('a'), now)
	if _, err := backend.Ensure(t.Context(), plan); err != nil {
		t.Fatalf("seed ensure: %v", err)
	}
	name, err := ChildName(run.UID, RoleWork)
	if err != nil {
		t.Fatal(err)
	}
	var current sandboxv1beta1.Sandbox
	if err := backend.client.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: name}, &current); err != nil {
		t.Fatal(err)
	}
	containerByName(&current, "agent").Image = "ghcr.io/attacker/agent@sha256:" + strings.Repeat("c", 64)
	// Forge the expected value, rather than the changed value. The actual
	// normalized spec comparison must still reject this object.
	current.Annotations[SandboxSpecFingerprintAnnotationKey] = sandboxSpecFingerprint(plan.Sandbox.Spec)
	if err := backend.client.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrSandboxSpecConflict) {
		t.Fatalf("Ensure forged fingerprint error=%v, want sandbox spec conflict", err)
	}
}

func TestObserveAndDeleteRejectSpecAndAnnotationMutationAfterRestart(t *testing.T) {
	for _, operation := range []string{"observe", "delete"} {
		t.Run(operation, func(t *testing.T) {
			now := fixedTime()
			run := testRun()
			backend := testBackend(t, now)
			ref, err := backend.Ensure(t.Context(), testPlan(run, RoleWork, digest('a'), now))
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}

			// Reconstruct only the persisted handle to model a controller restart;
			// no intended Sandbox object is available on this path.
			restartedRef := SandboxRef{
				Namespace: ref.Namespace, Name: ref.Name, Kind: ChildKindSandbox, UID: ref.UID,
				OwnerUID: ref.OwnerUID, Role: ref.Role, SpecDigest: ref.SpecDigest,
				PlanFingerprint: ref.PlanFingerprint,
			}
			var current sandboxv1beta1.Sandbox
			key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
			if err := backend.client.Get(t.Context(), key, &current); err != nil {
				t.Fatal(err)
			}
			containerByName(&current, "agent").Image = "ghcr.io/attacker/agent@sha256:" + strings.Repeat("d", 64)
			current.Annotations[SandboxSpecFingerprintAnnotationKey] = sandboxSpecFingerprint(current.Spec)
			if err := backend.client.Update(t.Context(), &current); err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "observe":
				_, err = backend.Observe(t.Context(), restartedRef)
			case "delete":
				err = backend.Delete(t.Context(), restartedRef)
			}
			if !errors.Is(err, ErrSandboxSpecConflict) {
				t.Fatalf("%s mutation error=%v, want sandbox spec conflict", operation, err)
			}
			if err := backend.client.Get(t.Context(), key, &sandboxv1beta1.Sandbox{}); err != nil {
				t.Fatalf("mutated Sandbox was deleted or unavailable: %v", err)
			}
		})
	}
}

func TestAgentSandboxBackendRejectsMismatchedChildKind(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	ref, err := backend.Ensure(t.Context(), testPlan(run, RoleWork, digest('a'), now))
	if err != nil {
		t.Fatal(err)
	}
	ref.Kind = ChildKindJob
	if _, err := backend.Observe(t.Context(), ref); !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("Observe mismatched kind error=%v, want reference conflict", err)
	}
	if err := backend.Delete(t.Context(), ref); !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("Delete mismatched kind error=%v, want reference conflict", err)
	}
}

func TestEnsureAcceptsDocumentedSandboxAndAPIServerDefaults(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	plan := testPlan(run, RoleWork, digest('a'), now)
	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatalf("seed ensure: %v", err)
	}
	var current sandboxv1beta1.Sandbox
	if err := backend.client.Get(t.Context(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	pod := &current.Spec.PodTemplate.Spec
	pod.DNSPolicy = corev1.DNSClusterFirst
	pod.SchedulerName = corev1.DefaultSchedulerName
	pod.EnableServiceLinks = boolPtr(true)
	pod.TerminationGracePeriodSeconds = int64Ptr(30)
	priority := int32(0)
	pod.Priority = &priority
	preemption := corev1.PreemptLowerPriority
	pod.PreemptionPolicy = &preemption
	for index := range pod.InitContainers {
		simulateAPIServerContainerDefaults(&pod.InitContainers[index])
	}
	for index := range pod.Containers {
		simulateAPIServerContainerDefaults(&pod.Containers[index])
	}
	current.Annotations[sandboxv1beta1.SandboxPodNameAnnotation] = ref.Name + "-pod"
	current.Annotations["opentelemetry.io/trace-context"] = `{"traceparent":"00-test"}`
	current.Annotations[SandboxSpecFingerprintAnnotationKey] = sandboxSpecFingerprint(current.Spec)
	if err := backend.client.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Ensure(t.Context(), plan); err != nil {
		t.Fatalf("documented defaults were rejected: %v", err)
	}
}

func TestEnsureRejectsExtraOwnerReference(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	plan := testPlan(run, RoleWork, digest('a'), now)
	if _, err := backend.Ensure(t.Context(), plan); err != nil {
		t.Fatalf("seed ensure: %v", err)
	}
	name, err := ChildName(run.UID, RoleWork)
	if err != nil {
		t.Fatal(err)
	}
	var current sandboxv1beta1.Sandbox
	if err := backend.client.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: name}, &current); err != nil {
		t.Fatal(err)
	}
	current.OwnerReferences = append(current.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: types.UID("foreign")})
	if err := backend.client.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrForeignChild) {
		t.Fatalf("Ensure extra owner reference error=%v, want foreign child", err)
	}
}

func TestEnsureAndObserveRejectMissingAndForeignChildren(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	digestValue := digest('a')
	plan := testPlan(run, RoleVerify, digestValue, now)
	_, ref, err := backend.prepare(plan)
	if err != nil {
		t.Fatal(err)
	}

	missing, err := backend.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe missing child: %v", err)
	}
	if missing.Exists {
		t.Fatal("missing child was reported as existing")
	}

	foreign := testPlan(run, RoleVerify, digestValue, now).Sandbox
	foreign.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: ownerAPIVersion, Kind: ownerKind, Name: "other-run", UID: types.UID("foreign"), Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
	}}
	foreign.Name = ref.Name
	foreign.Namespace = run.Namespace
	foreign.Labels = map[string]string{SpecDigestLabelKey: digestLabelValue(digestValue), RunUIDLabelKey: string(run.UID), RoleLabelKey: string(RoleVerify)}
	foreign.Annotations = map[string]string{SpecDigestAnnotationKey: digestValue}
	if err := backend.client.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign child: %v", err)
	}
	if _, err := backend.Ensure(context.Background(), plan); !errors.Is(err, ErrForeignChild) {
		t.Fatalf("ensure foreign child: got %v, want foreign-child error", err)
	}
	if _, err := backend.Observe(context.Background(), ref); !errors.Is(err, ErrForeignChild) {
		t.Fatalf("observe foreign child: got %v, want foreign-child error", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	ref, err := backend.Ensure(context.Background(), testPlan(run, RoleWork, digest('a'), now))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := backend.Delete(context.Background(), ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := backend.Delete(context.Background(), ref); err != nil {
		t.Fatalf("repeat delete should be idempotent: %v", err)
	}
	observation, err := backend.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe deleted child: %v", err)
	}
	if observation.Exists {
		t.Fatal("deleted child still exists")
	}
}

func TestObserveReadyAndFinishedIsBounded(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	ref, err := backend.Ensure(context.Background(), testPlan(run, RoleWork, digest('a'), now))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var child sandboxv1beta1.Sandbox
	if err := backend.client.Get(context.Background(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &child); err != nil {
		t.Fatalf("get child: %v", err)
	}
	child.Status.Conditions = []metav1.Condition{
		{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "DependenciesReady"},
		{Type: string(sandboxv1beta1.SandboxConditionFinished), Status: metav1.ConditionTrue, Reason: "PodSucceeded"},
	}
	if err := backend.client.Update(context.Background(), &child); err != nil {
		t.Fatalf("update status fixture: %v", err)
	}
	observation, err := backend.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !observation.Exists || !observation.Ready || !observation.Finished || len(observation.Conditions) != 2 {
		t.Fatalf("unexpected observation: %#v", observation)
	}

	child.Status.Conditions = make([]metav1.Condition, MaxObservedConditions+1)
	if err := backend.client.Update(context.Background(), &child); err != nil {
		t.Fatalf("update oversized status fixture: %v", err)
	}
	if _, err := backend.Observe(context.Background(), ref); !errors.Is(err, ErrObservationTooLarge) {
		t.Fatalf("oversized observation: got %v, want bound error", err)
	}
}

func TestEnsureRejectsInvalidSecurityPosture(t *testing.T) {
	now := fixedTime()
	run := testRun()
	cases := []struct {
		name   string
		mutate func(*sandboxv1beta1.Sandbox)
	}{
		{name: "host path volume", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}},
		{name: "privileged regular container", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.Privileged = boolPtr(true)
		}},
		{name: "allow privilege escalation omitted", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.AllowPrivilegeEscalation = nil
		}},
		{name: "allow privilege escalation enabled", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.AllowPrivilegeEscalation = boolPtr(true)
		}},
		{name: "host port regular container", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}},
		{name: "host port init container", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "clone").Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}},
		{name: "unmasked proc mount regular container", mutate: func(s *sandboxv1beta1.Sandbox) {
			procMount := corev1.UnmaskedProcMount
			containerByName(s, "agent").SecurityContext.ProcMount = &procMount
		}},
		{name: "unmasked proc mount init container", mutate: func(s *sandboxv1beta1.Sandbox) {
			procMount := corev1.UnmaskedProcMount
			initContainerByName(s, "clone").SecurityContext.ProcMount = &procMount
		}},
		{name: "regular container security context omitted", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext = nil
		}},
		{name: "regular container does not drop all", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.Capabilities.Drop = nil
		}},
		{name: "regular container adds capability", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.Capabilities.Add = []corev1.Capability{"NET_RAW"}
		}},
		{name: "regular container run as non-root omitted", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.RunAsNonRoot = nil
		}},
		{name: "regular container run as root", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.RunAsNonRoot = boolPtr(false)
			root := int64(0)
			containerByName(s, "agent").SecurityContext.RunAsUser = &root
		}},
		{name: "regular container writable root filesystem", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.ReadOnlyRootFilesystem = boolPtr(false)
		}},
		{name: "agent has wrong uid", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.RunAsUser = int64Ptr(2000)
		}},
		{name: "broker has wrong gid", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "broker").SecurityContext.RunAsGroup = int64Ptr(1000)
		}},
		{name: "init container writable root filesystem", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "clone").SecurityContext.ReadOnlyRootFilesystem = boolPtr(false)
		}},
		{name: "non-lockdown init adds capability", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "clone").SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		}},
		{name: "lockdown drops no all", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "lockdown").SecurityContext.Capabilities.Drop = nil
		}},
		{name: "lockdown adds another capability", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "lockdown").SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN", "NET_RAW"}
		}},
		{name: "lockdown omits net admin", mutate: func(s *sandboxv1beta1.Sandbox) {
			initContainerByName(s, "lockdown").SecurityContext.Capabilities.Add = nil
		}},
		{name: "lockdown is not last init", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.PodTemplate.Spec.InitContainers = append(s.Spec.PodTemplate.Spec.InitContainers, testContainer("after-lockdown", 1000, true, nil))
		}},
		{name: "host users omitted", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.HostUsers = nil }},
		{name: "host users enabled", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.HostUsers = boolPtr(true) }},
		{name: "service account token omitted", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.AutomountServiceAccountToken = nil }},
		{name: "service account token enabled", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.AutomountServiceAccountToken = boolPtr(true) }},
		{name: "host network", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.HostNetwork = true }},
		{name: "host pid", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.HostPID = true }},
		{name: "host ipc", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.HostIPC = true }},
		{name: "restart policy", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways }},
		{name: "pod seccomp omitted", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.PodTemplate.Spec.SecurityContext.SeccompProfile = nil }},
		{name: "pod seccomp unconfined", mutate: func(s *sandboxv1beta1.Sandbox) {
			s.Spec.PodTemplate.Spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
		}},
		{name: "container seccomp unconfined", mutate: func(s *sandboxv1beta1.Sandbox) {
			containerByName(s, "agent").SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
		}},
		{name: "shutdown omitted", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.ShutdownTime = nil }},
		{name: "shutdown too far", mutate: func(s *sandboxv1beta1.Sandbox) {
			future := metav1.NewTime(now.Add(DefaultMaxShutdownDuration + time.Second))
			s.Spec.ShutdownTime = &future
		}},
		{name: "shutdown policy omitted", mutate: func(s *sandboxv1beta1.Sandbox) { s.Spec.ShutdownPolicy = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := testBackend(t, now)
			plan := testPlan(run, RoleWork, digest('a'), now)
			tc.mutate(plan.Sandbox)
			if _, err := backend.Ensure(context.Background(), plan); !errors.Is(err, ErrInvalidSecurityPosture) {
				t.Fatalf("got %v, want invalid security posture", err)
			}
		})
	}
}

func TestEnsureAcceptsValidWorkAndVerifyShapes(t *testing.T) {
	now := fixedTime()
	run := testRun()
	for _, role := range []Role{RoleWork, RoleVerify} {
		t.Run(string(role), func(t *testing.T) {
			backend := testBackend(t, now)
			ref, err := backend.Ensure(context.Background(), testPlan(run, role, digest('a'), now))
			if err != nil {
				t.Fatalf("ensure valid %s Sandbox: %v", role, err)
			}
			if ref.Role != role || ref.Name == "" {
				t.Fatalf("unexpected %s Sandbox ref: %#v", role, ref)
			}
		})
	}
}

func TestCleanupOwnedRecoversChildWhenStatusReferenceWasLost(t *testing.T) {
	now := fixedTime()
	run := testRun()
	backend := testBackend(t, now)
	if _, err := backend.Ensure(t.Context(), testPlan(run, RoleWork, digest('a'), now)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := backend.CleanupOwned(t.Context(), run.Namespace, run.UID, RoleWork, digest('a')); err != nil {
		t.Fatalf("cleanup owned: %v", err)
	}
	name, err := ChildName(run.UID, RoleWork)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.client.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: name}, &sandboxv1beta1.Sandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("recovered child lookup error=%v, want NotFound", err)
	}
	if err := backend.CleanupOwned(t.Context(), run.Namespace, run.UID, RoleWork, digest('a')); err != nil {
		t.Fatalf("idempotent orphan cleanup: %v", err)
	}
}

func testBackend(t *testing.T, now time.Time) *AgentSandboxBackend {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sandboxv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Sandbox scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	backend, err := NewAgentSandboxBackend(c, BackendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	return backend
}

func testRun() *v1alpha1.AgentRun {
	return &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "agw-runs", Name: "repair-427", UID: types.UID("1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15")}}
}

func testPlan(run *v1alpha1.AgentRun, role Role, specDigest string, now time.Time) SandboxPlan {
	initContainers := []corev1.Container{
		testContainer("clone", 1000, true, nil),
		testContainer("skills", 1000, true, nil),
		testContainer("context", 0, false, nil),
		testContainer("lockdown", 0, false, []corev1.Capability{"NET_ADMIN"}),
	}
	containers := []corev1.Container{
		testContainer("agent", 1000, true, nil),
		testContainer("broker", 1337, true, nil),
	}
	if role == RoleVerify {
		initContainers = []corev1.Container{
			testContainer("fetch", 1000, true, nil),
			testContainer("apply", 1000, true, nil),
			testContainer("lockdown", 0, false, []corev1.Capability{"NET_ADMIN"}),
		}
		containers = []corev1.Container{testContainer("verify", 1000, true, nil)}
	}
	return SandboxPlan{Owner: run, Role: role, SpecDigest: specDigest, Sandbox: &sandboxv1beta1.Sandbox{
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{
					HostUsers:                    boolPtr(false),
					AutomountServiceAccountToken: boolPtr(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext:              &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					InitContainers:               initContainers,
					Containers:                   containers,
				}},
				Service: boolPtr(false),
			},
			Lifecycle: sandboxv1beta1.Lifecycle{ShutdownTime: timePtr(metav1.NewTime(now.Add(45 * time.Minute))), ShutdownPolicy: shutdownPolicyPtr(sandboxv1beta1.ShutdownPolicyDelete)},
		},
	}}
}

func testContainer(name string, uid int64, runAsNonRoot bool, add []corev1.Capability) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: "ghcr.io/astatide/agw-runtime@sha256:" + strings.Repeat("a", 64),
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                int64Ptr(uid),
			RunAsGroup:               int64Ptr(uid),
			RunAsNonRoot:             boolPtr(runAsNonRoot),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities:             &corev1.Capabilities{Add: add, Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

func containerByName(sandbox *sandboxv1beta1.Sandbox, name string) *corev1.Container {
	for index := range sandbox.Spec.PodTemplate.Spec.Containers {
		if sandbox.Spec.PodTemplate.Spec.Containers[index].Name == name {
			return &sandbox.Spec.PodTemplate.Spec.Containers[index]
		}
	}
	panic("regular container not found: " + name)
}

func initContainerByName(sandbox *sandboxv1beta1.Sandbox, name string) *corev1.Container {
	for index := range sandbox.Spec.PodTemplate.Spec.InitContainers {
		if sandbox.Spec.PodTemplate.Spec.InitContainers[index].Name == name {
			return &sandbox.Spec.PodTemplate.Spec.InitContainers[index]
		}
	}
	panic("init container not found: " + name)
}

func fixedTime() time.Time { return time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC) }

func digest(character byte) string { return "sha256:" + strings.Repeat(string(character), 64) }

func timePtr(value metav1.Time) *metav1.Time { return &value }

func int64Ptr(value int64) *int64 { return &value }

func shutdownPolicyPtr(value sandboxv1beta1.ShutdownPolicy) *sandboxv1beta1.ShutdownPolicy {
	return &value
}
