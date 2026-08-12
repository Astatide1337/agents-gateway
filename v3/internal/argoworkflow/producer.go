package argoworkflow

// This file contains the bounded work performed by the containers in the
// Argo lifecycle WorkflowTemplate.  It is intentionally not a reconciler or
// a lifecycle state machine: Argo owns ordering, retries, deadlines, and pod
// cleanup.  The producer only prepares one child, stages one immutable
// contract, waits through the condition-array bridge, and hands authenticated
// artifacts to the controller-owned AGW lifecycle.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// WorkspaceRoot is the only path shared by the work Sandbox and lifecycle
	// helper containers.  The agent never writes the reserved output path.
	WorkspaceRoot      = "/workspace"
	StageDir           = WorkspaceRoot + "/.agw"
	ResolvedPath       = StageDir + "/resolved-spec.json"
	CaptureSpecPath    = StageDir + "/capture-spec.json"
	ReadyMarkerPath    = StageDir + "/ready.json"
	FinishedMarkerPath = StageDir + "/finished.json"

	LifecycleOutputPath   = "/tmp/agw-lifecycle-output.json"
	PrepareSandboxPath    = "/tmp/agw-sandbox-name"
	PrepareSandboxUIDPath = "/tmp/agw-sandbox-uid"
	PrepareClaimPath      = "/tmp/agw-workspace-claim"
	PrepareSecretPath     = "/tmp/agw-work-secret-name"
	PrepareTimeoutPath    = "/tmp/agw-run-timeout"

	VerificationInputKind      = "verification-input"
	VerificationInputName      = "verification-input.json"
	VerificationInputMediaType = "application/vnd.agents-gateway.verification-input.v1+json"

	maxStageSnapshotBytes = 1 << 20
	maxStageSpecBytes     = 128 << 10
	maxWorkflowUIDBytes   = 128
)

var (
	ErrProducerInvalid     = errors.New("argoworkflow: invalid lifecycle producer input")
	ErrProducerUnavailable = errors.New("argoworkflow: lifecycle producer dependency is unavailable")
	ErrProducerArtifact    = errors.New("argoworkflow: lifecycle artifact is not authenticated")
	ErrProducerFile        = errors.New("argoworkflow: lifecycle workspace file is invalid")
	ErrProducerWorkflow    = errors.New("argoworkflow: live Workflow binding is invalid")
	ErrProducerTerminal    = errors.New("argoworkflow: runtime completion is not successful")
	ErrProducerConflict    = errors.New("argoworkflow: immutable lifecycle artifact conflicts")
)

// Request is the complete identity supplied by the Workflow parameters.  It
// contains no credentials and is validated against both the AgentRun and the
// content-addressed resolved snapshot before any child or output is trusted.
type Request struct {
	Namespace              string
	RunName                string
	RunUID                 string
	RunGeneration          int64
	SpecDigest             string
	BaseSHA                string
	WorkflowTemplateName   string
	WorkflowTemplateUID    string
	WorkflowTemplateDigest string
	SandboxUID             string
}

// URIResolver reconstructs an immutable provider URI for a generated key.
// It is needed for the runtime completion record because that record already
// exists and is read-only at handoff time.
type URIResolver interface {
	URI(string) (string, error)
}

// Producer is one-shot work invoked by separate Argo templates.  No method
// stores phase or retry state; re-entry is safe because all externally visible
// objects are deterministic and immutable.
type Producer struct {
	client                 client.Client
	store                  artifacts.Store
	uri                    URIResolver
	workflowTemplate       string
	workflowTemplateUID    string
	workflowTemplateDigest string
}

// NewProducer validates the immutable WorkflowTemplate binding used by every
// lifecycle phase. Prepare is deliberately read-only with respect to child
// resources: the trusted AGW operator creates the Sandbox and per-run Secret;
// Argo lifecycle pods only observe and consume their deterministic names.
func NewProducer(c client.Client, store artifacts.Store, uri URIResolver, workflowTemplate, workflowTemplateUID, workflowTemplateDigest string) (*Producer, error) {
	if workflowTemplate == "" || len(validation.IsDNS1123Subdomain(workflowTemplate)) != 0 || !validRunUID(workflowTemplateUID) || !canonical.ValidDigest(workflowTemplateDigest) {
		return nil, ErrProducerUnavailable
	}
	return &Producer{client: c, store: store, uri: uri, workflowTemplate: workflowTemplate, workflowTemplateUID: workflowTemplateUID, workflowTemplateDigest: workflowTemplateDigest}, nil
}

type PrepareResult struct {
	Sandbox sandbox.SandboxRef
	Claim   string
	Secret  string
	Timeout string
}

// Prepare observes the exact operator-owned Agent Sandbox. It never creates or
// mutates a Sandbox and never reads a Secret. The returned Secret is only the
// deterministic name that later token-bearing phases were given by Argo.
func (p *Producer) Prepare(ctx context.Context, request Request) (PrepareResult, error) {
	if p == nil || p.client == nil {
		return PrepareResult{}, ErrProducerUnavailable
	}
	if err := p.validateTemplateRequest(request); err != nil {
		return PrepareResult{}, err
	}
	run, err := p.loadRun(ctx, request)
	if err != nil {
		return PrepareResult{}, err
	}
	if _, err := runTimeout(run.Spec.Limits.Timeout); err != nil {
		return PrepareResult{}, fmt.Errorf("validate AgentRun timeout: %w", err)
	}
	name, err := sandbox.ChildName(types.UID(request.RunUID), sandbox.RoleWork)
	if err != nil {
		return PrepareResult{}, err
	}
	work := &sandboxv1beta1.Sandbox{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: name}, work); err != nil {
		if apierrors.IsNotFound(err) {
			return PrepareResult{}, fmt.Errorf("%w: operator-owned work Sandbox is missing", ErrProducerWorkflow)
		}
		return PrepareResult{}, fmt.Errorf("get operator-owned work Sandbox: %w", err)
	}
	if err := validateCleanupSandbox(work, request); err != nil {
		return PrepareResult{}, err
	}
	if work.Spec.ShutdownPolicy == nil || *work.Spec.ShutdownPolicy != sandboxv1beta1.ShutdownPolicyRetain {
		return PrepareResult{}, fmt.Errorf("%w: operator-owned work Sandbox is not retained", ErrProducerWorkflow)
	}
	claim, err := workload.WorkspaceClaimName(name)
	if err != nil {
		return PrepareResult{}, err
	}
	secret := workload.WorkSecretName(request.RunUID)
	return PrepareResult{Sandbox: sandbox.SandboxRef{
		Namespace: work.Namespace, Name: work.Name, Kind: sandbox.ChildKindSandbox,
		UID: work.UID, OwnerUID: types.UID(request.RunUID), Role: sandbox.RoleWork,
		SpecDigest: request.SpecDigest, PlanFingerprint: work.Annotations[sandbox.SandboxSpecFingerprintAnnotationKey],
	}, Claim: claim, Secret: secret, Timeout: run.Spec.Limits.Timeout}, nil
}

// Stage writes only trusted, immutable helper inputs.  Handoff reloads the
// resolved snapshot from object storage, so these files are convenience
// inputs for the capture image rather than an authority that the agent can
// rewrite into a lifecycle result.
func (p *Producer) Stage(ctx context.Context, request Request, workspace string) error {
	if p == nil || p.store == nil {
		return ErrProducerUnavailable
	}
	if err := p.validateTemplateRequest(request); err != nil {
		return err
	}
	if err := validateWorkspace(workspace); err != nil {
		return err
	}
	snapshot, canonicalSnapshot, err := p.loadSnapshot(ctx, request)
	if err != nil {
		return err
	}
	spec, err := captureExecutionSpec(snapshot, request.SpecDigest, request.BaseSHA)
	if err != nil {
		return err
	}
	specBody, err := strictJSON(spec)
	if err != nil || len(specBody) > maxStageSpecBytes {
		return fmt.Errorf("%w: capture specification exceeds bound", ErrProducerInvalid)
	}
	root := filepath.Join(workspace, ".agw")
	if err := ensureDirectory(root); err != nil {
		return err
	}
	if err := writeImmutableFile(filepath.Join(root, filepath.Base(ResolvedPath)), canonicalSnapshot, maxStageSnapshotBytes); err != nil {
		return err
	}
	if err := writeImmutableFile(filepath.Join(root, filepath.Base(CaptureSpecPath)), specBody, maxStageSpecBytes); err != nil {
		return err
	}
	return nil
}

// Handoff authenticates capture output, runtime completion, the completion
// record, and the live Workflow identity before constructing the one reserved
// canonical output.  It never reads a lifecycle JSON file from the workspace
// and never accepts a Gate/effect/publication/terminal-success claim.
func (p *Producer) Handoff(ctx context.Context, request Request, workspace string) (BoundLifecycleOutput, error) {
	var zero BoundLifecycleOutput
	if p == nil || p.client == nil || p.store == nil || p.uri == nil {
		return zero, ErrProducerUnavailable
	}
	if err := validateWorkspace(workspace); err != nil {
		return zero, err
	}
	if err := p.validateTemplateRequest(request); err != nil {
		return zero, err
	}
	run, snapshot, _, binding, workflow, err := p.load(ctx, request)
	if err != nil {
		return zero, err
	}
	if run == nil || workflow == nil || binding.Reference.UID == "" || binding.Reference.Generation <= 0 || request.SandboxUID == "" {
		return zero, ErrProducerWorkflow
	}
	if err := p.validatePreparedSandbox(ctx, request); err != nil {
		return zero, err
	}
	if err := validateSandboxMarkers(workspace, request); err != nil {
		return zero, err
	}
	limits, err := captureLimits(snapshot)
	if err != nil {
		return zero, err
	}
	resultJSON, err := readRegularFile(filepath.Join(workspace, ".agw", "capture", "result.json"), limits.MaxResultBytes)
	if err != nil {
		return zero, err
	}
	patch, err := readRegularFile(filepath.Join(workspace, ".agw", "capture", "patch.diff"), limits.MaxPatchBytes)
	if err != nil {
		return zero, err
	}
	manifest, err := readRegularFile(filepath.Join(workspace, ".agw", "capture", "patch-manifest.json"), limits.MaxManifestBytes)
	if err != nil {
		return zero, err
	}
	validated, err := capture.Validate(snapshot, request.BaseSHA, capture.CapturedOutput{ResultJSON: resultJSON, Patch: patch, Manifest: manifest}, limits)
	if err != nil {
		return zero, fmt.Errorf("validate capture output: %w", err)
	}
	captured, err := capture.Persist(ctx, p.store, validated)
	if err != nil {
		return zero, fmt.Errorf("persist capture artifacts: %w", err)
	}

	runtimeProof, err := p.runtimeProof(ctx, request)
	if err != nil {
		return zero, err
	}
	verificationRef, err := p.persistVerificationInput(ctx, request, captured.Patch, captured.Manifest)
	if err != nil {
		return zero, err
	}
	output := LifecycleOutput{
		SchemaVersion:          LifecycleOutputSchemaVersion,
		Kind:                   LifecycleOutputKind,
		RunUID:                 request.RunUID,
		RunGeneration:          request.RunGeneration,
		SpecDigest:             request.SpecDigest,
		WorkflowTemplateUID:    request.WorkflowTemplateUID,
		WorkflowTemplateDigest: request.WorkflowTemplateDigest,
		WorkflowUID:            string(binding.Reference.UID),
		WorkflowGeneration:     binding.Reference.Generation,
		BaseSHA:                request.BaseSHA,
		Patch: LifecyclePatchOutput{
			Ref: captured.Patch, ManifestRef: captured.Manifest, Digest: validated.Envelope.PatchDigest,
			FilesChanged: validated.Envelope.FilesChanged, LinesChanged: validated.Envelope.LinesChanged,
		},
		Runtime:      runtimeProof,
		Verification: VerificationInputs{InputRef: verificationRef, PatchRef: captured.Patch, BaseSHA: request.BaseSHA, SpecDigest: request.SpecDigest},
	}
	canonicalOutput, err := MarshalLifecycleOutput(output)
	if err != nil {
		return zero, err
	}
	digest := digestBytes(canonicalOutput)
	return BoundLifecycleOutput{Contract: output, Canonical: canonicalOutput, Digest: digest}, nil
}

func (p *Producer) validatePreparedSandbox(ctx context.Context, request Request) error {
	name, err := sandbox.ChildName(types.UID(request.RunUID), sandbox.RoleWork)
	if err != nil {
		return err
	}
	work := &sandboxv1beta1.Sandbox{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: name}, work); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: prepared work Sandbox is missing", ErrProducerWorkflow)
		}
		return fmt.Errorf("get prepared work Sandbox: %w", err)
	}
	if err := validateCleanupSandbox(work, request); err != nil {
		return err
	}
	if string(work.UID) != request.SandboxUID || work.Spec.ShutdownPolicy == nil || *work.Spec.ShutdownPolicy != sandboxv1beta1.ShutdownPolicyRetain {
		return fmt.Errorf("%w: prepared work Sandbox UID or retention policy does not match", ErrProducerWorkflow)
	}
	return nil
}

func validateSandboxMarkers(workspace string, request Request) error {
	name, err := sandbox.ChildName(types.UID(request.RunUID), sandbox.RoleWork)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		path      string
		condition string
	}{
		{path: ReadyMarkerPath, condition: "Ready"},
		{path: FinishedMarkerPath, condition: "Finished"},
	} {
		body, err := readRegularFile(filepath.Join(workspace, ".agw", filepath.Base(item.path)), 4096)
		if err != nil {
			return err
		}
		var marker struct {
			Version   string `json:"version"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			UID       string `json:"uid"`
			Condition string `json:"condition"`
			Observed  string `json:"observedAt"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&marker); err != nil {
			return fmt.Errorf("%w: decode Sandbox marker: %v", ErrProducerFile, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("%w: Sandbox marker contains trailing JSON", ErrProducerFile)
		}
		if marker.Version != "agents.astatide.com/phase0-marker/v1" || marker.Namespace != request.Namespace || marker.Name != name || marker.UID != request.SandboxUID || marker.Condition != item.condition || marker.Observed == "" {
			return fmt.Errorf("%w: Sandbox marker does not identify prepared UID", ErrProducerWorkflow)
		}
		if _, err := time.Parse(time.RFC3339Nano, marker.Observed); err != nil {
			return fmt.Errorf("%w: Sandbox marker timestamp is invalid", ErrProducerFile)
		}
	}
	return nil
}

func (p *Producer) load(ctx context.Context, request Request) (*v1alpha1.AgentRun, resolved.Snapshot, []byte, Binding, *unstructured.Unstructured, error) {
	if p == nil || p.client == nil {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, ErrProducerUnavailable
	}
	if err := validateRequest(request); err != nil {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, err
	}
	run := &v1alpha1.AgentRun{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.RunName}, run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, resolved.Snapshot{}, nil, Binding{}, nil, fmt.Errorf("%w: AgentRun is missing", ErrProducerInvalid)
		}
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, fmt.Errorf("get AgentRun: %w", err)
	}
	if string(run.UID) != request.RunUID || run.Generation != request.RunGeneration || run.Status.SpecDigest != request.SpecDigest || run.Status.BaseSHA != request.BaseSHA || run.Status.ResolvedSpecRef == nil || run.Status.ResolvedSpecRef.Digest != request.SpecDigest {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, ErrProducerInvalid
	}
	snapshot, body, err := p.loadSnapshot(ctx, request)
	if err != nil {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, err
	}
	translation, err := Translate(Inputs{Run: run, Snapshot: snapshot, ResolvedRef: *run.Status.ResolvedSpecRef, Config: Config{Namespace: request.Namespace, WorkflowTemplateName: p.workflowTemplate, WorkflowTemplateUID: p.workflowTemplateUID, WorkflowTemplateDigest: p.workflowTemplateDigest}})
	if err != nil {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, err
	}
	workflow := workflowObject()
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: translation.Binding.Reference.Name}, workflow); err != nil {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, fmt.Errorf("get lifecycle Workflow: %w", err)
	}
	binding, err := Bind(workflow, translation.Binding)
	if err != nil {
		return nil, resolved.Snapshot{}, nil, Binding{}, nil, fmt.Errorf("bind lifecycle Workflow: %w", err)
	}
	return run, snapshot, body, binding, workflow, nil
}

func (p *Producer) loadSnapshot(ctx context.Context, request Request) (resolved.Snapshot, []byte, error) {
	if p == nil || p.store == nil {
		return resolved.Snapshot{}, nil, ErrProducerUnavailable
	}
	if err := validateRequest(request); err != nil {
		return resolved.Snapshot{}, nil, err
	}
	writer, err := artifacts.NewWriter(p.store)
	if err != nil {
		return resolved.Snapshot{}, nil, err
	}
	body, err := writer.LoadResolvedSpec(ctx, request.RunUID, request.SpecDigest)
	if err != nil {
		return resolved.Snapshot{}, nil, fmt.Errorf("load resolved spec: %w", err)
	}
	snapshot, err := resolved.Decode(body, request.SpecDigest)
	if err != nil || snapshot.Run.Namespace != request.Namespace || snapshot.Run.Name != request.RunName || snapshot.Run.UID != request.RunUID || snapshot.Run.Generation != request.RunGeneration || snapshot.BaseSHA != request.BaseSHA {
		return resolved.Snapshot{}, nil, ErrProducerInvalid
	}
	return snapshot, append([]byte(nil), body...), nil
}

func (p *Producer) runtimeProof(ctx context.Context, request Request) (RuntimeCompletionProof, error) {
	repository, err := runtimeevents.NewRepository(p.store)
	if err != nil {
		return RuntimeCompletionProof{}, err
	}
	record, err := repository.LoadCompletion(ctx, request.RunUID, request.SpecDigest)
	if err != nil {
		return RuntimeCompletionProof{}, fmt.Errorf("load runtime completion: %w", err)
	}
	if record.RunUID != request.RunUID || record.SpecDigest != request.SpecDigest || record.BaseSHA != request.BaseSHA || record.TerminalType != runtimeevents.TerminalCompleted || record.TerminalSeq == 0 {
		return RuntimeCompletionProof{}, ErrProducerTerminal
	}
	terminal := &terminalLineWriter{target: record.TerminalSeq}
	stream, err := repository.WriteJSONL(ctx, request.RunUID, record.EventStream, terminal)
	if err != nil {
		return RuntimeCompletionProof{}, fmt.Errorf("replay runtime event stream: %w", err)
	}
	if err := terminal.Close(); err != nil || stream.RunUID != request.RunUID || stream.SpecDigest != request.SpecDigest || stream.TerminalType != runtimeevents.TerminalCompleted || stream.TerminalSeq != record.TerminalSeq || terminal.digest == "" {
		return RuntimeCompletionProof{}, ErrProducerArtifact
	}
	completionKey, err := runtimeevents.CompletionKey(request.RunUID, request.SpecDigest)
	if err != nil {
		return RuntimeCompletionProof{}, err
	}
	completionURI, err := p.uri.URI(completionKey)
	if err != nil {
		return RuntimeCompletionProof{}, fmt.Errorf("resolve runtime completion URI: %w", err)
	}
	completionBody, err := strictJSON(record)
	if err != nil || len(completionBody) > runtimeevents.MaxCompletionBytes {
		return RuntimeCompletionProof{}, ErrProducerArtifact
	}
	stored, err := p.store.Get(ctx, completionKey)
	if err != nil || !bytes.Equal(stored, completionBody) {
		return RuntimeCompletionProof{}, ErrProducerArtifact
	}
	completionRef := v1alpha1.ArtifactRef{URI: completionURI, Digest: digestBytes(completionBody), Kind: "runtime-completion", Name: "completion.json", MediaType: "application/json", SizeBytes: int64(len(completionBody))}
	eventRef := v1alpha1.ArtifactRef{URI: record.EventStream.URI, Digest: record.EventStream.Digest, Kind: record.EventStream.Kind, Name: record.EventStream.Name, MediaType: record.EventStream.MediaType, SizeBytes: record.EventStream.SizeBytes}
	return RuntimeCompletionProof{EventStreamRef: eventRef, CompletionRef: completionRef, TerminalEvent: string(record.TerminalType), TerminalSequence: record.TerminalSeq, TerminalEventDigest: terminal.digest, CompletionDigest: completionRef.Digest}, nil
}

func (p *Producer) persistVerificationInput(ctx context.Context, request Request, patch, manifest v1alpha1.ArtifactRef) (v1alpha1.ArtifactRef, error) {
	input := verificationInput{SchemaVersion: 1, Kind: "agents.astatide.com/verification-input", RunUID: request.RunUID, RunGeneration: request.RunGeneration, SpecDigest: request.SpecDigest, BaseSHA: request.BaseSHA, Patch: patch, Manifest: manifest}
	body, err := strictJSON(input)
	if err != nil || len(body) > maxStageSpecBytes {
		return v1alpha1.ArtifactRef{}, ErrProducerArtifact
	}
	digest := digestBytes(body)
	key := "runs/" + request.RunUID + "/verification/" + trimDigest(request.SpecDigest) + "/input/" + trimDigest(digest) + ".json"
	uri, err := p.putImmutable(ctx, key, body, VerificationInputMediaType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	return v1alpha1.ArtifactRef{URI: uri, Digest: digest, Kind: VerificationInputKind, Name: VerificationInputName, MediaType: VerificationInputMediaType, SizeBytes: int64(len(body))}, nil
}

func (p *Producer) putImmutable(ctx context.Context, key string, body []byte, contentType string) (string, error) {
	created, uri, err := p.store.Put(ctx, key, append([]byte(nil), body...), contentType)
	if err != nil {
		return "", fmt.Errorf("put immutable lifecycle artifact: %w", err)
	}
	if !created {
		existing, getErr := p.store.Get(ctx, key)
		if getErr != nil {
			return "", fmt.Errorf("verify immutable lifecycle artifact: %w", getErr)
		}
		if !bytes.Equal(existing, body) {
			return "", ErrProducerConflict
		}
	}
	if uri == "" || strings.ContainsAny(uri, "\x00\r\n@?#") {
		return "", ErrProducerArtifact
	}
	return uri, nil
}

type verificationInput struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Kind          string               `json:"kind"`
	RunUID        string               `json:"runUID"`
	RunGeneration int64                `json:"runGeneration"`
	SpecDigest    string               `json:"specDigest"`
	BaseSHA       string               `json:"baseSHA"`
	Patch         v1alpha1.ArtifactRef `json:"patch"`
	Manifest      v1alpha1.ArtifactRef `json:"manifest"`
}

type captureSpecContract struct {
	Version            int                       `json:"version"`
	Protocol           string                    `json:"protocol"`
	ResolvedSpecDigest string                    `json:"resolvedSpecDigest"`
	BaseSHA            string                    `json:"baseSHA"`
	RepoPath           string                    `json:"repoPath"`
	OutputDir          string                    `json:"outputDir"`
	PatchPath          string                    `json:"patchPath"`
	ManifestPath       string                    `json:"manifestPath"`
	ResultPath         string                    `json:"resultPath"`
	Scope              v1alpha1.ScopeSpec        `json:"scope"`
	Requirements       v1alpha1.GateRequirements `json:"requirements"`
	MaxPatchBytes      int64                     `json:"maxPatchBytes"`
	MaxResultBytes     int64                     `json:"maxResultBytes"`
	MaxManifestBytes   int64                     `json:"maxManifestBytes"`
}

func captureExecutionSpec(snapshot resolved.Snapshot, specDigest, baseSHA string) (captureSpecContract, error) {
	limits, err := captureLimits(snapshot)
	if err != nil {
		return captureSpecContract{}, err
	}
	if specDigest == "" || baseSHA == "" || snapshot.BaseSHA != baseSHA {
		return captureSpecContract{}, ErrProducerInvalid
	}
	return captureSpecContract{Version: 2, Protocol: capture.OutputProtocol, ResolvedSpecDigest: specDigest, BaseSHA: baseSHA, RepoPath: capture.RepoPath, OutputDir: capture.OutputDir, PatchPath: capture.PatchPath, ManifestPath: capture.ManifestPath, ResultPath: capture.ResultPath, Scope: snapshot.Spec.Scope, Requirements: snapshot.Gate.Require, MaxPatchBytes: limits.MaxPatchBytes, MaxResultBytes: limits.MaxResultBytes, MaxManifestBytes: limits.MaxManifestBytes}, nil
}

func captureLimits(snapshot resolved.Snapshot) (capture.ValidationLimits, error) {
	maxPatch := snapshot.Spec.Limits.MaxPatchBytes
	if maxPatch == 0 {
		maxPatch = capture.DefaultMaxPatchBytes
	}
	if maxPatch > capture.HardMaxPatchBytes || maxPatch <= 0 {
		return capture.ValidationLimits{}, ErrProducerInvalid
	}
	maxResult := capture.DefaultMaxResultBytes
	maxManifest := capture.DefaultMaxManifestBytes
	return capture.ValidationLimits{MaxPatchBytes: maxPatch, MaxResultBytes: maxResult, MaxManifestBytes: maxManifest}, nil
}

func validateRequest(request Request) error {
	if request.Namespace == "" || len(validation.IsDNS1123Label(request.Namespace)) != 0 || request.RunName == "" || len(validation.IsDNS1123Subdomain(request.RunName)) != 0 || request.RunUID == "" || len(validation.IsValidLabelValue(request.RunUID)) != 0 || request.RunGeneration <= 0 || !canonical.ValidDigest(request.SpecDigest) || !resolved.ValidBaseSHA(request.BaseSHA) || request.WorkflowTemplateName == "" || len(validation.IsDNS1123Subdomain(request.WorkflowTemplateName)) != 0 || !validRunUID(request.WorkflowTemplateUID) || !canonical.ValidDigest(request.WorkflowTemplateDigest) {
		return ErrProducerInvalid
	}
	if request.SandboxUID != "" && !validRunUID(request.SandboxUID) {
		return ErrProducerInvalid
	}
	return nil
}

func (p *Producer) validateTemplateRequest(request Request) error {
	if p == nil || request.WorkflowTemplateName != p.workflowTemplate || request.WorkflowTemplateUID != p.workflowTemplateUID || request.WorkflowTemplateDigest != p.workflowTemplateDigest {
		return ErrProducerWorkflow
	}
	return validateRequest(request)
}

func (p *Producer) loadRun(ctx context.Context, request Request) (*v1alpha1.AgentRun, error) {
	if p == nil || p.client == nil {
		return nil, ErrProducerUnavailable
	}
	if err := p.validateTemplateRequest(request); err != nil {
		return nil, err
	}
	run := &v1alpha1.AgentRun{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.RunName}, run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: AgentRun is missing", ErrProducerInvalid)
		}
		return nil, fmt.Errorf("get AgentRun: %w", err)
	}
	if string(run.UID) != request.RunUID || run.Generation != request.RunGeneration || run.Status.SpecDigest != request.SpecDigest || run.Status.BaseSHA != request.BaseSHA || run.Status.ResolvedSpecRef == nil || run.Status.ResolvedSpecRef.Digest != request.SpecDigest || !admitted(run) {
		return nil, ErrProducerInvalid
	}
	return run, nil
}

func runTimeout(value string) (time.Duration, error) {
	if value == "" || len(value) > 32 || strings.TrimSpace(value) != value {
		return 0, ErrProducerInvalid
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, ErrProducerInvalid
	}
	return duration, nil
}

func validateWorkspace(path string) error {
	if path == "" || path != filepath.Clean(path) || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return ErrProducerFile
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ErrProducerFile
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("%w: create stage directory: %v", ErrProducerFile, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrProducerFile
	}
	return nil
}

func writeImmutableFile(path string, body []byte, max int) error {
	if len(body) == 0 || len(body) > max || path == filepath.Clean(filepath.Dir(path)) {
		return ErrProducerFile
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrProducerFile
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existing, body) {
			return ErrProducerConflict
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrProducerFile
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrProducerConflict
		}
		return ErrProducerFile
	}
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return ErrProducerFile
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return ErrProducerFile
	}
	return nil
}

func readRegularFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > max {
		return nil, ErrProducerFile
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrProducerFile
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(body)) > max {
		return nil, ErrProducerFile
	}
	return body, nil
}

func strictJSON(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return strictjson.Normalize(body)
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func trimDigest(value string) string { return strings.TrimPrefix(value, canonical.DigestPrefix) }

type terminalLineWriter struct {
	target  uint64
	seq     uint64
	pending []byte
	digest  string
}

func (w *terminalLineWriter) Write(body []byte) (int, error) {
	if w == nil || len(body) == 0 || len(w.pending)+len(body) > runtimeproto.MaxFrameBytes+1 {
		return 0, ErrProducerArtifact
	}
	w.pending = append(w.pending, body...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), w.pending[:index+1]...)
		w.pending = w.pending[index+1:]
		w.seq++
		frame, err := runtimeproto.ParseLine(line)
		if err != nil || frame.Kind != proto.KindEvent || frame.Seq != w.seq {
			return 0, ErrProducerArtifact
		}
		if w.seq == w.target {
			w.digest = digestBytes(line)
		}
	}
	return len(body), nil
}

func (w *terminalLineWriter) Close() error {
	if w == nil || len(w.pending) != 0 || w.seq < w.target || w.digest == "" {
		return ErrProducerArtifact
	}
	return nil
}

var _ URIResolver = interface{ URI(string) (string, error) }(nil)
