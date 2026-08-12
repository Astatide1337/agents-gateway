#!/usr/bin/env bash
set -euo pipefail

CHART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VALUES_FILE="$CHART_DIR/tests/values-ci.yaml"

fail() {
  printf 'chart validation failed: %s\n' "$1" >&2
  exit 1
}

for required_file in Chart.yaml values.yaml values.schema.json README.md templates/_helpers.tpl templates/deployment.yaml templates/validating-webhook.yaml templates/webhook-cert-secret.yaml templates/argo-workflow-template.yaml templates/servicemonitor.yaml; do
  test -f "$CHART_DIR/$required_file" || fail "missing $required_file"
done

test ! -e "$CHART_DIR/crds" || fail "CRDs must remain sourced from v3/config/crd, not copied into the chart"

grep -Fq 'failurePolicy: Fail' "$CHART_DIR/templates/validating-webhook.yaml" || fail 'webhook is not fail-closed'
grep -Fq 'lookup "v1" "Secret"' "$CHART_DIR/templates/_helpers.tpl" || fail 'certificate reuse does not use lookup'
grep -Fq 'genCA' "$CHART_DIR/templates/_helpers.tpl" || fail 'certificate generation does not use genCA'
grep -Fq 'genSignedCert' "$CHART_DIR/templates/_helpers.tpl" || fail 'certificate generation does not use genSignedCert'
grep -Fq '/tmp/k8s-webhook-server/serving-certs' "$CHART_DIR/templates/deployment.yaml" || fail 'controller-runtime serving path is not mounted'
grep -Fq -- '--system-namespace=' "$CHART_DIR/templates/deployment.yaml" || fail 'configured system namespace is not passed to the operator'
grep -Fq -- '--sandbox-backend=' "$CHART_DIR/templates/deployment.yaml" || fail 'sandbox backend selector is not passed to the operator'
grep -Fq -- '--orchestration-backend=' "$CHART_DIR/templates/deployment.yaml" || fail 'orchestration backend selector is not passed to the operator'
grep -Fq -- '--argo-workflow-template=' "$CHART_DIR/templates/deployment.yaml" || fail 'Argo WorkflowTemplate selector is not passed to the operator'
grep -Fq -- '--argo-workflow-template-uid=' "$CHART_DIR/templates/deployment.yaml" || fail 'Argo WorkflowTemplate UID is not passed to the operator'
grep -Fq -- '--argo-workflow-template-digest=' "$CHART_DIR/templates/deployment.yaml" || fail 'Argo WorkflowTemplate digest is not passed to the operator'
grep -Fq -- '--verification-attestation-enabled=' "$CHART_DIR/templates/deployment.yaml" || fail 'verification attestation selector is not passed to the operator'
grep -Fq -- '--critic-enabled=' "$CHART_DIR/templates/deployment.yaml" || fail 'critic selector is not passed to the operator'
grep -Fq -- '--agentgateway-enabled=' "$CHART_DIR/templates/deployment.yaml" || fail 'work-pod agentgateway selector is not passed to the operator'
  grep -Fq 'phaseSupervisor.enabled' "$CHART_DIR/templates/_helpers.tpl" || fail 'trusted phase supervisor fail-closed guard is missing'
  grep -Fq 'agentGateway.enabled' "$CHART_DIR/templates/_helpers.tpl" || fail 'work-pod agentgateway fail-closed guard is missing'
  grep -Fq 'enabled: false' "$CHART_DIR/values.yaml" || fail 'trusted phase supervisor must default to disabled'
  grep -Fq 'AGW_TRUSTED_PHASE_SOCKET' "$CHART_DIR/templates/deployment.yaml" && fail 'chart must not wire the retired unproven trusted phase socket'
grep -Fq -- '--cosign-key-ref=' "$CHART_DIR/templates/deployment.yaml" || fail 'external cosign key reference is not conditionally passed to the operator'
grep -Fq 'enum": ["agent-sandbox", "job"]' "$CHART_DIR/values.schema.json" || fail 'sandbox backend schema enum is missing'
grep -Fq 'enum": ["direct", "argo"]' "$CHART_DIR/values.schema.json" || fail 'orchestration backend schema enum is missing'
grep -Fq 'sandboxBackend: job' "$CHART_DIR/README.md" || fail 'Job backend prerequisite guidance is missing'
grep -Fq 'ca.crt:' "$CHART_DIR/templates/validating-webhook.yaml" && fail 'webhook must consume the generated material, not a ca.crt literal'
grep -Fq 'agents.x-k8s.io/v1beta1' "$CHART_DIR/README.md" || fail 'Agent Sandbox prerequisite is not documented'
grep -Fq 'v3/config/crd' "$CHART_DIR/README.md" || fail 'CRD source is not documented'
grep -Fq 'ServiceMonitor' "$CHART_DIR/README.md" || fail 'optional ServiceMonitor integration is not documented'
grep -Fq 'count/sandboxes.agents.x-k8s.io' "$CHART_DIR/values.yaml" || fail 'sandbox quota is missing'
grep -Fq 'networking.k8s.io/v1' "$CHART_DIR/templates/networkpolicy.yaml" || fail 'baseline NetworkPolicy is missing'
grep -Fq 'operator: NotIn' "$CHART_DIR/templates/networkpolicy.yaml" || fail 'baseline DNS policy does not exclude offline capture pods'
grep -Fq 'verify-fetch-public' "$CHART_DIR/templates/networkpolicy.yaml" || fail 'verify setup egress selector is missing'
grep -Fq 'kind: WorkflowTemplate' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle WorkflowTemplate is missing'
grep -Fq 'lifecycle-producer: argo-bounded-handoff-v1' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle producer marker is missing'
grep -Fq 'args: ["prepare"]' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle prepare step is missing'
grep -Fq 'args: ["stage"]' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle stage step is missing'
grep -Fq -- '- wait' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle condition-array wait step is missing'
grep -Fq 'args: ["handoff"]' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle handoff step is missing'
grep -Fq 'args: ["cleanup"]' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle cleanup template is missing'
grep -Fq 'onExit: cleanup' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle onExit cleanup is missing'
grep -Fq 'name: agw-lifecycle-output' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle output parameter is missing'
grep -Fq 'cleanupServiceAccountName' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle cleanup ServiceAccount is missing'
grep -Fq 'lifecycle-api-egress' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle API egress policy is missing'
grep -Fq 'lifecycle-public-egress' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle public egress policy is missing'
grep -Fq 'objectStoreEgressCIDRs' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle object-store egress allowlist is missing'
grep -Fq 'lifecycle-capture-deny-egress' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle capture deny policy is missing'
grep -Fq 'preflight.apiServerCIDR' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle API CIDR is not configured'
grep -Fq 'argo.apiServerPort' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle API port is not configured'
grep -Fq 'steps.prepare.outputs.parameters.run-timeout' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle wait timeout is not per-run'
grep -Fq '/tmp/agw-run-timeout' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'Argo lifecycle prepare timeout output is missing'
grep -Fq 'podSpecPatch: |' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'tokenless lifecycle pod patches are missing'
grep -Fq 'automountServiceAccountToken: false' "$CHART_DIR/templates/argo-workflow-template.yaml" || fail 'stage/capture tokenless contract is missing'
grep -Fq 'successCondition' "$CHART_DIR/templates/argo-workflow-template.yaml" && fail 'unsupported Sandbox successCondition exists in lifecycle template'
grep -Fq 'failureCondition' "$CHART_DIR/templates/argo-workflow-template.yaml" && fail 'unsupported Sandbox failureCondition exists in lifecycle template'
grep -Fq 'agw-phase0-sandbox-lifecycle' "$CHART_DIR/templates/argo-workflow-template.yaml" && fail 'Phase0 fixture leaked into production lifecycle template'

python3 - "$CHART_DIR/values.schema.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    schema = json.load(handle)

for key in ("namespaces", "image", "runtimeImages", "skillsGateway", "githubApp", "reportSigning", "verificationAttestation", "objectStore", "artifactSTS", "preflight", "orchestrationBackend", "agentGateway", "phaseSupervisor", "argo", "serviceMonitor"):
    assert key in schema["required"], f"top-level required value missing: {key}"
assert schema["properties"]["orchestrationBackend"]["enum"] == ["direct", "argo"]
service_monitor = schema["properties"]["serviceMonitor"]
assert set(service_monitor["required"]) == {"enabled", "labels", "interval", "scrapeTimeout"}
assert service_monitor["properties"]["enabled"]["type"] == "boolean"
assert service_monitor["properties"]["labels"]["additionalProperties"]["type"] == "string"
assert service_monitor["properties"]["interval"]["pattern"] == r"^[1-9][0-9]*(s|m|h)$"
assert service_monitor["properties"]["scrapeTimeout"]["pattern"] == r"^[1-9][0-9]*(s|m|h)$"
assert set(schema["properties"]["agentGateway"]["required"]) == {"enabled", "image", "configMapName", "capabilityVersion", "capabilityVerified", "evidenceDigest", "configDigest"}
assert schema["properties"]["agentGateway"]["properties"]["image"]["properties"]["digest"]["pattern"] == r"^$|^sha256:[a-f0-9]{64}$"
assert any(rule.get("properties", {}).get("enabled", {}).get("const") is False for rule in schema["properties"]["agentGateway"]["oneOf"])
assert any(rule.get("properties", {}).get("enabled", {}).get("const") is True for rule in schema["properties"]["agentGateway"]["oneOf"])
assert schema["properties"]["phaseSupervisor"]["properties"]["enabled"]["const"] is False
assert "workflowTemplateName" in schema["properties"]["argo"]["required"]
assert "workflowTemplateUID" in schema["properties"]["argo"]["required"]
assert "workflowTemplateDigest" in schema["properties"]["argo"]["required"]
assert "cleanupServiceAccountName" in schema["properties"]["argo"]["required"]
assert "apiServerPort" in schema["properties"]["argo"]["required"]
assert "objectStoreEgressCIDRs" in schema["properties"]["argo"]["required"]
assert schema["properties"]["runtimeImages"]["properties"]["lifecycle"]["$ref"] == "#/definitions/optionalImmutableImage"
assert any(rule.get("if", {}).get("properties", {}).get("orchestrationBackend", {}).get("const") == "argo" for rule in schema["allOf"])
assert "digest" in schema["properties"]["image"]["required"]
for helper in ("clone", "skills", "context", "lockdown", "broker", "capture", "verifyFetch", "verifyApply", "preflight", "lifecycle"):
    assert helper in schema["properties"]["runtimeImages"]["required"]
assert "bucket" in schema["properties"]["objectStore"]["required"]
assert "prefix" in schema["properties"]["objectStore"]["required"]
assert "region" in schema["properties"]["objectStore"]["required"]
assert "nodeFingerprint" in schema["properties"]["preflight"]["required"]
assert "runtimeFingerprint" in schema["properties"]["preflight"]["required"]
assert "hostPathIdmapRequired" in schema["properties"]["preflight"]["required"]
assert schema["properties"]["preflight"]["properties"]["hostPathIdmapRequired"]["const"] is True
assert schema["properties"]["reportSigning"]["properties"]["existingSecret"]["minLength"] == 1
assert set(schema["properties"]["verificationAttestation"]["required"]) == {"enabled", "cosignPath", "keyRef", "verifyKeyRef", "timeout"}
assert set(schema["properties"]["critic"]["required"]) == {"enabled", "image", "agentGateway", "gateway", "objectStoreEgressCIDRs", "timeout", "maxOutputBytes"}
assert schema["properties"]["critic"]["properties"]["agentGateway"]["properties"]["image"]["$ref"] == "#/definitions/immutableImage"
cosign_pattern = schema["properties"]["verificationAttestation"]["properties"]["cosignPath"]["pattern"]
assert cosign_pattern == r"^/[^\s]+$", f"unexpected cosignPath pattern: {cosign_pattern!r}"
key_pattern = schema["properties"]["verificationAttestation"]["properties"]["keyRef"]["pattern"]
verify_key_pattern = schema["properties"]["verificationAttestation"]["properties"]["verifyKeyRef"]["pattern"]
assert key_pattern == verify_key_pattern and "//" in key_pattern and "\\s" in key_pattern
assert any(rule.get("if", {}).get("properties", {}).get("keyRef", {}).get("pattern") == "^/" for rule in schema["properties"]["verificationAttestation"]["allOf"])
assert "\\u0000" not in cosign_pattern and "\\r" not in cosign_pattern and "\\n" not in cosign_pattern
assert "roleARN" in schema["properties"]["artifactSTS"]["required"]
assert schema["properties"]["artifactSTS"]["properties"]["credentialTTL"]["pattern"]
assert schema["properties"]["retention"]["properties"]["ledgerRetentionDays"]["minimum"] == 30
print("JSON schema checks passed")
PY

if command -v helm >/dev/null 2>&1; then
  rendered="$(mktemp)"
  trap 'rm -f "$rendered"' EXIT
  helm lint "$CHART_DIR" --values "$VALUES_FILE"
  helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" > "$rendered"

  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" --set phaseSupervisor.enabled=true >/dev/null 2>&1; then
    fail 'phaseSupervisor.enabled=true must fail closed at render time'
  fi

  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set orchestrationBackend=argo \
    --set-string runtimeImages.lifecycle.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    --set argo.objectStoreEgressCIDRs[0]=203.0.113.10/32 \
    --set argo.workflowTemplateUID='' \
    --set argo.workflowTemplateDigest='' >/dev/null 2>&1; then
    fail 'Argo without WorkflowTemplate UID/digest must fail closed'
  fi

  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set verificationAttestation.enabled=true \
    --set-string verificationAttestation.keyRef='/var/run/agw/cosign.key' >/dev/null 2>&1; then
    fail 'file-backed verification attestation without a separate public key must fail closed'
  fi

  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set verificationAttestation.enabled=true \
    --set-string verificationAttestation.keyRef='keys/cosign.key' \
    --set-string verificationAttestation.verifyKeyRef='/var/run/agw/cosign.pub' >/dev/null 2>&1; then
    fail 'relative verification key references must fail closed'
  fi

  agent_gateway_error="$(mktemp)"
  trap 'rm -f "$rendered" "$agent_gateway_error"' EXIT
  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set agentGateway.enabled=true \
    --set agentGateway.image.repository=cr.agentgateway.dev/agentgateway \
    --set-string agentGateway.image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    --set agentGateway.configMapName=agw-run-agentgateway-config \
    --set agentGateway.capabilityVersion=v1.4.1 \
    --set agentGateway.capabilityVerified=true \
    --set-string agentGateway.evidenceDigest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    --set-string agentGateway.configDigest=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc > /dev/null 2>"$agent_gateway_error"; then
    fail 'agentGateway.enabled=true must fail closed at render time'
  fi
  grep -Fq 'agentGateway.enabled=true is deliberately rejected' "$agent_gateway_error" || fail 'agentGateway render failure does not explain the unproven adapter'

  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set-string agentGateway.image.repository=cr.agentgateway.dev/agentgateway >/dev/null 2>&1; then
    fail 'partial disabled agentGateway configuration must fail closed'
  fi

  grep -Fq 'failurePolicy: Fail' "$rendered" || fail 'rendered webhook is not fail-closed'
  grep -Fq 'kind: ValidatingWebhookConfiguration' "$rendered" || fail 'rendered webhook is missing'
  grep -Fq 'kind: NetworkPolicy' "$rendered" || fail 'rendered NetworkPolicies are missing'
  grep -Fq 'kind: ResourceQuota' "$rendered" || fail 'rendered ResourceQuotas are missing'
  grep -Fq 'kind: LimitRange' "$rendered" || fail 'rendered LimitRanges are missing'
  grep -Fq 'kind: PodDisruptionBudget' "$rendered" || fail 'rendered PDB is missing'
  grep -Fq '/tmp/k8s-webhook-server/serving-certs' "$rendered" || fail 'rendered serving-certificate mount is missing'
grep -Fq -- '--github-app-secret=github-app' "$rendered" || fail 'rendered GitHub App selector is missing'
grep -Fq -- '--credential-secret-names=github-app,agw-objectstore-credentials,skills-credentials' "$rendered" || fail 'rendered source credential allowlist is missing'
grep -Fq -- '--allowed-runtime-classes=gvisor' "$rendered" || fail 'rendered RuntimeClass allowlist is missing'
grep -Fq -- '--allowed-storage-classes=local-path' "$rendered" || fail 'rendered StorageClass allowlist is missing'
grep -Fq 'resourceNames:' "$rendered" || fail 'operator Secret RBAC is not name-scoped'
  grep -Fq -- '--clone-image=ghcr.io/astatide/agw-clone@sha256:' "$rendered" || fail 'rendered clone helper is not configured'
  grep -Fq -- '--context-image=ghcr.io/astatide/agw-context@sha256:' "$rendered" || fail 'rendered context helper is not configured'
  grep -Fq -- '--verify-fetch-image=ghcr.io/astatide/agw-verify-fetch@sha256:' "$rendered" || fail 'rendered verify-fetch helper is not configured'
  grep -Fq -- '--verify-apply-image=ghcr.io/astatide/agw-verify-apply@sha256:' "$rendered" || fail 'rendered verify-apply helper is not configured'
  grep -Fq 'kind: CronJob' "$rendered" || fail 'rendered recurring preflight CronJob is missing'
  grep -Fq 'kind: Job' "$rendered" || fail 'rendered on-start preflight Job is missing'
  grep -Fq 'name: agw-preflight' "$rendered" || fail 'rendered fixed preflight ConfigMap is missing'
  grep -Fq 'hostUsers: false' "$rendered" || fail 'preflight pod does not use user namespaces'
  grep -Fq 'automountServiceAccountToken: false' "$rendered" || fail 'preflight pod does not disable ambient tokens'
  grep -Fq 'concurrencyPolicy: Forbid' "$rendered" || fail 'preflight CronJob is not overlap-safe'
  grep -Fq 'activeDeadlineSeconds: 120' "$rendered" || fail 'preflight deadline is missing'
  grep -Fq 'resources: [configmaps]' "$rendered" || fail 'preflight ConfigMap RBAC is missing'
  grep -Fq -- '--report-signer-secret=agw-report-signer' "$rendered" || fail 'rendered report signer Secret selector is missing'
  grep -Fq -- '--verification-attestation-enabled=false' "$rendered" || fail 'verification attestation must be explicitly disabled by default'
  grep -Fq -- '--critic-enabled=false' "$rendered" || fail 'critic must be explicitly disabled by default'
  grep -Fq -- '--agentgateway-enabled=false' "$rendered" || fail 'work-pod agentgateway must be explicitly disabled by default'
  grep -Fq -- '--agentgateway-image=' "$rendered" && fail 'disabled work-pod agentgateway must not render an image flag'
  grep -Fq -- '--critic-image=' "$rendered" && fail 'disabled critic must not render a workload image'
  grep -Fq -- '--cosign-key-ref=' "$rendered" && fail 'disabled verification attestation must not render a key reference'
  grep -Fq -- '--artifact-sts-role-arn=arn:aws:iam::123456789012:role/agw-v3-artifacts' "$rendered" || fail 'rendered artifact STS role is missing'
  grep -Fq -- '--artifact-credential-ttl=1h' "$rendered" || fail 'rendered artifact credential TTL is missing'
  grep -Fq -- '--artifact-sts-external-id=synthetic-external-id' "$rendered" || fail 'rendered artifact STS external ID is missing'
  grep -Fq -- '--sandbox-backend=agent-sandbox' "$rendered" || fail 'rendered primary sandbox backend selector is missing'
  grep -Fq -- '--orchestration-backend=direct' "$rendered" || fail 'rendered direct orchestration selector is missing'
  grep -Fq -- '--argo-workflow-template=agw-agent-run-lifecycle' "$rendered" || fail 'rendered Argo WorkflowTemplate selector is missing'
  grep -Fq 'resources: [workflows]' "$rendered" && fail 'direct backend must not request Argo Workflow RBAC'
  grep -Fq 'resources: [sandboxes]' "$rendered" || fail 'primary backend Sandbox RBAC is missing'
  grep -A2 -F 'resources: [persistentvolumeclaims]' "$rendered" | grep -Fq 'verbs: [create]' && fail 'primary backend must not receive direct PVC-create permission'
  grep -Fq -- '--skills-gateway-endpoint=https://skills.example.test/mcp' "$rendered" || fail 'rendered skills gateway endpoint is missing'
  grep -Fq 'QUdXX1dFQkhPT0tfQ0FfQlVORExFX1JFUVVJUkVE' "$rendered" && fail 'sentinel CA escaped into rendered output'
  grep -Fq 'kind: Ingress' "$rendered" && fail 'chart must not create an Ingress'
  grep -Fq 'kind: WorkflowTemplate' "$rendered" && fail 'direct test render unexpectedly includes the Argo lifecycle template'
  grep -Fq 'kind: ServiceMonitor' "$rendered" && fail 'ServiceMonitor must remain absent when disabled by default'

  service_monitor_rendered="$(mktemp)"
  trap 'rm -f "$service_monitor_rendered"' EXIT
  helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set serviceMonitor.enabled=true \
    --set serviceMonitor.labels.release=kube-prometheus-stack > "$service_monitor_rendered"
  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set serviceMonitor.enabled=true --set serviceMonitor.interval=0s >/dev/null 2>&1; then
    fail 'invalid ServiceMonitor interval must fail schema validation'
  fi
  if helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set serviceMonitor.enabled=true --set serviceMonitor.unexpected=true >/dev/null 2>&1; then
    fail 'unknown ServiceMonitor values must fail schema validation'
  fi
  python3 - "$service_monitor_rendered" <<'PY'
import sys

import yaml

documents = [document for document in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if document]
service_monitors = [document for document in documents if document.get("kind") == "ServiceMonitor"]
assert len(service_monitors) == 1, f"expected exactly one ServiceMonitor, found {len(service_monitors)}"
monitor = service_monitors[0]
assert monitor["apiVersion"] == "monitoring.coreos.com/v1"
assert monitor["metadata"]["name"] == "agw-operator-agw-operator-metrics"
assert monitor["metadata"]["namespace"] == "agw-system"
assert monitor["metadata"]["labels"]["app.kubernetes.io/name"] == "agw-operator"
assert monitor["metadata"]["labels"]["app.kubernetes.io/instance"] == "agw-operator"
assert monitor["metadata"]["labels"]["app.kubernetes.io/component"] == "metrics"
assert monitor["metadata"]["labels"]["release"] == "kube-prometheus-stack"
assert monitor["spec"]["namespaceSelector"] == {"matchNames": ["agw-system"]}
assert monitor["spec"]["selector"]["matchLabels"] == {
    "app.kubernetes.io/name": "agw-operator",
    "app.kubernetes.io/instance": "agw-operator",
    "app.kubernetes.io/component": "controller",
}
assert monitor["spec"]["endpoints"] == [{
    "port": "metrics",
    "path": "/metrics",
    "scheme": "http",
    "interval": "30s",
    "scrapeTimeout": "10s",
}]
print("ServiceMonitor opt-in labels, selector, namespace, and endpoint contract passed")
PY

  job_rendered="$(mktemp)"
  trap 'rm -f "$rendered" "$job_rendered"' EXIT
  helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" --set sandboxBackend=job > "$job_rendered"
  grep -Fq -- '--sandbox-backend=job' "$job_rendered" || fail 'rendered Job backend selector is missing'
  grep -Fq 'resources: [sandboxes]' "$job_rendered" && fail 'Job backend must not request Sandbox RBAC'
  grep -Fq 'count/sandboxes.agents.x-k8s.io' "$job_rendered" && fail 'Job backend must not request Sandbox quota'
  grep -A2 -F 'resources: [persistentvolumeclaims]' "$job_rendered" | grep -Fq 'verbs: [create]' || fail 'Job backend direct PVC-create permission is missing'

  argo_rendered="$(mktemp)"
  trap 'rm -f "$rendered" "$job_rendered" "$argo_rendered"' EXIT
  helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set orchestrationBackend=argo \
    --set argo.workflowTemplateName=agw-agent-run-lifecycle \
    --set-string runtimeImages.lifecycle.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa > "$argo_rendered"
  grep -Fq -- '--orchestration-backend=argo' "$argo_rendered" || fail 'rendered Argo orchestration selector is missing'
  grep -Fq -- '--argo-workflow-template=agw-agent-run-lifecycle' "$argo_rendered" || fail 'rendered Argo WorkflowTemplate selector is missing'
  grep -Fq -- '--argo-workflow-template-uid=template-uid-ci' "$argo_rendered" || fail 'rendered Argo WorkflowTemplate UID is missing'
  grep -Fq -- '--argo-workflow-template-digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$argo_rendered" || fail 'rendered Argo WorkflowTemplate digest is missing'
  grep -Fq 'resources: [workflows]' "$argo_rendered" || fail 'Argo backend Workflow RBAC is missing'
  grep -A2 -F 'resources: [workflows]' "$argo_rendered" | grep -Fq 'verbs: [get, create, delete]' || fail 'Argo backend Workflow RBAC is too broad or incomplete'
  grep -Fq 'kind: WorkflowTemplate' "$argo_rendered" || fail 'Argo lifecycle WorkflowTemplate is missing'
  grep -Fq 'name: agw-lifecycle-output' "$argo_rendered" || fail 'authenticated lifecycle output parameter is missing'
  grep -Fq 'args: ["prepare"]' "$argo_rendered" || fail 'lifecycle prepare step is missing'
  grep -Fq 'args: ["stage"]' "$argo_rendered" || fail 'lifecycle stage step is missing'
  grep -Fq -- '- wait' "$argo_rendered" || fail 'lifecycle condition-array wait step is missing'
  grep -Fq 'args: ["handoff"]' "$argo_rendered" || fail 'lifecycle handoff step is missing'
  grep -Fq 'args: ["cleanup"]' "$argo_rendered" || fail 'lifecycle cleanup template is missing'
  grep -Fq 'onExit: cleanup' "$argo_rendered" || fail 'lifecycle onExit cleanup is missing'
  grep -Fq 'lifecycle-producer: argo-bounded-handoff-v1' "$argo_rendered" || fail 'lifecycle producer marker is missing'
  grep -Fq 'kind: ServiceAccount' "$argo_rendered" || fail 'lifecycle ServiceAccount is missing'
  grep -Fq 'name: agw-argo-cleanup' "$argo_rendered" || fail 'cleanup ServiceAccount/RBAC is missing'
  grep -Fq 'lifecycle-api-egress' "$argo_rendered" || fail 'lifecycle API NetworkPolicy is missing'
  grep -Fq 'lifecycle-public-egress' "$argo_rendered" || fail 'lifecycle public NetworkPolicy is missing'
  grep -Fq 'lifecycle-capture-deny-egress' "$argo_rendered" || fail 'capture deny NetworkPolicy is missing'
  grep -Fq 'cidr: "10.43.0.1/32"' "$argo_rendered" || fail 'lifecycle API NetworkPolicy CIDR is not exact'
  grep -Fq 'port: 443' "$argo_rendered" || fail 'lifecycle API/public NetworkPolicy port is missing'
  grep -Fq 'artifact-session-token' "$argo_rendered" || fail 'per-run artifact Secret projection is missing'
  grep -Fq 'successCondition' "$argo_rendered" && fail 'unsupported Sandbox successCondition was rendered'
  grep -Fq 'failureCondition' "$argo_rendered" && fail 'unsupported Sandbox failureCondition was rendered'
  grep -Fq 'agw-agent-run-lifecycle@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$argo_rendered" || fail 'lifecycle image is not digest-pinned'

  python3 - "$argo_rendered" <<'PY'
import sys
import yaml

documents = [document for document in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if document]
workflow = next(document for document in documents if document.get("kind") == "WorkflowTemplate")
assert workflow["spec"]["onExit"] == "cleanup"
templates = {template["name"]: template for template in workflow["spec"]["templates"]}
for name in ("prepare", "stage", "wait", "capture", "handoff", "cleanup"):
    assert name in templates, f"missing Argo template {name}"
    assert templates[name]["metadata"]["labels"]["agents.astatide.com/lifecycle-role"] == name
assert templates["cleanup"]["serviceAccountName"] == "agw-argo-cleanup"
assert templates["cleanup"]["container"]["args"] == ["cleanup"]
assert workflow["spec"]["serviceAccountName"] == "agw-argo-lifecycle"
roles = {document["metadata"]["name"]: document for document in documents if document.get("kind") == "Role"}
prepare_role = roles["agw-argo-lifecycle"]
assert all(rule.get("resources") != ["secrets"] for rule in prepare_role["rules"]), "prepare may not access run Secrets"
sandbox_rule = next(rule for rule in prepare_role["rules"] if rule.get("resources") == ["sandboxes"])
assert sandbox_rule["verbs"] == ["get"], "prepare may only observe the operator-owned Sandbox"
assert "agw-argo-cleanup" not in roles, "Argo cleanup must not have a Role"
service_accounts = {document["metadata"]["name"]: document for document in documents if document.get("kind") == "ServiceAccount"}
for service_account in ("agw-argo-lifecycle", "agw-argo-wait", "agw-argo-handoff", "agw-argo-cleanup"):
    assert service_accounts[service_account]["automountServiceAccountToken"] is False
assert templates["prepare"]["automountServiceAccountToken"] is True
assert templates["wait"]["automountServiceAccountToken"] is True
assert templates["handoff"]["automountServiceAccountToken"] is True
assert templates["cleanup"]["automountServiceAccountToken"] is False
for name in ("prepare", "stage", "wait", "capture", "handoff", "cleanup"):
    patch = templates[name]["podSpecPatch"]
    assert "hostUsers: false" in patch
    assert "shareProcessNamespace: false" in patch
    assert "enableServiceLinks: false" in patch
    assert "type: RuntimeDefault" in patch
assert "automountServiceAccountToken: false" in templates["stage"]["podSpecPatch"]
assert "automountServiceAccountToken: false" in templates["capture"]["podSpecPatch"]
for name in ("prepare", "wait", "handoff", "cleanup"):
    assert "podSpecPatch" not in templates[name] or "automountServiceAccountToken: false" not in templates[name]["podSpecPatch"], f"{name} lost API credentials"
wait_args = templates["wait"]["container"]["args"]
assert "45m" not in wait_args and "{{inputs.parameters.timeout}}" in " ".join(wait_args)
policies = {document["metadata"]["name"]: document for document in documents if document.get("kind") == "NetworkPolicy"}
api = next(policy for name, policy in policies.items() if name.endswith("-lifecycle-api-egress"))
assert api["spec"]["podSelector"]["matchExpressions"][0]["values"] == ["prepare", "wait", "handoff"]
assert api["spec"]["egress"][0]["to"][0]["ipBlock"]["cidr"] == "10.43.0.1/32"
assert api["spec"]["egress"][0]["ports"] == [{"protocol": "TCP", "port": 443}]
public = next(policy for name, policy in policies.items() if name.endswith("-lifecycle-public-egress"))
assert public["spec"]["podSelector"]["matchExpressions"][0]["values"] == ["stage", "handoff"]
assert public["spec"]["egress"] == [{"to": [{"ipBlock": {"cidr": "203.0.113.10/32"}}], "ports": [{"protocol": "TCP", "port": 443}]}]
assert all(entry["to"][0]["ipBlock"]["cidr"] not in ("0.0.0.0/0", "::/0") for entry in public["spec"]["egress"])
capture = next(policy for name, policy in policies.items() if name.endswith("-lifecycle-capture-deny-egress"))
assert capture["spec"]["podSelector"]["matchLabels"]["agents.astatide.com/lifecycle-role"] == "capture"
assert capture["spec"]["egress"] == []
dns = next(policy for name, policy in policies.items() if name.endswith("-runs-dns"))
dns_exclusions = {
    (expression["key"], tuple(expression["values"]))
    for expression in dns["spec"]["podSelector"]["matchExpressions"]
    if expression["operator"] == "NotIn"
}
assert ("agents.astatide.com/role", ("capture",)) in dns_exclusions
assert ("agents.astatide.com/lifecycle-role", ("capture",)) in dns_exclusions
print("rendered lifecycle timeout, cleanup, token, and NetworkPolicy checks passed")
PY

  critic_rendered="$(mktemp)"
  trap 'rm -f "$rendered" "$job_rendered" "$argo_rendered" "$critic_rendered"' EXIT
  helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set critic.enabled=true \
    --set-string critic.image.digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    --set critic.agentGateway.capabilityVerified=true \
    --set-string critic.agentGateway.evidenceDigest=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
    --set critic.gateway.endpoint=http://agentgateway.agw-system.svc.cluster.local:8080 \
    --set critic.gateway.routeRef=critic-anthropic \
    --set critic.gateway.jwt.issuer=https://kubernetes.default.svc \
    --set critic.gateway.jwt.audience=agents-gateway-critic \
    --set critic.gateway.jwt.policyRef=critic-jwt \
    --set critic.gateway.jwt.verified=true \
    --set-string critic.gateway.jwt.evidenceDigest=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
    --set critic.gateway.podSelector.app=agentgateway \
    --set critic.objectStoreEgressCIDRs[0]=203.0.113.10/32 \
    --set objectStore.endpoint=https://s3.example.test > "$critic_rendered"
  grep -Fq -- '--critic-enabled=true' "$critic_rendered" || fail 'enabled critic selector is missing'
  grep -Fq -- '--critic-image=ghcr.io/astatide/agw-critic@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "$critic_rendered" || fail 'critic image is not digest-pinned'
  grep -Fq -- '--critic-agentgateway-image=cr.agentgateway.dev/agentgateway@sha256:efd79355b89094a8225a9db465d9a01dc656b377f0bab458761b935a13231d29' "$critic_rendered" || fail 'reviewed agentgateway digest is missing'
  grep -Fq -- '--critic-gateway-pod-selector-json={\"app\":\"agentgateway\"}' "$critic_rendered" || fail 'central gateway Pod selector is missing'
  grep -Fq -- '--critic-object-store-egress-cidrs=203.0.113.10/32' "$critic_rendered" || fail 'critic object-store egress CIDR is missing'
  grep -Fq 'name: agw-critic' "$critic_rendered" || fail 'critic gateway-client ServiceAccount is missing'
  grep -A16 -F 'name: agw-critic' "$critic_rendered" | grep -Fq 'automountServiceAccountToken: false' || fail 'critic ServiceAccount permits ambient tokens'

  attestation_rendered="$(mktemp)"
  trap 'rm -f "$rendered" "$job_rendered" "$argo_rendered" "$critic_rendered" "$attestation_rendered"' EXIT
  helm template agw-operator "$CHART_DIR" --namespace agw-system --values "$VALUES_FILE" \
    --set verificationAttestation.enabled=true \
    --set-string verificationAttestation.keyRef='awskms://arn:aws:kms:us-east-1:123456789012:key/test-key' \
    --set-string verificationAttestation.verifyKeyRef='/var/run/agw/cosign.pub' > "$attestation_rendered"
  grep -Fq -- '--verification-attestation-enabled=true' "$attestation_rendered" || fail 'enabled verification attestation selector is missing'
  grep -Fq -- '--cosign-key-ref=awskms://arn:aws:kms:us-east-1:123456789012:key/test-key' "$attestation_rendered" || fail 'external cosign KMS reference is missing'
  grep -Fq -- '--cosign-verify-key-ref=/var/run/agw/cosign.pub' "$attestation_rendered" || fail 'separate cosign verification key reference is missing'

  python3 - "$rendered" <<'PY'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
secret = re.search(r"kind: Secret\nmetadata:\n  name: agw-operator-webhook-tls[\s\S]*?\ndata:\n  ca\.crt: \"([^\"]+)\"", text)
webhook = re.search(r"kind: ValidatingWebhookConfiguration[\s\S]*?\n      caBundle: \"([^\"]+)\"", text)
assert secret, "rendered TLS Secret ca.crt is missing"
assert webhook, "rendered webhook caBundle is missing"
assert secret.group(1) == webhook.group(1), "webhook caBundle differs from TLS Secret ca.crt"
assert len(secret.group(1)) > 100, "rendered CA bundle is suspiciously short"
images = re.findall(r"^\s+image:\s+\"?([^\"\s]+)\"?\s*$", text, re.MULTILINE)
assert images and all(re.search(r"@sha256:[a-f0-9]{64}$", image) for image in images), "rendered image is not digest-pinned"
print("rendered certificate wiring checks passed")
PY

  python3 - "$rendered" <<'PY'
import sys

import yaml

documents = [document for document in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if document]

def pod_spec(document):
    if document["kind"] == "CronJob":
        return document["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    return document["spec"]["template"]["spec"]

airlock_workloads = []
for document in documents:
    if document["kind"] not in {"CronJob", "Job"}:
        continue
    name = document["metadata"]["name"]
    if name.endswith("-preflight") or name.endswith("-preflight-start"):
        airlock_workloads.append(document)

assert len(airlock_workloads) == 2, f"expected airlock CronJob and startup Job, found {len(airlock_workloads)}"
for document in airlock_workloads:
    spec = pod_spec(document)
    assert len(spec.get("initContainers", [])) == 1, f"{document['kind']} must have exactly one initContainer"
    assert spec["initContainers"][0]["name"] == "lockdown", "lockdown must be the sole initContainer"
    lockdown = "\n".join(spec["initContainers"][0]["args"])
    assert "iptables-restore --wait --noflush" in lockdown and "ip6tables-restore --wait --noflush" in lockdown, "preflight must use the same restore mechanism as the production lockdown"
    assert "--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8081" in lockdown, "preflight IPv4 agent allow rule is broader than production"
    assert "--uid-owner 1000 -p tcp -d ::1 --dport 8081" in lockdown, "preflight IPv6 agent allow rule is broader than production"
    assert "-A OUTPUT -o lo -j ACCEPT" not in lockdown, "preflight must not allow every loopback destination"
    assert [container["name"] for container in spec["containers"]] == ["attestor", "agent-probe"], "attestor and agent-probe must be regular concurrent containers"
    assert spec["securityContext"]["fsGroup"] == 1337, "evidence volume fsGroup must be 1337"
    assert spec["securityContext"]["fsGroupChangePolicy"] == "OnRootMismatch", "evidence fsGroup change policy is not bounded"
    assert 1337 in spec["securityContext"]["supplementalGroups"], "agent-probe must share the evidence group"

    volumes = {volume["name"]: volume for volume in spec["volumes"]}
    evidence = volumes["evidence"]["emptyDir"]
    assert evidence["medium"] == "Memory" and evidence["sizeLimit"] == "16Mi", "evidence volume is not bounded in memory"
    assert "hostPath" in volumes["host-proc"] and "hostPath" in volumes["host-kubelet"], "hostPath fingerprint checks were removed"

    mounts = {
        container["name"]: {mount["name"]: mount for mount in container.get("volumeMounts", [])}
        for container in spec["containers"]
    }
    assert "evidence" in mounts["attestor"] and "evidence" in mounts["agent-probe"]
    assert "preflight-api" in mounts["attestor"] and "preflight-api" not in mounts["agent-probe"], "agent-probe received ambient API credentials"
    assert all(not mount.get("readOnly", False) for mount in (mounts["attestor"]["evidence"], mounts["agent-probe"]["evidence"])), "shared evidence volume must be writable by both probes"

print("rendered preflight concurrency, fsGroup, and hostPath checks passed")
PY
else
  printf '%s\n' 'Helm is not installed; completed dependency-free static chart checks.'
fi

printf '%s\n' 'Agents Gateway v3 chart validation passed.'
