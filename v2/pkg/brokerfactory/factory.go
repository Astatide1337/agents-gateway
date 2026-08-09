// Package brokerfactory resolves immutable control-plane resources into one
// short-lived host-side handler for a sandbox run. Secrets are resolved only
// at the final provider boundary and are never serialized into client.json.
package brokerfactory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerbridge"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/modelbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/skills"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

const (
	defaultOpenAIResponsesURL     = "https://api.openai.com/v1/responses"
	defaultOpenRouterResponsesURL = "https://openrouter.ai/api/v1/responses"
)

var environmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// Factory is deliberately small enough for the owner-operated profile. A
// distributed deployment can replace it without changing sandbox protocol.
type Factory struct {
	Store         store.Store
	Artifacts     *artifact.Store
	Effects       toolbroker.EffectLedger
	LookupEnv     func(string) (string, bool)
	OpenAIURL     string
	OpenRouterURL string
	Skills        *skills.GatewayClient
}

func (f *Factory) NewHandler(ctx context.Context, request brokerdispatch.HandlerRequest) (brokerdispatch.HandlerSpec, error) {
	if f == nil || f.Store == nil || f.Artifacts == nil || f.Effects == nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupFactory)
	}
	if request.Input.Contract.ModelRoute == nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupModelRoute)
	}
	scope := store.Scope{OrganizationID: request.Binding.OrgID, ProjectID: request.Binding.ProjectID}
	modelRoute, err := resolveResource[*spec.ModelRoute](ctx, f.Store, scope, *request.Input.Contract.ModelRoute)
	if err != nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupModelRoute)
	}
	if len(modelRoute.Spec.Providers) != 1 {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupModelRoute)
	}
	provider := modelRoute.Spec.Providers[0]
	upstream := ""
	switch provider.Kind {
	case "openai", "openai-responses":
		upstream = strings.TrimSpace(f.OpenAIURL)
		if upstream == "" {
			upstream = defaultOpenAIResponsesURL
		}
	case "openrouter", "openrouter-responses":
		upstream = strings.TrimSpace(f.OpenRouterURL)
		if upstream == "" {
			upstream = defaultOpenRouterResponsesURL
		}
	default:
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupModelRoute)
	}
	if provider.Credential == "" {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupModelRoute)
	}
	credentials := &credentialResolver{store: f.Store, scope: scope, lookup: f.LookupEnv}
	responses, err := modelbroker.NewResponsesProxy(modelbroker.ResponsesProxyConfig{
		UpstreamURL:   upstream,
		AllowedModels: []string{provider.Model},
		Credential: func(ctx context.Context) ([]byte, error) {
			return credentials.ResolveCredential(ctx, scope.OrganizationID, provider.Credential)
		},
	})
	if err != nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupModelBoundary)
	}

	mux := http.NewServeMux()
	mux.Handle(brokerbridge.ResponsesPath, responses)
	if request.Input.Contract.ToolSet != nil {
		toolSet, resolveErr := resolveResource[*spec.ToolSet](ctx, f.Store, scope, *request.Input.Contract.ToolSet)
		if resolveErr != nil {
			return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupToolSet)
		}
		mcp, mcpErr := f.mcpHandler(scope, request.Binding, toolSet, credentials)
		if mcpErr != nil {
			return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupMCPBoundary)
		}
		mux.Handle(brokerbridge.MCPPath, mcp)
	}
	upload, err := artifact.NewUploadHandler(f.Artifacts, artifact.UploadHandlerConfig{
		OrganizationID:    request.Binding.OrgID,
		ProjectID:         request.Binding.ProjectID,
		RunID:             request.Binding.RunID,
		Name:              "output.json",
		Path:              brokerbridge.ArtifactPath,
		Method:            http.MethodPut,
		MaxBytes:          8 << 20,
		AllowedMediaTypes: []string{"application/vnd.agw.run-output+json"},
		OnStored: func(ctx context.Context, metadata artifact.Metadata, _ string) error {
			return f.publishArtifact(ctx, scope, request.Binding.RunID, metadata, artifactcatalog.PublishInput{
				ArtifactID: metadata.ID, VersionID: metadata.ID, Title: metadata.Name,
				Description: "Immutable output from run " + request.Binding.RunID + ".",
				URI:         "artifact://catalog/" + metadata.ID + "/" + metadata.ID,
				Digest:      metadata.Digest, MediaType: metadata.MediaType,
				SizeBytes: metadata.SizeBytes, CreatedAt: time.Now().UTC(),
			})
		},
	})
	if err != nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupArtifact)
	}
	mux.Handle(brokerbridge.ArtifactPath, upload)
	publish, err := artifact.NewPublishHandler(f.Artifacts, artifact.PublishHandlerConfig{
		OrganizationID: request.Binding.OrgID, ProjectID: request.Binding.ProjectID, RunID: request.Binding.RunID,
		Path: brokerbridge.ArtifactCreatePath, MaxBytes: 8 << 20,
		OnStored: func(ctx context.Context, metadata artifact.Metadata, _ string, descriptor artifact.PublishDescriptor) error {
			return f.publishArtifact(ctx, scope, request.Binding.RunID, metadata, artifactcatalog.PublishInput{
				ArtifactID: metadata.ID, VersionID: metadata.ID, Title: descriptor.Title, Description: descriptor.Description,
				URI:    "artifact://catalog/" + metadata.ID + "/" + metadata.ID,
				Digest: metadata.Digest, MediaType: metadata.MediaType, SizeBytes: metadata.SizeBytes, CreatedAt: time.Now().UTC(),
				ContentKind: descriptor.ContentKind,
				Security:    artifactcatalog.SecurityPolicy{Allow: append([]artifactcatalog.Capability(nil), descriptor.Capabilities...)},
			})
		},
	})
	if err != nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupArtifact)
	}
	mux.Handle(brokerbridge.ArtifactCreatePath, publish)
	prepare, err := f.skillsPreparer(ctx, scope, request.Input.Contract)
	if err != nil {
		return brokerdispatch.HandlerSpec{}, brokerdispatch.NewHandlerSetupError(brokerdispatch.HandlerSetupSkills)
	}
	return brokerdispatch.HandlerSpec{
		Handler:        mux,
		AllowedModel:   provider.Model,
		PolicyDigest:   policyDigest(request.Input.Contract),
		TTL:            brokerTTL(request.Input.Execution.Resources.Timeout),
		PrepareSandbox: prepare,
	}, nil
}

func (f *Factory) publishArtifact(ctx context.Context, scope store.Scope, runID string, metadata artifact.Metadata, input artifactcatalog.PublishInput) error {
	catalog, ok := f.Store.(store.ArtifactCatalog)
	if !ok {
		return errors.New("artifact catalog is unavailable")
	}
	version, err := artifactcatalog.NewRunOutput(input)
	if err != nil {
		return err
	}
	document, err := json.Marshal(version)
	if err != nil {
		return err
	}
	_, err = catalog.PutArtifactVersion(ctx, store.ArtifactVersion{
		Scope: scope, ArtifactID: version.ArtifactID, VersionID: version.VersionID,
		RunID: runID, VersionNumber: 1, Document: document,
		ContentObjectKey: metadata.ObjectKey, SourceObjectKey: metadata.ObjectKey,
	})
	return err
}

type skillReference struct{ id, digest string }

func (f *Factory) skillsPreparer(ctx context.Context, scope store.Scope, contract workflow.ExecutionContract) (func(context.Context, string) error, error) {
	references := make([]skillReference, 0, len(contract.InlineSkills))
	for _, reference := range contract.InlineSkills {
		references = append(references, skillReference{id: reference.Ref, digest: reference.Digest})
	}
	if contract.SkillSet != nil {
		set, err := resolveResource[*spec.SkillSet](ctx, f.Store, scope, *contract.SkillSet)
		if err != nil {
			return nil, fmt.Errorf("resolve skill set: %w", err)
		}
		for _, reference := range set.Spec.Skills {
			references = append(references, skillReference{id: reference.Ref, digest: reference.Digest})
		}
	}
	if len(references) == 0 {
		return nil, nil
	}
	if f.Skills == nil {
		return nil, errors.New("agent references skills but Skills Gateway is not configured")
	}
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		key := reference.id + "\x00" + reference.digest
		if _, exists := seen[key]; exists {
			return nil, errors.New("agent contains a duplicate skill reference")
		}
		seen[key] = struct{}{}
	}
	return func(prepareCtx context.Context, sessionDirectory string) error {
		root := filepath.Join(sessionDirectory, "skills")
		if err := os.Mkdir(root, 0700); err != nil {
			return errors.New("create skills staging root")
		}
		ok := false
		defer func() {
			if !ok {
				_ = makeTreeRemovable(root)
				_ = os.RemoveAll(root)
			}
		}()
		for index, reference := range references {
			digest := sha256.Sum256([]byte(reference.id + "\x00" + reference.digest))
			destination := filepath.Join(root, fmt.Sprintf("%04d-%s", index+1, hex.EncodeToString(digest[:8])))
			if _, err := f.Skills.Materialize(prepareCtx, reference.id, reference.digest, destination); err != nil {
				return errors.New("materialize immutable skill")
			}
		}
		if err := os.Chmod(root, 0555); err != nil {
			return errors.New("protect skills staging root")
		}
		ok = true
		return nil
	}, nil
}

func makeTreeRemovable(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0700)
		}
		return nil
	})
}

func brokerTTL(runTimeout time.Duration) time.Duration {
	if runTimeout <= 0 {
		runTimeout = 30 * time.Minute
	}
	ttl := runTimeout + 5*time.Minute
	if ttl > 24*time.Hour+5*time.Minute {
		return 24*time.Hour + 5*time.Minute
	}
	return ttl
}

func (f *Factory) mcpHandler(scope store.Scope, binding runbroker.SessionBinding, set *spec.ToolSet, credentials *credentialResolver) (http.Handler, error) {
	grants := make([]toolpolicy.Grant, 0)
	exposed := make([]toolbroker.ExposedTool, 0)
	servers := make(staticServers)
	seenTools := make(map[string]struct{})
	for _, server := range set.Spec.Servers {
		if _, exists := servers[server.Name]; exists {
			return nil, errors.New("tool set contains a duplicate server")
		}
		servers[server.Name] = toolbroker.Server{Name: server.Name, Endpoint: server.Ref, CredentialRef: server.Credentials}
		for _, grant := range server.Tools {
			if _, exists := seenTools[grant.Name]; exists {
				return nil, errors.New("tool names must be unique across the run catalog")
			}
			seenTools[grant.Name] = struct{}{}
			if len(grant.Resources) > 1 || (len(grant.Resources) == 1 && strings.ContainsAny(grant.Resources[0], "*?[")) {
				return nil, errors.New("MCP adapter currently requires one exact resource or no resource per tool")
			}
			resource := ""
			if len(grant.Resources) == 1 {
				resource = grant.Resources[0]
			}
			effect := toolpolicy.Effect(grant.Effect)
			if effect == "" || effect == toolpolicy.EffectUnknown {
				effect = toolpolicy.EffectWrite
			}
			approval := toolpolicy.ApprovalMode(grant.Approval)
			if approval == "" {
				if effect == toolpolicy.EffectRead {
					approval = toolpolicy.ApprovalAllow
				} else {
					approval = toolpolicy.ApprovalRequired
				}
			}
			schema := grant.InputSchema
			if len(schema) == 0 {
				schema = map[string]any{"type": "object", "additionalProperties": true}
			}
			rawSchema, err := json.Marshal(schema)
			if err != nil {
				return nil, errors.New("tool schema is not JSON serializable")
			}
			grants = append(grants, toolpolicy.Grant{Server: server.Name, Tool: grant.Name, Resources: exactResources(resource), Effect: effect, Approval: approval})
			exposed = append(exposed, toolbroker.ExposedTool{Name: grant.Name, InputSchema: rawSchema, Server: server.Name, Resource: resource, Effect: effect})
		}
	}
	broker, err := toolbroker.New(staticPolicy(grants), credentials, servers, f.Effects, storeAuditSink{storage: f.Store}, nil)
	if err != nil {
		return nil, errors.New("configure MCP broker")
	}
	return toolbroker.NewMCPHandler(broker, toolbroker.MCPHandlerConfig{
		OrganizationID: scope.OrganizationID,
		ProjectID:      scope.ProjectID,
		UserID:         binding.UserID,
		RunID:          binding.RunID,
		Tools:          exposed,
	})
}

type credentialResolver struct {
	store  store.Store
	scope  store.Scope
	lookup func(string) (string, bool)
}

func (r *credentialResolver) ResolveCredential(ctx context.Context, organizationID, name string) ([]byte, error) {
	if organizationID != r.scope.OrganizationID {
		return nil, errors.New("credential scope mismatch")
	}
	if name == "" {
		return []byte{}, nil
	}
	resource, err := r.store.GetResource(ctx, r.scope, spec.KindCredential, name)
	if err != nil {
		return nil, errors.New("credential reference was not found")
	}
	decoded, err := spec.Decode(resource.Document)
	if err != nil || len(decoded) != 1 {
		return nil, errors.New("credential resource is invalid")
	}
	credential, ok := decoded[0].(*spec.Credential)
	if !ok || credential.Metadata.Name != name || !strings.HasPrefix(credential.Spec.SecretRef, "env://") {
		return nil, errors.New("credential secretRef must use env:// in standalone mode")
	}
	environment := strings.TrimPrefix(credential.Spec.SecretRef, "env://")
	if !environmentName.MatchString(environment) {
		return nil, errors.New("credential environment reference is invalid")
	}
	lookup := r.lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, exists := lookup(environment)
	if !exists || value == "" {
		return nil, errors.New("credential environment value is unavailable")
	}
	return []byte(value), nil
}

type staticPolicy []toolpolicy.Grant

func (p staticPolicy) Grants(context.Context, string, string, string) ([]toolpolicy.Grant, error) {
	return append([]toolpolicy.Grant(nil), p...), nil
}

type staticServers map[string]toolbroker.Server

func (s staticServers) Server(_ context.Context, _, _, name string) (toolbroker.Server, error) {
	server, ok := s[name]
	if !ok {
		return toolbroker.Server{}, errors.New("MCP server is outside the run catalog")
	}
	return server, nil
}

// storeAuditSink is the production adapter from the broker's privacy-bounded
// audit contract to the durable append-only control-plane audit stream.
type storeAuditSink struct{ storage store.Store }

func (s storeAuditSink) AppendAudit(ctx context.Context, record toolbroker.AuditRecord) error {
	metadata, err := json.Marshal(struct {
		RunID         string `json:"run_id"`
		Server        string `json:"server"`
		Tool          string `json:"tool"`
		Resource      string `json:"resource,omitempty"`
		Phase         string `json:"phase"`
		Outcome       string `json:"outcome"`
		RequestDigest string `json:"request_digest"`
		EffectDigest  string `json:"effect_digest,omitempty"`
	}{
		RunID: record.RunID, Server: record.Server, Tool: record.Tool, Resource: record.Resource,
		Phase: record.Phase, Outcome: record.Outcome, RequestDigest: record.RequestDigest, EffectDigest: record.EffectDigest,
	})
	if err != nil {
		return err
	}
	identity := sha256.Sum256([]byte(strings.Join([]string{record.Server, record.Tool, record.Resource}, "\x00")))
	return s.storage.AppendAudit(ctx, store.AuditEvent{
		Scope:        store.Scope{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID},
		PrincipalID:  record.UserID,
		Action:       "mcp.tool." + record.Phase,
		ResourceType: "mcp.tool",
		ResourceID:   "sha256:" + hex.EncodeToString(identity[:]),
		Decision:     record.Decision,
		Metadata:     metadata,
	})
}

func resolveResource[T spec.Resource](ctx context.Context, source store.Store, scope store.Scope, ref workflow.RevisionRef) (T, error) {
	var zero T
	resource, err := source.GetResource(ctx, scope, ref.Kind, ref.Name)
	if err != nil {
		return zero, err
	}
	if resource.Digest != ref.Digest {
		return zero, errors.New("resource revision digest changed")
	}
	decoded, err := spec.Decode(resource.Document)
	if err != nil || len(decoded) != 1 {
		return zero, errors.New("resource document is invalid")
	}
	typed, ok := decoded[0].(T)
	if !ok || typed.Meta().Metadata.Name != ref.Name || typed.Meta().Kind != ref.Kind {
		return zero, errors.New("resource identity does not match reference")
	}
	return typed, nil
}

func policyDigest(contract workflow.ExecutionContract) string {
	material := "agents-gateway.broker-policy.v1\x00" + contract.Agent.Digest + "\x00" + contract.SandboxProfile.Digest
	for _, ref := range []*workflow.RevisionRef{contract.ModelRoute, contract.ToolSet, contract.SkillSet} {
		if ref != nil {
			material += "\x00" + ref.Kind + "\x00" + ref.Name + "\x00" + ref.Digest
		}
	}
	digest := sha256.Sum256([]byte(material))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func exactResources(resource string) []string {
	if resource == "" {
		return nil
	}
	return []string{resource}
}
