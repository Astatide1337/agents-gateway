package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestSchemeRegistersExactlySevenNamespacedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	want := []runtime.Object{
		&AgentRun{}, &Agent{}, &Gate{}, &ToolSet{}, &ModelRoute{}, &Policy{}, &ContextStrategy{},
	}
	for _, object := range want {
		gvks, unversioned, err := scheme.ObjectKinds(object)
		if err != nil {
			t.Errorf("%T lookup: %v", object, err)
			continue
		}
		if unversioned {
			t.Errorf("%T unexpectedly registered as unversioned", object)
		}
		if len(gvks) != 1 || gvks[0].GroupVersion() != GroupVersion {
			t.Errorf("%T registered as %v, want %s", object, gvks, GroupVersion.String())
		}
	}
}

func TestAgentRunDeepCopyDoesNotShareBoundedState(t *testing.T) {
	task := "repair the issue"
	run := &AgentRun{}
	run.Spec.Task.Inline = &task
	run.Spec.Scope.Paths = []string{"src/**"}
	run.Status.Conditions = []Condition{{Type: string(ConditionAdmitted), Message: "ok"}}

	copy := run.DeepCopy()
	copy.Spec.Task.Inline = stringPtr("changed")
	copy.Spec.Scope.Paths[0] = "tests/**"
	copy.Status.Conditions[0].Message = "changed"

	if *run.Spec.Task.Inline != "repair the issue" {
		t.Fatal("task pointer was shared")
	}
	if run.Spec.Scope.Paths[0] != "src/**" {
		t.Fatal("scope slice was shared")
	}
	if run.Status.Conditions[0].Message != "ok" {
		t.Fatal("conditions slice was shared")
	}
}

func stringPtr(value string) *string { return &value }
