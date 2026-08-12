package main

import (
	"context"
	"errors"
	"io"

	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const verifyContainerName = "verify"

var (
	errVerifyEvidenceConfig   = errors.New("verify evidence reader is not configured")
	errVerifyEvidenceIdentity = errors.New("verify pod identity could not be proven")
)

// podVerifyEvidenceReader reads one bounded stdout frame only after the
// verification driver has independently observed the execution child as
// Finished. It accepts exactly one Pod owned by the expected child UID, so labels
// alone cannot redirect Gate evidence to an attacker-created Pod.
type podVerifyEvidenceReader struct{ client kubernetes.Interface }

func (r podVerifyEvidenceReader) ReadFrame(ctx context.Context, ref sandbox.SandboxRef, maxBytes int64) ([]byte, error) {
	if r.client == nil || ctx == nil || maxBytes <= 0 || ref.Namespace == "" || ref.Name == "" || ref.Kind == "" || ref.UID == "" || ref.OwnerUID == "" || ref.Role != sandbox.RoleVerify {
		return nil, errVerifyEvidenceConfig
	}
	selector, ok := childPodSelector(ref)
	if !ok {
		return nil, errVerifyEvidenceConfig
	}
	pods, err := r.client.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 2})
	if err != nil {
		return nil, verifycontroller.ErrEvidenceUnavailable
	}
	if len(pods.Items) == 0 {
		return nil, verifycontroller.ErrEvidenceMissing
	}
	if len(pods.Items) != 1 {
		return nil, errVerifyEvidenceIdentity
	}
	pod := &pods.Items[0]
	if pod.Namespace != ref.Namespace || pod.Labels[sandbox.RunUIDLabelKey] != string(ref.OwnerUID) || pod.Labels[sandbox.RoleLabelKey] != string(sandbox.RoleVerify) || !validChildPodLabels(pod, ref) || !ownedByChild(pod.OwnerReferences, ref) || !hasExactlyOneContainer(pod.Spec.Containers, verifyContainerName) {
		return nil, errVerifyEvidenceIdentity
	}
	stream, err := r.client.CoreV1().Pods(ref.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: verifyContainerName, Follow: false, Timestamps: false,
	}).Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, verifycontroller.ErrEvidenceMissing
		}
		return nil, verifycontroller.ErrEvidenceUnavailable
	}
	defer stream.Close()
	body, err := io.ReadAll(io.LimitReader(stream, maxBytes+1))
	if err != nil {
		return nil, verifycontroller.ErrEvidenceUnavailable
	}
	if len(body) == 0 {
		return nil, verifycontroller.ErrEvidenceMissing
	}
	if int64(len(body)) > maxBytes {
		return nil, verifycontroller.ErrEvidenceMalformed
	}
	return body, nil
}

func hasExactlyOneContainer(containers []corev1.Container, name string) bool {
	if len(containers) != 1 {
		return false
	}
	return containers[0].Name == name
}
