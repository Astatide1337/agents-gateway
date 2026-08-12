// Package v1alpha1 contains the Agents Gateway v3 API.
//
// +kubebuilder:object:generate=true
// +groupName=agents.astatide.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "agents.astatide.com", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&AgentRun{}, &AgentRunList{},
		&Agent{}, &AgentList{},
		&Gate{}, &GateList{},
		&ToolSet{}, &ToolSetList{},
		&ModelRoute{}, &ModelRouteList{},
		&Policy{}, &PolicyList{},
		&ContextStrategy{}, &ContextStrategyList{},
	)
}

// Ensure the package keeps the runtime import in the generated API contract.
var _ runtime.Object = (*AgentRun)(nil)
