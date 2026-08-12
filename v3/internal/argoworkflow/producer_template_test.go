package argoworkflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleWorkflowTemplateIsRealBoundedProducer(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "agw-operator", "templates", "argo-workflow-template.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lifecycle WorkflowTemplate: %v", err)
	}
	template := string(body)
	for _, required := range []string{
		`{{- if eq .Values.orchestrationBackend "argo" }}`,
		"kind: WorkflowTemplate",
		"argo-bounded-handoff-v1",
		"kind: ServiceAccount",
		"kind: RoleBinding",
		"name: prepare",
		"name: stage",
		"name: wait-ready",
		"name: wait-finished",
		"name: capture",
		"name: handoff",
		`args: ["prepare"]`,
		`args: ["stage"]`,
		`- wait`,
		`args: ["handoff"]`,
		`args: ["cleanup"]`,
		"$lifecycleImage",
		"$captureImage",
		"agents.x-k8s.io",
		"artifact-access-key-id",
		"artifact-secret-access-key",
		"artifact-session-token",
		"name: agw-lifecycle-output",
		"/tmp/agw-lifecycle-output.json",
		"/tmp/agw-run-timeout",
		"/workspace/.agw/ready.json",
		"/workspace/.agw/finished.json",
		"podGC:",
		"onExit: cleanup",
		"cleanupServiceAccountName",
		"retryPolicy: Always",
		"agents.astatide.com/lifecycle-role: prepare",
		"agents.astatide.com/lifecycle-role: stage",
		"agents.astatide.com/lifecycle-role: wait",
		"agents.astatide.com/lifecycle-role: capture",
		"agents.astatide.com/lifecycle-role: handoff",
		"agents.astatide.com/lifecycle-role: cleanup",
		"lifecycle-api-egress",
		"lifecycle-public-egress",
		"lifecycle-capture-deny-egress",
		"preflight.apiServerCIDR",
		"argo.apiServerPort",
	} {
		if !strings.Contains(template, required) {
			t.Fatalf("lifecycle template is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"fail-closed-scaffold",
		"fail-closed-producer",
		`args: ["produce"]`,
		"agw-phase0-sandbox-lifecycle",
		"fixture",
		"successCondition",
		"failureCondition",
		"Gate verdict",
		"Publication",
		"effect-ledger",
		"terminal-success",
	} {
		if strings.Contains(template, forbidden) {
			t.Fatalf("lifecycle template contains forbidden scaffold/state-machine content %q", forbidden)
		}
	}
	if got := strings.Count(template, "name: agw-lifecycle-output"); got != 2 {
		t.Fatalf("lifecycle output declaration count=%d, want producer plus entrypoint forwarding declaration", got)
	}
	if strings.Count(template, "image: {{ $lifecycleImage | quote }}") != 5 {
		t.Fatalf("lifecycle image use count=%d, want prepare/stage/wait/handoff/cleanup", strings.Count(template, "image: {{ $lifecycleImage | quote }}"))
	}
	if strings.Count(template, "readOnlyRootFilesystem: true") < 6 {
		t.Fatal("lifecycle helper pods do not all declare read-only roots")
	}
}

func TestChartKeepsDirectOrchestrationAsDefault(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "agw-operator", "values.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	values := string(body)
	if !strings.Contains(values, "orchestrationBackend: direct") {
		t.Fatal("chart no longer defaults to the direct orchestration backend")
	}
	if !strings.Contains(values, "serviceAccountName: agw-argo-lifecycle") {
		t.Fatal("Argo lifecycle ServiceAccount name is not configured")
	}
}

func TestLifecycleTemplateDoesNotUseArgoSimpleSandboxConditions(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "agw-operator", "templates", "argo-workflow-template.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lifecycle template: %v", err)
	}
	if strings.Contains(string(body), "successCondition") || strings.Contains(string(body), "failureCondition") {
		t.Fatal("condition-array wait bridge was replaced by unsupported Argo resource conditions")
	}
	if !strings.Contains(string(body), "--condition") || !strings.Contains(string(body), "--require-marker") {
		t.Fatal("lifecycle template does not invoke the condition-array wait bridge")
	}
	template := string(body)
	if strings.Contains(template, "--timeout\n          - 45m") || !strings.Contains(template, "steps.prepare.outputs.parameters.run-timeout") {
		t.Fatal("wait timeout is not resolved from the admitted AgentRun")
	}
	if !strings.Contains(template, "podSpecPatch: |\n        automountServiceAccountToken: false\n        hostUsers: false\n        shareProcessNamespace: false\n        enableServiceLinks: false") {
		t.Fatal("stage/capture tokenless pod patch is missing")
	}
	for _, name := range []string{"prepare", "stage", "wait", "capture", "handoff", "cleanup"} {
		start := strings.Index(template, "    - name: "+name)
		if start < 0 {
			t.Fatalf("%s template is missing", name)
		}
		end := strings.Index(template[start+len("    - name: "+name):], "\n    - name: ")
		if end >= 0 {
			end += start + len("    - name: "+name)
		} else {
			end = len(template)
		}
		section := template[start:end]
		for _, field := range []string{"hostUsers: false", "shareProcessNamespace: false", "enableServiceLinks: false", "type: RuntimeDefault"} {
			if !strings.Contains(section, field) {
				t.Fatalf("%s template is missing hardened pod field %q", name, field)
			}
		}
	}
	handoffStart := strings.Index(template, "    - name: handoff")
	cleanupStart := strings.Index(template, "    - name: cleanup")
	if handoffStart < 0 || cleanupStart < handoffStart {
		t.Fatal("cleanup template is missing after handoff")
	}
	handoff := template[handoffStart:cleanupStart]
	if !strings.Contains(handoff, "automountServiceAccountToken: true") {
		t.Fatal("handoff does not explicitly request the lifecycle ServiceAccount token")
	}
	if strings.Contains(handoff, "automountServiceAccountToken: false") {
		t.Fatal("handoff lost the lifecycle ServiceAccount token required for API identity binding")
	}
	stageStart := strings.Index(template, "    - name: stage")
	waitStart := strings.Index(template, "    - name: wait")
	if stageStart < 0 || waitStart < stageStart {
		t.Fatal("stage/wait template boundaries are missing")
	}
	stage := template[stageStart:waitStart]
	if !strings.Contains(stage, "automountServiceAccountToken: false") {
		t.Fatal("stage no longer disables ambient ServiceAccount credentials")
	}
	if strings.Contains(stage, "lifecycleSTSAndImagesEnv") || strings.Contains(handoff, "lifecycleSTSAndImagesEnv") {
		t.Fatal("stage/handoff still receive prepare-only STS or helper-image configuration")
	}
	captureStart := strings.Index(template, "    - name: capture")
	if captureStart < 0 || captureStart >= handoffStart {
		t.Fatal("capture/handoff template boundaries are missing")
	}
	capture := template[captureStart:handoffStart]
	if !strings.Contains(capture, "automountServiceAccountToken: false") {
		t.Fatal("capture no longer disables ambient ServiceAccount credentials")
	}
	if !strings.Contains(template, "egress: []") {
		t.Fatal("capture does not have an explicit empty egress policy")
	}
}
