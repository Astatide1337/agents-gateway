package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	agwadmission "github.com/Astatide1337/agents-gateway/v3/internal/admission"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	agwcontroller "github.com/Astatide1337/agents-gateway/v3/internal/controller"
	"github.com/Astatide1337/agents-gateway/v3/internal/criticworkload"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/retention"
	"github.com/Astatide1337/agents-gateway/v3/internal/runplan"
	"github.com/Astatide1337/agents-gateway/v3/internal/runsecret"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	defaultLeaderElectionID = "agw-operator.agents.astatide.com"
	defaultWebhookPath      = "/validate-agents-astatide-com-v1alpha1-agentrun"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var webhookPort int
	var leaderElect bool
	var leaderNamespace string
	var preflightNodeFingerprint string
	var preflightRuntimeFingerprint string
	var preflightTTL time.Duration
	var runsNamespace string
	var systemNamespace string
	var objectStoreBucket string
	var objectStorePrefix string
	var objectStoreRegion string
	var objectStoreEndpoint string
	var objectStorePathStyle bool
	var objectStoreMaxBytes int64
	var cloneImage string
	var skillsImage string
	var contextImage string
	var lockdownImage string
	var brokerImage string
	var agentGatewayEnabled bool
	var agentGatewayImage string
	var agentGatewayConfigMapName string
	var agentGatewayCapabilityVersion string
	var agentGatewayCapabilityVerified bool
	var agentGatewayEvidenceDigest string
	var agentGatewayConfigDigest string
	var captureImage string
	var verifyFetchImage string
	var verifyApplyImage string
	var skillsTokenSecret string
	var skillsTokenKey string
	var skillsGatewayEndpoint string
	var githubAppSecret string
	var credentialSecretNamesValue string
	var allowedRuntimeClassesValue string
	var allowedStorageClassesValue string
	var reportSignerSecret string
	var verificationAttestationEnabled bool
	var cosignPath string
	var cosignKeyRef string
	var cosignVerifyKeyRef string
	var cosignTimeout time.Duration
	var artifactSTSRoleARN string
	var artifactSTSExternalID string
	var artifactSTSEndpoint string
	var artifactCredentialTTL time.Duration
	var retentionEnabled bool
	var retentionInterval time.Duration
	var retentionArtifactDays int
	var retentionWorkspaceDays int
	var retentionLedgerDays int
	var retentionLedgerPrefix string
	var retentionMaxActions int
	var retentionMaxRuns int
	var retentionMaxResources int
	var retentionMaxObjects int
	var retentionMaxLedgerBodyBytes int64
	var retentionDryRun bool
	var retentionEnforce bool
	var retentionLifecycleAttested bool
	var retentionMaxEvents int
	var sandboxBackendValue string
	var orchestrationBackendValue string
	var argoWorkflowTemplate string
	var argoWorkflowTemplateUID string
	var argoWorkflowTemplateDigest string
	var criticEnabled bool
	var criticImage string
	var criticAgentGatewayImage string
	var criticAgentGatewayCapabilityVerified bool
	var criticAgentGatewayEvidenceDigest string
	var criticGatewayEndpoint string
	var criticGatewayRouteRef string
	var criticGatewayServiceAccount string
	var criticGatewayTokenTTL int64
	var criticGatewayJWTIssuer string
	var criticGatewayJWTAudience string
	var criticGatewayJWTPolicyRef string
	var criticGatewayJWTVerified bool
	var criticGatewayJWTEvidenceDigest string
	var criticGatewayPodSelectorJSON string
	var criticObjectStoreEgressCIDRs string
	var criticTimeout time.Duration
	var criticMaxOutputBytes int64

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The TLS port for admission webhooks.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election for the controller manager.")
	flag.StringVar(&leaderNamespace, "leader-election-namespace", "agw-system", "Namespace for the leader-election Lease.")
	flag.StringVar(&preflightNodeFingerprint, "preflight-node-fingerprint", "", "Expected node fingerprint for agw-preflight; required for admission.")
	flag.StringVar(&preflightRuntimeFingerprint, "preflight-runtime-fingerprint", "", "Expected runtime fingerprint for agw-preflight; required for admission.")
	flag.DurationVar(&preflightTTL, "preflight-ttl", 10*time.Minute, "Maximum age of agw-preflight evidence.")
	flag.StringVar(&runsNamespace, "runs-namespace", "agw-runs", "The single namespace managed for AgentRun execution resources.")
	flag.StringVar(&systemNamespace, "system-namespace", "agw-system", "Namespace containing operator configuration, credentials, and preflight evidence.")
	flag.StringVar(&sandboxBackendValue, "sandbox-backend", string(sandbox.BackendAgentSandbox), "Execution child backend: agent-sandbox or job.")
	flag.StringVar(&orchestrationBackendValue, "orchestration-backend", string(agwcontroller.OrchestrationBackendDirect), "Lifecycle sequencing backend: direct or argo.")
	flag.StringVar(&argoWorkflowTemplate, "argo-workflow-template", "agw-agent-run-lifecycle", "Pinned namespaced Argo WorkflowTemplate used by the opt-in Argo backend.")
	flag.StringVar(&argoWorkflowTemplateUID, "argo-workflow-template-uid", "", "Immutable UID of the pinned Argo WorkflowTemplate; required by the Argo backend.")
	flag.StringVar(&argoWorkflowTemplateDigest, "argo-workflow-template-digest", "", "sha256 content digest of the pinned Argo WorkflowTemplate; required by the Argo backend.")
	flag.BoolVar(&criticEnabled, "critic-enabled", false, "Enable the authenticated different-family critic Job path.")
	flag.StringVar(&criticImage, "critic-image", "", "Digest-pinned agw-critic image; required when the critic is enabled.")
	flag.StringVar(&criticAgentGatewayImage, "critic-agentgateway-image", "", "Exact official digest-pinned agentgateway sidecar image.")
	flag.BoolVar(&criticAgentGatewayCapabilityVerified, "critic-agentgateway-capability-verified", false, "Attest the reviewed agentgateway image supports native Anthropic Messages with one total attempt.")
	flag.StringVar(&criticAgentGatewayEvidenceDigest, "critic-agentgateway-evidence-digest", "", "Digest of the exact agentgateway capability probe evidence.")
	flag.StringVar(&criticGatewayEndpoint, "critic-gateway-endpoint", "", "Internal agw-system central agentgateway endpoint.")
	flag.StringVar(&criticGatewayRouteRef, "critic-gateway-route-ref", "", "Central gateway route that owns critic provider credentials.")
	flag.StringVar(&criticGatewayServiceAccount, "critic-gateway-service-account", "", "ServiceAccount projected only into the run-local gateway sidecar.")
	flag.Int64Var(&criticGatewayTokenTTL, "critic-gateway-token-ttl-seconds", 900, "Projected critic gateway JWT lifetime in seconds.")
	flag.StringVar(&criticGatewayJWTIssuer, "critic-gateway-jwt-issuer", "", "Issuer proven by the central gateway JWT policy.")
	flag.StringVar(&criticGatewayJWTAudience, "critic-gateway-jwt-audience", "", "Audience proven by the central gateway JWT policy.")
	flag.StringVar(&criticGatewayJWTPolicyRef, "critic-gateway-jwt-policy-ref", "", "Reviewed central gateway JWT policy identity.")
	flag.BoolVar(&criticGatewayJWTVerified, "critic-gateway-jwt-verified", false, "Attest the central route rejects missing/invalid critic JWTs.")
	flag.StringVar(&criticGatewayJWTEvidenceDigest, "critic-gateway-jwt-evidence-digest", "", "Digest of the central gateway JWT capability evidence.")
	flag.StringVar(&criticGatewayPodSelectorJSON, "critic-gateway-pod-selector-json", "", "JSON label selector for only the central gateway data-plane Pods.")
	flag.StringVar(&criticObjectStoreEgressCIDRs, "critic-object-store-egress-cidrs", "", "Comma-separated exact object-store CIDRs allowed to critic input materializers.")
	flag.DurationVar(&criticTimeout, "critic-timeout", 10*time.Minute, "Maximum critic Job duration.")
	flag.Int64Var(&criticMaxOutputBytes, "critic-max-output-bytes", criticworkload.MaxOutputBytes, "Maximum canonical critic evidence bytes.")
	flag.StringVar(&objectStoreBucket, "object-store-bucket", "", "S3-compatible bucket for immutable v3 state (required).")
	flag.StringVar(&objectStorePrefix, "object-store-prefix", "agents-gateway/v3", "Traversal-free object key prefix.")
	flag.StringVar(&objectStoreRegion, "object-store-region", "us-east-1", "S3 signing region.")
	flag.StringVar(&objectStoreEndpoint, "object-store-endpoint", "", "Optional HTTPS S3-compatible service endpoint.")
	flag.BoolVar(&objectStorePathStyle, "object-store-path-style", false, "Use path-style S3 addressing (for MinIO-compatible endpoints).")
	flag.Int64Var(&objectStoreMaxBytes, "object-store-max-bytes", 64<<20, "Maximum immutable object size read or written by the operator.")
	flag.StringVar(&cloneImage, "clone-image", "", "Digest-pinned image for the clone initContainer (required).")
	flag.StringVar(&skillsImage, "skills-image", "", "Digest-pinned image for the skills initContainer (required).")
	flag.StringVar(&contextImage, "context-image", "", "Digest-pinned image for the ContextPack initContainer (required).")
	flag.StringVar(&lockdownImage, "lockdown-image", "", "Digest-pinned image for the user-namespace network airlock (required).")
	flag.StringVar(&brokerImage, "broker-image", "", "Digest-pinned image for the broker sidecar (required).")
	flag.BoolVar(&agentGatewayEnabled, "agentgateway-enabled", false, "Enable the guarded per-run agentgateway sidecar; rejected until its work-pod adapter is proven.")
	flag.StringVar(&agentGatewayImage, "agentgateway-image", "", "Exact digest-pinned agentgateway sidecar image; only used with --agentgateway-enabled.")
	flag.StringVar(&agentGatewayConfigMapName, "agentgateway-configmap-name", "", "Immutable credential-free per-run agentgateway ConfigMap; only used with --agentgateway-enabled.")
	flag.StringVar(&agentGatewayCapabilityVersion, "agentgateway-capability-version", "", "Reviewed agentgateway capability version; only used with --agentgateway-enabled.")
	flag.BoolVar(&agentGatewayCapabilityVerified, "agentgateway-capability-verified", false, "Attest the reviewed per-run agentgateway capability; only used with --agentgateway-enabled.")
	flag.StringVar(&agentGatewayEvidenceDigest, "agentgateway-evidence-digest", "", "Digest of the exact per-run agentgateway capability evidence.")
	flag.StringVar(&agentGatewayConfigDigest, "agentgateway-config-digest", "", "Digest of the exact per-run agentgateway configuration.")
	flag.StringVar(&captureImage, "capture-image", "", "Digest-pinned image for the networkless capture Job (required).")
	flag.StringVar(&verifyFetchImage, "verify-fetch-image", "", "Digest-pinned image for the verification fetch initContainer (required).")
	flag.StringVar(&verifyApplyImage, "verify-apply-image", "", "Digest-pinned image for the offline patch-apply initContainer (required).")
	flag.StringVar(&skillsTokenSecret, "skills-token-secret", "", "Optional immutable skills credential Secret in the system namespace.")
	flag.StringVar(&skillsTokenKey, "skills-token-key", "", "Key in --skills-token-secret; both flags must be supplied together.")
	flag.StringVar(&skillsGatewayEndpoint, "skills-gateway-endpoint", "", "Credential-free HTTPS /mcp endpoint used only when an Agent declares skills.")
	flag.StringVar(&githubAppSecret, "github-app-secret", "", "Immutable GitHub App credential Secret approved for clone and controller-owned publish (required).")
	flag.StringVar(&credentialSecretNamesValue, "credential-secret-names", "", "Comma-separated allowlist of source credential Secret names permitted for ToolSet, ModelRoute, and skills projections (required).")
	flag.StringVar(&allowedRuntimeClassesValue, "allowed-runtime-classes", "", "Comma-separated exact RuntimeClass names allowed by resolved Agent specs; empty denies caller-selected RuntimeClasses.")
	flag.StringVar(&allowedStorageClassesValue, "allowed-storage-classes", "", "Comma-separated exact StorageClass names allowed by resolved AgentRun specs; empty denies caller-selected StorageClasses.")
	flag.StringVar(&reportSignerSecret, "report-signer-secret", "", "Immutable Ed25519 verification-report signing Secret (required).")
	flag.BoolVar(&verificationAttestationEnabled, "verification-attestation-enabled", false, "Create and verify a cosign/in-toto bundle for every completed Gate report.")
	flag.StringVar(&cosignPath, "cosign-path", "/usr/local/bin/cosign", "Pinned cosign v3 executable used only when verification attestation is enabled.")
	flag.StringVar(&cosignKeyRef, "cosign-key-ref", "", "Explicit cosign KMS URI or externally managed key path; private key bytes are never accepted.")
	flag.StringVar(&cosignVerifyKeyRef, "cosign-verify-key-ref", "", "Optional explicit cosign public-key path or verification KMS URI; defaults to --cosign-key-ref.")
	flag.DurationVar(&cosignTimeout, "cosign-timeout", 3*time.Minute, "Maximum duration of each cosign sign or verify operation.")
	flag.StringVar(&artifactSTSRoleARN, "artifact-sts-role-arn", "", "IAM role assumed for per-run object-store credentials (required).")
	flag.StringVar(&artifactSTSExternalID, "artifact-sts-external-id", "", "Optional external ID required by the artifact STS role trust policy.")
	flag.StringVar(&artifactSTSEndpoint, "artifact-sts-endpoint", "", "Optional credential-free HTTPS STS endpoint.")
	flag.DurationVar(&artifactCredentialTTL, "artifact-credential-ttl", time.Hour, "Lifetime of one per-run artifact credential (15m..1h).")
	flag.BoolVar(&retentionEnabled, "retention-enabled", true, "Enable the periodic retention inventory and applier.")
	flag.DurationVar(&retentionInterval, "retention-interval", 15*time.Minute, "Retention inventory interval.")
	flag.IntVar(&retentionArtifactDays, "retention-artifact-days", retention.DefaultArtifactRetentionDays, "Immutable artifact retention window in days.")
	flag.IntVar(&retentionWorkspaceDays, "retention-workspace-days", retention.DefaultWorktreeRetentionDays, "Workspace PVC retention window in days.")
	flag.IntVar(&retentionLedgerDays, "retention-ledger-days", retention.DefaultLedgerRetentionDays, "Terminal effect claim/outcome retention window in days (minimum 30).")
	flag.StringVar(&retentionLedgerPrefix, "retention-ledger-prefix", "publish-effects", "Object-store ledger prefix protected by retention evidence checks.")
	flag.IntVar(&retentionMaxActions, "retention-max-actions", retention.DefaultMaxActionsPerPlan, "Maximum deletion actions in one retention cycle.")
	flag.IntVar(&retentionMaxRuns, "retention-max-runs", retention.DefaultInventoryLimits().MaxRuns, "Maximum AgentRuns inventoried in one retention cycle.")
	flag.IntVar(&retentionMaxResources, "retention-max-resources", retention.DefaultInventoryLimits().MaxResources, "Maximum Secrets or PVCs inventoried in one retention cycle.")
	flag.IntVar(&retentionMaxObjects, "retention-max-objects", retention.DefaultInventoryLimits().MaxObjects, "Maximum object-store objects inventoried in one retention cycle.")
	flag.Int64Var(&retentionMaxLedgerBodyBytes, "retention-max-ledger-body-bytes", retention.DefaultInventoryLimits().MaxLedgerBodyBytes, "Maximum canonical effect-ledger body size read in one retention cycle.")
	flag.BoolVar(&retentionDryRun, "retention-dry-run", true, "Plan retention deletions without applying them; must be false for enforcement.")
	flag.BoolVar(&retentionEnforce, "retention-enforce", false, "Explicitly enable retention deletion after lifecycle attestation.")
	flag.BoolVar(&retentionLifecycleAttested, "retention-lifecycle-attested", false, "Attest the real Agent Sandbox/PVC lifecycle before permitting enforcement.")
	flag.IntVar(&retentionMaxEvents, "retention-max-events", 32, "Maximum Kubernetes retention Events emitted per cycle.")
	flag.Parse()
	credentialSecretNames, ok := parseCredentialSecretNames(credentialSecretNamesValue)
	if !ok {
		os.Exit(2)
	}
	allowedRuntimeClasses, ok := parseOptionalNameList(allowedRuntimeClassesValue)
	if !ok {
		os.Exit(2)
	}
	allowedStorageClasses, ok := parseOptionalNameList(allowedStorageClassesValue)
	if !ok {
		os.Exit(2)
	}
	criticGatewayPodSelector := map[string]string(nil)
	criticEgressCIDRs := []string(nil)
	if criticEnabled {
		if err := json.Unmarshal([]byte(criticGatewayPodSelectorJSON), &criticGatewayPodSelector); err != nil || len(criticGatewayPodSelector) == 0 {
			os.Exit(2)
		}
		for _, raw := range strings.Split(criticObjectStoreEgressCIDRs, ",") {
			cidr := strings.TrimSpace(raw)
			if cidr == "" {
				os.Exit(2)
			}
			criticEgressCIDRs = append(criticEgressCIDRs, cidr)
		}
	}
	sandboxBackend, err := sandbox.ParseBackendKind(sandboxBackendValue)
	if err != nil {
		os.Exit(2)
	}
	orchestrationBackend, err := agwcontroller.ParseOrchestrationBackend(orchestrationBackendValue)
	if err != nil {
		os.Exit(2)
	}
	if orchestrationBackend == agwcontroller.OrchestrationBackendArgo && (argoWorkflowTemplate == "" || argoWorkflowTemplateUID == "" || argoWorkflowTemplateDigest == "") {
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	if preflightNodeFingerprint == "" || preflightRuntimeFingerprint == "" || preflightTTL <= 0 || runsNamespace == "" || systemNamespace == "" || objectStoreBucket == "" ||
		!allImagesPinned(cloneImage, skillsImage, contextImage, lockdownImage, brokerImage, captureImage, verifyFetchImage, verifyApplyImage) || githubAppSecret == "" || reportSignerSecret == "" || artifactSTSRoleARN == "" || artifactCredentialTTL < artifactauth.MinSTSLeaseLifetime || artifactCredentialTTL > artifactauth.MaxLeaseLifetime ||
		(skillsTokenSecret == "") != (skillsTokenKey == "") || verificationAttestationEnabled != (cosignKeyRef != "") || (!verificationAttestationEnabled && cosignVerifyKeyRef != "") || cosignTimeout <= 0 || cosignTimeout > time.Hour || retentionInterval <= 0 || retentionArtifactDays <= 0 || retentionWorkspaceDays <= 0 || retentionLedgerDays < retention.DefaultLedgerRetentionDays || retentionMaxActions <= 0 || retentionMaxActions > retention.MaxRetentionActions || retentionMaxRuns <= 0 || retentionMaxRuns > retention.MaxInventoryItems || retentionMaxResources <= 0 || retentionMaxResources > retention.MaxInventoryItems || retentionMaxObjects <= 0 || retentionMaxObjects > retention.MaxInventoryItems || retentionMaxLedgerBodyBytes <= 0 || retentionMaxLedgerBodyBytes > retention.DefaultInventoryLimits().MaxLedgerBodyBytes || retentionMaxEvents <= 0 || (retentionEnforce && retentionDryRun) ||
		(criticEnabled && (!allImagesPinned(criticImage, criticAgentGatewayImage) || !criticAgentGatewayCapabilityVerified || !criticGatewayJWTVerified || criticGatewayEndpoint == "" || criticGatewayRouteRef == "" || criticGatewayServiceAccount == "" || criticGatewayJWTIssuer == "" || criticGatewayJWTAudience == "" || criticGatewayJWTPolicyRef == "" || criticAgentGatewayEvidenceDigest == "" || criticGatewayJWTEvidenceDigest == "" || criticTimeout <= 0 || criticMaxOutputBytes <= 0 || objectStoreEndpoint == "" || len(criticEgressCIDRs) == 0)) {
		// A manager without an expected identity or a positive freshness window
		// cannot establish the execution trust boundary. Refuse startup rather
		// than appearing healthy while admission is misconfigured.
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		os.Exit(1)
	}
	if err := sandboxv1beta1.AddToScheme(scheme); err != nil {
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{
			runsNamespace:   {},
			systemNamespace: {},
		}},
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        defaultLeaderElectionID,
		LeaderElectionNamespace: leaderNamespace,
		WebhookServer:           webhook.NewServer(webhook.Options{Port: webhookPort}),
	})
	if err != nil {
		os.Exit(1)
	}

	// This is the integration seam for the AgentRun and configuration-resource
	// reconcilers. No reconciler is installed until child-resource and effect
	// ledger behavior is ready.
	var skillsToken *runsecret.SecretKeyRef
	if skillsTokenSecret != "" {
		skillsToken = &runsecret.SecretKeyRef{SecretName: skillsTokenSecret, Key: skillsTokenKey}
	}
	identity := preflight.Fingerprint{Node: preflightNodeFingerprint, Runtime: preflightRuntimeFingerprint}
	retentionPolicy := retention.DefaultPolicy()
	retentionPolicy.ArtifactRetentionDays = retentionArtifactDays
	retentionPolicy.WorktreeRetentionDays = retentionWorkspaceDays
	retentionPolicy.LedgerRetentionDays = retentionLedgerDays
	retentionPolicy.LedgerPrefix = retentionLedgerPrefix
	retentionPolicy.MaxActionsPerPlan = retentionMaxActions
	retentionConfig := retention.ControllerConfig{
		Policy: retentionPolicy, Interval: retentionInterval,
		InventoryLimits: retention.InventoryLimits{MaxRuns: retentionMaxRuns, MaxResources: retentionMaxResources, MaxObjects: retentionMaxObjects, MaxLedgerBodyBytes: retentionMaxLedgerBodyBytes},
		Enforce:         retentionEnforce, DryRun: retentionDryRun, LifecycleAttested: retentionLifecycleAttested,
		MaxEventsPerCycle: retentionMaxEvents,
	}
	if err := registerControllers(context.Background(), mgr, controllerOptions{
		ObjectStore: objectstore.Config{
			Bucket: objectStoreBucket, Prefix: objectStorePrefix, Region: objectStoreRegion,
			Endpoint: objectStoreEndpoint, ForcePathStyle: objectStorePathStyle, MaxObjectBytes: objectStoreMaxBytes,
		},
		RunPlan: runplan.Config{
			CloneImage: cloneImage, SkillsImage: skillsImage, ContextImage: contextImage,
			LockdownImage: lockdownImage, BrokerImage: brokerImage,
			AgentGateway: workload.AgentGatewaySidecarOptions{
				Enabled: agentGatewayEnabled, Image: agentGatewayImage, ConfigMapName: agentGatewayConfigMapName,
				CapabilityVersion: agentGatewayCapabilityVersion, CapabilityVerified: agentGatewayCapabilityVerified,
				EvidenceDigest: agentGatewayEvidenceDigest, ConfigDigest: agentGatewayConfigDigest,
			},
			SkillsToken: skillsToken, SkillsEndpoint: skillsGatewayEndpoint, GitHubAppSecret: githubAppSecret,
			CredentialSecretNames: credentialSecretNames,
			AllowedRuntimeClasses: allowedRuntimeClasses, AllowedStorageClasses: allowedStorageClasses,
		},
		SystemNamespace: systemNamespace, RunsNamespace: runsNamespace, PreflightTTL: preflightTTL, PreflightIdentity: identity,
		SandboxBackend:             sandboxBackend,
		OrchestrationBackend:       orchestrationBackend,
		ArgoWorkflowTemplate:       argoWorkflowTemplate,
		ArgoWorkflowTemplateUID:    argoWorkflowTemplateUID,
		ArgoWorkflowTemplateDigest: argoWorkflowTemplateDigest,
		CaptureImage:               captureImage, VerifyFetchImage: verifyFetchImage, VerifyApplyImage: verifyApplyImage,
		ReportSignerSecret: reportSignerSecret,
		AttestationEnabled: verificationAttestationEnabled,
		CosignPath:         cosignPath, CosignKeyRef: cosignKeyRef, CosignVerifyKeyRef: cosignVerifyKeyRef, CosignTimeout: cosignTimeout,
		ArtifactSTS:      artifactauth.STSConfig{RoleARN: artifactSTSRoleARN, Region: objectStoreRegion, ExternalID: artifactSTSExternalID, Endpoint: artifactSTSEndpoint},
		ArtifactTTL:      artifactCredentialTTL,
		RetentionEnabled: retentionEnabled, Retention: retentionConfig,
		CriticEnabled: criticEnabled,
		CriticBuild: criticworkload.BuildOptions{
			CriticImage: criticImage, AgentGatewayImage: criticAgentGatewayImage, LockdownImage: lockdownImage,
			AgentGatewayCapability: criticworkload.AgentGatewayCapability{
				Image: criticAgentGatewayImage, Version: criticworkload.AgentGatewayVersion,
				AnthropicMessages: criticAgentGatewayCapabilityVerified, RetryAttemptsOne: criticAgentGatewayCapabilityVerified,
				EvidenceDigest: criticAgentGatewayEvidenceDigest,
			},
			Gateway: criticworkload.GatewayClientOptions{
				Endpoint: criticGatewayEndpoint, RouteRef: criticGatewayRouteRef, ServiceAccountName: criticGatewayServiceAccount,
				TokenExpirationSeconds: criticGatewayTokenTTL,
				JWTCapability: criticworkload.GatewayJWTCapability{
					Issuer: criticGatewayJWTIssuer, Audience: criticGatewayJWTAudience, PolicyRef: criticGatewayJWTPolicyRef,
					Verified: criticGatewayJWTVerified, EvidenceDigest: criticGatewayJWTEvidenceDigest,
				},
				PodSelector: criticGatewayPodSelector,
			},
			ArtifactStore: criticworkload.ArtifactStoreOptions{
				Endpoint: objectStoreEndpoint, Region: objectStoreRegion, Bucket: objectStoreBucket,
				ForcePathStyle: objectStorePathStyle, EgressCIDRs: criticEgressCIDRs,
			},
			MaxOutputBytes: criticMaxOutputBytes, Timeout: criticTimeout,
		},
	}); err != nil {
		os.Exit(1)
	}
	validator := agwadmission.NewValidator(mgr.GetScheme(), mgr.GetAPIReader(), preflightTTL, identity)
	validator.Namespace = runsNamespace
	validator.PreflightNamespace = systemNamespace
	registerWebhooks(mgr, validator)

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("ready", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}

func allImagesPinned(images ...string) bool {
	if len(images) == 0 {
		return false
	}
	for _, image := range images {
		if !resolved.ValidPinnedImage(image) {
			return false
		}
	}
	return true
}

func parseCredentialSecretNames(raw string) ([]string, bool) {
	return parseNameList(raw, false)
}

func parseOptionalNameList(raw string) ([]string, bool) {
	return parseNameList(raw, true)
}

func parseNameList(raw string, allowEmpty bool) ([]string, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, allowEmpty
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, false
		}
		if _, exists := seen[name]; exists {
			return nil, false
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, true
}
