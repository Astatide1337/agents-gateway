package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodProcessObserverProvesAgentExit(t *testing.T) {
	controller := true
	ref := sandbox.SandboxRef{Namespace: "agw-runs", Name: "work-sandbox", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: types.UID("run-uid"), Role: sandbox.RoleWork}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "work-pod", Namespace: ref.Namespace, UID: types.UID("pod-uid"),
			Labels:          map[string]string{sandbox.RunUIDLabelKey: "run-uid", sandbox.RoleLabelKey: "work"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: ref.Name, UID: ref.UID, Controller: &controller}},
		},
		Spec:   corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "agent"}, {Name: "broker"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", ContainerID: "containerd://agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 17}}}}},
	}
	exited, result, err := (podProcessObserver{client: fake.NewSimpleClientset(pod)}).ObserveAgentExit(context.Background(), ref)
	if err != nil || !exited || !result.Known || result.Code != 17 {
		t.Fatalf("exited=%v result=%#v err=%v", exited, result, err)
	}
}

func TestPodProcessObserverFailsClosedOnAmbiguousIdentity(t *testing.T) {
	controller := true
	ref := sandbox.SandboxRef{Namespace: "agw-runs", Name: "work-sandbox", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: types.UID("run-uid"), Role: sandbox.RoleWork}
	makePod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ref.Namespace, UID: types.UID(name + "-uid"),
			Labels:          map[string]string{sandbox.RunUIDLabelKey: "run-uid", sandbox.RoleLabelKey: "work"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: ref.Name, UID: ref.UID, Controller: &controller}},
		}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "agent"}}}}
	}
	_, _, err := (podProcessObserver{client: fake.NewSimpleClientset(makePod("one"), makePod("two"))}).ObserveAgentExit(context.Background(), ref)
	if !errors.Is(err, errProcessObserverIdentity) {
		t.Fatalf("error=%v", err)
	}

	foreign := makePod("foreign")
	foreign.OwnerReferences[0].UID = "foreign"
	_, _, err = (podProcessObserver{client: fake.NewSimpleClientset(foreign)}).ObserveAgentExit(context.Background(), ref)
	if !errors.Is(err, errProcessObserverIdentity) {
		t.Fatalf("foreign owner error=%v", err)
	}
}

func TestPodProcessObserverRejectsUnboundOrAmbiguousContainerStatus(t *testing.T) {
	controller := true
	ref := sandbox.SandboxRef{Namespace: "agw-runs", Name: "work-sandbox", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: types.UID("run-uid"), Role: sandbox.RoleWork}
	base := func() *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "work-pod", Namespace: ref.Namespace, UID: types.UID("pod-uid"),
			Labels:          map[string]string{sandbox.RunUIDLabelKey: "run-uid", sandbox.RoleLabelKey: "work"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: ref.Name, UID: ref.UID, Controller: &controller}},
		}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "agent"}, {Name: "broker"}}}}
	}
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{name: "missing pod uid", mutate: func(pod *corev1.Pod) { pod.UID = "" }},
		{name: "restartable pod", mutate: func(pod *corev1.Pod) { pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure }},
		{name: "terminated status without container identity", mutate: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
		}},
		{name: "duplicate agent statuses", mutate: func(pod *corev1.Pod) {
			status := corev1.ContainerStatus{Name: "agent", ContainerID: "containerd://agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{status, status}
		}},
		{name: "contradictory running and waiting state", mutate: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}, Waiting: &corev1.ContainerStateWaiting{}}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := base()
			test.mutate(pod)
			_, _, err := (podProcessObserver{client: fake.NewSimpleClientset(pod)}).ObserveAgentExit(context.Background(), ref)
			if !errors.Is(err, errProcessObserverIdentity) {
				t.Fatalf("error=%v, want identity failure", err)
			}
		})
	}
}

func TestPodProcessObserverTreatsRunningOrAbsentPodAsPending(t *testing.T) {
	ref := sandbox.SandboxRef{Namespace: "agw-runs", Name: "work-sandbox", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: types.UID("run-uid"), Role: sandbox.RoleWork}
	exited, result, err := (podProcessObserver{client: fake.NewSimpleClientset()}).ObserveAgentExit(context.Background(), ref)
	if err != nil || exited || result.Known {
		t.Fatalf("absent pod exited=%v result=%#v err=%v", exited, result, err)
	}
}
