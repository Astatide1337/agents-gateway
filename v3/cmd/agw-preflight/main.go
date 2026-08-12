// Command agw-preflight is the production attestor for the v3 admission
// preflight contract. It is intentionally split across two containers in the
// Helm workload: this process is the only writer and receives the explicit
// short-lived Kubernetes API token; the agent-probe process has no token mount
// and proves that a non-broker UID cannot use the pod network.
//
// The command is one-shot. Helm runs it from an on-install/on-upgrade Job and
// from a recurring CronJob. A failed or incomplete attempt writes a failed
// result before checks begin, and a passing result is written only after every
// required check and the final ConfigMap update have succeeded.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	modeAttest     = "attest"
	modeAgentProbe = "agent-probe"

	defaultNamespace = preflight.ConfigMapNamespace
	defaultName      = preflight.ConfigMapName
	defaultAPIURL    = "https://kubernetes.default.svc:443"

	defaultTokenFile   = "/var/run/agw/preflight/token"
	defaultCAFile      = "/var/run/agw/preflight/ca.crt"
	defaultEvidence    = "/evidence"
	defaultHostProc    = "/hostproc"
	defaultHostKubelet = "/host/kubelet"
	defaultRunc        = "/host/runtime/runc"
	defaultContainerd  = "/host/runtime/containerd"
	defaultK3s         = "/host/runtime/k3s"
	defaultLockdown    = "/evidence/lockdown.marker"
	defaultAgent       = "/evidence/agent.json"
	defaultAPITarget   = "/evidence/api-target"

	maxProbeOutput     = 4096
	maxCheckDetail     = 256
	maxChecks          = 16
	maxAgentResultSize = 4096
	maxTokenBytes      = 8192
	minUIDMapLength    = uint64(65536)
)

var requiredAttestorChecks = []string{
	"writer-uid",
	"iptables-owner-rules",
	"userns-uid-map",
	"node-runtime-fingerprint",
	"broker-egress",
	"agent-uid",
	"agent-service-account-token-absent",
	"agent-ambient-credential-absent",
	"agent-api-server-denied",
	"agent-dns-denied",
}

var requiredAgentChecks = []string{
	"uid",
	"service-account-token-absent",
	"ambient-credential-absent",
	"api-server-denied",
	"dns-denied",
}

type config struct {
	Mode                       string
	Namespace                  string
	ConfigMapName              string
	ExpectedNodeName           string
	ExpectedNodeFingerprint    string
	ExpectedRuntimeFingerprint string
	APIURL                     string
	Deadline                   time.Duration
	AgentWait                  time.Duration
	TokenFile                  string
	CAFile                     string
	EvidenceDir                string
	HostProc                   string
	HostKubelet                string
	RuncPath                   string
	ContainerdPath             string
	K3sPath                    string
}

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type report struct {
	SchemaVersion      int     `json:"schema_version"`
	State              string  `json:"state"`
	Passed             bool    `json:"passed"`
	NodeFingerprint    string  `json:"node_fingerprint"`
	RuntimeFingerprint string  `json:"runtime_fingerprint"`
	Checks             []check `json:"checks"`
}

type agentReport struct {
	SchemaVersion int     `json:"schema_version"`
	Passed        bool    `json:"passed"`
	Checks        []check `json:"checks"`
}

type uidMap struct {
	Inside  uint64
	Outside uint64
	Length  uint64
}

type fingerprints struct {
	NodeName          string
	Kernel            string
	Architecture      string
	KubeletFS         string
	RuncVersion       string
	ContainerdVersion string
	K3sVersion        string
	Node              string
	Runtime           string
}

type configMapWriter struct {
	client    kubernetes.Interface
	namespace string
	name      string
}

func main() {
	cfg := parseConfig()
	if err := validateConfig(cfg); err != nil {
		fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Deadline)
	defer cancel()

	var err error
	switch cfg.Mode {
	case modeAgentProbe:
		err = runAgentProbe(ctx, cfg)
	case modeAttest:
		err = runAttestor(ctx, cfg)
	default:
		err = fmt.Errorf("unsupported mode %q", cfg.Mode)
	}
	if err != nil {
		fatal(err)
	}
}

func parseConfig() config {
	var cfg config
	flag.StringVar(&cfg.Mode, "mode", modeAttest, "attest or agent-probe")
	flag.StringVar(&cfg.Namespace, "namespace", defaultNamespace, "system namespace containing the fixed preflight ConfigMap")
	flag.StringVar(&cfg.ConfigMapName, "configmap-name", defaultName, "fixed preflight ConfigMap name")
	flag.StringVar(&cfg.ExpectedNodeName, "node-name", "", "expected selected node name")
	flag.StringVar(&cfg.ExpectedNodeFingerprint, "node-fingerprint", "", "expected selected node fingerprint")
	flag.StringVar(&cfg.ExpectedRuntimeFingerprint, "runtime-fingerprint", "", "expected selected runtime fingerprint")
	flag.StringVar(&cfg.APIURL, "api-server-url", defaultAPIURL, "Kubernetes API server URL")
	flag.DurationVar(&cfg.Deadline, "deadline", 90*time.Second, "hard command deadline")
	flag.DurationVar(&cfg.AgentWait, "agent-wait", 30*time.Second, "maximum wait for the isolated agent probe")
	flag.StringVar(&cfg.TokenFile, "token-file", defaultTokenFile, "explicit projected API token file")
	flag.StringVar(&cfg.CAFile, "ca-file", defaultCAFile, "projected Kubernetes CA file")
	flag.StringVar(&cfg.EvidenceDir, "evidence-dir", defaultEvidence, "shared evidence directory")
	flag.StringVar(&cfg.HostProc, "host-proc", defaultHostProc, "read-only host /proc mount")
	flag.StringVar(&cfg.HostKubelet, "host-kubelet", defaultHostKubelet, "read-only host kubelet root mount")
	flag.StringVar(&cfg.RuncPath, "runc-path", defaultRunc, "read-only host runc binary")
	flag.StringVar(&cfg.ContainerdPath, "containerd-path", defaultContainerd, "read-only host containerd binary")
	flag.StringVar(&cfg.K3sPath, "k3s-path", defaultK3s, "read-only host k3s binary")
	flag.Parse()
	return cfg
}

func validateConfig(cfg config) error {
	if cfg.Mode != modeAttest && cfg.Mode != modeAgentProbe {
		return fmt.Errorf("mode must be %q or %q", modeAttest, modeAgentProbe)
	}
	if cfg.Deadline <= 0 || cfg.Deadline > 2*time.Minute {
		return fmt.Errorf("deadline must be greater than zero and at most two minutes")
	}
	if cfg.AgentWait <= 0 || cfg.AgentWait > cfg.Deadline {
		return fmt.Errorf("agent-wait must be positive and no greater than deadline")
	}
	if cfg.EvidenceDir == "" || strings.ContainsAny(cfg.EvidenceDir, "\r\n") {
		return fmt.Errorf("evidence-dir is invalid")
	}
	if cfg.Mode == modeAgentProbe {
		return nil
	}
	if len(validation.IsDNS1123Label(cfg.Namespace)) != 0 || cfg.Namespace == "" {
		return fmt.Errorf("namespace is not a DNS label")
	}
	if cfg.ConfigMapName != defaultName {
		return fmt.Errorf("configmap-name must remain %q", defaultName)
	}
	if err := validateHTTPSURL(cfg.APIURL); err != nil {
		return fmt.Errorf("api-server-url: %w", err)
	}
	for label, value := range map[string]string{
		"node-name":           cfg.ExpectedNodeName,
		"node-fingerprint":    cfg.ExpectedNodeFingerprint,
		"runtime-fingerprint": cfg.ExpectedRuntimeFingerprint,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") || len(value) > 512 {
			return fmt.Errorf("%s must be a non-empty bounded single-line value", label)
		}
	}
	for label, value := range map[string]string{
		"token-file":      cfg.TokenFile,
		"ca-file":         cfg.CAFile,
		"host-proc":       cfg.HostProc,
		"host-kubelet":    cfg.HostKubelet,
		"runc-path":       cfg.RuncPath,
		"containerd-path": cfg.ContainerdPath,
		"k3s-path":        cfg.K3sPath,
	} {
		if value == "" || !strings.HasPrefix(value, "/") || strings.Contains(value, "..") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s is not a normalized absolute path", label)
		}
	}
	return nil
}

func validateHTTPSURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an https URL without userinfo, query, or fragment")
	}
	return nil
}

func runAttestor(ctx context.Context, cfg config) error {
	writer, err := newConfigMapWriter(cfg)
	if err != nil {
		return err
	}

	initial := newReport("running", "unavailable", "unavailable")
	if err := writer.write(ctx, initial); err != nil {
		return fmt.Errorf("failed to revoke prior preflight evidence: %w", err)
	}

	attestation := performChecks(ctx, cfg)
	if !attestation.Passed {
		writeCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := writer.write(writeCtx, attestation); err != nil {
			return fmt.Errorf("failed to write failed preflight evidence: %w", err)
		}
		return fmt.Errorf("preflight failed; passing evidence was not written")
	}

	// The final write is the only operation that can publish a passing result.
	// It is intentionally a second, bounded API transaction after all checks.
	writeCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := writer.writePassing(writeCtx, attestation); err != nil {
		return fmt.Errorf("failed to publish passing preflight evidence: %w", err)
	}
	fmt.Printf("preflight passed node_fingerprint=%s runtime_fingerprint=%s\n", attestation.NodeFingerprint, attestation.RuntimeFingerprint)
	return nil
}

func newConfigMapWriter(cfg config) (*configMapWriter, error) {
	token, err := readBoundedFile(cfg.TokenFile, maxTokenBytes)
	if err != nil {
		return nil, fmt.Errorf("explicit projected API token is unavailable: %w", err)
	}
	if len(bytes.TrimSpace(token)) == 0 {
		return nil, errors.New("explicit projected API token is empty")
	}
	if _, err := os.Stat(cfg.CAFile); err != nil {
		return nil, fmt.Errorf("projected Kubernetes CA is unavailable: %w", err)
	}
	parsed, err := url.Parse(cfg.APIURL)
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}
	restConfig := &rest.Config{
		Host:            cfg.APIURL,
		BearerTokenFile: cfg.TokenFile,
		TLSClientConfig: rest.TLSClientConfig{CAFile: cfg.CAFile, ServerName: parsed.Hostname()},
		Timeout:         5 * time.Second,
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return &configMapWriter{client: clientset, namespace: cfg.Namespace, name: cfg.ConfigMapName}, nil
}

func (w *configMapWriter) write(ctx context.Context, r report) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.State == "passed" || r.Passed {
		return errors.New("internal error: non-passing writer received passing report")
	}
	return w.update(ctx, r)
}

func (w *configMapWriter) writePassing(ctx context.Context, r report) error {
	if !r.Passed || r.State != "passed" || !allRequiredChecksPassed(r.Checks) {
		return errors.New("internal error: passing evidence did not satisfy every check")
	}
	return w.update(ctx, r)
}

func (w *configMapWriter) update(ctx context.Context, r report) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rawResult, err := resultJSON(r)
	if err != nil {
		return err
	}
	rawChecks, err := boundedJSON(r, 12<<10)
	if err != nil {
		return err
	}
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		current, err := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, w.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Data == nil {
			current.Data = map[string]string{}
		}
		// The attestor owns these two keys. It deliberately leaves no stale
		// result.json behind when a check is incomplete.
		current.Data[preflight.ResultDataKey] = string(rawResult)
		current.Data["checks.json"] = string(rawChecks)
		_, err = w.client.CoreV1().ConfigMaps(w.namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}

func resultJSON(r report) ([]byte, error) {
	result := preflight.Result{
		SchemaVersion:      preflight.CurrentSchemaVersion,
		Passed:             r.Passed && r.State == "passed" && allRequiredChecksPassed(r.Checks),
		Timestamp:          time.Now().UTC(),
		NodeFingerprint:    boundedFingerprint(r.NodeFingerprint),
		RuntimeFingerprint: boundedFingerprint(r.RuntimeFingerprint),
	}
	return json.Marshal(result)
}

func boundedJSON(value any, max int) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) > max {
		return nil, fmt.Errorf("diagnostic evidence exceeds %d bytes", max)
	}
	return raw, nil
}

func boundedFingerprint(value string) string {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value {
		return "unavailable"
	}
	return value
}

func newReport(state, node, runtimeValue string) report {
	return report{
		SchemaVersion:      1,
		State:              state,
		NodeFingerprint:    boundedFingerprint(node),
		RuntimeFingerprint: boundedFingerprint(runtimeValue),
		Checks:             make([]check, 0, maxChecks),
	}
}

func performChecks(ctx context.Context, cfg config) report {
	reportValue := newReport("failed", "unavailable", "unavailable")
	add := func(name string, passed bool, detail string) {
		if len(reportValue.Checks) >= maxChecks {
			return
		}
		status := "failed"
		if passed {
			status = "passed"
		}
		reportValue.Checks = append(reportValue.Checks, check{Name: name, Status: status, Detail: boundDetail(detail)})
	}

	if os.Geteuid() != 1337 {
		add("writer-uid", false, "writer must run as UID 1337")
	} else {
		add("writer-uid", true, "writer UID is 1337")
	}

	if lockdownMarkerOK(cfg.EvidenceDir + "/lockdown.marker") {
		add("iptables-owner-rules", true, "lockdown init confirmed IPv4 and IPv6 owner rules")
	} else {
		add("iptables-owner-rules", false, "lockdown marker is missing or incomplete")
	}

	uidOK, uidDetail := userNamespaceOK(cfg.HostProc)
	add("userns-uid-map", uidOK, uidDetail)

	facts, err := collectFingerprints(ctx, cfg)
	if err != nil {
		add("node-runtime-fingerprint", false, err.Error())
	} else {
		reportValue.NodeFingerprint = facts.Node
		reportValue.RuntimeFingerprint = facts.Runtime
		add("node-runtime-fingerprint", facts.Node == cfg.ExpectedNodeFingerprint && facts.Runtime == cfg.ExpectedRuntimeFingerprint && facts.NodeName == cfg.ExpectedNodeName, fingerprintDetail(facts, cfg))
	}

	apiIP, err := brokerResolveAPI(ctx, cfg.APIURL)
	if err != nil {
		add("broker-egress", false, "UID 1337 could not resolve the Kubernetes API service")
	} else {
		add("broker-egress", true, "UID 1337 resolved the Kubernetes API service")
		if err := writeAtomic(cfg.EvidenceDir+"/api-target", []byte(apiIP+"\n"), 0644); err != nil {
			add("agent-probe-input", false, "could not publish bounded API probe target")
		} else {
			agent, agentErr := waitForAgent(ctx, cfg.EvidenceDir+"/agent.json", cfg.AgentWait)
			if agentErr != nil {
				add("agent-network-denial", false, agentErr.Error())
			} else {
				for _, item := range agent.Checks {
					if len(reportValue.Checks) >= maxChecks {
						break
					}
					add("agent-"+item.Name, item.Status == "passed", item.Detail)
				}
			}
		}
	}

	if allRequiredChecksPassed(reportValue.Checks) && reportValue.NodeFingerprint == cfg.ExpectedNodeFingerprint && reportValue.RuntimeFingerprint == cfg.ExpectedRuntimeFingerprint {
		reportValue.State = "passed"
		reportValue.Passed = true
	}
	return reportValue
}

func lockdownMarkerOK(path string) bool {
	raw, err := readBoundedFile(path, 1024)
	if err != nil {
		return false
	}
	contents := string(raw)
	return strings.Contains(contents, "ipv4_owner_rule=installed\n") && strings.Contains(contents, "ipv6_owner_rule=installed\n")
}

func userNamespaceOK(hostProc string) (bool, string) {
	if _, err := hostProcFilesystem(hostProc); err != nil {
		return false, err.Error()
	}
	uidRaw, err := readBoundedFile("/proc/self/uid_map", 1024)
	if err != nil {
		return false, "uid_map could not be read"
	}
	gidRaw, err := readBoundedFile("/proc/self/gid_map", 1024)
	if err != nil {
		return false, "gid_map could not be read"
	}
	uid, err := parseIDMap(string(uidRaw))
	if err != nil {
		return false, "uid_map is not a non-host mapping"
	}
	if _, err := parseIDMap(string(gidRaw)); err != nil {
		return false, "gid_map is not a non-host mapping"
	}
	currentNS, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		return false, "current user namespace could not be identified"
	}
	hostNS, err := os.Readlink(hostProc + "/1/ns/user")
	if err != nil {
		return false, "host user namespace could not be identified"
	}
	if currentNS == hostNS {
		return false, "pod user namespace is the host user namespace"
	}
	return true, fmt.Sprintf("uid 0 maps to subordinate host UID %d for %d IDs", uid.Outside, uid.Length)
}

func parseIDMap(raw string) (uidMap, error) {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return uidMap{}, errors.New("id map has an invalid number of fields")
		}
		inside, err1 := strconv.ParseUint(fields[0], 10, 64)
		outside, err2 := strconv.ParseUint(fields[1], 10, 64)
		length, err3 := strconv.ParseUint(fields[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || inside != 0 || outside == 0 || length < minUIDMapLength {
			return uidMap{}, errors.New("id map does not begin with a subordinate 65536-ID range")
		}
		return uidMap{Inside: inside, Outside: outside, Length: length}, nil
	}
	if err := scanner.Err(); err != nil {
		return uidMap{}, err
	}
	return uidMap{}, errors.New("id map is empty")
}

func collectFingerprints(ctx context.Context, cfg config) (fingerprints, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" || strings.TrimSpace(nodeName) != nodeName {
		return fingerprints{}, errors.New("selected node name is unavailable")
	}
	if _, err := hostProcFilesystem(cfg.HostProc); err != nil {
		return fingerprints{}, fmt.Errorf("host proc check failed: %w", err)
	}
	kernelRaw, err := readBoundedFile(cfg.HostProc+"/sys/kernel/osrelease", 256)
	if err != nil {
		return fingerprints{}, errors.New("host kernel fingerprint is unavailable")
	}
	kernel := strings.TrimSpace(string(kernelRaw))
	if kernel == "" || strings.ContainsAny(kernel, "\r\n") {
		return fingerprints{}, errors.New("host kernel fingerprint is malformed")
	}
	for _, volume := range []struct {
		label string
		path  string
	}{
		{label: "host-kubelet", path: cfg.HostKubelet},
		{label: "host-runc", path: cfg.RuncPath},
		{label: "host-containerd", path: cfg.ContainerdPath},
		{label: "host-k3s", path: cfg.K3sPath},
	} {
		if _, err := volumeFilesystemFamily(volume.path); err != nil {
			return fingerprints{}, fmt.Errorf("%s hostPath filesystem-family precheck failed (this does not prove an idmapped mount): %w", volume.label, err)
		}
	}
	fsName, err := volumeFilesystemFamily(cfg.HostKubelet)
	if err != nil {
		return fingerprints{}, fmt.Errorf("kubelet hostPath filesystem-family fingerprint is unavailable (this does not prove an idmapped mount): %w", err)
	}
	canonicalVersion := func(value string) string {
		return strings.Join(strings.Fields(value), " ")
	}
	runVersionBounded := func(path string) (string, error) {
		return runVersion(ctx, path)
	}
	truncRunc, err := runVersionBounded(cfg.RuncPath)
	if err != nil {
		return fingerprints{}, errors.New("runc version fingerprint is unavailable")
	}
	truncContainerd, err := runVersionBounded(cfg.ContainerdPath)
	if err != nil {
		return fingerprints{}, errors.New("containerd version fingerprint is unavailable")
	}
	truncK3s, err := runVersionBounded(cfg.K3sPath)
	if err != nil {
		return fingerprints{}, errors.New("k3s version fingerprint is unavailable")
	}
	result := fingerprints{
		NodeName:          nodeName,
		Kernel:            kernel,
		Architecture:      runtime.GOARCH,
		KubeletFS:         fsName,
		RuncVersion:       canonicalVersion(truncRunc),
		ContainerdVersion: canonicalVersion(truncContainerd),
		K3sVersion:        canonicalVersion(truncK3s),
	}
	result.Node = digestFingerprint("agw.node.v1", result.NodeName, result.Kernel, result.Architecture, result.KubeletFS)
	result.Runtime = digestFingerprint("agw.runtime.v1", result.RuncVersion, result.ContainerdVersion, result.K3sVersion)
	return result, nil
}

const procFilesystemMagic = 0x9fa0

// hostProcFilesystem validates the identity/proc mount used to inspect the
// host namespace. procfs is a namespace-inspection surface, not a volume
// filesystem candidate for the idmapped-mount family precheck.
func hostProcFilesystem(path string) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", fmt.Errorf("host proc mount %q cannot be inspected: %w", path, err)
	}
	if int64(stat.Type) != procFilesystemMagic {
		return "", fmt.Errorf("host proc mount %q has filesystem magic 0x%x, want procfs (0x%x)", path, uint64(stat.Type), procFilesystemMagic)
	}
	return "procfs", nil
}

// volumeFilesystemFamily returns only a conservative filesystem-family
// precheck for a mounted hostPath. It deliberately does not claim that the
// filesystem supports the specific idmapped mount operation; that remains a
// live Phase-0 assertion on the target kernel, runtime, and mount.
func volumeFilesystemFamily(path string) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", err
	}
	// These are the filesystem families eligible for the conservative
	// user-namespace/idmap family precheck in the v3 proposal. Unknown
	// filesystems fail closed; this list is not proof that an idmapped mount
	// will work.
	known := map[int64]string{
		0xEF53:     "ext4",
		0x58465342: "xfs",
		0x9123683E: "btrfs",
		0x794C7630: "overlayfs",
	}
	name, ok := known[int64(stat.Type)]
	if !ok {
		return "", fmt.Errorf("unsupported filesystem magic 0x%x", uint64(stat.Type))
	}
	return name, nil
}

func runVersion(ctx context.Context, path string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, path, "--version")
	command.Env = []string{"PATH=/usr/bin:/bin"}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return "", err
	}
	raw := output.Bytes()
	if len(raw) == 0 || len(raw) > maxProbeOutput || bytes.IndexByte(raw, 0) >= 0 {
		return "", errors.New("version output is missing or too large")
	}
	return string(raw), nil
}

func digestFingerprint(domain string, values ...string) string {
	hash := sha256.New()
	for _, value := range append([]string{domain}, values...) {
		_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, value)
		_, _ = io.WriteString(hash, "\x00")
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func brokerResolveAPI(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := parsed.Hostname()
	if net.ParseIP(host) != nil {
		return host, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupHost(probeCtx, host)
	if err != nil {
		return "", err
	}
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil {
			return ip.String(), nil
		}
	}
	return "", errors.New("API service resolved without an IP address")
}

func runAgentProbe(ctx context.Context, cfg config) error {
	reportValue := agentReport{SchemaVersion: 1, Checks: make([]check, 0, 8)}
	add := func(name string, passed bool, detail string) {
		status := "failed"
		if passed {
			status = "passed"
		}
		reportValue.Checks = append(reportValue.Checks, check{Name: name, Status: status, Detail: boundDetail(detail)})
	}
	add("uid", os.Geteuid() == 1000, "agent probe UID must be 1000")
	add("service-account-token-absent", !pathExists("/var/run/secrets/kubernetes.io/serviceaccount/token") && !pathExists(defaultTokenFile), "agent probe has no service-account token mount")
	add("ambient-credential-absent", !ambientCredentialEnv(), "agent probe has no credential-like environment variables")

	target, err := waitForTarget(ctx, cfg.EvidenceDir+"/api-target", cfg.AgentWait)
	if err != nil {
		add("api-server-denied", false, "API probe target was not published")
		add("dns-denied", false, "DNS probe target was not published")
	} else {
		add("api-server-denied", deniedTCP(ctx, target), "non-broker UID cannot connect to the Kubernetes API service")
		add("dns-denied", deniedDNS(ctx, cfg.APIURL), "non-broker UID cannot resolve the Kubernetes API service")
	}
	reportValue.Passed = allNamedChecksPassed(reportValue.Checks, requiredAgentChecks)
	raw, err := json.Marshal(reportValue)
	if err != nil {
		return err
	}
	if len(raw) > maxAgentResultSize {
		return errors.New("agent probe report is too large")
	}
	if err := writeAtomic(cfg.EvidenceDir+"/agent.json", raw, 0644); err != nil {
		return fmt.Errorf("write agent probe report: %w", err)
	}
	if !reportValue.Passed {
		return errors.New("agent probe failed")
	}
	return nil
}

func deniedTCP(ctx context.Context, target string) bool {
	ip := net.ParseIP(strings.TrimSpace(target))
	if ip == nil {
		return false
	}
	network := "tcp6"
	if ip.To4() != nil {
		network = "tcp4"
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), "443"))
	if err == nil {
		_ = connection.Close()
		return false
	}
	return true
}

func deniedDNS(ctx context.Context, rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || net.ParseIP(parsed.Hostname()) != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = net.DefaultResolver.LookupHost(probeCtx, parsed.Hostname())
	return err != nil
}

func waitForTarget(ctx context.Context, path string, maxWait time.Duration) (string, error) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		raw, err := readBoundedFile(path, 128)
		if err == nil {
			value := strings.TrimSpace(string(raw))
			if net.ParseIP(value) != nil {
				return value, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", errors.New("timed out waiting for API probe target")
}

func waitForAgent(ctx context.Context, path string, maxWait time.Duration) (agentReport, error) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		raw, err := readBoundedFile(path, maxAgentResultSize)
		if err == nil {
			var result agentReport
			if strictjson.ValidateObject(raw) == nil && decodeStrict(raw, &result) == nil && result.SchemaVersion == 1 && result.Passed && allNamedChecksPassed(result.Checks, requiredAgentChecks) {
				return result, nil
			}
			return agentReport{}, errors.New("agent probe report is malformed")
		}
		select {
		case <-ctx.Done():
			return agentReport{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return agentReport{}, errors.New("timed out waiting for agent probe")
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func allChecksPassed(checks []check) bool {
	if len(checks) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(checks))
	for _, item := range checks {
		if item.Name == "" || item.Status != "passed" {
			return false
		}
		if _, exists := seen[item.Name]; exists {
			return false
		}
		seen[item.Name] = struct{}{}
	}
	return true
}

func allRequiredChecksPassed(checks []check) bool {
	return allNamedChecksPassed(checks, requiredAttestorChecks)
}

func allNamedChecksPassed(checks []check, required []string) bool {
	if len(checks) != len(required) || !allChecksPassed(checks) {
		return false
	}
	wanted := make(map[string]struct{}, len(required))
	for _, name := range required {
		if name == "" {
			return false
		}
		wanted[name] = struct{}{}
	}
	if len(wanted) != len(required) {
		return false
	}
	for _, item := range checks {
		if _, exists := wanted[item.Name]; !exists {
			return false
		}
	}
	return true
}

func fingerprintDetail(f fingerprints, cfg config) string {
	if f.Node != cfg.ExpectedNodeFingerprint || f.Runtime != cfg.ExpectedRuntimeFingerprint || f.NodeName != cfg.ExpectedNodeName {
		return "selected node or runtime fingerprint did not match configured evidence"
	}
	return "selected node and runtime fingerprints matched"
}

func boundDetail(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || unicodeControl(r) {
			return ' '
		}
		return r
	}, value)
	if len(value) > maxCheckDetail {
		return value[:maxCheckDetail]
	}
	return value
}

func unicodeControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

func ambientCredentialEnv() bool {
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PRIVATE_KEY", "API_KEY"} {
			if strings.Contains(upper, marker) {
				return true
			}
		}
	}
	return false
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func readBoundedFile(path string, max int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("file exceeds bounded read limit")
	}
	return data, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if len(path) == 0 || len(data) > maxAgentResultSize {
		return errors.New("atomic write is outside bounds")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agw-preflight-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "agw-preflight: %s\n", boundDetail(err.Error()))
	}
	os.Exit(1)
}
