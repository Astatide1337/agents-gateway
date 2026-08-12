package config_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const agwGroup = "agents.astatide.com"

var credentialLiteralPattern = regexp.MustCompile(`(?i)(?:sk-[a-z0-9]{16,}|gh[pousr]_[a-z0-9_]{16,}|github_pat_[a-z0-9_]{16,}|glsa_[a-z0-9_]{16,}|xox[baprs]-[a-z0-9-]{16,}|-----begin [a-z ]+private key-----|bearer\s+[a-z0-9._~+/=-]{12,})`)
var credentialFieldPattern = regexp.MustCompile(`(?i)(?:api[-_]?key|password|passwd|secret|token|private[-_]?key|credential)`)

type manifestDocument struct {
	path  string
	index int
	data  map[string]interface{}
}

func TestManifestDocumentsParse(t *testing.T) {
	documents := loadManifestDocuments(t)
	if len(documents) == 0 {
		t.Fatal("no v3 manifest documents were found")
	}
	for _, document := range documents {
		document := document
		t.Run(fmt.Sprintf("parse/%s#%d", document.path, document.index), func(t *testing.T) {
			if strings.TrimSpace(stringValue(document.data["apiVersion"])) == "" {
				t.Fatalf("%s document %d has no apiVersion", document.path, document.index)
			}
			if strings.TrimSpace(stringValue(document.data["kind"])) == "" {
				t.Fatalf("%s document %d has no kind", document.path, document.index)
			}
		})
	}
}

func TestAGWCRDsAreNamespacedStructuralAndStatusEnabled(t *testing.T) {
	expected := map[string]string{
		"agents.agents.astatide.com":            "Agent",
		"agentruns.agents.astatide.com":         "AgentRun",
		"contextstrategies.agents.astatide.com": "ContextStrategy",
		"gates.agents.astatide.com":             "Gate",
		"modelroutes.agents.astatide.com":       "ModelRoute",
		"policies.agents.astatide.com":          "Policy",
		"toolsets.agents.astatide.com":          "ToolSet",
	}
	actual := make(map[string]apiextensionsv1.CustomResourceDefinition)
	for _, document := range loadManifestDocuments(t) {
		if stringValue(document.data["kind"]) != "CustomResourceDefinition" {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		decodeTyped(t, document, &crd)
		if crd.Spec.Group != agwGroup {
			continue
		}
		if _, duplicate := actual[crd.Name]; duplicate {
			t.Fatalf("duplicate AGW CRD %q in %s document %d", crd.Name, document.path, document.index)
		}
		actual[crd.Name] = crd
	}

	if len(actual) != len(expected) {
		t.Fatalf("expected exactly %d AGW CRDs, found %d (%v)", len(expected), len(actual), sortedKeys(actual))
	}
	for name, kind := range expected {
		crd, ok := actual[name]
		if !ok {
			t.Errorf("missing AGW CRD %q", name)
			continue
		}
		if crd.Spec.Names.Kind != kind {
			t.Errorf("CRD %q has kind %q, want %q", name, crd.Spec.Names.Kind, kind)
		}
		if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
			t.Errorf("CRD %q has scope %q, want Namespaced", name, crd.Spec.Scope)
		}
		if len(crd.Spec.Versions) == 0 {
			t.Errorf("CRD %q has no versions", name)
		}
		for _, version := range crd.Spec.Versions {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				t.Errorf("CRD %q version %q has no OpenAPI schema", name, version.Name)
				continue
			}
			internalSchema := &apiextensions.JSONSchemaProps{}
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internalSchema, nil); err != nil {
				t.Errorf("CRD %q version %q schema conversion failed: %v", name, version.Name, err)
				continue
			}
			if _, err := structuralschema.NewStructural(internalSchema); err != nil {
				t.Errorf("CRD %q version %q is not structural: %v", name, version.Name, err)
			}
			if version.Subresources == nil || version.Subresources.Status == nil {
				t.Errorf("CRD %q version %q does not expose the status subresource", name, version.Name)
			}
		}
	}
}

func TestWebhookOnlyTargetsAgentRunCreateAndUpdate(t *testing.T) {
	var configurations []admissionregistrationv1.ValidatingWebhookConfiguration
	for _, document := range loadManifestDocuments(t) {
		if stringValue(document.data["kind"]) != "ValidatingWebhookConfiguration" {
			continue
		}
		var configuration admissionregistrationv1.ValidatingWebhookConfiguration
		decodeTyped(t, document, &configuration)
		configurations = append(configurations, configuration)
	}
	if len(configurations) != 1 {
		t.Fatalf("expected exactly one ValidatingWebhookConfiguration, found %d", len(configurations))
	}
	webhooks := configurations[0].Webhooks
	if len(webhooks) != 1 {
		t.Fatalf("expected exactly one validating webhook, found %d", len(webhooks))
	}
	webhook := webhooks[0]
	if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("webhook failurePolicy must be Fail, got %v", webhook.FailurePolicy)
	}
	if len(webhook.ClientConfig.CABundle) == 0 {
		t.Fatal("webhook caBundle must be non-empty; deployment overlays replace the explicit sentinel")
	}
	const caBundleSentinel = "AGW_WEBHOOK_CA_BUNDLE_REQUIRED"
	if string(webhook.ClientConfig.CABundle) != caBundleSentinel {
		t.Fatalf("base webhook caBundle must be the explicit injection sentinel %q, got %q", caBundleSentinel, string(webhook.ClientConfig.CABundle))
	}
	annotations := configurations[0].Annotations
	if annotations["agents.astatide.com/ca-bundle-required"] != "true" {
		t.Fatalf("webhook must declare that CA injection is required, annotations=%v", annotations)
	}
	if annotations["agents.astatide.com/ca-bundle-source"] != "kustomize-replacement:agw-webhook-ca.data.caBundle" {
		t.Fatalf("webhook must declare its CA injection source, annotations=%v", annotations)
	}
	for _, rule := range webhooks[0].Rules {
		if len(rule.Operations) != 2 || !hasOperation(rule.Operations, admissionregistrationv1.Create) || !hasOperation(rule.Operations, admissionregistrationv1.Update) {
			t.Fatalf("webhook operations must be exactly CREATE and UPDATE, got %v", rule.Operations)
		}
		if rule.APIGroups == nil || len(rule.APIGroups) != 1 || rule.APIGroups[0] != agwGroup {
			t.Fatalf("webhook API groups must be exactly %q, got %v", agwGroup, rule.APIGroups)
		}
		if len(rule.APIVersions) != 1 || rule.APIVersions[0] != "v1alpha1" {
			t.Fatalf("webhook API versions must be exactly v1alpha1, got %v", rule.APIVersions)
		}
		if len(rule.Resources) != 1 || rule.Resources[0] != "agentruns" {
			t.Fatalf("webhook resources must be exactly agentruns, got %v", rule.Resources)
		}
		if rule.Scope == nil || *rule.Scope != admissionregistrationv1.NamespacedScope {
			t.Fatalf("webhook scope must be Namespaced, got %v", rule.Scope)
		}
	}
}

func TestOperatorRBACIsNamespacedAndLeastPrivilege(t *testing.T) {
	documents := loadManifestDocuments(t)
	roles := make(map[string]rbacv1.Role)
	var bindings []rbacv1.RoleBinding
	var serviceAccounts []corev1.ServiceAccount
	for _, document := range documents {
		switch stringValue(document.data["kind"]) {
		case "Role":
			var role rbacv1.Role
			decodeTyped(t, document, &role)
			key := role.Namespace + "/" + role.Name
			if _, exists := roles[key]; exists {
				t.Fatalf("duplicate Role %q", key)
			}
			roles[key] = role
		case "RoleBinding":
			var binding rbacv1.RoleBinding
			decodeTyped(t, document, &binding)
			bindings = append(bindings, binding)
		case "ClusterRole", "ClusterRoleBinding":
			t.Fatalf("v3 operator must not ship broad %s RBAC", stringValue(document.data["kind"]))
		case "ServiceAccount":
			var account corev1.ServiceAccount
			decodeTyped(t, document, &account)
			serviceAccounts = append(serviceAccounts, account)
		}
	}

	if len(serviceAccounts) != 1 || serviceAccounts[0].Namespace != "agw-system" || serviceAccounts[0].Name != "agw-operator" {
		t.Fatalf("expected only agw-system/agw-operator ServiceAccount, got %#v", serviceAccounts)
	}

	expectedSystem := []rbacv1.PolicyRule{
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"agw-preflight"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
	}
	expectedRuns := []rbacv1.PolicyRule{
		{APIGroups: []string{"agents.astatide.com"}, Resources: []string{"agents", "agentruns", "gates", "toolsets", "modelroutes", "policies", "contextstrategies"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"agents.astatide.com"}, Resources: []string{"agentruns/status"}, Verbs: []string{"get", "update", "patch"}},
		{APIGroups: []string{"agents.astatide.com"}, Resources: []string{"agentruns/finalizers"}, Verbs: []string{"update"}},
		{APIGroups: []string{"agents.x-k8s.io"}, Resources: []string{"sandboxes"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"get", "list", "watch", "delete"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
	}
	assertRoleRules(t, roles, "agw-system/agw-operator-system", expectedSystem)
	assertRoleRules(t, roles, "agw-runs/agw-operator-runs", expectedRuns)
	if len(roles) != 2 {
		t.Fatalf("expected exactly two namespace-scoped operator Roles, got %v", sortedKeys(roles))
	}

	if len(bindings) != 2 {
		t.Fatalf("expected exactly two operator RoleBindings, got %d", len(bindings))
	}
	seenBindings := make(map[string]bool)
	for _, binding := range bindings {
		key := binding.Namespace + "/" + binding.Name
		seenBindings[key] = true
		if binding.RoleRef.Kind != "Role" || binding.RoleRef.APIGroup != rbacv1.GroupName {
			t.Errorf("RoleBinding %s must reference a namespaced Role, got %#v", key, binding.RoleRef)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Name != "agw-operator" || binding.Subjects[0].Namespace != "agw-system" {
			t.Errorf("RoleBinding %s has unexpected subject %#v", key, binding.Subjects)
		}
	}
	for _, want := range []string{"agw-system/agw-operator-system", "agw-runs/agw-operator-runs"} {
		if !seenBindings[want] {
			t.Errorf("missing RoleBinding %s", want)
		}
	}

	for roleKey, role := range roles {
		for _, rule := range role.Rules {
			if contains(rule.Resources, "secrets") && contains(rule.Verbs, "watch") {
				t.Errorf("Role %s grants Secret watch; retention only needs bounded list: resources=%v verbs=%v", roleKey, rule.Resources, rule.Verbs)
			}
			if contains(rule.Resources, "secrets") && contains(rule.Verbs, "*") {
				t.Errorf("Role %s grants wildcard Secret access", roleKey)
			}
		}
	}
}

func TestSamplesContainNoSecretsOrCredentialLiterals(t *testing.T) {
	for _, document := range loadManifestDocuments(t) {
		if !isUnder(document.path, filepath.Join("config", "samples")) {
			continue
		}
		if stringValue(document.data["kind"]) == "Secret" {
			t.Fatalf("sample %s document %d commits a Secret resource", document.path, document.index)
		}
		assertNoCredentialLiterals(t, document.data, document.path, document.index, "")
	}
}

func TestRunsNetworkPoliciesKeepTheExecutionNamespaceFailClosed(t *testing.T) {
	policies := map[string]networkingv1.NetworkPolicy{}
	for _, document := range loadManifestDocuments(t) {
		if stringValue(document.data["kind"]) != "NetworkPolicy" {
			continue
		}
		var policy networkingv1.NetworkPolicy
		decodeTyped(t, document, &policy)
		if policy.Namespace == "agw-runs" {
			policies[policy.Name] = policy
		}
	}
	if len(policies) != 4 {
		t.Fatalf("expected exactly four agw-runs NetworkPolicies, found %d", len(policies))
	}
	deny, ok := policies["agw-runs-default-deny"]
	if !ok || len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 || len(deny.Spec.PolicyTypes) != 2 {
		t.Fatalf("default deny policy is incomplete: %#v", deny.Spec)
	}
	public, ok := policies["agw-runs-public-egress"]
	if !ok || public.Spec.PodSelector.MatchLabels["agents.astatide.com/egress"] != "broker-public" {
		t.Fatalf("public egress is not explicitly selected: %#v", public.Spec.PodSelector)
	}
	verifyFetch, ok := policies["agw-runs-verify-fetch-egress"]
	if !ok || verifyFetch.Spec.PodSelector.MatchLabels["agents.astatide.com/egress"] != "verify-fetch-public" {
		t.Fatalf("verify fetch egress is not explicitly selected: %#v", verifyFetch.Spec.PodSelector)
	}
	dns, ok := policies["agw-runs-dns"]
	if !ok || !selectorExcludesCapture(dns.Spec.PodSelector) {
		t.Fatalf("DNS policy must exclude direct and Argo capture pods: %#v", dns.Spec.PodSelector)
	}
	for name, selected := range map[string]networkingv1.NetworkPolicy{"broker": public, "verify-fetch": verifyFetch} {
		wantExcept := map[string]bool{"10.0.0.0/8": false, "100.64.0.0/10": false, "169.254.0.0/16": false, "172.16.0.0/12": false, "192.168.0.0/16": false, "fc00::/7": false, "fe80::/10": false}
		for _, rule := range selected.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock == nil {
					continue
				}
				for _, excluded := range peer.IPBlock.Except {
					if _, tracked := wantExcept[excluded]; tracked {
						wantExcept[excluded] = true
					}
				}
			}
		}
		for cidr, found := range wantExcept {
			if !found {
				t.Errorf("%s public egress policy does not exclude %s", name, cidr)
			}
		}
	}
}

func selectorExcludesCapture(selector metav1.LabelSelector) bool {
	want := map[string]bool{
		"agents.astatide.com/role":           false,
		"agents.astatide.com/lifecycle-role": false,
	}
	for _, expression := range selector.MatchExpressions {
		if expression.Operator != metav1.LabelSelectorOpNotIn || len(expression.Values) != 1 || expression.Values[0] != "capture" {
			continue
		}
		if _, ok := want[expression.Key]; ok {
			want[expression.Key] = true
		}
	}
	return want["agents.astatide.com/role"] && want["agents.astatide.com/lifecycle-role"]
}

func assertRoleRules(t *testing.T, roles map[string]rbacv1.Role, key string, expected []rbacv1.PolicyRule) {
	t.Helper()
	role, ok := roles[key]
	if !ok {
		t.Fatalf("missing Role %s", key)
	}
	want := make(map[string]struct{}, len(expected))
	for _, rule := range expected {
		want[policyRuleKey(rule)] = struct{}{}
	}
	got := make(map[string]struct{}, len(role.Rules))
	for _, rule := range role.Rules {
		got[policyRuleKey(rule)] = struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("Role %s has %d unique rules, want %d: got=%v want=%v", key, len(got), len(want), got, want)
	}
	for rule := range want {
		if _, ok := got[rule]; !ok {
			t.Errorf("Role %s is missing rule %s", key, rule)
		}
	}
	for rule := range got {
		if _, ok := want[rule]; !ok {
			t.Errorf("Role %s has unexpected rule %s", key, rule)
		}
	}
}

func policyRuleKey(rule rbacv1.PolicyRule) string {
	groups := append([]string(nil), rule.APIGroups...)
	resources := append([]string(nil), rule.Resources...)
	resourceNames := append([]string(nil), rule.ResourceNames...)
	verbs := append([]string(nil), rule.Verbs...)
	sort.Strings(groups)
	sort.Strings(resources)
	sort.Strings(resourceNames)
	sort.Strings(verbs)
	return strings.Join([]string{
		strings.Join(groups, ","),
		strings.Join(resources, ","),
		strings.Join(resourceNames, ","),
		strings.Join(verbs, ","),
	}, "|")
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func loadManifestDocuments(t *testing.T) []manifestDocument {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate config test source")
	}
	configDir := filepath.Dir(sourceFile)
	manifestRoots := []string{
		filepath.Join(configDir, "crd"),
		filepath.Join(configDir, "rbac"),
		filepath.Join(configDir, "webhook"),
		filepath.Join(configDir, "networkpolicy"),
		filepath.Join(configDir, "samples"),
		filepath.Join(filepath.Dir(configDir), "test", "e2e", "phase0"),
	}

	var paths []string
	for _, root := range manifestRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("manifest root %s is unavailable: %v", root, err)
		}
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && (strings.EqualFold(filepath.Ext(path), ".yaml") || strings.EqualFold(filepath.Ext(path), ".yml")) {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk manifest root %s: %v", root, err)
		}
	}
	sort.Strings(paths)

	var documents []manifestDocument
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
		index := 0
		for {
			var raw json.RawMessage
			err := decoder.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = file.Close()
				t.Fatalf("decode %s document %d: %v", path, index+1, err)
			}
			if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			var data map[string]interface{}
			if err := json.Unmarshal(raw, &data); err != nil {
				_ = file.Close()
				t.Fatalf("decode %s document %d as an object: %v", path, index+1, err)
			}
			if len(data) == 0 {
				_ = file.Close()
				t.Fatalf("%s document %d is empty", path, index+1)
			}
			index++
			documents = append(documents, manifestDocument{
				path:  filepath.ToSlash(relativeToV3(filepath.Dir(configDir), path)),
				index: index,
				data:  data,
			})
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
	}
	return documents
}

func decodeTyped(t *testing.T, document manifestDocument, target interface{}) {
	t.Helper()
	raw, err := json.Marshal(document.data)
	if err != nil {
		t.Fatalf("marshal %s document %d: %v", document.path, document.index, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s document %d: %v", document.path, document.index, err)
	}
}

func assertNoCredentialLiterals(t *testing.T, value interface{}, path string, document int, fieldPath string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			childPath := key
			if fieldPath != "" {
				childPath = fieldPath + "." + key
			}
			lowerKey := strings.ToLower(key)
			if credentialFieldPattern.MatchString(key) && !strings.HasSuffix(lowerKey, "ref") {
				if text, ok := child.(string); ok && strings.TrimSpace(text) != "" {
					t.Errorf("%s document %d has credential-shaped field %s", path, document, childPath)
				}
			}
			assertNoCredentialLiterals(t, child, path, document, childPath)
		}
	case []interface{}:
		for index, child := range typed {
			assertNoCredentialLiterals(t, child, path, document, fmt.Sprintf("%s[%d]", fieldPath, index))
		}
	case string:
		if credentialLiteralPattern.MatchString(typed) {
			t.Errorf("%s document %d has credential-shaped literal at %s", path, document, fieldPath)
		}
	}
}

func hasOperation(operations []admissionregistrationv1.OperationType, wanted admissionregistrationv1.OperationType) bool {
	for _, operation := range operations {
		if operation == wanted {
			return true
		}
	}
	return false
}

func isUnder(path, root string) bool {
	path = filepath.ToSlash(path)
	root = filepath.ToSlash(root)
	return path == root || strings.HasPrefix(path, root+"/")
}

func relativeToV3(v3Dir, path string) string {
	relative, err := filepath.Rel(v3Dir, path)
	if err != nil {
		return path
	}
	return relative
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringValue(value interface{}) string {
	text, _ := value.(string)
	return text
}
