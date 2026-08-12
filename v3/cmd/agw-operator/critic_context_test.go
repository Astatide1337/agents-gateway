package main

import (
	"context"
	"errors"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveCriticContextRefBindsExactAgentRunStatus(t *testing.T) {
	patch := v1alpha1.ArtifactRef{URI: "s3://artifacts/runs/run-uid/patch.diff", Digest: testDigest("1"), Kind: "patch", Name: "patch.diff", MediaType: "text/x-diff", SizeBytes: 10}
	contextRef := v1alpha1.ArtifactRef{URI: "s3://artifacts/runs/run-uid/context/pack.json", Digest: testDigest("2"), Kind: "context-pack", Name: "context-pack.json", MediaType: "application/json", SizeBytes: 20}
	run := &v1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agw-runs", Name: "run", UID: types.UID("run-uid"), Generation: 3},
		Status: v1alpha1.AgentRunStatus{
			Phase: v1alpha1.PhaseVerifying, ObservedGeneration: 3, SpecDigest: testDigest("3"),
			BaseSHA: "0123456789012345678901234567890123456789", ContextPackRef: &contextRef,
			Patch: &v1alpha1.PatchSummary{Ref: &patch},
		},
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	input := verifycontroller.CriticRunInput{
		Snapshot:   resolved.Snapshot{Run: resolved.RunIdentity{Namespace: "agw-runs", Name: "run", UID: "run-uid", Generation: 3}, BaseSHA: run.Status.BaseSHA},
		SpecDigest: run.Status.SpecDigest, BaseSHA: run.Status.BaseSHA, Patch: patch,
	}
	got, err := resolveCriticContextRef(context.Background(), reader, input)
	if err != nil || got != contextRef {
		t.Fatalf("context ref=%#v err=%v", got, err)
	}

	input.Patch.Digest = testDigest("4")
	if _, err := resolveCriticContextRef(context.Background(), reader, input); !errors.Is(err, errCriticContextIdentity) {
		t.Fatalf("mismatched patch error=%v", err)
	}
}

func testDigest(character string) string {
	value := ""
	for len(value) < 64 {
		value += character
	}
	return "sha256:" + value[:64]
}
