package argoworkflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStageWritesOnlyCanonicalAGWPaths(t *testing.T) {
	input := fixture(t)
	store := &producerMemoryStore{objects: map[string][]byte{}, uri: "s3://agw-artifacts"}
	writer, err := artifactsWriter(store)
	if err != nil {
		t.Fatalf("artifacts writer: %v", err)
	}
	body, err := canonical.CanonicalizeResolvedSpec(input.Snapshot)
	if err != nil {
		t.Fatalf("canonical snapshot: %v", err)
	}
	if _, err := writer.SaveResolvedSpec(context.Background(), string(input.Run.UID), input.ResolvedRef.Digest, body); err != nil {
		t.Fatalf("save resolved snapshot: %v", err)
	}
	producer, err := NewProducer(nil, store, store, input.Config.WorkflowTemplateName, input.Config.WorkflowTemplateUID, input.Config.WorkflowTemplateDigest)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	request := requestFromInput(input)
	workspace := t.TempDir()
	if err := producer.Stage(context.Background(), request, workspace); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	for _, path := range []string{filepath.Join(workspace, ".agw", "resolved-spec.json"), filepath.Join(workspace, ".agw", "capture-spec.json")} {
		if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("staged contract %s is not a regular file: %v", path, statErr)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "resolved-spec.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolved spec escaped reserved .agw directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "capture-spec.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture spec escaped reserved .agw directory: %v", err)
	}
}

func TestCleanupWorkResourcesIsIdempotentAndLeavesEvidenceStorageAlone(t *testing.T) {
	request := cleanupRequest()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sandboxName, err := sandbox.ChildName(types.UID(request.RunUID), sandbox.RoleWork)
	if err != nil {
		t.Fatal(err)
	}
	controller := true
	blockOwnerDeletion := true
	owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version, Kind: "AgentRun", Name: request.RunName, UID: types.UID(request.RunUID), Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion}
	work := &v1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: request.Namespace, Name: sandboxName,
		UID:             types.UID("sandbox-uid"),
		Labels:          map[string]string{sandbox.RunUIDLabelKey: request.RunUID, sandbox.RoleLabelKey: string(sandbox.RoleWork), sandbox.SpecDigestLabelKey: cleanupDigestLabel(request.SpecDigest)},
		Annotations:     map[string]string{sandbox.SpecDigestAnnotationKey: request.SpecDigest, sandbox.SandboxSpecFingerprintAnnotationKey: "sha256:" + strings.Repeat("b", 64)},
		OwnerReferences: []metav1.OwnerReference{owner},
	}}
	immutable := true
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: request.Namespace, Name: workload.WorkSecretName(request.RunUID),
		UID:         types.UID("work-secret-uid"),
		Labels:      map[string]string{runSecretManagedByLabel: runSecretManagedByValue, runSecretPhaseLabel: "work", runSecretUIDLabel: request.RunUID},
		Annotations: map[string]string{runSecretDigestAnnotation: request.SpecDigest}, OwnerReferences: []metav1.OwnerReference{owner},
	}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"artifact-session-token": []byte("evidence-credential")}}
	// This PVC represents retained workspace evidence. Cleanup must not touch it.
	claimName, err := workload.WorkspaceClaimName(sandboxName)
	if err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: request.Namespace, Name: claimName, UID: types.UID("workspace-pvc-uid"),
		Labels:          map[string]string{sandbox.RunUIDLabelKey: request.RunUID, sandbox.RoleLabelKey: string(sandbox.RoleWork), "agents.x-k8s.io/sandbox-name-hash": "sandbox-name-hash"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1beta1.GroupVersion.String(), Kind: v1beta1.SandboxKind, Name: sandboxName, UID: work.UID, Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion}},
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(work, secret, pvc).Build()
	for attempt := 0; attempt < 2; attempt++ {
		if err := CleanupWorkResources(context.Background(), kube, request); err != nil {
			t.Fatalf("cleanup attempt %d: %v", attempt+1, err)
		}
	}
	liveWork := &v1beta1.Sandbox{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(work), liveWork); err != nil {
		t.Fatalf("work Sandbox was deleted and PVC ownership was lost: %v", err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err == nil {
		t.Fatal("work Secret remained after cleanup")
	}
	livePVC := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(pvc), livePVC); err != nil {
		t.Fatalf("retained workspace PVC was deleted: %v", err)
	}
	if len(livePVC.OwnerReferences) != 1 || livePVC.OwnerReferences[0].UID != work.UID {
		t.Fatalf("retained PVC owner reference=%#v, want Sandbox UID %q", livePVC.OwnerReferences, work.UID)
	}
}

func TestCleanupRefusesForeignSecretWithoutDeletingIt(t *testing.T) {
	request := cleanupRequest()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	controller := true
	blockOwnerDeletion := true
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: request.Namespace, Name: workload.WorkSecretName(request.RunUID), UID: types.UID("foreign-secret"),
		Labels:          map[string]string{runSecretManagedByLabel: runSecretManagedByValue, runSecretPhaseLabel: "work", runSecretUIDLabel: request.RunUID},
		Annotations:     map[string]string{runSecretDigestAnnotation: request.SpecDigest},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version, Kind: "AgentRun", Name: request.RunName, UID: types.UID("foreign"), Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion}},
	}, Type: corev1.SecretTypeOpaque, Immutable: boolPtr(true)}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	if err := CleanupWorkResources(context.Background(), kube, request); !errors.Is(err, ErrProducerConflict) {
		t.Fatalf("foreign cleanup error=%v, want ErrProducerConflict", err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.Secret{}); err != nil {
		t.Fatalf("foreign Secret was mutated/deleted: %v", err)
	}
}

func boolPtr(value bool) *bool { return &value }

type producerMemoryStore struct {
	objects map[string][]byte
	uri     string
}

func (s *producerMemoryStore) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	if _, ok := s.objects[key]; ok {
		return false, s.uri + "/" + key, nil
	} else {
		s.objects[key] = append([]byte(nil), body...)
		return true, s.uri + "/" + key, nil
	}
}

func (s *producerMemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, errors.New("object missing")
	}
	return append([]byte(nil), body...), nil
}

func (s *producerMemoryStore) URI(key string) (string, error) { return s.uri + "/" + key, nil }

func artifactsWriter(store *producerMemoryStore) (*artifacts.Writer, error) {
	return artifacts.NewWriter(store)
}

func requestFromInput(input Inputs) Request {
	return Request{Namespace: input.Run.Namespace, RunName: input.Run.Name, RunUID: string(input.Run.UID), RunGeneration: input.Run.Generation, SpecDigest: input.ResolvedRef.Digest, BaseSHA: input.Snapshot.BaseSHA, WorkflowTemplateName: input.Config.WorkflowTemplateName, WorkflowTemplateUID: input.Config.WorkflowTemplateUID, WorkflowTemplateDigest: input.Config.WorkflowTemplateDigest}
}

func cleanupRequest() Request {
	return Request{Namespace: "agw-runs", RunName: "run", RunUID: "run-uid", RunGeneration: 1, SpecDigest: "sha256:" + strings.Repeat("a", 64), BaseSHA: strings.Repeat("b", 40), WorkflowTemplateName: "agw-agent-run-lifecycle", WorkflowTemplateUID: "template-uid-1", WorkflowTemplateDigest: "sha256:" + strings.Repeat("d", 64)}
}
