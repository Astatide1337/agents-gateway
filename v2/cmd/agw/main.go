package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/compat"
	"github.com/Astatide1337/agents-gateway/v2/pkg/identity"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"gopkg.in/yaml.v3"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "validate":
		return validateCommand(args[1:], stdout, stderr)
	case "plan":
		return planCommand(args[1:], stdout, stderr)
	case "migrate-manifest":
		return migrateManifestCommand(args[1:], stdout, stderr)
	case "apply":
		return applyCommand(args[1:], stdout, stderr)
	case "run":
		return runCommand(args[1:], stdout, stderr)
	case "cancel":
		return cancelCommand(args[1:], stdout, stderr)
	case "approve":
		return approveCommand(args[1:], stdout, stderr)
	case "reply":
		return replyCommand(args[1:], stdout, stderr)
	case "generate-signing-key":
		return generateSigningKeyCommand(args[1:], stdout, stderr)
	case "generate-local-token":
		return generateLocalTokenCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "agw: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "agw - Agents Gateway v2 contract CLI")
	fmt.Fprintln(w, "usage: agw <validate|plan|apply|run|cancel|approve|reply|migrate-manifest|generate-signing-key|generate-local-token> [options]")
	fmt.Fprintln(w, "       agw run -f agent-bundle.yaml [--input FILE]")
}

func generateLocalTokenCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("generate-local-token", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "agw generate-local-token: no arguments are accepted")
		return 2
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fmt.Fprintln(stderr, "agw generate-local-token: entropy source unavailable")
		return 1
	}
	fmt.Fprintln(stdout, base64.RawURLEncoding.EncodeToString(raw))
	return 0
}

func generateSigningKeyCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("generate-signing-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "agw generate-signing-key: no arguments are accepted")
		return 2
	}
	public, private, err := identity.GenerateSigningKey()
	if err != nil {
		fmt.Fprintln(stderr, "agw generate-signing-key: entropy source unavailable")
		return 1
	}
	// The private value is intentionally stdout-only so operators can pipe it
	// directly into a secret manager without writing a file. The public key is
	// informational and safe on stderr.
	fmt.Fprintln(stdout, base64.RawURLEncoding.EncodeToString(private))
	fmt.Fprintf(stderr, "public-key=%s\n", base64.RawURLEncoding.EncodeToString(public))
	return 0
}

func applyCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var files stringList
	flags.Var(&files, "f", "YAML manifest file or directory; may be repeated")
	server := flags.String("server", envOr("AGW_SERVER_URL", ""), "Agents Gateway server origin")
	organization := flags.String("organization", envOr("AGW_ORGANIZATION_ID", ""), "organization id")
	project := flags.String("project", envOr("AGW_PROJECT_ID", ""), "project id")
	development := flags.Bool("development", false, "permit development-only manifest settings")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(files) == 0 || *organization == "" || *project == "" {
		fmt.Fprintln(stderr, "agw apply: -f, -organization, and -project are required")
		return 2
	}
	client, err := newAPIClient(*server, os.Getenv("AGW_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "agw apply: %v\n", err)
		return 2
	}
	resources, err := loadPaths(files)
	if err != nil {
		fmt.Fprintf(stderr, "agw apply: %v\n", err)
		return 1
	}
	if err := spec.ValidateAll(resources, spec.ValidationOptions{Production: !*development}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sort.Slice(resources, func(i, j int) bool {
		left, right := resources[i].Meta(), resources[j].Meta()
		return spec.ResourceKind(resources[i])+"/"+left.Metadata.Name < spec.ResourceKind(resources[j])+"/"+right.Metadata.Name
	})
	if err := applyResources(client, *organization, *project, resources, stdout); err != nil {
		fmt.Fprintf(stderr, "agw apply: %v\n", err)
		return 1
	}
	return 0
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	// Accept the ergonomic `agw run FILE` spelling. Pulling the positional
	// bundle path before flag parsing also lets callers put flags after it.
	positionalBundle := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positionalBundle, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", envOr("AGW_SERVER_URL", ""), "Agents Gateway server origin")
	organization := flags.String("organization", envOr("AGW_ORGANIZATION_ID", ""), "organization id")
	project := flags.String("project", envOr("AGW_PROJECT_ID", ""), "project id")
	kind := flags.String("kind", "AgentRun", "AgentRun or WorkflowRun")
	ref := flags.String("ref", "", "applied agent or workflow name")
	digest := flags.String("revision", "", "applied sha256 revision")
	inputRef := flags.String("input-ref", "", "immutable external input reference")
	inputFile := flags.String("input", "", "input file, or - to read stdin (bundle runs only)")
	bundleFile := flags.String("f", "", "AgentBundle YAML/JSON file")
	follow := flags.Bool("follow", true, "follow a bundle run until it reaches a terminal state")
	idempotencyKey := flags.String("idempotency-key", "", "safe retry key")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if positionalBundle == "" && flags.NArg() > 0 {
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "agw run: expected one AgentBundle file")
			return 2
		}
		positionalBundle = flags.Arg(0)
	}
	if *bundleFile != "" || positionalBundle != "" {
		if *bundleFile != "" && positionalBundle != "" {
			fmt.Fprintln(stderr, "agw run: use either -f or a positional AgentBundle file, not both")
			return 2
		}
		if *ref != "" || *digest != "" || *kind != spec.KindAgentRun {
			fmt.Fprintln(stderr, "agw run: bundle runs cannot combine with low-level ref, revision, or non-AgentRun flags")
			return 2
		}
		path := positionalBundle
		if path == "" {
			path = *bundleFile
		}
		return runBundleCommand(path, *server, *organization, *project, *inputRef, *inputFile, *idempotencyKey, *follow, stdout, stderr)
	}
	if *organization == "" || *project == "" || *ref == "" || *digest == "" || (*kind != spec.KindAgentRun && *kind != spec.KindWorkflowRun) {
		fmt.Fprintln(stderr, "agw run: organization, project, ref, valid kind, and revision are required")
		return 2
	}
	client, err := newAPIClient(*server, os.Getenv("AGW_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "agw run: %v\n", err)
		return 2
	}
	request := map[string]any{"kind": *kind, "definitionDigest": *digest}
	if *kind == spec.KindAgentRun {
		request["agentRef"] = *ref
	} else {
		request["workflowRef"] = *ref
	}
	if *inputRef != "" {
		request["inputRef"] = *inputRef
	}
	if *idempotencyKey != "" {
		request["idempotencyKey"] = *idempotencyKey
	}
	body, _ := json.Marshal(request)
	path := fmt.Sprintf("/api/v1alpha1/organizations/%s/projects/%s/runs", url.PathEscape(*organization), url.PathEscape(*project))
	response, err := client.request(context.Background(), http.MethodPost, path, body)
	if err != nil {
		fmt.Fprintf(stderr, "agw run: %v\n", err)
		return 1
	}
	var envelope struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(response, &envelope) != nil || envelope.Data.ID == "" {
		fmt.Fprintln(stderr, "agw run: server returned an invalid response")
		return 1
	}
	fmt.Fprintf(stdout, "run %s status=%s\n", envelope.Data.ID, envelope.Data.Status)
	return 0
}

func runBundleCommand(path, server, organization, project, inputRef, inputFile, idempotencyKey string, follow bool, stdout, stderr io.Writer) int {
	if strings.TrimSpace(organization) == "" || strings.TrimSpace(project) == "" {
		fmt.Fprintln(stderr, "agw run: organization and project are required for AgentBundle runs")
		return 2
	}
	if inputRef != "" && inputFile != "" {
		fmt.Fprintln(stderr, "agw run: --input and --input-ref are mutually exclusive")
		return 2
	}
	bundle, baseDir, err := spec.LoadAgentBundle(path)
	if err != nil {
		fmt.Fprintf(stderr, "agw run: %v\n", err)
		return 1
	}
	input, err := readBundleInput(inputFile)
	if err != nil {
		fmt.Fprintf(stderr, "agw run: %v\n", err)
		return 1
	}
	compiled, err := spec.CompileAgentBundle(bundle, spec.BundleCompileOptions{BaseDir: baseDir, Input: input})
	if err != nil {
		fmt.Fprintf(stderr, "agw run: compile AgentBundle: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "bundle %s digest=%s\n", bundle.Metadata.Name, compiled.BundleDigest)
	client, err := newAPIClient(server, os.Getenv("AGW_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "agw run: %v\n", err)
		return 2
	}
	if err := applyResources(client, organization, project, compiled.Resources, stdout); err != nil {
		fmt.Fprintf(stderr, "agw run: apply AgentBundle: %v\n", err)
		return 1
	}
	request := map[string]any{"kind": spec.KindAgentRun, "agentRef": compiled.Agent.Metadata.Name, "definitionDigest": compiled.AgentDigest}
	if inputRef != "" {
		request["inputRef"] = inputRef
	}
	if idempotencyKey != "" {
		request["idempotencyKey"] = idempotencyKey
	}
	body, _ := json.Marshal(request)
	runPath := fmt.Sprintf("/api/v1alpha1/organizations/%s/projects/%s/runs", url.PathEscape(organization), url.PathEscape(project))
	response, err := client.request(context.Background(), http.MethodPost, runPath, body)
	if err != nil {
		fmt.Fprintf(stderr, "agw run: start AgentBundle: %v\n", err)
		return 1
	}
	run, err := decodeRunResponse(response)
	if err != nil {
		fmt.Fprintf(stderr, "agw run: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run %s status=%s\n", run.ID, run.Status)
	if !follow {
		return 0
	}
	status, err := followBundleRun(client, organization, project, run.ID, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "agw run: follow run: %v\n", err)
		return 1
	}
	if status != "Succeeded" && status != "Completed" {
		fmt.Fprintf(stderr, "agw run: run %s finished with status %s\n", run.ID, status)
		return 1
	}
	return 0
}

type cliRunResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func decodeRunResponse(response []byte) (cliRunResponse, error) {
	var envelope struct {
		Data cliRunResponse `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.Data.ID == "" {
		return cliRunResponse{}, errors.New("server returned an invalid run response")
	}
	return envelope.Data, nil
}

func followBundleRun(client *apiClient, organization, project, runID string, stdout io.Writer) (string, error) {
	base := fmt.Sprintf("/api/v1alpha1/organizations/%s/projects/%s/runs/%s", url.PathEscape(organization), url.PathEscape(project), url.PathEscape(runID))
	lastSequence := int64(0)
	for {
		statusBody, err := client.request(context.Background(), http.MethodGet, base, nil)
		if err != nil {
			return "", err
		}
		current, err := decodeRunResponse(statusBody)
		if err != nil {
			return "", err
		}
		eventsPath := base + "/events?after=" + fmt.Sprintf("%d", lastSequence) + "&follow=false"
		eventsBody, err := client.request(context.Background(), http.MethodGet, eventsPath, nil)
		if err != nil {
			return "", err
		}
		var eventsEnvelope struct {
			Data struct {
				Events []struct {
					Sequence int64  `json:"sequence"`
					Type     string `json:"type"`
				} `json:"events"`
			} `json:"data"`
		}
		if err := json.Unmarshal(eventsBody, &eventsEnvelope); err != nil {
			return "", errors.New("server returned invalid run events")
		}
		for _, event := range eventsEnvelope.Data.Events {
			if event.Sequence > lastSequence {
				lastSequence = event.Sequence
			}
			fmt.Fprintf(stdout, "  · %s\n", event.Type)
		}
		if isTerminalCLIStatus(current.Status) {
			return current.Status, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func isTerminalCLIStatus(status string) bool {
	switch status {
	case "Succeeded", "Completed", "Failed", "Cancelled", "Canceled", "Terminated":
		return true
	default:
		return false
	}
}

func readBundleInput(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	var reader io.Reader
	if path == "-" {
		reader = io.LimitReader(os.Stdin, 1<<20+1)
	} else {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read input %q: %w", path, err)
		}
		defer file.Close()
		reader = io.LimitReader(file, 1<<20+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", errors.New("read input")
	}
	if len(data) > 1<<20 {
		return "", errors.New("input exceeds the 1 MiB limit")
	}
	return string(data), nil
}

func applyResources(client *apiClient, organization, project string, resources []spec.Resource, stdout io.Writer) error {
	for _, resource := range resources {
		body, err := spec.AsJSON(resource)
		if err != nil {
			return fmt.Errorf("encode %s/%s: %w", spec.ResourceKind(resource), resource.Meta().Metadata.Name, err)
		}
		path := fmt.Sprintf("/api/v1alpha1/organizations/%s/projects/%s/resources/%s/%s", url.PathEscape(organization), url.PathEscape(project), url.PathEscape(spec.ResourceKind(resource)), url.PathEscape(resource.Meta().Metadata.Name))
		if _, err := client.request(context.Background(), http.MethodPut, path, body); err != nil {
			return fmt.Errorf("%s/%s: %w", spec.ResourceKind(resource), resource.Meta().Metadata.Name, err)
		}
		digest, err := spec.RevisionDigest(resource)
		if err != nil {
			return fmt.Errorf("digest %s/%s: %w", spec.ResourceKind(resource), resource.Meta().Metadata.Name, err)
		}
		fmt.Fprintf(stdout, "applied %s/%s revision=%s\n", spec.ResourceKind(resource), resource.Meta().Metadata.Name, digest)
	}
	return nil
}

type runControlFlags struct {
	server, organization, project, runID, idempotencyKey *string
}

func addRunControlFlags(flags *flag.FlagSet) runControlFlags {
	return runControlFlags{
		server:         flags.String("server", envOr("AGW_SERVER_URL", ""), "Agents Gateway server origin"),
		organization:   flags.String("organization", envOr("AGW_ORGANIZATION_ID", ""), "organization id"),
		project:        flags.String("project", envOr("AGW_PROJECT_ID", ""), "project id"),
		runID:          flags.String("run-id", "", "run id"),
		idempotencyKey: flags.String("idempotency-key", "", "stable retry key"),
	}
}

func (c runControlFlags) validate() error {
	if strings.TrimSpace(*c.organization) == "" || strings.TrimSpace(*c.project) == "" || strings.TrimSpace(*c.runID) == "" {
		return errors.New("organization, project, and run-id are required")
	}
	if strings.TrimSpace(*c.idempotencyKey) == "" {
		return errors.New("idempotency-key is required for safe retries")
	}
	return nil
}

func cancelCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cancel", flag.ContinueOnError)
	flags.SetOutput(stderr)
	common := addRunControlFlags(flags)
	reason := flags.String("reason", "", "bounded cancellation reason")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := common.validate(); err != nil {
		fmt.Fprintf(stderr, "agw cancel: %v\n", err)
		return 2
	}
	return sendRunControl("cancel", common, map[string]string{"reason": *reason}, stdout, stderr)
}

func approveCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("approve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	common := addRunControlFlags(flags)
	approvalID := flags.String("approval-id", "", "approval id")
	stepID := flags.String("step-id", "", "workflow step id")
	decision := flags.String("decision", "approved", "approved or denied")
	target := flags.String("target", "workflow", "workflow or agent")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := common.validate(); err != nil || strings.TrimSpace(*approvalID) == "" || (*decision != "approved" && *decision != "denied") || (*target != "workflow" && *target != "agent") || (*target == "agent" && strings.TrimSpace(*stepID) == "") {
		fmt.Fprintln(stderr, "agw approve: valid organization, project, run-id, idempotency-key, approval-id, decision, target, and agent step-id are required")
		return 2
	}
	return sendRunControl("approval", common, map[string]string{"approvalId": *approvalID, "stepId": *stepID, "decision": *decision, "target": *target}, stdout, stderr)
}

func replyCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("reply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	common := addRunControlFlags(flags)
	replyRef := flags.String("reply-ref", "", "immutable reply reference")
	stepID := flags.String("step-id", "", "workflow step id")
	taskID := flags.String("task-id", "", "runner task id")
	target := flags.String("target", "workflow", "workflow or agent")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := common.validate(); err != nil || strings.TrimSpace(*replyRef) == "" || (*target != "workflow" && *target != "agent") || (*target == "agent" && strings.TrimSpace(*stepID) == "") {
		fmt.Fprintln(stderr, "agw reply: valid organization, project, run-id, idempotency-key, reply-ref, target, and agent step-id are required")
		return 2
	}
	return sendRunControl("reply", common, map[string]string{"replyRef": *replyRef, "stepId": *stepID, "taskId": *taskID, "target": *target}, stdout, stderr)
}

func sendRunControl(kind string, common runControlFlags, payload map[string]string, stdout, stderr io.Writer) int {
	client, err := newAPIClient(*common.server, os.Getenv("AGW_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "agw %s: %v\n", kind, err)
		return 2
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(stderr, "agw %s: encode request\n", kind)
		return 1
	}
	path := fmt.Sprintf("/api/v1alpha1/organizations/%s/projects/%s/runs/%s/%s", url.PathEscape(*common.organization), url.PathEscape(*common.project), url.PathEscape(*common.runID), kind)
	if _, err := client.requestWithHeaders(context.Background(), http.MethodPost, path, body, map[string]string{"Idempotency-Key": *common.idempotencyKey}); err != nil {
		fmt.Fprintf(stderr, "agw %s: %v\n", kind, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s accepted for run %s\n", kind, *common.runID)
	return 0
}

type apiClient struct {
	origin string
	token  string
	http   *http.Client
}

func newAPIClient(rawOrigin, token string) (*apiClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawOrigin))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("server must be an origin URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1")) {
		return nil, errors.New("server must use HTTPS (HTTP loopback is allowed for development)")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("AGW_TOKEN is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &apiClient{
		origin: strings.TrimSuffix(parsed.String(), "/"), token: strings.TrimSpace(token),
		http: &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are forbidden") }},
	}, nil
}

func (c *apiClient) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return c.requestWithHeaders(ctx, method, path, body, nil)
}

func (c *apiClient) requestWithHeaders(ctx context.Context, method, path string, body []byte, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("response could not be read safely")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("server returned HTTP %d", response.StatusCode)
	}
	return data, nil
}

func migrateManifestCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrate-manifest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("f", "", "legacy agent.yaml path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*input) == "" {
		fmt.Fprintln(stderr, "agw migrate-manifest: -f is required")
		return 2
	}
	translation, err := compat.TranslateFile(*input)
	if err != nil {
		fmt.Fprintf(stderr, "agw migrate-manifest: %v\n", err)
		return 1
	}
	for _, warning := range translation.Warnings {
		fmt.Fprintf(stderr, "warning[%s] %s: %s\n", warning.Code, warning.Field, warning.Message)
	}
	for index, resource := range translation.Resources {
		if index > 0 {
			fmt.Fprintln(stdout, "---")
		}
		encoded, err := yaml.Marshal(resource)
		if err != nil {
			fmt.Fprintf(stderr, "agw migrate-manifest: encode %s: %v\n", spec.ResourceKind(resource), err)
			return 1
		}
		if _, err := stdout.Write(encoded); err != nil {
			fmt.Fprintf(stderr, "agw migrate-manifest: write output: %v\n", err)
			return 1
		}
	}
	return 0
}

func validateCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var files stringList
	production := flags.Bool("production", false, "apply production validation, including digest pinning")
	flags.Var(&files, "f", "YAML manifest file or directory; may be repeated")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "agw validate: at least one -f path is required")
		return 2
	}
	resources, err := loadPaths(files)
	if err != nil {
		fmt.Fprintf(stderr, "agw validate: %v\n", err)
		return 1
	}
	if err := spec.ValidateAll(resources, spec.ValidationOptions{Production: *production}); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "valid: %d resource(s)\n", len(resources))
	return 0
}

func planCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var files stringList
	production := flags.Bool("production", false, "apply production validation, including digest pinning")
	flags.Var(&files, "f", "YAML manifest file or directory; may be repeated")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "agw plan: at least one -f path is required")
		return 2
	}
	resources, err := loadPaths(files)
	if err != nil {
		fmt.Fprintf(stderr, "agw plan: %v\n", err)
		return 1
	}
	if err := spec.ValidateAll(resources, spec.ValidationOptions{Production: *production}); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	type planned struct {
		kind, namespace, name, digest string
		resource                      spec.Resource
	}
	plans := make([]planned, 0, len(resources))
	for _, resource := range resources {
		digest, digestErr := spec.RevisionDigest(resource)
		if digestErr != nil {
			fmt.Fprintf(stderr, "agw plan: %v\n", digestErr)
			return 1
		}
		meta := resource.Meta()
		plans = append(plans, planned{kind: spec.ResourceKind(resource), namespace: meta.Metadata.Namespace, name: meta.Metadata.Name, digest: digest, resource: resource})
	}
	sort.Slice(plans, func(i, j int) bool {
		left := plans[i].kind + "/" + plans[i].namespace + "/" + plans[i].name
		right := plans[j].kind + "/" + plans[j].namespace + "/" + plans[j].name
		return left < right
	})
	fmt.Fprintf(stdout, "plan: %d resource(s)\n", len(plans))
	for _, item := range plans {
		if item.namespace == "" {
			fmt.Fprintf(stdout, "  %s/%s revision=%s\n", item.kind, item.name, item.digest)
		} else {
			fmt.Fprintf(stdout, "  %s/%s/%s revision=%s\n", item.kind, item.namespace, item.name, item.digest)
		}
	}
	return 0
}

func loadPaths(paths []string) ([]spec.Resource, error) {
	var resources []spec.Resource
	seen := map[string]struct{}{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", path, err)
		}
		files := []string{path}
		if info.IsDir() {
			files = nil
			err = filepath.Walk(path, func(candidate string, candidateInfo os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if candidateInfo.IsDir() || !isYAML(candidate) {
					return nil
				}
				files = append(files, candidate)
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("walk %q: %w", path, err)
			}
			sort.Strings(files)
		}
		for _, file := range files {
			data, readErr := os.ReadFile(file)
			if readErr != nil {
				return nil, fmt.Errorf("read %q: %w", file, readErr)
			}
			decoded, decodeErr := spec.Decode(data)
			if decodeErr != nil {
				return nil, fmt.Errorf("%s: %w", file, decodeErr)
			}
			for _, resource := range decoded {
				identity := spec.ResourceKind(resource) + "/" + resource.Meta().Metadata.Namespace + "/" + resource.Meta().Metadata.Name
				if _, exists := seen[identity]; exists {
					return nil, fmt.Errorf("%s: duplicate resource identity %s", file, identity)
				}
				seen[identity] = struct{}{}
				resources = append(resources, resource)
			}
		}
	}
	return resources, nil
}

func isYAML(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	return extension == ".yaml" || extension == ".yml"
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("path must not be empty")
	}
	*s = append(*s, value)
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
