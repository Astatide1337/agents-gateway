{{/* Expand the chart name while keeping every generated Kubernetes name DNS-safe. */}}
{{- define "agw-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "agw-operator.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "agw-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.labels" -}}
helm.sh/chart: {{ include "agw-operator.chart" . }}
app.kubernetes.io/name: {{ include "agw-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: agents-gateway-v3
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "agw-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agw-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "agw-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{- .Values.serviceAccount.name -}}
{{- else if .Values.serviceAccount.create -}}
{{- include "agw-operator.fullname" . -}}
{{- else -}}
{{- fail "serviceAccount.name is required when serviceAccount.create is false" -}}
{{- end -}}
{{- end -}}

{{- define "agw-operator.webhookServiceName" -}}
{{- printf "%s-webhook" (include "agw-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.runtimeConfigName" -}}
{{- printf "%s-runtime" (include "agw-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.systemRoleName" -}}
{{- printf "%s-system" (include "agw-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.runsRoleName" -}}
{{- printf "%s-runs" (include "agw-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.preflightServiceAccountName" -}}
{{- printf "%s-preflight" (include "agw-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.preflightRoleName" -}}
{{- printf "%s-preflight" (include "agw-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agw-operator.validate" -}}
{{- if eq .Values.namespaces.system .Values.namespaces.runs -}}
{{- fail "namespaces.system and namespaces.runs must be different namespaces" -}}
{{- end -}}
{{- $agentGateway := .Values.agentGateway -}}
{{- if not $agentGateway.enabled -}}
{{- if or $agentGateway.image.repository $agentGateway.image.digest $agentGateway.configMapName $agentGateway.capabilityVersion $agentGateway.capabilityVerified $agentGateway.evidenceDigest $agentGateway.configDigest -}}
{{- fail "agentGateway is disabled: image, configMapName, capabilityVersion, capability evidence, and configDigest must all remain empty" -}}
{{- end -}}
{{- else -}}
{{- fail "agentGateway.enabled=true is deliberately rejected: the work-pod guard-to-agentgateway adapter and live capability evidence are not proven" -}}
{{- end -}}
{{- if and .Values.test.enabled (not .Values.test.image.digest) -}}
{{- fail "test.image.digest must be a sha256 digest when Helm tests are enabled" -}}
{{- end -}}
{{- if and .Values.preflight.enabled (not .Values.preflight.airlockProven) -}}
{{- fail "preflight.airlockProven must be true after the real Phase-0 UID/iptables checks pass" -}}
{{- end -}}
{{- if and .Values.preflight.enabled (not .Values.preflight.hostPathIdmapRequired) -}}
{{- fail "preflight.hostPathIdmapRequired must remain true for hostUsers:false hostPath mounts" -}}
{{- end -}}
{{- if .Values.phaseSupervisor.enabled -}}
{{- fail "phaseSupervisor.enabled is deliberately disabled: no trusted phase supervisor is wired" -}}
{{- end -}}
{{- $attestation := .Values.verificationAttestation -}}
{{- if not $attestation.enabled -}}
{{- if or $attestation.keyRef $attestation.verifyKeyRef -}}
{{- fail "verificationAttestation key references must be empty while attestation is disabled" -}}
{{- end -}}
{{- else -}}
{{- $keyRef := lower (default "" $attestation.keyRef) -}}
{{- if or (hasPrefix "env://" $keyRef) (hasPrefix "http://" $keyRef) (hasPrefix "https://" $keyRef) -}}
{{- fail "verificationAttestation.keyRef must be an explicit KMS/provider URI or absolute file path; ambient/remote key sources are forbidden" -}}
{{- end -}}
{{- if and (not (hasPrefix "/" $keyRef)) (not (contains "://" $keyRef)) -}}
{{- fail "verificationAttestation.keyRef must be an absolute file path or explicit provider/KMS URI" -}}
{{- end -}}
{{- if and (hasPrefix "/" $keyRef) (eq (default "" $attestation.verifyKeyRef) "") -}}
{{- fail "verificationAttestation.verifyKeyRef is required for file-backed signing so verification never reuses a private key path" -}}
{{- end -}}
{{- if $attestation.verifyKeyRef -}}
{{- $verifyKeyRef := lower $attestation.verifyKeyRef -}}
{{- if or (hasPrefix "env://" $verifyKeyRef) (hasPrefix "http://" $verifyKeyRef) (hasPrefix "https://" $verifyKeyRef) -}}
{{- fail "verificationAttestation.verifyKeyRef cannot use ambient or remote key sources" -}}
{{- end -}}
{{- if and (not (hasPrefix "/" $verifyKeyRef)) (not (contains "://" $verifyKeyRef)) -}}
{{- fail "verificationAttestation.verifyKeyRef must be an absolute file path or explicit provider/KMS URI" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "agw-operator.image" -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- end -}}

{{- define "agw-operator.testImage" -}}
{{- printf "%s@%s" .Values.test.image.repository .Values.test.image.digest -}}
{{- end -}}

{{- define "agw-operator.webhookPath" -}}
/validate-agents-astatide-com-v1alpha1-agentrun
{{- end -}}

{{/* Identity is passed as Workflow parameters, never copied from the workspace. */}}
{{- define "agw.lifecycleIdentityEnv" -}}
- name: AGW_RUN_UID
  value: "{{`{{workflow.parameters.agw-run-uid}}`}}"
- name: AGW_RUN_NAME
  value: "{{`{{workflow.parameters.agw-run-name}}`}}"
- name: AGW_NAMESPACE
  value: "{{`{{workflow.parameters.agw-namespace}}`}}"
- name: AGW_SPEC_DIGEST
  value: "{{`{{workflow.parameters.agw-resolved-spec-digest}}`}}"
- name: AGW_BASE_SHA
  value: "{{`{{workflow.parameters.agw-base-sha}}`}}"
- name: AGW_RUN_GENERATION
  value: "{{`{{workflow.parameters.agw-run-generation}}`}}"
- name: AGW_WORKFLOW_TEMPLATE_UID
  value: "{{`{{workflow.parameters.agw-workflow-template-uid}}`}}"
- name: AGW_WORKFLOW_TEMPLATE_DIGEST
  value: "{{`{{workflow.parameters.agw-workflow-template-digest}}`}}"
- name: AGW_WORKFLOW_TEMPLATE
  value: {{ .Values.argo.workflowTemplateName | quote }}
{{- end -}}

{{- define "agw.lifecycleStoreEnv" -}}
- name: AGW_OBJECT_STORE_BUCKET
  value: {{ .Values.objectStore.bucket | quote }}
- name: AGW_OBJECT_STORE_PREFIX
  value: {{ .Values.objectStore.prefix | quote }}
- name: AGW_OBJECT_STORE_REGION
  value: {{ .Values.objectStore.region | quote }}
- name: AGW_OBJECT_STORE_ENDPOINT
  value: {{ .Values.objectStore.endpoint | quote }}
- name: AGW_OBJECT_STORE_PATH_STYLE
  value: {{ .Values.objectStore.pathStyle | quote }}
- name: AGW_OBJECT_STORE_MAX_BYTES
  value: {{ .Values.operator.objectStoreMaxBytes | quote }}
{{- end -}}

{{- define "agw.lifecycleSTSAndImagesEnv" -}}
- name: AGW_ARTIFACT_STS_ROLE_ARN
  value: {{ .Values.artifactSTS.roleARN | quote }}
- name: AGW_ARTIFACT_STS_EXTERNAL_ID
  value: {{ .Values.artifactSTS.externalID | quote }}
- name: AGW_ARTIFACT_STS_ENDPOINT
  value: {{ .Values.artifactSTS.endpoint | quote }}
- name: AGW_ARTIFACT_CREDENTIAL_TTL
  value: {{ .Values.artifactSTS.credentialTTL | quote }}
- name: AGW_CLONE_IMAGE
  value: {{ printf "%s@%s" .Values.runtimeImages.clone.repository .Values.runtimeImages.clone.digest | quote }}
- name: AGW_SKILLS_IMAGE
  value: {{ printf "%s@%s" .Values.runtimeImages.skills.repository .Values.runtimeImages.skills.digest | quote }}
- name: AGW_CONTEXT_IMAGE
  value: {{ printf "%s@%s" .Values.runtimeImages.context.repository .Values.runtimeImages.context.digest | quote }}
- name: AGW_LOCKDOWN_IMAGE
  value: {{ printf "%s@%s" .Values.runtimeImages.lockdown.repository .Values.runtimeImages.lockdown.digest | quote }}
- name: AGW_BROKER_IMAGE
  value: {{ printf "%s@%s" .Values.runtimeImages.broker.repository .Values.runtimeImages.broker.digest | quote }}
{{- end -}}

{{/*
  A single render must use one CA for both the Secret and the webhook. Helm's
  crypto functions are random, so cache their result in the in-memory Values
  map for this render. On upgrades lookup returns the existing material first;
  no certificate is regenerated while the Secret exists.
*/}}
{{- define "agw-operator.webhookMaterial" -}}
{{- if not (hasKey .Values "__agw_webhook_material") -}}
  {{- $systemNamespace := .Values.namespaces.system -}}
  {{- $secretName := .Values.webhook.tlsSecretName -}}
  {{- $existing := lookup "v1" "Secret" $systemNamespace $secretName -}}
  {{- $material := dict -}}
  {{- if $existing -}}
    {{- $data := default (dict) $existing.data -}}
    {{- $caCrt := default "" (index $data "ca.crt") -}}
    {{- $tlsCrt := default "" (index $data "tls.crt") -}}
    {{- $tlsKey := default "" (index $data "tls.key") -}}
    {{- $material = dict "caCrt" (required (printf "Secret %s/%s is missing data.ca.crt" $systemNamespace $secretName) $caCrt) "tlsCrt" (required (printf "Secret %s/%s is missing data.tls.crt" $systemNamespace $secretName) $tlsCrt) "tlsKey" (required (printf "Secret %s/%s is missing data.tls.key" $systemNamespace $secretName) $tlsKey) -}}
  {{- else -}}
    {{- $serviceName := include "agw-operator.webhookServiceName" . -}}
    {{- $dnsNames := list $serviceName (printf "%s.%s" $serviceName $systemNamespace) (printf "%s.%s.svc" $serviceName $systemNamespace) (printf "%s.%s.svc.cluster.local" $serviceName $systemNamespace) -}}
    {{- $ca := genCA (printf "%s-ca" $serviceName) (.Values.webhook.caValidityDays | int) -}}
    {{- $cert := genSignedCert $serviceName nil $dnsNames (.Values.webhook.certValidityDays | int) $ca -}}
    {{- $material = dict "caCrt" ($ca.Cert | b64enc) "tlsCrt" ($cert.Cert | b64enc) "tlsKey" ($cert.Key | b64enc) -}}
  {{- end -}}
  {{- $_ := set .Values "__agw_webhook_material" $material -}}
{{- end -}}
{{- end -}}
