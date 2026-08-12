package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestJobBackendEnsureIsIdempotentAndConvertsWorkAndVerifyPlans(t *testing.T) {
	for _, role := range []Role{RoleWork, RoleVerify} {
		t.Run(string(role), func(t *testing.T) {
			backend, kube := newJobBackendTest(t, fixedTime())
			run := testRun()
			plan := testPlan(run, role, digest('a'), fixedTime())
			if role == RoleWork {
				addJobBackendPVCPlan(&plan)
			}

			first, err := backend.Ensure(t.Context(), plan)
			if err != nil {
				t.Fatalf("first ensure: %v", err)
			}
			second, err := backend.Ensure(t.Context(), plan)
			if err != nil {
				t.Fatalf("second ensure: %v", err)
			}
			if first.Name != second.Name || first.UID != second.UID || first.OwnerUID != second.OwnerUID || first.SpecDigest != second.SpecDigest {
				t.Fatalf("idempotent refs differ: %#v %#v", first, second)
			}

			var job batchv1.Job
			if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: first.Name}, &job); err != nil {
				t.Fatalf("get Job: %v", err)
			}
			if job.Labels[RunUIDLabelKey] != string(run.UID) || job.Labels[RoleLabelKey] != string(role) || job.Labels[ChildNameLabelKey] != first.Name {
				t.Fatalf("Job identity labels=%v", job.Labels)
			}
			if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != run.UID || !boolValue(job.OwnerReferences[0].Controller) {
				t.Fatalf("Job owner reference=%#v", job.OwnerReferences)
			}
			if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.Completions == nil || *job.Spec.Completions != 1 {
				t.Fatalf("unexpected Job retry/completion policy: %#v", job.Spec)
			}
			if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 45*60 {
				t.Fatalf("active deadline=%v, want 45 minutes", job.Spec.ActiveDeadlineSeconds)
			}
			if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
				t.Fatalf("restart policy=%q, want Never", job.Spec.Template.Spec.RestartPolicy)
			}
			if role == RoleWork {
				if got := containerNames(job.Spec.Template.Spec.Containers); got != "agent,broker" {
					t.Fatalf("work containers=%q", got)
				}
				var pvcList corev1.PersistentVolumeClaimList
				if err := kube.List(t.Context(), &pvcList, client.InNamespace(run.Namespace)); err != nil {
					t.Fatalf("list work PVCs: %v", err)
				}
				if len(pvcList.Items) != 1 {
					t.Fatalf("work PVC count=%d, want one", len(pvcList.Items))
				}
			} else if got := containerNames(job.Spec.Template.Spec.Containers); got != "verify" {
				t.Fatalf("verify containers=%q", got)
			}
		})
	}
}

func TestJobBackendUsesAgentSandboxPVCNameAndRewritesClaim(t *testing.T) {
	now := fixedTime()
	backend, kube := newJobBackendTest(t, now)
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	addJobBackendPVCPlan(&plan)

	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	claimName := "workspace-" + ref.Name
	var job batchv1.Job
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &job); err != nil {
		t.Fatalf("get Job: %v", err)
	}
	if len(job.Spec.Template.Spec.Volumes) != 1 || job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil || job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != claimName {
		t.Fatalf("Job PVC volume was not rewritten: %#v", job.Spec.Template.Spec.Volumes)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: claimName}, &pvc); err != nil {
		t.Fatalf("get generated PVC: %v", err)
	}
	if pvc.Labels[ChildNameLabelKey] != ref.Name {
		t.Fatalf("PVC identity labels=%v", pvc.Labels)
	}
	if len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].UID != run.UID || !boolValue(pvc.OwnerReferences[0].Controller) {
		t.Fatalf("PVC owner reference=%#v", pvc.OwnerReferences)
	}

	if err := backend.Delete(t.Context(), ref); err != nil {
		t.Fatalf("delete Job: %v", err)
	}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job after delete: %v, want NotFound", err)
	}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: claimName}, &pvc); err != nil {
		t.Fatalf("retained PVC after Job delete: %v", err)
	}
	if err := backend.Delete(t.Context(), ref); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
}

func TestJobBackendCleanupOwnedRecoversChildWhenStatusReferenceWasLost(t *testing.T) {
	now := fixedTime()
	backend, kube := newJobBackendTest(t, now)
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	addJobBackendPVCPlan(&plan)
	if _, err := backend.Ensure(t.Context(), plan); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := backend.CleanupOwned(t.Context(), run.Namespace, run.UID, RoleWork, digest('a')); err != nil {
		t.Fatalf("cleanup owned: %v", err)
	}
	name, err := ChildName(run.UID, RoleWork)
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: name}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("recovered Job lookup error=%v, want NotFound", err)
	}
	if err := backend.CleanupOwned(t.Context(), run.Namespace, run.UID, RoleWork, digest('a')); err != nil {
		t.Fatalf("idempotent orphan cleanup: %v", err)
	}
}

func TestJobBackendTimeoutConversionRoundsUpFromBackendClock(t *testing.T) {
	now := fixedTime()
	backend, kube := newJobBackendTest(t, now)
	run := testRun()
	plan := testPlan(run, RoleVerify, digest('a'), now)
	shutdown := metav1.NewTime(now.Add(90*time.Second + 500*time.Millisecond))
	plan.Sandbox.Spec.ShutdownTime = &shutdown
	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var job batchv1.Job
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &job); err != nil {
		t.Fatalf("get Job: %v", err)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 91 {
		t.Fatalf("active deadline=%v, want rounded-up 91 seconds", job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobBackendRejectsUnsupportedLifecycleServiceAndSecurity(t *testing.T) {
	now := fixedTime()
	run := testRun()
	cases := []struct {
		name   string
		mutate func(*SandboxPlan)
	}{
		{name: "service enabled", mutate: func(plan *SandboxPlan) { plan.Sandbox.Spec.Service = boolPtr(true) }},
		{name: "suspended lifecycle", mutate: func(plan *SandboxPlan) {
			plan.Sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
		}},
		{name: "host path", mutate: func(plan *SandboxPlan) {
			plan.Sandbox.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, _ := newJobBackendTest(t, now)
			plan := testPlan(run, RoleWork, digest('a'), now)
			tc.mutate(&plan)
			if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrInvalidSecurityPosture) {
				t.Fatalf("ensure error=%v, want invalid security posture", err)
			}
		})
	}
}

func TestJobBackendRejectsCallerOwnedJobControllerLabels(t *testing.T) {
	now := fixedTime()
	backend, _ := newJobBackendTest(t, now)
	plan := testPlan(testRun(), RoleVerify, digest('a'), now)
	plan.Sandbox.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{jobControllerUIDLabel: "caller-selected-uid"}
	if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("caller-owned Job controller label error=%v, want invalid plan", err)
	}
}

func TestJobBackendHandlesPartialPVCAndJobCreation(t *testing.T) {
	now := fixedTime()
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	addJobBackendPVCPlan(&plan)

	t.Run("PVC exists before Job", func(t *testing.T) {
		backend, kube := newJobBackendTest(t, now)
		_, expectedJob, expectedPVCs, _, err := backend.prepare(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(t.Context(), expectedPVCs[0].DeepCopy()); err != nil {
			t.Fatalf("seed PVC: %v", err)
		}
		ref, err := backend.Ensure(t.Context(), plan)
		if err != nil {
			t.Fatalf("retry after PVC-only partial state: %v", err)
		}
		var job batchv1.Job
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(expectedJob), &job); err != nil {
			t.Fatalf("get Job: %v", err)
		}
		if job.Name != ref.Name {
			t.Fatalf("Job name=%q, ref=%q", job.Name, ref.Name)
		}
	})

	t.Run("Job exists before PVC", func(t *testing.T) {
		backend, kube := newJobBackendTest(t, now)
		_, expectedJob, expectedPVCs, _, err := backend.prepare(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(t.Context(), expectedJob.DeepCopy()); err != nil {
			t.Fatalf("seed Job: %v", err)
		}
		if ref, err := backend.Ensure(t.Context(), plan); err != nil {
			t.Fatalf("retry after Job-only partial state: %v", err)
		} else if ref.Name != expectedJob.Name {
			t.Fatalf("ref name=%q, want %q", ref.Name, expectedJob.Name)
		}
		var pvc corev1.PersistentVolumeClaim
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(expectedPVCs[0]), &pvc); err != nil {
			t.Fatalf("missing PVC after Job-only retry: %v", err)
		}
	})
}

func TestJobBackendAcceptsCreateRaceOnlyAfterValidation(t *testing.T) {
	now := fixedTime()
	run := testRun()
	plan := testPlan(run, RoleVerify, digest('a'), now)
	base := fakeJobBackendClient(t)
	intercepted := fake.NewClientBuilder().WithScheme(base.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, ok := object.(*batchv1.Job); ok {
				if err := underlying.Create(ctx, object, options...); err != nil {
					return err
				}
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, object.GetName())
			}
			return underlying.Create(ctx, object, options...)
		},
	}).Build()
	backend, err := NewJobBackend(intercepted, BackendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Ensure(t.Context(), plan); err != nil {
		t.Fatalf("create race should converge: %v", err)
	}
}

func TestJobBackendAcceptsRealisticAPIServerJobDefaulting(t *testing.T) {
	now := fixedTime()
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	plan.Sandbox.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{"app": "agent-work"}

	base := fakeJobBackendClient(t)
	intercepted := fake.NewClientBuilder().WithScheme(base.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			job, ok := object.(*batchv1.Job)
			if !ok {
				return underlying.Create(ctx, object, options...)
			}

			// Simulate the fields visible after a real API-server/controller
			// round trip. The UID is generated by the server before the Job
			// controller publishes these labels; the value itself is opaque to
			// this backend and must not enter the caller-owned fingerprint.
			controllerUID := "api-server-generated-job-uid"
			job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{jobControllerUIDLabel: controllerUID}}
			job.Spec.Template.Labels = map[string]string{
				"app":                       "agent-work",
				jobControllerUIDLabel:       controllerUID,
				jobNameLabel:                job.Name,
				legacyJobControllerUIDLabel: controllerUID,
				legacyJobNameLabel:          job.Name,
			}
			job.Spec.ManualSelector = boolPtr(false)
			job.Spec.Suspend = boolPtr(false)
			completionMode := batchv1.NonIndexedCompletion
			job.Spec.CompletionMode = &completionMode
			replacementPolicy := batchv1.TerminatingOrFailed
			job.Spec.PodReplacementPolicy = &replacementPolicy

			pod := &job.Spec.Template.Spec
			pod.DNSPolicy = corev1.DNSClusterFirst
			pod.SchedulerName = corev1.DefaultSchedulerName
			pod.EnableServiceLinks = boolPtr(true)
			pod.TerminationGracePeriodSeconds = int64Ptr(30)
			priority := int32(0)
			pod.Priority = &priority
			for index := range pod.InitContainers {
				simulateAPIServerContainerDefaults(&pod.InitContainers[index])
			}
			for index := range pod.Containers {
				simulateAPIServerContainerDefaults(&pod.Containers[index])
			}
			return underlying.Create(ctx, object, options...)
		},
	}).Build()
	backend, err := NewJobBackend(intercepted, BackendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatalf("Ensure rejected realistic API-server defaults: %v", err)
	}
	if _, err := backend.Ensure(t.Context(), plan); err != nil {
		t.Fatalf("repeat Ensure rejected realistic API-server defaults: %v", err)
	}
	observation, err := backend.Observe(t.Context(), ref)
	if err != nil {
		t.Fatalf("Observe rejected realistic API-server defaults: %v", err)
	}
	if !observation.Exists {
		t.Fatal("defaulted Job was not observed")
	}
}

func TestJobBackendObserveFailsClosedWhenGeneratedPVCIsMissing(t *testing.T) {
	now := fixedTime()
	backend, kube := newJobBackendTest(t, now)
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	addJobBackendPVCPlan(&plan)
	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	claimName := "workspace-" + ref.Name
	var pvc corev1.PersistentVolumeClaim
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: claimName}, &pvc); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Observe(t.Context(), ref); !errors.Is(err, ErrMissingGeneratedPVC) {
		t.Fatalf("Observe after generated PVC deletion error=%v, want missing-PVC error", err)
	}
}

func simulateAPIServerContainerDefaults(container *corev1.Container) {
	container.ImagePullPolicy = defaultImagePullPolicy(container.Image)
	container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	container.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
}

func TestJobBackendRejectsForeignAndSpecConflictsWithoutMutation(t *testing.T) {
	now := fixedTime()
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	addJobBackendPVCPlan(&plan)

	t.Run("foreign Job", func(t *testing.T) {
		backend, kube := newJobBackendTest(t, now)
		_, expectedJob, _, _, err := backend.prepare(plan)
		if err != nil {
			t.Fatal(err)
		}
		expectedJob.OwnerReferences[0].UID = types.UID("foreign")
		if err := kube.Create(t.Context(), expectedJob); err != nil {
			t.Fatalf("seed foreign Job: %v", err)
		}
		if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrForeignChild) {
			t.Fatalf("ensure foreign Job error=%v", err)
		}
		var pvcs corev1.PersistentVolumeClaimList
		if err := kube.List(t.Context(), &pvcs, client.InNamespace(run.Namespace)); err != nil {
			t.Fatal(err)
		}
		if len(pvcs.Items) != 0 {
			t.Fatalf("foreign Job path created %d PVCs", len(pvcs.Items))
		}
	})

	t.Run("Job spec conflict", func(t *testing.T) {
		backend, kube := newJobBackendTest(t, now)
		ref, err := backend.Ensure(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		var job batchv1.Job
		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &job); err != nil {
			t.Fatal(err)
		}
		job.Spec.Template.Spec.Containers[0].Image = "ghcr.io/foreign/image@sha256:" + strings.Repeat("b", 64)
		if err := kube.Update(t.Context(), &job); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrJobSpecConflict) {
			t.Fatalf("mutated Job error=%v, want spec conflict", err)
		}
		var unchangedPVC corev1.PersistentVolumeClaim
		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: "workspace-" + ref.Name}, &unchangedPVC); err != nil {
			t.Fatalf("PVC disappeared after Job conflict: %v", err)
		}
	})

	t.Run("PVC spec conflict", func(t *testing.T) {
		backend, kube := newJobBackendTest(t, now)
		_, _, expectedPVCs, _, err := backend.prepare(plan)
		if err != nil {
			t.Fatal(err)
		}
		conflicting := expectedPVCs[0].DeepCopy()
		conflicting.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("99Gi")
		conflicting.Annotations[pvcSpecFingerprintAnnotationKey] = pvcSpecFingerprint(conflicting.Spec)
		if err := kube.Create(t.Context(), conflicting); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Ensure(t.Context(), plan); !errors.Is(err, ErrJobSpecConflict) {
			t.Fatalf("mutated PVC error=%v, want spec conflict", err)
		}
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(conflicting), &corev1.PersistentVolumeClaim{}); err != nil {
			t.Fatalf("conflicting PVC was deleted or mutated: %v", err)
		}
	})
}

func TestJobBackendObserveAndDeleteRejectSpecAndAnnotationMutationAfterRestart(t *testing.T) {
	for _, operation := range []string{"observe", "delete"} {
		t.Run(operation, func(t *testing.T) {
			now := fixedTime()
			backend, kube := newJobBackendTest(t, now)
			run := testRun()
			ref, err := backend.Ensure(t.Context(), testPlan(run, RoleVerify, digest('a'), now))
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			restartedRef := SandboxRef{
				Namespace: ref.Namespace, Name: ref.Name, Kind: ChildKindJob, UID: ref.UID,
				OwnerUID: ref.OwnerUID, Role: ref.Role, SpecDigest: ref.SpecDigest,
				PlanFingerprint: ref.PlanFingerprint,
			}
			var current batchv1.Job
			key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
			if err := kube.Get(t.Context(), key, &current); err != nil {
				t.Fatal(err)
			}
			current.Spec.Template.Spec.Containers[0].Image = "ghcr.io/attacker/verify@sha256:" + strings.Repeat("d", 64)
			current.Annotations[jobSpecFingerprintAnnotationKey] = jobSpecFingerprint(current.Spec)
			if err := kube.Update(t.Context(), &current); err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "observe":
				_, err = backend.Observe(t.Context(), restartedRef)
			case "delete":
				err = backend.Delete(t.Context(), restartedRef)
			}
			if !errors.Is(err, ErrJobSpecConflict) {
				t.Fatalf("%s mutation error=%v, want Job spec conflict", operation, err)
			}
			if err := kube.Get(t.Context(), key, &batchv1.Job{}); err != nil {
				t.Fatalf("mutated Job was deleted or unavailable: %v", err)
			}
		})
	}
}

func TestJobBackendRejectsMismatchedChildKind(t *testing.T) {
	now := fixedTime()
	backend, _ := newJobBackendTest(t, now)
	run := testRun()
	ref, err := backend.Ensure(t.Context(), testPlan(run, RoleWork, digest('a'), now))
	if err != nil {
		t.Fatal(err)
	}
	ref.Kind = ChildKindSandbox
	if _, err := backend.Observe(t.Context(), ref); !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("Observe mismatched kind error=%v, want reference conflict", err)
	}
	if err := backend.Delete(t.Context(), ref); !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("Delete mismatched kind error=%v, want reference conflict", err)
	}
}

func TestJobBackendObserveMapsActiveSuccessFailureAndBoundsConditions(t *testing.T) {
	now := fixedTime()
	backend, kube := newJobBackendTest(t, now)
	run := testRun()
	plan := testPlan(run, RoleVerify, digest('a'), now)
	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active = 1
	if err := kube.Status().Update(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
	observation, err := backend.Observe(t.Context(), ref)
	if err != nil {
		t.Fatalf("observe active: %v", err)
	}
	if !observation.Exists || !observation.Ready || observation.Finished {
		t.Fatalf("active observation=%#v", observation)
	}

	job.Status.Active = 0
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed", LastTransitionTime: metav1.NewTime(now)}}
	if err := kube.Status().Update(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
	observation, err = backend.Observe(t.Context(), ref)
	if err != nil {
		t.Fatalf("observe success: %v", err)
	}
	if !observation.Ready || !observation.Finished || len(observation.Conditions) != 1 || observation.Conditions[0].Type != string(batchv1.JobComplete) {
		t.Fatalf("success observation=%#v", observation)
	}

	job.Status.Succeeded = 0
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "Failed", LastTransitionTime: metav1.NewTime(now)}}
	if err := kube.Status().Update(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
	observation, err = backend.Observe(t.Context(), ref)
	if err != nil {
		t.Fatalf("observe failure: %v", err)
	}
	if !observation.Ready || !observation.Finished || observation.Conditions[0].Type != string(batchv1.JobFailed) {
		t.Fatalf("failure observation=%#v", observation)
	}

	job.Status.Conditions = make([]batchv1.JobCondition, MaxObservedConditions+1)
	if err := kube.Status().Update(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Observe(t.Context(), ref); !errors.Is(err, ErrObservationTooLarge) {
		t.Fatalf("oversized conditions error=%v", err)
	}
}

func TestJobBackendDeleteRejectsForeignReferencedPVC(t *testing.T) {
	now := fixedTime()
	backend, kube := newJobBackendTest(t, now)
	run := testRun()
	plan := testPlan(run, RoleWork, digest('a'), now)
	addJobBackendPVCPlan(&plan)
	ref, err := backend.Ensure(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	claimName := "workspace-" + ref.Name
	var pvc corev1.PersistentVolumeClaim
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: claimName}, &pvc); err != nil {
		t.Fatal(err)
	}
	pvc.OwnerReferences[0].UID = types.UID("foreign")
	if err := kube.Update(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(t.Context(), ref); !errors.Is(err, ErrForeignChild) {
		t.Fatalf("delete with foreign PVC error=%v", err)
	}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &batchv1.Job{}); err != nil {
		t.Fatalf("Job was deleted despite foreign PVC: %v", err)
	}
}

func TestJobBackendObserveMissingIsIdempotent(t *testing.T) {
	now := fixedTime()
	backend, _ := newJobBackendTest(t, now)
	run := testRun()
	name, err := ChildName(run.UID, RoleVerify)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(run, RoleVerify, digest('a'), now)
	_, _, _, ref, err := backend.prepare(plan)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != name {
		t.Fatalf("prepared child name=%q, want %q", ref.Name, name)
	}
	observation, err := backend.Observe(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Exists || observation.Ready || observation.Finished {
		t.Fatalf("missing observation=%#v", observation)
	}
	if err := backend.Delete(t.Context(), ref); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func addJobBackendPVCPlan(plan *SandboxPlan) {
	mode := corev1.PersistentVolumeFilesystem
	plan.Sandbox.Spec.VolumeClaimTemplates = []sandboxv1beta1.PersistentVolumeClaimTemplate{{
		EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "workspace", Labels: map[string]string{"storage": "workspace"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  &mode,
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}},
		},
	}}
	plan.Sandbox.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "workspace"}}}}
}

func newJobBackendTest(t *testing.T, now time.Time) (*JobBackend, client.Client) {
	t.Helper()
	kube := fakeJobBackendClient(t)
	backend, err := NewJobBackend(kube, BackendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return backend, kube
}

func fakeJobBackendClient(t *testing.T) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func containerNames(containers []corev1.Container) string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		names = append(names, container.Name)
	}
	return strings.Join(names, ",")
}
