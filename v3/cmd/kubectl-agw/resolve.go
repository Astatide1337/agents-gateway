package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	agwsandbox "github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	agentRunResource = "agentruns.agents.astatide.com"
	sandboxResource  = "sandboxes.agents.x-k8s.io"
	jobResource      = "jobs.batch"
	podResource      = "pods"

	runUIDLabelKey    = "agents.astatide.com/run-uid"
	runRoleLabelKey   = "agents.astatide.com/role"
	specDigestLabel   = "agents.astatide.com/spec-digest"
	specDigestAnn     = "agents.astatide.com/spec-digest"
	sandboxPodAnn     = "agents.x-k8s.io/pod-name"
	sandboxAPIVersion = "agents.x-k8s.io/v1beta1"
	sandboxKind       = "Sandbox"
	jobAPIVersion     = "batch/v1"
	jobKind           = "Job"
	podAPIVersion     = "v1"
	podKind           = "Pod"
	jobControllerUID  = "batch.kubernetes.io/controller-uid"
	jobNameLabel      = "batch.kubernetes.io/job-name"
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type objectMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []ownerReference  `json:"ownerReferences"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}

type childReference struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	UID             string `json:"uid"`
	Role            string `json:"role"`
	SpecDigest      string `json:"specDigest"`
	PlanFingerprint string `json:"planFingerprint"`
}

type agentRunSnapshot struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetadata `json:"metadata"`
	Spec       struct {
		GateRef string `json:"gateRef"`
		Source  struct {
			Repo string `json:"repo"`
		} `json:"source"`
	} `json:"spec"`
	Status struct {
		Phase            string          `json:"phase"`
		SpecDigest       string          `json:"specDigest"`
		BaseSHA          string          `json:"baseSHA"`
		WorkSandboxRef   *childReference `json:"workSandboxRef"`
		VerifySandboxRef *childReference `json:"verifySandboxRef"`
		Patch            *struct {
			Ref *artifactReference `json:"ref"`
		} `json:"patch"`
		Gate *struct {
			Name       string             `json:"name"`
			UID        string             `json:"uid"`
			Generation int64              `json:"generation"`
			Mode       string             `json:"mode"`
			Verdict    string             `json:"verdict"`
			ReportRef  *artifactReference `json:"reportRef"`
		} `json:"gate"`
	} `json:"status"`
}

type artifactReference struct {
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

type agentRunListSnapshot struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Items      []agentRunSnapshot `json:"items"`
}

type configMapSnapshot struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   objectMetadata    `json:"metadata"`
	Immutable  *bool             `json:"immutable"`
	Data       map[string]string `json:"data"`
}

type configMapListSnapshot struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Items      []configMapSnapshot `json:"items"`
}

type sandboxSnapshot struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetadata `json:"metadata"`
}

type sandboxListSnapshot struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Items      []sandboxSnapshot `json:"items"`
}

type jobSnapshot struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetadata `json:"metadata"`
}

type podListSnapshot struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Items      []podSnapshot `json:"items"`
}

type podSnapshot struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetadata `json:"metadata"`
	Spec       struct {
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
}

type sandboxTarget struct {
	Name            string
	Kind            string
	UID             string
	SpecDigest      string
	PlanFingerprint string
}

func deterministicSandboxName(runUID, role string) string {
	hash := sha256.Sum256([]byte(runUID + "\x00" + role))
	return "agw-" + role + "-" + hex.EncodeToString(hash[:10])
}

func (c *CLI) resolvePodForLogs(ctx context.Context, globals []string, namespace, runName, container string) (string, error) {
	runData, err := c.capture(ctx, globals, "get AgentRun", "get", agentRunResource, runName, "--namespace", namespace, "--output", "json")
	if err != nil {
		return "", err
	}
	var run agentRunSnapshot
	if err := decodeBoundedJSON(runData, &run); err != nil {
		return "", fmt.Errorf("decode AgentRun %s/%s: %w", namespace, runName, err)
	}
	if err := validateRunSnapshot(run, namespace, runName); err != nil {
		return "", err
	}

	role := "work"
	if container == "verify" {
		role = "verify"
	}
	var reference *childReference
	if role == "work" {
		reference = run.Status.WorkSandboxRef
	} else {
		reference = run.Status.VerifySandboxRef
	}
	var target sandboxTarget
	if reference != nil {
		if err := validateChildReference(*reference, run.Metadata.UID, role); err != nil {
			return "", err
		}
		target = sandboxTarget{Name: reference.Name, Kind: reference.Kind, UID: reference.UID, SpecDigest: reference.SpecDigest, PlanFingerprint: reference.PlanFingerprint}
	} else {
		target, err = c.findSandboxByLabels(ctx, globals, namespace, run, role)
		if err != nil {
			return "", err
		}
	}

	if target.Kind == jobKind {
		return c.resolveJobPod(ctx, globals, namespace, run, role, target, container)
	}
	if target.Kind != sandboxKind {
		return "", fmt.Errorf("AgentRun %s/%s references unsupported child kind %q", namespace, runName, target.Kind)
	}
	return c.resolveSandboxPod(ctx, globals, namespace, run, role, target, container)
}

func (c *CLI) resolveSandboxPod(ctx context.Context, globals []string, namespace string, run agentRunSnapshot, role string, target sandboxTarget, container string) (string, error) {
	sandboxData, err := c.capture(ctx, globals, "get Sandbox", "get", sandboxResource, target.Name, "--namespace", namespace, "--output", "json")
	if err != nil {
		return "", err
	}
	var sandbox sandboxSnapshot
	if err := decodeBoundedJSON(sandboxData, &sandbox); err != nil {
		return "", fmt.Errorf("decode Sandbox %s/%s: %w", namespace, target.Name, err)
	}
	if err := validateSandboxSnapshot(sandbox, namespace, run, role, target); err != nil {
		return "", err
	}

	podName := sandbox.Metadata.Annotations[sandboxPodAnn]
	if podName == "" {
		// The agent-sandbox controller uses the Sandbox name for a cold-created
		// pod. Warm-pool adoption is represented by the validated annotation.
		podName = sandbox.Metadata.Name
	}
	if problems := validation.IsDNS1123Subdomain(podName); len(problems) > 0 {
		return "", fmt.Errorf("Sandbox %s/%s resolved an invalid pod name %q", namespace, sandbox.Metadata.Name, podName)
	}
	podData, err := c.capture(ctx, globals, "get Pod", "get", podResource, podName, "--namespace", namespace, "--output", "json")
	if err != nil {
		return "", err
	}
	var pod podSnapshot
	if err := decodeBoundedJSON(podData, &pod); err != nil {
		return "", fmt.Errorf("decode Pod %s/%s: %w", namespace, podName, err)
	}
	if err := validatePodSnapshot(pod, namespace, podName, sandbox, run, container); err != nil {
		return "", err
	}
	return podName, nil
}

func (c *CLI) resolveJobPod(ctx context.Context, globals []string, namespace string, run agentRunSnapshot, role string, target sandboxTarget, container string) (string, error) {
	jobData, err := c.capture(ctx, globals, "get Job", "get", jobResource, target.Name, "--namespace", namespace, "--output", "json")
	if err != nil {
		return "", err
	}
	var job jobSnapshot
	if err := decodeBoundedJSON(jobData, &job); err != nil {
		return "", fmt.Errorf("decode Job %s/%s: %w", namespace, target.Name, err)
	}
	if err := validateJobSnapshot(job, namespace, run, role, target); err != nil {
		return "", err
	}
	selector := runUIDLabelKey + "=" + run.Metadata.UID + "," + runRoleLabelKey + "=" + role + "," + agwsandbox.ChildNameLabelKey + "=" + job.Metadata.Name + "," + jobControllerUID + "=" + job.Metadata.UID
	podData, err := c.capture(ctx, globals, "list Job Pods", "get", podResource, "--namespace", namespace, "--selector", selector, "--output", "json")
	if err != nil {
		return "", err
	}
	var pods podListSnapshot
	if err := decodeBoundedJSON(podData, &pods); err != nil {
		return "", fmt.Errorf("decode Job pod list for %s/%s: %w", namespace, job.Metadata.Name, err)
	}
	if pods.APIVersion != podAPIVersion || pods.Kind != "PodList" {
		return "", fmt.Errorf("Job pod list for %s/%s has unexpected type %s %s", namespace, job.Metadata.Name, pods.APIVersion, pods.Kind)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no Pod currently owned by Job %s/%s", namespace, job.Metadata.Name)
	}
	if len(pods.Items) != 1 {
		return "", fmt.Errorf("expected exactly one Pod for Job %s/%s, found %d", namespace, job.Metadata.Name, len(pods.Items))
	}
	pod := pods.Items[0]
	if err := validateJobPodSnapshot(pod, namespace, job, run, role, container); err != nil {
		return "", err
	}
	return pod.Metadata.Name, nil
}

func (c *CLI) findSandboxByLabels(ctx context.Context, globals []string, namespace string, run agentRunSnapshot, role string) (sandboxTarget, error) {
	selector := runUIDLabelKey + "=" + run.Metadata.UID + "," + runRoleLabelKey + "=" + role
	data, err := c.capture(ctx, globals, "list Sandboxes", "get", sandboxResource, "--namespace", namespace, "--selector", selector, "--output", "json")
	if err != nil {
		return sandboxTarget{}, err
	}
	var list sandboxListSnapshot
	if err := decodeBoundedJSON(data, &list); err != nil {
		return sandboxTarget{}, fmt.Errorf("decode Sandbox list for %s/%s: %w", namespace, run.Metadata.Name, err)
	}
	if list.APIVersion != "agents.x-k8s.io/v1beta1" || list.Kind != "SandboxList" {
		return sandboxTarget{}, fmt.Errorf("Sandbox list for %s/%s has unexpected type %s %s", namespace, run.Metadata.Name, list.APIVersion, list.Kind)
	}
	if len(list.Items) == 0 {
		return sandboxTarget{}, fmt.Errorf("no %s Sandbox is currently referenced by AgentRun %s/%s", role, namespace, run.Metadata.Name)
	}
	if len(list.Items) != 1 {
		return sandboxTarget{}, fmt.Errorf("expected one %s Sandbox for AgentRun %s/%s, found %d", role, namespace, run.Metadata.Name, len(list.Items))
	}
	candidate := list.Items[0]
	target := sandboxTarget{Name: candidate.Metadata.Name, Kind: sandboxKind, UID: candidate.Metadata.UID, SpecDigest: run.Status.SpecDigest, PlanFingerprint: candidate.Metadata.Annotations[agwsandbox.SandboxSpecFingerprintAnnotationKey]}
	if err := validateSandboxSnapshot(candidate, namespace, run, role, target); err != nil {
		return sandboxTarget{}, err
	}
	return target, nil
}

func validateRunSnapshot(run agentRunSnapshot, namespace, name string) error {
	if run.APIVersion != agentRunAPIVersion || run.Kind != agentRunKind {
		return fmt.Errorf("AgentRun %s/%s has unexpected type %s %s", namespace, name, run.APIVersion, run.Kind)
	}
	if run.Metadata.Name != name || run.Metadata.Namespace != namespace {
		return fmt.Errorf("AgentRun response identity does not match %s/%s", namespace, name)
	}
	if !safeServerIdentifier(run.Metadata.UID) {
		return fmt.Errorf("AgentRun %s/%s has no usable UID", namespace, name)
	}
	if run.Status.SpecDigest != "" && !digestPattern.MatchString(run.Status.SpecDigest) {
		return fmt.Errorf("AgentRun %s/%s has an invalid status.specDigest", namespace, name)
	}
	return nil
}

func validateChildReference(reference childReference, runUID, role string) error {
	if !agwsandbox.ValidChildKind(reference.Kind) {
		return fmt.Errorf("AgentRun status references unexpected %s child kind %q", role, reference.Kind)
	}
	expected := deterministicSandboxName(runUID, role)
	if reference.Name != expected {
		return fmt.Errorf("AgentRun status references non-deterministic %s Sandbox %q, expected %q", role, reference.Name, expected)
	}
	if reference.UID != "" && !safeServerIdentifier(reference.UID) {
		return fmt.Errorf("AgentRun status contains an invalid %s child UID", role)
	}
	if reference.SpecDigest != "" && !digestPattern.MatchString(reference.SpecDigest) {
		return fmt.Errorf("AgentRun status contains an invalid %s child specDigest", role)
	}
	if !digestPattern.MatchString(reference.PlanFingerprint) {
		return fmt.Errorf("AgentRun status contains an invalid %s child planFingerprint", role)
	}
	return nil
}

func validateSandboxSnapshot(sandbox sandboxSnapshot, namespace string, run agentRunSnapshot, role string, target sandboxTarget) error {
	if sandbox.APIVersion != sandboxAPIVersion || sandbox.Kind != sandboxKind {
		return fmt.Errorf("Sandbox %s/%s has unexpected type %s %s", namespace, target.Name, sandbox.APIVersion, sandbox.Kind)
	}
	expectedName := deterministicSandboxName(run.Metadata.UID, role)
	if sandbox.Metadata.Name != expectedName || sandbox.Metadata.Name != target.Name || sandbox.Metadata.Namespace != namespace {
		return fmt.Errorf("Sandbox identity is not the expected %s child of AgentRun %s/%s", role, namespace, run.Metadata.Name)
	}
	if !safeServerIdentifier(sandbox.Metadata.UID) {
		return fmt.Errorf("Sandbox %s/%s has no usable UID", namespace, sandbox.Metadata.Name)
	}
	if target.UID != "" && sandbox.Metadata.UID != target.UID {
		return fmt.Errorf("Sandbox %s/%s UID does not match AgentRun status", namespace, sandbox.Metadata.Name)
	}
	if sandbox.Metadata.Labels[runUIDLabelKey] != run.Metadata.UID || sandbox.Metadata.Labels[runRoleLabelKey] != role {
		return fmt.Errorf("Sandbox %s/%s ownership labels do not match AgentRun %s/%s", namespace, sandbox.Metadata.Name, namespace, run.Metadata.Name)
	}
	if run.Status.SpecDigest != "" {
		if sandbox.Metadata.Labels[specDigestLabel] != strings.TrimPrefix(run.Status.SpecDigest, "sha256:") || sandbox.Metadata.Annotations[specDigestAnn] != run.Status.SpecDigest {
			return fmt.Errorf("Sandbox %s/%s spec digest does not match AgentRun status", namespace, sandbox.Metadata.Name)
		}
	}
	if target.SpecDigest != "" && (sandbox.Metadata.Annotations[specDigestAnn] != target.SpecDigest || sandbox.Metadata.Labels[specDigestLabel] != strings.TrimPrefix(target.SpecDigest, "sha256:")) {
		return fmt.Errorf("Sandbox %s/%s spec digest does not match its AgentRun child reference", namespace, sandbox.Metadata.Name)
	}
	if target.PlanFingerprint == "" || sandbox.Metadata.Annotations[agwsandbox.SandboxSpecFingerprintAnnotationKey] != target.PlanFingerprint {
		return fmt.Errorf("Sandbox %s/%s plan fingerprint does not match its AgentRun child reference", namespace, sandbox.Metadata.Name)
	}
	if !hasOwner(sandbox.Metadata.OwnerReferences, agentRunAPIVersion, agentRunKind, run.Metadata.Name, run.Metadata.UID) {
		return fmt.Errorf("Sandbox %s/%s is not controlled by AgentRun %s/%s", namespace, sandbox.Metadata.Name, namespace, run.Metadata.Name)
	}
	return nil
}

func validateJobSnapshot(job jobSnapshot, namespace string, run agentRunSnapshot, role string, target sandboxTarget) error {
	if job.APIVersion != jobAPIVersion || job.Kind != jobKind {
		return fmt.Errorf("Job %s/%s has unexpected type %s %s", namespace, target.Name, job.APIVersion, job.Kind)
	}
	expectedName := deterministicSandboxName(run.Metadata.UID, role)
	if job.Metadata.Name != expectedName || job.Metadata.Name != target.Name || job.Metadata.Namespace != namespace {
		return fmt.Errorf("Job identity is not the expected %s child of AgentRun %s/%s", role, namespace, run.Metadata.Name)
	}
	if !safeServerIdentifier(job.Metadata.UID) {
		return fmt.Errorf("Job %s/%s has no usable UID", namespace, job.Metadata.Name)
	}
	if target.UID != "" && job.Metadata.UID != target.UID {
		return fmt.Errorf("Job %s/%s UID does not match AgentRun status", namespace, job.Metadata.Name)
	}
	if job.Metadata.Labels[runUIDLabelKey] != run.Metadata.UID || job.Metadata.Labels[runRoleLabelKey] != role || job.Metadata.Labels[agwsandbox.ChildNameLabelKey] != job.Metadata.Name {
		return fmt.Errorf("Job %s/%s ownership labels do not match AgentRun %s/%s", namespace, job.Metadata.Name, namespace, run.Metadata.Name)
	}
	if run.Status.SpecDigest != "" && (job.Metadata.Labels[specDigestLabel] != strings.TrimPrefix(run.Status.SpecDigest, "sha256:") || job.Metadata.Annotations[specDigestAnn] != run.Status.SpecDigest) {
		return fmt.Errorf("Job %s/%s spec digest does not match AgentRun status", namespace, job.Metadata.Name)
	}
	if target.SpecDigest != "" && (job.Metadata.Labels[specDigestLabel] != strings.TrimPrefix(target.SpecDigest, "sha256:") || job.Metadata.Annotations[specDigestAnn] != target.SpecDigest) {
		return fmt.Errorf("Job %s/%s spec digest does not match its AgentRun child reference", namespace, job.Metadata.Name)
	}
	if target.PlanFingerprint == "" || job.Metadata.Annotations[agwsandbox.JobSpecFingerprintAnnotationKey] != target.PlanFingerprint {
		return fmt.Errorf("Job %s/%s plan fingerprint does not match its AgentRun child reference", namespace, job.Metadata.Name)
	}
	if !hasOwner(job.Metadata.OwnerReferences, agentRunAPIVersion, agentRunKind, run.Metadata.Name, run.Metadata.UID) {
		return fmt.Errorf("Job %s/%s is not controlled by AgentRun %s/%s", namespace, job.Metadata.Name, namespace, run.Metadata.Name)
	}
	return nil
}

func validateJobPodSnapshot(pod podSnapshot, namespace string, job jobSnapshot, run agentRunSnapshot, role, container string) error {
	if err := validatePodSnapshotType(pod, namespace); err != nil {
		return err
	}
	if pod.Metadata.Namespace != namespace || !safeServerIdentifier(pod.Metadata.UID) {
		return fmt.Errorf("Pod %s/%s has invalid identity", namespace, pod.Metadata.Name)
	}
	if pod.Metadata.Labels[runUIDLabelKey] != run.Metadata.UID || pod.Metadata.Labels[runRoleLabelKey] != role || pod.Metadata.Labels[agwsandbox.ChildNameLabelKey] != job.Metadata.Name || pod.Metadata.Labels[jobControllerUID] != job.Metadata.UID || pod.Metadata.Labels[jobNameLabel] != job.Metadata.Name {
		return fmt.Errorf("Pod %s/%s labels do not identify the expected Job child", namespace, pod.Metadata.Name)
	}
	if !hasOwner(pod.Metadata.OwnerReferences, jobAPIVersion, jobKind, job.Metadata.Name, job.Metadata.UID) {
		return fmt.Errorf("Pod %s/%s is not controlled by Job %s/%s", namespace, pod.Metadata.Name, namespace, job.Metadata.Name)
	}
	return validatePodContainer(pod, namespace, container)
}

func validatePodSnapshot(pod podSnapshot, namespace, expectedName string, sandbox sandboxSnapshot, run agentRunSnapshot, container string) error {
	if err := validatePodSnapshotType(pod, namespace); err != nil {
		return err
	}
	if pod.Metadata.Namespace != namespace || pod.Metadata.Name != expectedName {
		return fmt.Errorf("Pod identity does not match %s/%s", namespace, expectedName)
	}
	if !safeServerIdentifier(pod.Metadata.UID) {
		return fmt.Errorf("Pod %s/%s has no usable UID", namespace, pod.Metadata.Name)
	}
	if !hasOwner(pod.Metadata.OwnerReferences, sandboxAPIVersion, sandboxKind, sandbox.Metadata.Name, sandbox.Metadata.UID) {
		return fmt.Errorf("Pod %s/%s is not controlled by Sandbox %s/%s", namespace, pod.Metadata.Name, namespace, sandbox.Metadata.Name)
	}
	if label := pod.Metadata.Labels[runUIDLabelKey]; label != "" && label != run.Metadata.UID {
		return fmt.Errorf("Pod %s/%s has a conflicting AgentRun label", namespace, pod.Metadata.Name)
	}
	role := "work"
	if container == "verify" {
		role = "verify"
	}
	if label := pod.Metadata.Labels[runRoleLabelKey]; label != "" && label != role {
		return fmt.Errorf("Pod %s/%s has a conflicting AgentRun role label", namespace, pod.Metadata.Name)
	}
	return validatePodContainer(pod, namespace, container)
}

func validatePodSnapshotType(pod podSnapshot, namespace string) error {
	if pod.APIVersion != podAPIVersion || pod.Kind != podKind {
		return fmt.Errorf("Pod %s/%s has unexpected type %s %s", namespace, pod.Metadata.Name, pod.APIVersion, pod.Kind)
	}
	return nil
}

func validatePodContainer(pod podSnapshot, namespace, container string) error {
	for _, candidate := range pod.Spec.Containers {
		if candidate.Name == container {
			return nil
		}
	}
	return fmt.Errorf("Pod %s/%s does not contain container %q", namespace, pod.Metadata.Name, container)
}

func hasOwner(references []ownerReference, apiVersion, kind, name, uid string) bool {
	for _, reference := range references {
		if reference.APIVersion == apiVersion && reference.Kind == kind && reference.Name == name && reference.UID == uid && reference.Controller != nil && *reference.Controller {
			return true
		}
	}
	return false
}

func safeServerIdentifier(value string) bool {
	if value == "" || len(value) > 128 || len(validation.IsValidLabelValue(value)) != 0 {
		return false
	}
	return true
}

func decodeBoundedJSON(data []byte, destination any) error {
	if len(data) == 0 {
		return fmt.Errorf("empty kubectl JSON response")
	}
	if len(data) > maxJSONOutputBytes {
		return fmt.Errorf("kubectl JSON response exceeds %d bytes", maxJSONOutputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("kubectl JSON response contains multiple values")
	} else if err != io.EOF {
		return fmt.Errorf("invalid trailing kubectl JSON: %w", err)
	}
	return nil
}
