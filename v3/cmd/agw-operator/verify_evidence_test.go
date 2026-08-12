package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestVerifyEvidenceReaderBindsPodAndBoundsLogs(t *testing.T) {
	controller := true
	ref := sandbox.SandboxRef{Namespace: "agw-runs", Name: "verify-sandbox", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: types.UID("run-uid"), Role: sandbox.RoleVerify}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "verify-pod", Namespace: ref.Namespace,
		Labels:          map[string]string{sandbox.RunUIDLabelKey: "run-uid", sandbox.RoleLabelKey: "verify"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: ref.Name, UID: ref.UID, Controller: &controller}},
	}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyContainerName}}}}
	client := fake.NewSimpleClientset(pod)

	// client-go's fake log request returns its generated body. The identity
	// assertions above are the security property; DecodeEvidenceFrame owns the
	// exact framing contract.
	reader := podVerifyEvidenceReader{client: client}
	_, err := reader.ReadFrame(context.Background(), ref, 1024)
	if err != nil && !errors.Is(err, verifycontroller.ErrEvidenceMissing) && !errors.Is(err, verifycontroller.ErrEvidenceUnavailable) {
		t.Fatalf("unexpected bounded log result: %v", err)
	}
}

func TestVerifyEvidenceReaderRejectsForeignOrAmbiguousPods(t *testing.T) {
	controller := true
	ref := sandbox.SandboxRef{Namespace: "agw-runs", Name: "verify-sandbox", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: types.UID("run-uid"), Role: sandbox.RoleVerify}
	makePod := func(name, owner string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ref.Namespace,
			Labels:          map[string]string{sandbox.RunUIDLabelKey: "run-uid", sandbox.RoleLabelKey: "verify"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: owner, UID: ref.UID, Controller: &controller}},
		}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyContainerName}}}}
	}
	for _, test := range []struct {
		name string
		pods []runtime.Object
	}{
		{name: "foreign owner", pods: []runtime.Object{makePod("one", "other")}},
		{name: "ambiguous", pods: []runtime.Object{makePod("one", ref.Name), makePod("two", ref.Name)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (podVerifyEvidenceReader{client: fake.NewSimpleClientset(test.pods...)}).ReadFrame(context.Background(), ref, 1024)
			if !errors.Is(err, errVerifyEvidenceIdentity) {
				t.Fatalf("err=%v, want identity rejection", err)
			}
		})
	}
	bad := ref
	bad.Role = sandbox.RoleWork
	_, err := (podVerifyEvidenceReader{client: fake.NewSimpleClientset()}).ReadFrame(context.Background(), bad, int64(len(strings.Repeat("x", 8))))
	if !errors.Is(err, errVerifyEvidenceConfig) {
		t.Fatalf("err=%v, want configuration rejection", err)
	}
}
