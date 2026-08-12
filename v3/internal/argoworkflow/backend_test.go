package argoworkflow

import (
	"context"
	"errors"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type assigningCreateClient struct {
	client.Client
	creates int
}

func (c *assigningCreateClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.creates++
	object.SetUID(types.UID("workflow-uid-created"))
	object.SetGeneration(1)
	return c.Client.Create(ctx, object, options...)
}

func TestBackendEnsureCreatesExactlyOnceAndBindsReadBackIdentity(t *testing.T) {
	input := fixture(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(input.Run, workflowTemplateFixture(input.Run.Namespace, input.Config.WorkflowTemplateName, input.Config.WorkflowTemplateUID)).Build()
	client := &assigningCreateClient{Client: baseClient}
	backend, err := NewBackend(client, input.Config)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	first, err := backend.Ensure(context.Background(), input.Run, input.Snapshot)
	if err != nil {
		t.Fatalf("Ensure(first): %v", err)
	}
	second, err := backend.Ensure(context.Background(), input.Run, input.Snapshot)
	if err != nil {
		t.Fatalf("Ensure(second): %v", err)
	}
	if client.creates != 1 {
		t.Fatalf("create calls=%d, want exactly one", client.creates)
	}
	if first != second || first.Reference.UID != types.UID("workflow-uid-created") || first.Reference.Generation != 1 {
		t.Fatalf("bindings differ or are not API-server bound: first=%#v second=%#v", first, second)
	}

	observed, err := backend.Observe(context.Background(), first)
	if err != nil {
		t.Fatalf("Observe(bound): %v", err)
	}
	if observed.Reference != first.Reference || observed.Phase != BackendPending {
		t.Fatalf("observation=%#v, want bound pending observation", observed)
	}
}

func TestBackendEnsureAlreadyExistsBindsOnlyTheMatchingWorkflow(t *testing.T) {
	input := fixture(t)
	translation, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	translation.Workflow.SetUID(types.UID("workflow-uid-existing"))
	translation.Workflow.SetGeneration(2)
	client := fake.NewClientBuilder().WithObjects(translation.Workflow, workflowTemplateFixture(input.Run.Namespace, input.Config.WorkflowTemplateName, input.Config.WorkflowTemplateUID)).Build()
	backend, err := NewBackend(client, input.Config)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	binding, err := backend.Ensure(context.Background(), input.Run, input.Snapshot)
	if err != nil {
		t.Fatalf("Ensure(existing): %v", err)
	}
	if binding.Reference.UID != types.UID("workflow-uid-existing") || binding.Reference.Generation != 2 {
		t.Fatalf("binding=%#v, want existing API identity", binding)
	}
}

func TestBackendRejectsForeignWorkflowAndNeverDeletesIt(t *testing.T) {
	input := fixture(t)
	translation, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	foreign := translation.Workflow.DeepCopy()
	foreign.SetUID(types.UID("workflow-uid-foreign"))
	foreign.SetGeneration(1)
	owners := foreign.GetOwnerReferences()
	owners[0].UID = types.UID("foreign-run")
	foreign.SetOwnerReferences(owners)
	kube := fake.NewClientBuilder().WithObjects(foreign, workflowTemplateFixture(input.Run.Namespace, input.Config.WorkflowTemplateName, input.Config.WorkflowTemplateUID)).Build()
	backend, err := NewBackend(kube, input.Config)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	if _, err := backend.Ensure(context.Background(), input.Run, input.Snapshot); !errors.Is(err, ErrBinding) {
		t.Fatalf("Ensure foreign error=%v, want ErrBinding", err)
	}
	binding := translation.Binding
	binding.Reference.UID = foreign.GetUID()
	binding.Reference.Generation = foreign.GetGeneration()
	if err := backend.Delete(context.Background(), binding); !errors.Is(err, ErrBinding) {
		t.Fatalf("Delete foreign error=%v, want ErrBinding", err)
	}
	remaining := &unstructured.Unstructured{}
	remaining.SetGroupVersionKind(translation.Workflow.GroupVersionKind())
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(foreign), remaining); err != nil {
		t.Fatalf("foreign Workflow was deleted or unreadable: %v", err)
	}
}

func TestBindDoesNotTrustWorkflowStatus(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-status"))
	workflow.SetGeneration(1)
	workflow.Object["status"] = map[string]any{
		"phase":   "Succeeded",
		"message": "status is not a Gate verdict",
		"secret":  "must never be copied",
	}
	status := lifecycleStatus(t, workflow, translation.Binding)
	status["secret"] = "must never be copied"
	workflow.Object["status"] = status
	binding, err := Bind(workflow, translation.Binding)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if binding.Reference.UID != workflow.GetUID() || binding.Reference.Generation != workflow.GetGeneration() {
		t.Fatalf("binding=%#v, want API identity", binding)
	}
	if _, err := Observe(workflow, binding); err != nil {
		t.Fatalf("Observe after persisted bind: %v", err)
	}
}
