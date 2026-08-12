// Command agw-agent-run-lifecycle is the bounded producer used by the Argo
// lifecycle WorkflowTemplate. Argo owns sequencing and retry semantics; this
// binary has no persistent phase machine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/argoworkflow"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/phase0/argo-sandbox/bridge"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultKubeAPI        = "https://kubernetes.default.svc:443"
	serviceAccountToken   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceAccountCA      = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	maxCredentialBytes    = 16 << 10
	maxOutputParameter    = 1 << 20
	rootObjectStoreSecret = "AGW_OBJECT_STORE_CREDENTIAL_SECRET"
)

// There is intentionally no supervise mode. This image runs bounded Argo
// lifecycle steps and can observe an Agent Sandbox through its narrowly scoped
// wait ServiceAccount; it is not a trusted in-pod phase supervisor and must
// not be given a phase-transition credential.
var errUsage = errors.New("usage: agw-agent-run-lifecycle prepare|stage|wait|handoff|cleanup")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agw-agent-run-lifecycle:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	if ctx == nil || stderr == nil || len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "prepare":
		return runPrepare(ctx)
	case "stage":
		if len(args) != 1 {
			return errUsage
		}
		return runStage(ctx)
	case "wait":
		return runWait(ctx, args[1:], stderr)
	case "handoff":
		if len(args) != 1 {
			return errUsage
		}
		return runHandoff(ctx)
	case "cleanup":
		if len(args) != 1 {
			return errUsage
		}
		return runCleanup(ctx)
	default:
		return errUsage
	}
}

type producerConfig struct {
	request         argoworkflow.Request
	systemNamespace string
	objectStore     objectStoreConfig
}

type objectStoreConfig struct {
	Bucket         string
	Prefix         string
	Region         string
	Endpoint       string
	ForcePathStyle bool
	MaxObjectBytes int64
}

func runPrepare(ctx context.Context) error {
	cfg, err := loadPrepareConfig()
	if err != nil {
		return err
	}
	kube, err := inClusterClient()
	if err != nil {
		return err
	}
	if err := loadRootObjectStoreCredentials(ctx, kube, cfg); err != nil {
		return err
	}
	store, err := newObjectStore(ctx, cfg.objectStore)
	if err != nil {
		return err
	}
	producer, err := argoworkflow.NewProducer(kube, store, store, cfg.request.WorkflowTemplateName, cfg.request.WorkflowTemplateUID, cfg.request.WorkflowTemplateDigest)
	if err != nil {
		return err
	}
	result, err := producer.Prepare(ctx, cfg.request)
	if err != nil {
		return err
	}
	if err := writeOutputParameter(argoworkflow.PrepareSandboxPath, result.Sandbox.Name); err != nil {
		return err
	}
	if err := writeOutputParameter(argoworkflow.PrepareSandboxUIDPath, string(result.Sandbox.UID)); err != nil {
		return err
	}
	if err := writeOutputParameter(argoworkflow.PrepareClaimPath, result.Claim); err != nil {
		return err
	}
	if err := writeOutputParameter(argoworkflow.PrepareSecretPath, result.Secret); err != nil {
		return err
	}
	return writeOutputParameter(argoworkflow.PrepareTimeoutPath, result.Timeout)
}

func runStage(ctx context.Context) error {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return err
	}
	workspace, err := requiredEnv("AGW_WORKSPACE")
	if err != nil {
		return err
	}
	if err := loadProjectedObjectStoreCredentials(); err != nil {
		return err
	}
	store, err := newObjectStore(ctx, cfg.objectStore)
	if err != nil {
		return err
	}
	producer, err := argoworkflow.NewProducer(nil, store, store, cfg.request.WorkflowTemplateName, cfg.request.WorkflowTemplateUID, cfg.request.WorkflowTemplateDigest)
	if err != nil {
		return err
	}
	return producer.Stage(ctx, cfg.request, workspace)
}

func runHandoff(ctx context.Context) error {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return err
	}
	sandboxUID, err := requiredEnv("AGW_SANDBOX_UID")
	if err != nil {
		return err
	}
	cfg.request.SandboxUID = sandboxUID
	workspace, err := requiredEnv("AGW_WORKSPACE")
	if err != nil {
		return err
	}
	if err := loadProjectedObjectStoreCredentials(); err != nil {
		return err
	}
	store, err := newObjectStore(ctx, cfg.objectStore)
	if err != nil {
		return err
	}
	kube, err := inClusterClient()
	if err != nil {
		return err
	}
	producer, err := argoworkflow.NewProducer(kube, store, store, cfg.request.WorkflowTemplateName, cfg.request.WorkflowTemplateUID, cfg.request.WorkflowTemplateDigest)
	if err != nil {
		return err
	}
	output, err := producer.Handoff(ctx, cfg.request, workspace)
	if err != nil {
		return err
	}
	if len(output.Canonical) == 0 || len(output.Canonical) > argoworkflow.MaxLifecycleOutputBytes {
		return errors.New("lifecycle output is outside the bounded contract")
	}
	return writeOutputParameter(argoworkflow.LifecycleOutputPath, string(output.Canonical))
}

func runCleanup(ctx context.Context) error {
	// Argo's onExit hook is deliberately tokenless and has no RBAC binding.
	// It validates that the immutable lifecycle identity was propagated, while
	// the trusted operator owns credential, Sandbox, and PVC cleanup. Keeping
	// deletion out of a retryable workflow pod avoids granting a compromised
	// lifecycle image namespace-wide Secret mutation.
	_, err := loadRequest()
	return err
}

func runWait(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "Sandbox name")
	namespace := flags.String("namespace", "", "Sandbox namespace")
	condition := flags.String("condition", "", "Ready or Finished")
	uid := flags.String("uid", "", "prepared Sandbox UID")
	timeout := flags.Duration("timeout", 0, "maximum wait (resolved from the admitted AgentRun)")
	poll := flags.Duration("poll-interval", 2*time.Second, "poll interval")
	marker := flags.String("marker", "", "identity marker path")
	requiredMarker := flags.String("require-marker", "", "required marker path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	if *name == "" || *namespace == "" || *uid == "" {
		return errors.New("wait requires --name, --namespace, and --uid")
	}
	if *timeout <= 0 {
		return errors.New("wait requires a positive per-run --timeout")
	}
	apiServer := os.Getenv("AGW_KUBE_API_SERVER")
	if apiServer == "" {
		apiServer = defaultKubeAPI
	}
	token, err := os.ReadFile(serviceAccountToken)
	if err != nil || len(token) == 0 {
		return errors.New("read Kubernetes service-account token")
	}
	ca, err := os.ReadFile(serviceAccountCA)
	if err != nil || len(ca) == 0 {
		return errors.New("read Kubernetes service-account CA")
	}
	apiClient, err := bridge.NewClient(apiServer, string(token), ca)
	if err != nil {
		return err
	}
	logf := func(format string, values ...any) {
		_, _ = fmt.Fprintf(stderr, format+"\n", values...)
	}
	if err := bridge.Wait(ctx, apiClient, bridge.Config{Namespace: *namespace, Name: *name, Condition: *condition, Timeout: *timeout, PollInterval: *poll, MarkerPath: *marker, RequiredMarkerPath: *requiredMarker}, logf); err != nil {
		return err
	}
	if *marker == "" {
		return errors.New("wait requires an identity marker path")
	}
	return verifyLifecycleMarker(*marker, *namespace, *name, *uid, *condition)
}

func verifyLifecycleMarker(path, namespace, name, uid, condition string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 {
		return errors.New("Sandbox identity marker is invalid")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return errors.New("read Sandbox identity marker")
	}
	var marker bridge.Marker
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return errors.New("decode Sandbox identity marker")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("Sandbox identity marker contains trailing data")
	}
	if marker.Version != bridge.MarkerVersion || marker.Namespace != namespace || marker.Name != name || marker.UID != uid || marker.Condition != condition || marker.Observed == "" {
		return errors.New("Sandbox identity marker does not match prepared UID")
	}
	if _, err := time.Parse(time.RFC3339Nano, marker.Observed); err != nil {
		return errors.New("Sandbox identity marker timestamp is invalid")
	}
	return nil
}

func loadRuntimeConfig() (producerConfig, error) {
	request, err := loadRequest()
	if err != nil {
		return producerConfig{}, err
	}
	objectStore, err := loadObjectStoreConfig()
	if err != nil {
		return producerConfig{}, err
	}
	return producerConfig{request: request, objectStore: objectStore}, nil
}

func loadPrepareConfig() (producerConfig, error) {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return producerConfig{}, err
	}
	systemNamespace, err := requiredEnv("AGW_SYSTEM_NAMESPACE")
	if err != nil {
		return producerConfig{}, err
	}
	cfg.systemNamespace = systemNamespace
	return cfg, nil
}

func loadRequest() (argoworkflow.Request, error) {
	generation, err := positiveInt64Env("AGW_RUN_GENERATION")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	namespace, err := requiredEnv("AGW_NAMESPACE")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	name, err := requiredEnv("AGW_RUN_NAME")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	uid, err := requiredEnv("AGW_RUN_UID")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	digest, err := requiredEnv("AGW_SPEC_DIGEST")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	baseSHA, err := requiredEnv("AGW_BASE_SHA")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	template, err := requiredEnv("AGW_WORKFLOW_TEMPLATE")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	templateUID, err := requiredEnv("AGW_WORKFLOW_TEMPLATE_UID")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	templateDigest, err := requiredEnv("AGW_WORKFLOW_TEMPLATE_DIGEST")
	if err != nil {
		return argoworkflow.Request{}, err
	}
	return argoworkflow.Request{Namespace: namespace, RunName: name, RunUID: uid, RunGeneration: generation, SpecDigest: digest, BaseSHA: baseSHA, WorkflowTemplateName: template, WorkflowTemplateUID: templateUID, WorkflowTemplateDigest: templateDigest}, nil
}

func loadObjectStoreConfig() (objectStoreConfig, error) {
	bucket, err := requiredEnv("AGW_OBJECT_STORE_BUCKET")
	if err != nil {
		return objectStoreConfig{}, err
	}
	prefix, err := requiredEnv("AGW_OBJECT_STORE_PREFIX")
	if err != nil {
		return objectStoreConfig{}, err
	}
	region, err := requiredEnv("AGW_OBJECT_STORE_REGION")
	if err != nil {
		return objectStoreConfig{}, err
	}
	maxText := os.Getenv("AGW_OBJECT_STORE_MAX_BYTES")
	max := objectstore.GeneralMaxObjectBytes
	if maxText != "" {
		max, err = strconv.ParseInt(maxText, 10, 64)
		if err != nil || max <= 0 || max > objectstore.GeneralMaxObjectBytes {
			return objectStoreConfig{}, errors.New("AGW_OBJECT_STORE_MAX_BYTES is invalid")
		}
	}
	pathStyle, err := parseBoolEnv("AGW_OBJECT_STORE_PATH_STYLE", false)
	if err != nil {
		return objectStoreConfig{}, err
	}
	return objectStoreConfig{Bucket: bucket, Prefix: prefix, Region: region, Endpoint: os.Getenv("AGW_OBJECT_STORE_ENDPOINT"), ForcePathStyle: pathStyle, MaxObjectBytes: max}, nil
}

func inClusterClient() (client.Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.New("load in-cluster Kubernetes configuration")
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(config, client.Options{Scheme: scheme})
}

func loadRootObjectStoreCredentials(ctx context.Context, kube client.Client, cfg producerConfig) error {
	secretName, err := requiredEnv(rootObjectStoreSecret)
	if err != nil {
		return err
	}
	accessKey, err := requiredEnv("AGW_OBJECT_STORE_ACCESS_KEY_ID_KEY")
	if err != nil {
		return err
	}
	secretKey, err := requiredEnv("AGW_OBJECT_STORE_SECRET_ACCESS_KEY_KEY")
	if err != nil {
		return err
	}
	sessionKey := os.Getenv("AGW_OBJECT_STORE_SESSION_TOKEN_KEY")
	secret := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: cfg.systemNamespace, Name: secretName}, secret); err != nil {
		return errors.New("read operator object-store credential Secret")
	}
	if secret.Type != corev1.SecretTypeOpaque || len(secret.Data[accessKey]) == 0 || len(secret.Data[secretKey]) == 0 || len(secret.Data[accessKey]) > maxCredentialBytes || len(secret.Data[secretKey]) > maxCredentialBytes {
		return errors.New("operator object-store credential Secret is invalid")
	}
	if err := setCredentialEnv("AWS_ACCESS_KEY_ID", secret.Data[accessKey]); err != nil {
		return err
	}
	if err := setCredentialEnv("AWS_SECRET_ACCESS_KEY", secret.Data[secretKey]); err != nil {
		return err
	}
	if sessionKey != "" && len(secret.Data[sessionKey]) > 0 {
		return setCredentialEnv("AWS_SESSION_TOKEN", secret.Data[sessionKey])
	}
	return nil
}

func loadProjectedObjectStoreCredentials() error {
	values := map[string]string{"AWS_ACCESS_KEY_ID": "AGW_OBJECT_STORE_ACCESS_KEY_FILE", "AWS_SECRET_ACCESS_KEY": "AGW_OBJECT_STORE_SECRET_ACCESS_KEY_FILE", "AWS_SESSION_TOKEN": "AGW_OBJECT_STORE_SESSION_TOKEN_FILE"}
	for envName, fileEnv := range values {
		path := os.Getenv(fileEnv)
		if path == "" {
			return fmt.Errorf("%s is required", fileEnv)
		}
		body, err := readCredentialFile(path)
		if err != nil {
			return err
		}
		if err := setCredentialEnv(envName, body); err != nil {
			return err
		}
	}
	return nil
}

func readCredentialFile(path string) ([]byte, error) {
	if path == "" || path != filepath.Clean(path) || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return nil, errors.New("credential file path is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCredentialBytes {
		return nil, errors.New("credential file is invalid")
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 || bytesContainControl(body) {
		return nil, errors.New("credential file could not be read safely")
	}
	return body, nil
}

func setCredentialEnv(name string, value []byte) error {
	if len(value) == 0 || len(value) > maxCredentialBytes || bytesContainControl(value) {
		return errors.New("object-store credential contains invalid bytes")
	}
	return os.Setenv(name, string(value))
}

func newObjectStore(ctx context.Context, cfg objectStoreConfig) (*objectstore.Store, error) {
	return objectstore.New(ctx, objectstore.Config{Bucket: cfg.Bucket, Prefix: cfg.Prefix, Region: cfg.Region, Endpoint: cfg.Endpoint, ForcePathStyle: cfg.ForcePathStyle, MaxObjectBytes: cfg.MaxObjectBytes})
}

func writeOutputParameter(path, value string) error {
	if path == "" || len(value) == 0 || len(value) > maxOutputParameter || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("Argo output parameter is invalid")
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		return fmt.Errorf("write Argo output parameter: %w", err)
	}
	return nil
}

func requiredEnv(name string) (string, error) {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func positiveInt64Env(name string) (int64, error) {
	value, err := requiredEnv(name)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	return parsed, nil
}

func parseBoolEnv(name string, defaultValue bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s is invalid", name)
	}
	return parsed, nil
}

func bytesContainControl(body []byte) bool {
	for _, value := range body {
		if value == 0 || value == '\r' || value == '\n' {
			return true
		}
	}
	return false
}
