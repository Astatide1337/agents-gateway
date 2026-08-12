package main

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/argoworkflow"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/capturecontroller"
	agwcontroller "github.com/Astatide1337/agents-gateway/v3/internal/controller"
	"github.com/Astatide1337/agents-gateway/v3/internal/cosignattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/criticworkload"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubpublish"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/publishcontroller"
	"github.com/Astatide1337/agents-gateway/v3/internal/retention"
	"github.com/Astatide1337/agents-gateway/v3/internal/runplan"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verificationattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	k8smetadata "k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type controllerOptions struct {
	ObjectStore                objectstore.Config
	RunPlan                    runplan.Config
	SystemNamespace            string
	RunsNamespace              string
	PreflightTTL               time.Duration
	PreflightIdentity          preflight.Fingerprint
	CaptureImage               string
	VerifyFetchImage           string
	VerifyApplyImage           string
	ReportSignerSecret         string
	AttestationEnabled         bool
	CosignPath                 string
	CosignKeyRef               string
	CosignVerifyKeyRef         string
	CosignTimeout              time.Duration
	ArtifactSTS                artifactauth.STSConfig
	ArtifactTTL                time.Duration
	RetentionEnabled           bool
	Retention                  retention.ControllerConfig
	SandboxBackend             sandbox.BackendKind
	OrchestrationBackend       agwcontroller.OrchestrationBackendKind
	ArgoWorkflowTemplate       string
	ArgoWorkflowTemplateUID    string
	ArgoWorkflowTemplateDigest string
	CriticEnabled              bool
	CriticBuild                criticworkload.BuildOptions
}

// directSecretClient deliberately separates uncached named reads from writes.
// It prevents runplan from starting a Secret informer, so the operator needs
// neither list nor watch permission for long-lived credentials.
type directSecretClient struct {
	reader client.Reader
	writer client.Client
}

// metadataInventoryReader uses the client-go metadata API rather than a
// typed SecretList. The latter includes Secret.data in every list response,
// which would make the retention controller a credential reader even though
// retention only needs ownership metadata.
type metadataInventoryReader struct {
	client k8smetadata.Interface
}

func (r metadataInventoryReader) ListMetadata(ctx context.Context, resource schema.GroupVersionResource, namespace string, limit int) (*metav1.PartialObjectMetadataList, error) {
	return r.client.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: int64(limit)})
}

func (c directSecretClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	return c.reader.Get(ctx, key, object, options...)
}

func (c directSecretClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	return c.writer.Create(ctx, object, options...)
}

type executionPreflightGate struct {
	reader    client.Reader
	namespace string
	ttl       time.Duration
	expected  preflight.Fingerprint
}

func (g executionPreflightGate) CheckExecution(ctx context.Context, now time.Time) agwcontroller.ExecutionDecision {
	decision := preflight.CheckConfigMapInNamespace(ctx, g.reader, g.namespace, now, g.ttl, g.expected)
	return agwcontroller.ExecutionDecision{Allowed: decision.Allowed, Reason: string(decision.Reason)}
}

func registerControllers(ctx context.Context, mgr ctrl.Manager, options controllerOptions) error {
	store, err := objectstore.New(ctx, options.ObjectStore)
	if err != nil {
		return fmt.Errorf("configure immutable object store: %w", err)
	}
	artifactWriter, err := artifacts.NewWriter(store)
	if err != nil {
		return fmt.Errorf("configure artifact writer: %w", err)
	}
	sandboxBackend, err := newSandboxBackend(mgr.GetClient(), options.SandboxBackend)
	if err != nil {
		return fmt.Errorf("configure sandbox backend %q: %w", options.SandboxBackend, err)
	}
	var orchestration agwcontroller.OrchestrationDriver
	switch options.OrchestrationBackend {
	case "", agwcontroller.OrchestrationBackendDirect:
		options.OrchestrationBackend = agwcontroller.OrchestrationBackendDirect
	case agwcontroller.OrchestrationBackendArgo:
		if options.SandboxBackend != sandbox.BackendAgentSandbox {
			return fmt.Errorf("Argo orchestration requires the operator-owned agent-sandbox backend; refusing the Job rollback backend")
		}
		backend, err := argoworkflow.NewBackend(mgr.GetClient(), argoworkflow.Config{
			Namespace: options.RunsNamespace, WorkflowTemplateName: options.ArgoWorkflowTemplate,
			WorkflowTemplateUID: options.ArgoWorkflowTemplateUID, WorkflowTemplateDigest: options.ArgoWorkflowTemplateDigest,
		})
		if err != nil {
			return fmt.Errorf("configure Argo orchestration backend: %w", err)
		}
		orchestration = backend
	default:
		return fmt.Errorf("unsupported orchestration backend %q", options.OrchestrationBackend)
	}
	secretClient := directSecretClient{reader: mgr.GetAPIReader(), writer: mgr.GetClient()}
	runPlanConfig := options.RunPlan
	runPlanConfig.Client = secretClient
	runPlanConfig.SystemNamespace = options.SystemNamespace
	stsMinter, err := artifactauth.NewSTSMinter(options.ArtifactSTS)
	if err != nil {
		return fmt.Errorf("configure scoped artifact STS minter: %w", err)
	}
	artifactIssuer, err := artifactauth.New(artifactauth.Config{Minter: stsMinter})
	if err != nil {
		return fmt.Errorf("configure scoped artifact credential issuer: %w", err)
	}
	runPlanConfig.ArtifactIssuer = artifactIssuer
	runPlanConfig.ArtifactStoreEndpoint = options.ObjectStore.Endpoint
	runPlanConfig.ArtifactStoreRegion = options.ObjectStore.Region
	runPlanConfig.ArtifactStoreBucket = options.ObjectStore.Bucket
	runPlanConfig.ArtifactStorePrefix = options.ObjectStore.Prefix
	runPlanConfig.ArtifactStoreForcePathStyle = options.ObjectStore.ForcePathStyle
	runPlanConfig.ArtifactStoreMaxObjectBytes = options.ObjectStore.MaxObjectBytes
	runPlanConfig.ArtifactCredentialTTL = options.ArtifactTTL
	workFactory, err := runplan.New(runPlanConfig)
	if err != nil {
		return fmt.Errorf("configure work Sandbox planner: %w", err)
	}
	eventsRepository, err := runtimeevents.NewRepository(store)
	if err != nil {
		return fmt.Errorf("configure runtime completion repository: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("configure capture pod-log reader: %w", err)
	}
	captureDriver, err := capturecontroller.New(mgr.GetClient(), capturecontroller.Options{
		Logs: podLogReader{client: clientset}, Artifacts: store,
		MaxLogBytes: capture.MaxEncodedFrameBytes(capture.DefaultMaxResultBytes, capture.DefaultMaxPatchBytes, capture.DefaultMaxManifestBytes),
	})
	if err != nil {
		return fmt.Errorf("configure capture phase: %w", err)
	}
	reportSigner, err := loadReportSigner(ctx, mgr.GetAPIReader(), options.SystemNamespace, options.ReportSignerSecret)
	if err != nil {
		return fmt.Errorf("configure verification report signer: %w", err)
	}
	gatePublicKey, err := reportSigner.PublicKey()
	if err != nil {
		return fmt.Errorf("configure trusted verification report key: %w", err)
	}
	var criticAdapter *criticworkload.VerifyAdapter
	if options.CriticEnabled {
		criticAuth, err := criticworkload.NewEd25519Authenticator(reportSigner.privateKey, gatePublicKey)
		if err != nil {
			return fmt.Errorf("configure critic evidence authenticator: %w", err)
		}
		criticRunner, err := criticworkload.NewRunner(criticworkload.RunnerOptions{
			Client: mgr.GetClient(), Logs: criticworkload.KubernetesLogReader{Client: clientset},
			Store: store, Auth: criticAuth, Build: options.CriticBuild,
		})
		if err != nil {
			return fmt.Errorf("configure critic workload runner: %w", err)
		}
		criticSource, err := criticworkload.NewSource(criticworkload.SourceOptions{
			Reader: mgr.GetAPIReader(), Store: store, Auth: criticAuth,
			Namespace: options.RunsNamespace, Bucket: options.ObjectStore.Bucket,
			Prefix: options.ObjectStore.Prefix, MaxOutputBytes: options.CriticBuild.MaxOutputBytes,
		})
		if err != nil {
			return fmt.Errorf("configure critic evidence source: %w", err)
		}
		criticAdapter = &criticworkload.VerifyAdapter{
			Runner: criticRunner, Source: criticSource,
			ResolveContext: func(ctx context.Context, input verifycontroller.CriticRunInput) (v1alpha1.ArtifactRef, error) {
				return resolveCriticContextRef(ctx, mgr.GetAPIReader(), input)
			},
		}
	}
	var reportAttester verifycontroller.ReportAttester
	if options.AttestationEnabled {
		cosign, err := cosignattestation.New(cosignattestation.Config{
			CosignPath: options.CosignPath, KeyRef: options.CosignKeyRef, VerifyKeyRef: options.CosignVerifyKeyRef,
			PredicateType: evidenceattestation.PredicateType, MediaType: evidenceattestation.MediaType,
			Timeout: options.CosignTimeout,
		})
		if err != nil {
			return fmt.Errorf("configure cosign verification attestation: %w", err)
		}
		lifecycle, err := verificationattestation.New(verificationattestation.Config{
			Cosign: cosign, Store: store, TrustedGatePublicKey: gatePublicKey,
		})
		if err != nil {
			return fmt.Errorf("configure verification attestation lifecycle: %w", err)
		}
		reportAttester = lifecycle
	}
	verifyDriver, err := verifycontroller.New(verifycontroller.Options{
		Backend: sandboxBackend, Evidence: podVerifyEvidenceReader{client: clientset},
		ReportStore: store, ReportSigner: reportSigner, ReportAttester: reportAttester,
		CriticRunner: criticAdapter, CriticEvidence: criticAdapter,
	})
	if err != nil {
		return fmt.Errorf("configure independent verification phase: %w", err)
	}
	githubMinter, err := workFactory.GitHubMinter(ctx)
	if err != nil {
		return fmt.Errorf("configure controller-owned GitHub App publisher: %w", err)
	}
	githubClient, err := githubpublish.New(githubpublish.Config{
		Minter: githubMinter, BaseURL: options.RunPlan.GitHubAPIBaseURL,
		HTTPClient: options.RunPlan.GitHubHTTPClient,
	})
	if err != nil {
		return fmt.Errorf("configure GitHub publication adapter: %w", err)
	}
	effectLedger, err := effects.New(store, "publish-effects", nil)
	if err != nil {
		return fmt.Errorf("configure publication effect ledger: %w", err)
	}
	publisher, err := publish.New(githubClient, effectLedger)
	if err != nil {
		return fmt.Errorf("configure publication protocol: %w", err)
	}
	publicationArtifacts, err := publishcontroller.NewStoreReader(store, options.ObjectStore.Bucket, options.ObjectStore.Prefix)
	if err != nil {
		return fmt.Errorf("configure publication artifact reader: %w", err)
	}
	publicationDriver, err := publishcontroller.New(publishcontroller.Options{
		Artifacts: publicationArtifacts, Publisher: publisher, TrustedGatePublicKey: gatePublicKey,
	})
	if err != nil {
		return fmt.Errorf("configure publication evidence driver: %w", err)
	}
	reconciler := &agwcontroller.AgentRunReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		OrchestrationBackend: options.OrchestrationBackend, Orchestration: orchestration,
		Artifacts: artifactWriter, LifecycleOutputs: artifactWriter, ContextArtifacts: artifactWriter, Sandboxes: sandboxBackend, Work: workFactory, Events: eventsRepository, Source: workFactory,
		Capture: capturePhaseAdapter{driver: captureDriver, image: options.CaptureImage}, Processes: podProcessObserver{client: clientset},
		Verify: verifyPhaseAdapter{
			driver: verifyDriver, factory: workFactory, store: options.ObjectStore, contextStore: store,
			fetchImage: options.VerifyFetchImage, applyImage: options.VerifyApplyImage,
			lockdownImage:         options.RunPlan.LockdownImage,
			allowedRuntimeClasses: append([]string(nil), options.RunPlan.AllowedRuntimeClasses...),
			allowedStorageClasses: append([]string(nil), options.RunPlan.AllowedStorageClasses...),
			artifactTTL:           options.ArtifactTTL,
		},
		Publish: publishPhaseAdapter{driver: publicationDriver},
		Execution: executionPreflightGate{
			reader: mgr.GetAPIReader(), namespace: options.SystemNamespace,
			ttl: options.PreflightTTL, expected: options.PreflightIdentity,
		},
	}
	if err := reconciler.SetupWithManager(mgr, options.SandboxBackend); err != nil {
		return fmt.Errorf("register AgentRun reconciler: %w", err)
	}
	if options.RetentionEnabled {
		metadataClient, err := k8smetadata.NewForConfig(mgr.GetConfig())
		if err != nil {
			return fmt.Errorf("configure metadata client for retention: %w", err)
		}
		retentionObjects, err := retention.NewS3Inventory(ctx, retention.S3Config{
			Bucket: options.ObjectStore.Bucket, Prefix: options.ObjectStore.Prefix,
			Region: options.ObjectStore.Region, Endpoint: options.ObjectStore.Endpoint,
			ForcePathStyle:     options.ObjectStore.ForcePathStyle,
			MaxLedgerBodyBytes: options.Retention.InventoryLimits.MaxLedgerBodyBytes,
		})
		if err != nil {
			return fmt.Errorf("configure retention object inventory: %w", err)
		}
		source := &retention.KubernetesSource{
			Reader: mgr.GetAPIReader(), Objects: retentionObjects,
			Metadata:  metadataInventoryReader{client: metadataClient},
			Namespace: options.RunsNamespace, ObjectPrefix: options.ObjectStore.Prefix,
			LedgerPrefix: options.Retention.Policy.LedgerPrefix,
			Limits:       options.Retention.InventoryLimits,
		}
		events := retention.KubernetesEventSink{Reader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorderFor("agw-retention")}
		retentionController, err := retention.NewController(source, mgr.GetClient(), retentionObjects, options.Retention, events, ctrl.Log.WithName("retention"))
		if err != nil {
			return fmt.Errorf("configure retention controller: %w", err)
		}
		if err := mgr.Add(retentionController); err != nil {
			return fmt.Errorf("register retention controller: %w", err)
		}
	}
	return nil
}

func newSandboxBackend(c client.Client, kind sandbox.BackendKind) (sandbox.SandboxBackend, error) {
	switch kind {
	case sandbox.BackendAgentSandbox:
		return sandbox.NewAgentSandboxBackend(c, sandbox.BackendOptions{})
	case sandbox.BackendJob:
		return sandbox.NewJobBackend(c, sandbox.BackendOptions{})
	default:
		return nil, fmt.Errorf("unsupported sandbox backend %q", kind)
	}
}
