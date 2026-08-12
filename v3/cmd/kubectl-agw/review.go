package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/shadowreview"
)

const (
	configMapResource = "configmaps"
	maxMatrixIssues   = 64
)

type selfSubjectReviewSnapshot struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		UserInfo struct {
			Username string   `json:"username"`
			UID      string   `json:"uid"`
			Groups   []string `json:"groups"`
		} `json:"userInfo"`
	} `json:"status"`
}

type matrixReport struct {
	Rows     []shadowreview.MatrixRow `json:"rows"`
	Excluded []string                 `json:"excludedReviews,omitempty"`
}

type matrixJSONRow struct {
	Repository     string `json:"repository"`
	GateRef        string `json:"gateRef"`
	GateName       string `json:"gateName"`
	GateUID        string `json:"gateUID"`
	GateGeneration int64  `json:"gateGeneration"`
	AcceptedGood   int    `json:"acceptedGood"`
	AcceptedBad    int    `json:"acceptedBad"`
	RejectedGood   int    `json:"rejectedGood"`
	RejectedBad    int    `json:"rejectedBad"`
	FalseAccepts   int    `json:"falseAccepts"`
	Total          int    `json:"total"`
}

func (c *CLI) review(ctx context.Context, globals []string, options reviewOptions) error {
	reviewer, err := c.currentReviewer(ctx, globals)
	if err != nil {
		return err
	}
	run, err := c.getAgentRunForReview(ctx, globals, options.namespace, options.run)
	if err != nil {
		return err
	}
	review, err := reviewFromRun(run, options.classification, reviewer)
	if err != nil {
		return err
	}
	body, err := shadowreview.CanonicalBytes(review)
	if err != nil {
		return err
	}
	digest, err := shadowreview.Digest(review)
	if err != nil {
		return err
	}
	name, err := shadowreview.ConfigMapName(review.RunUID)
	if err != nil {
		return err
	}

	existing, exists, err := c.getConfigMap(ctx, globals, options.namespace, name)
	if err != nil {
		return err
	}
	if exists {
		return c.finishExistingReview(existing, name, body, digest)
	}

	configMap, err := reviewConfigMap(options.namespace, name, body, digest)
	if err != nil {
		return err
	}
	// Create is intentionally handled with a single direct invocation so the
	// exact bytes are visible in tests and no shell or kubectl apply merge can
	// overwrite an earlier review.
	_, err = c.createReviewConfigMap(ctx, globals, options.namespace, configMap)
	if err == nil {
		fmt.Fprintf(c.stdout, "configmap/%s created %s\n", name, digest)
		return nil
	}
	// A concurrent creator is safe only when the immutable payload matches
	// byte-for-byte. Any other failure remains an error.
	existingAfter, found, getErr := c.getConfigMap(ctx, globals, options.namespace, name)
	if getErr != nil {
		return fmt.Errorf("create shadow review: %v; verify concurrent result: %w", err, getErr)
	}
	if !found {
		return err
	}
	return c.finishExistingReview(existingAfter, name, body, digest)
}

func (c *CLI) currentReviewer(ctx context.Context, globals []string) (shadowreview.Reviewer, error) {
	data, err := c.capture(ctx, globals, "identify Kubernetes reviewer", "auth", "whoami", "--output", "json")
	if err != nil {
		return shadowreview.Reviewer{}, fmt.Errorf("cannot establish Kubernetes reviewer identity: %w", err)
	}
	var response selfSubjectReviewSnapshot
	if err := decodeBoundedJSON(data, &response); err != nil {
		return shadowreview.Reviewer{}, fmt.Errorf("decode Kubernetes reviewer identity: %w", err)
	}
	if response.Kind != "SelfSubjectReview" || response.Status.UserInfo.Username == "" {
		return shadowreview.Reviewer{}, fmt.Errorf("Kubernetes reviewer identity is unavailable or unsupported")
	}
	return shadowreview.Reviewer{
		Username: response.Status.UserInfo.Username,
		UID:      response.Status.UserInfo.UID,
		Groups:   append([]string(nil), response.Status.UserInfo.Groups...),
	}, nil
}

func (c *CLI) getAgentRunForReview(ctx context.Context, globals []string, namespace, name string) (agentRunSnapshot, error) {
	data, err := c.capture(ctx, globals, "get AgentRun for shadow review", "get", agentRunResource, name, "--namespace", namespace, "--output", "json")
	if err != nil {
		return agentRunSnapshot{}, err
	}
	var run agentRunSnapshot
	if err := decodeBoundedJSON(data, &run); err != nil {
		return agentRunSnapshot{}, fmt.Errorf("decode AgentRun %s/%s: %w", namespace, name, err)
	}
	if err := validateRunSnapshot(run, namespace, name); err != nil {
		return agentRunSnapshot{}, err
	}
	return run, nil
}

func reviewFromRun(run agentRunSnapshot, classification string, reviewer shadowreview.Reviewer) (shadowreview.Review, error) {
	if run.Status.Phase != "Succeeded" && run.Status.Phase != "Rejected" {
		return shadowreview.Review{}, fmt.Errorf("AgentRun %s/%s is not terminal: phase %q", run.Metadata.Namespace, run.Metadata.Name, run.Status.Phase)
	}
	if run.Spec.Source.Repo == "" || run.Spec.GateRef == "" || run.Status.SpecDigest == "" || run.Status.BaseSHA == "" || run.Status.Patch == nil || run.Status.Patch.Ref == nil || run.Status.Gate == nil || run.Status.Gate.ReportRef == nil {
		return shadowreview.Review{}, fmt.Errorf("AgentRun %s/%s does not expose the complete immutable review binding", run.Metadata.Namespace, run.Metadata.Name)
	}
	gate := run.Status.Gate
	if gate.Mode != "shadow" || gate.Name == "" || gate.UID == "" || gate.Generation <= 0 || (gate.Verdict != "Accepted" && gate.Verdict != "Rejected") {
		return shadowreview.Review{}, fmt.Errorf("AgentRun %s/%s does not contain a complete shadow Gate identity and verdict", run.Metadata.Namespace, run.Metadata.Name)
	}
	return shadowreview.Review{
		SchemaVersion: shadowreview.SchemaVersion, ArtifactType: shadowreview.ArtifactType,
		RunUID: run.Metadata.UID, Namespace: run.Metadata.Namespace, RunName: run.Metadata.Name,
		Repository: run.Spec.Source.Repo, GateRef: run.Spec.GateRef, GateName: gate.Name, GateUID: gate.UID,
		GateGeneration: gate.Generation, GateMode: gate.Mode, MachineVerdict: gate.Verdict,
		SpecDigest: run.Status.SpecDigest, BaseSHA: run.Status.BaseSHA, PatchDigest: run.Status.Patch.Ref.Digest,
		ReportDigest: gate.ReportRef.Digest, DiffClassification: shadowreview.Classification(classification),
		Reviewer: reviewer, Provenance: shadowreview.Provenance,
	}, nil
}

func reviewConfigMap(namespace, name string, body []byte, digest string) ([]byte, error) {
	if namespace == "" || name == "" || len(body) == 0 || digest == "" {
		return nil, fmt.Errorf("invalid immutable shadow review ConfigMap input")
	}
	configMap := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]string{
				"agents.astatide.com/review": "shadow",
			},
			"annotations": map[string]string{
				"agents.astatide.com/review-digest": digest,
			},
		},
		"immutable": true,
		"data":      map[string]string{shadowreview.ConfigMapDataKey: string(body)},
	}
	encoded, err := json.Marshal(configMap)
	if err != nil {
		return nil, fmt.Errorf("marshal shadow review ConfigMap: %w", err)
	}
	return encoded, nil
}

func (c *CLI) createReviewConfigMap(ctx context.Context, globals []string, namespace string, body []byte) (bool, error) {
	stdout := newBoundedBuffer(maxJSONOutputBytes)
	_, err := c.invokeWithWriters(ctx, globals, "create immutable shadow review", body, stdout, "create", "--filename", "-", "--namespace", namespace, "--output", "name")
	if err != nil {
		return false, err
	}
	if stdout.truncated {
		return false, fmt.Errorf("create immutable shadow review output exceeds %d bytes", maxJSONOutputBytes)
	}
	return true, nil
}

func (c *CLI) getConfigMap(ctx context.Context, globals []string, namespace, name string) (configMapSnapshot, bool, error) {
	data, err := c.capture(ctx, globals, "get shadow review ConfigMap", "get", configMapResource, name, "--namespace", namespace, "--ignore-not-found=true", "--output", "json")
	if err != nil {
		return configMapSnapshot{}, false, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return configMapSnapshot{}, false, nil
	}
	var configMap configMapSnapshot
	if err := decodeBoundedJSON(data, &configMap); err != nil {
		return configMapSnapshot{}, false, fmt.Errorf("decode shadow review ConfigMap %s/%s: %w", namespace, name, err)
	}
	if configMap.APIVersion != "v1" || configMap.Kind != "ConfigMap" || configMap.Metadata.Namespace != namespace || configMap.Metadata.Name != name {
		return configMapSnapshot{}, false, fmt.Errorf("shadow review ConfigMap identity does not match %s/%s", namespace, name)
	}
	return configMap, true, nil
}

func (c *CLI) finishExistingReview(configMap configMapSnapshot, name string, wantBody []byte, digest string) error {
	if configMap.Immutable == nil || !*configMap.Immutable || len(configMap.Data) != 1 || configMap.Data[shadowreview.ConfigMapDataKey] == "" {
		return fmt.Errorf("shadow review ConfigMap %s exists but is not an immutable review record", name)
	}
	gotBody := []byte(configMap.Data[shadowreview.ConfigMapDataKey])
	if !bytes.Equal(gotBody, wantBody) {
		return fmt.Errorf("shadow review for %s already exists with different immutable content; refusing overwrite", name)
	}
	gotReview, err := shadowreview.ParseCanonicalBytes(gotBody)
	if err != nil {
		return fmt.Errorf("existing shadow review %s is invalid: %w", name, err)
	}
	gotDigest, err := shadowreview.Digest(gotReview)
	if err != nil || gotDigest != digest {
		return fmt.Errorf("existing shadow review %s digest does not match", name)
	}
	fmt.Fprintf(c.stdout, "configmap/%s already exists (identical) %s\n", name, digest)
	return nil
}

func (c *CLI) matrix(ctx context.Context, globals []string, options matrixOptions) error {
	runs, err := c.listAgentRuns(ctx, globals, options.namespace)
	if err != nil {
		return err
	}
	configMaps, err := c.listConfigMaps(ctx, globals, options.namespace)
	if err != nil {
		return err
	}
	runByUID := make(map[string]agentRunSnapshot, len(runs.Items))
	issues := make([]string, 0)
	for _, run := range runs.Items {
		if !safeServerIdentifier(run.Metadata.UID) {
			continue
		}
		if _, exists := runByUID[run.Metadata.UID]; exists {
			issues = appendIssue(issues, fmt.Sprintf("duplicate AgentRun UID %s", run.Metadata.UID))
			continue
		}
		runByUID[run.Metadata.UID] = run
	}
	reviews := make([]shadowreview.Review, 0)
	seenRuns := make(map[string]string)
	reviewRecords := make(map[string]struct{})
	for _, configMap := range configMaps.Items {
		encoded, ok := configMap.Data[shadowreview.ConfigMapDataKey]
		if !ok {
			continue
		}
		review, parseErr := shadowreview.ParseCanonicalBytes([]byte(encoded))
		if parseErr != nil {
			issues = appendIssue(issues, fmt.Sprintf("ConfigMap %s: %v", configMap.Metadata.Name, parseErr))
			continue
		}
		if options.repository != "" && review.Repository != options.repository {
			continue
		}
		if options.gate != "" && review.GateRef != options.gate && review.GateName != options.gate {
			continue
		}
		// Record a syntactically valid label after filters but before the
		// remaining binding checks. A malformed, non-immutable, or mismatched
		// record is already reported below; it should not also be described as
		// absent. A review for a different repository/Gate must not mask a
		// missing label when a filtered matrix is requested.
		reviewRecords[review.RunUID] = struct{}{}
		if configMap.Immutable == nil || !*configMap.Immutable {
			issues = appendIssue(issues, fmt.Sprintf("ConfigMap %s is not immutable", configMap.Metadata.Name))
			continue
		}
		expectedName, nameErr := shadowreview.ConfigMapName(review.RunUID)
		if nameErr != nil || configMap.Metadata.Name != expectedName {
			issues = appendIssue(issues, fmt.Sprintf("ConfigMap %s has a non-deterministic review name", configMap.Metadata.Name))
			continue
		}
		run, exists := runByUID[review.RunUID]
		if !exists {
			issues = appendIssue(issues, fmt.Sprintf("review %s has no live AgentRun with UID %s", configMap.Metadata.Name, review.RunUID))
			continue
		}
		if previous, exists := seenRuns[review.RunUID]; exists {
			issues = appendIssue(issues, fmt.Sprintf("run UID %s has multiple review records (%s and %s)", review.RunUID, previous, configMap.Metadata.Name))
			continue
		}
		if bindErr := validateReviewBinding(review, run, options.namespace); bindErr != nil {
			issues = appendIssue(issues, fmt.Sprintf("ConfigMap %s: %v", configMap.Metadata.Name, bindErr))
			continue
		}
		seenRuns[review.RunUID] = configMap.Metadata.Name
		reviews = append(reviews, review)
	}
	// Completeness is part of the evidence contract: a matrix over only the
	// labels that happen to exist can otherwise make an unlabelled terminal
	// shadow run disappear from the sample. Only terminal runs with a complete
	// shadow Gate verdict are candidates; in-progress, failed, and non-shadow
	// runs are intentionally outside the human-labeling protocol.
	for _, run := range runs.Items {
		if !isShadowReviewCandidate(run) || (options.repository != "" && run.Spec.Source.Repo != options.repository) ||
			(options.gate != "" && run.Spec.GateRef != options.gate && (run.Status.Gate == nil || run.Status.Gate.Name != options.gate)) {
			continue
		}
		if _, exists := reviewRecords[run.Metadata.UID]; !exists {
			issues = appendIssue(issues, fmt.Sprintf("AgentRun %s/%s has no shadow review record", run.Metadata.Namespace, run.Metadata.Name))
		}
	}
	rows := shadowreview.DeriveMatrix(reviews)
	if err := c.writeMatrix(options.output, matrixReport{Rows: rows, Excluded: issues}); err != nil {
		return err
	}
	if len(issues) != 0 {
		return fmt.Errorf("shadow matrix is incomplete: %d review record(s) excluded", len(issues))
	}
	return nil
}

func isShadowReviewCandidate(run agentRunSnapshot) bool {
	if run.Status.Phase != "Succeeded" && run.Status.Phase != "Rejected" {
		return false
	}
	if run.Status.Gate == nil || run.Status.Gate.Mode != "shadow" {
		return false
	}
	return run.Status.Gate.Verdict == "Accepted" || run.Status.Gate.Verdict == "Rejected"
}

func (c *CLI) listAgentRuns(ctx context.Context, globals []string, namespace string) (agentRunListSnapshot, error) {
	data, err := c.capture(ctx, globals, "list AgentRuns for shadow matrix", "get", agentRunResource, "--namespace", namespace, "--output", "json")
	if err != nil {
		return agentRunListSnapshot{}, err
	}
	var list agentRunListSnapshot
	if err := decodeBoundedJSON(data, &list); err != nil {
		return agentRunListSnapshot{}, fmt.Errorf("decode AgentRun list: %w", err)
	}
	if list.Kind != "AgentRunList" {
		return agentRunListSnapshot{}, fmt.Errorf("AgentRun list has unexpected kind %q", list.Kind)
	}
	return list, nil
}

func (c *CLI) listConfigMaps(ctx context.Context, globals []string, namespace string) (configMapListSnapshot, error) {
	data, err := c.capture(ctx, globals, "list ConfigMaps for shadow matrix", "get", configMapResource, "--namespace", namespace, "--output", "json")
	if err != nil {
		return configMapListSnapshot{}, err
	}
	var list configMapListSnapshot
	if err := decodeBoundedJSON(data, &list); err != nil {
		return configMapListSnapshot{}, fmt.Errorf("decode ConfigMap list: %w", err)
	}
	if list.Kind != "ConfigMapList" {
		return configMapListSnapshot{}, fmt.Errorf("ConfigMap list has unexpected kind %q", list.Kind)
	}
	return list, nil
}

func validateReviewBinding(review shadowreview.Review, run agentRunSnapshot, namespace string) error {
	if run.Metadata.Namespace != namespace || run.Metadata.UID != review.RunUID || run.Metadata.Name != review.RunName || review.Namespace != namespace {
		return fmt.Errorf("review binding does not match AgentRun identity")
	}
	if run.Status.Phase != "Succeeded" && run.Status.Phase != "Rejected" {
		return fmt.Errorf("review binding targets a non-terminal AgentRun phase %q", run.Status.Phase)
	}
	if run.Spec.Source.Repo != review.Repository || run.Spec.GateRef != review.GateRef || run.Status.SpecDigest != review.SpecDigest || run.Status.BaseSHA != review.BaseSHA {
		return fmt.Errorf("review binding does not match repository, Gate reference, spec digest, or base SHA")
	}
	if run.Status.Patch == nil || run.Status.Patch.Ref == nil || run.Status.Patch.Ref.Digest != review.PatchDigest || run.Status.Gate == nil || run.Status.Gate.ReportRef == nil || run.Status.Gate.ReportRef.Digest != review.ReportDigest {
		return fmt.Errorf("review binding does not match patch or report digest")
	}
	gate := run.Status.Gate
	if gate.Name != review.GateName || gate.UID != review.GateUID || gate.Generation != review.GateGeneration || gate.Mode != review.GateMode || gate.Mode != "shadow" || gate.Verdict != review.MachineVerdict {
		return fmt.Errorf("review binding does not match the controller-projected Gate revision and verdict")
	}
	return nil
}

func appendIssue(issues []string, value string) []string {
	if len(issues) < maxMatrixIssues {
		return append(issues, value)
	}
	if len(issues) == maxMatrixIssues {
		return append(issues, "additional review issues omitted")
	}
	return issues
}

func (c *CLI) writeMatrix(output string, report matrixReport) error {
	if output == "json" {
		rows := make([]matrixJSONRow, 0, len(report.Rows))
		for _, row := range report.Rows {
			rows = append(rows, matrixJSONRow{
				Repository: row.Repository, GateRef: row.GateRef, GateName: row.GateName, GateUID: row.GateUID, GateGeneration: row.GateGeneration,
				AcceptedGood: row.AcceptedGood, AcceptedBad: row.AcceptedBad, RejectedGood: row.RejectedGood, RejectedBad: row.RejectedBad,
				FalseAccepts: row.FalseAccepts(), Total: row.Total(),
			})
		}
		encoded, err := json.Marshal(struct {
			Rows     []matrixJSONRow `json:"rows"`
			Excluded []string        `json:"excludedReviews,omitempty"`
		}{Rows: rows, Excluded: report.Excluded})
		if err != nil {
			return fmt.Errorf("encode shadow matrix: %w", err)
		}
		fmt.Fprintln(c.stdout, string(encoded))
		return nil
	}
	fmt.Fprintln(c.stdout, "REPOSITORY\tGATE\tGATE_UID\tGEN\tACCEPT_GOOD\tACCEPT_BAD_FALSE_ACCEPT\tREJECT_GOOD\tREJECT_BAD\tTOTAL")
	for _, row := range report.Rows {
		fmt.Fprintf(c.stdout, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n", row.Repository, row.GateRef, row.GateUID, row.GateGeneration, row.AcceptedGood, row.FalseAccepts(), row.RejectedGood, row.RejectedBad, row.Total())
	}
	if len(report.Excluded) != 0 {
		fmt.Fprintf(c.stdout, "EXCLUDED\t%d\n", len(report.Excluded))
		for _, issue := range report.Excluded {
			fmt.Fprintf(c.stdout, "-\t%s\n", strings.ReplaceAll(issue, "\n", " "))
		}
	}
	return nil
}
