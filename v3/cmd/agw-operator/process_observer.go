package main

import (
	"context"
	"errors"

	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

var (
	errProcessObserverConfig   = errors.New("process observer is not configured")
	errProcessObserverIdentity = errors.New("work pod identity could not be proven")
)

// podProcessObserver obtains kubelet-reported container termination from the
// Kubernetes API, never from the runtime container. This is independent exit
// evidence, not a live PID observer and not a phase authority. It performs one
// bounded label-selected Pod list and accepts exactly one Pod owned by the
// expected execution child UID (Sandbox or Job).
type podProcessObserver struct{ client kubernetes.Interface }

func (o podProcessObserver) ObserveAgentExit(ctx context.Context, ref sandbox.SandboxRef) (bool, runtimeevents.ProcessExit, error) {
	if o.client == nil || ctx == nil || ref.Namespace == "" || ref.Name == "" || ref.Kind == "" || ref.UID == "" || ref.OwnerUID == "" || ref.Role != sandbox.RoleWork {
		return false, runtimeevents.ProcessExit{}, errProcessObserverConfig
	}
	selector, ok := childPodSelector(ref)
	if !ok {
		return false, runtimeevents.ProcessExit{}, errProcessObserverConfig
	}
	pods, err := o.client.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 2})
	if err != nil {
		return false, runtimeevents.ProcessExit{}, err
	}
	if len(pods.Items) == 0 {
		return false, runtimeevents.ProcessExit{}, nil
	}
	if len(pods.Items) != 1 {
		return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
	}
	pod := &pods.Items[0]
	if pod.Namespace != ref.Namespace || pod.UID == "" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever || pod.Labels[sandbox.RunUIDLabelKey] != string(ref.OwnerUID) || pod.Labels[sandbox.RoleLabelKey] != string(sandbox.RoleWork) || !validChildPodLabels(pod, ref) || !ownedByChild(pod.OwnerReferences, ref) {
		return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
	}
	foundSpec := false
	for _, container := range pod.Spec.Containers {
		if container.Name == "agent" {
			if foundSpec {
				return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
			}
			foundSpec = true
		}
	}
	if !foundSpec {
		return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
	}
	foundStatus := false
	var agentStatus *corev1.ContainerStatus
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "agent" {
			continue
		}
		if foundStatus {
			return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
		}
		foundStatus = true
		statusCopy := status
		agentStatus = &statusCopy
	}
	if agentStatus == nil {
		return false, runtimeevents.ProcessExit{}, nil
	}
	if agentStatus.State.Terminated == nil {
		if agentStatus.State.Running != nil && agentStatus.State.Waiting != nil {
			return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
		}
		return false, runtimeevents.ProcessExit{}, nil
	}
	if agentStatus.State.Running != nil || agentStatus.State.Waiting != nil || agentStatus.ContainerID == "" {
		return false, runtimeevents.ProcessExit{}, errProcessObserverIdentity
	}
	return true, runtimeevents.ProcessExit{Known: true, Code: int(agentStatus.State.Terminated.ExitCode)}, nil
}

func childPodSelector(ref sandbox.SandboxRef) (string, bool) {
	selector := labels.Set{
		sandbox.RunUIDLabelKey: string(ref.OwnerUID),
		sandbox.RoleLabelKey:   string(ref.Role),
	}
	switch ref.Kind {
	case sandbox.ChildKindSandbox:
	case sandbox.ChildKindJob:
		selector[sandbox.ChildNameLabelKey] = ref.Name
		selector["batch.kubernetes.io/controller-uid"] = string(ref.UID)
	default:
		return "", false
	}
	return selector.AsSelector().String(), true
}

func validChildPodLabels(pod *corev1.Pod, ref sandbox.SandboxRef) bool {
	if ref.Kind != sandbox.ChildKindJob {
		return true
	}
	return pod.Labels[sandbox.ChildNameLabelKey] == ref.Name && pod.Labels["batch.kubernetes.io/controller-uid"] == string(ref.UID) && pod.Labels["batch.kubernetes.io/job-name"] == ref.Name
}

func ownedByChild(owners []metav1.OwnerReference, ref sandbox.SandboxRef) bool {
	var wantAPIVersion, wantKind string
	switch ref.Kind {
	case sandbox.ChildKindSandbox:
		wantAPIVersion, wantKind = "agents.x-k8s.io/v1beta1", sandbox.ChildKindSandbox
	case sandbox.ChildKindJob:
		wantAPIVersion, wantKind = "batch/v1", sandbox.ChildKindJob
	default:
		return false
	}
	found := false
	for _, owner := range owners {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		if found || owner.APIVersion != wantAPIVersion || owner.Kind != wantKind || owner.Name != ref.Name || owner.UID != ref.UID {
			return false
		}
		found = true
	}
	return found
}
