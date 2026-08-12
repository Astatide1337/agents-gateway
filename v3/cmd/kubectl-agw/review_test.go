package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/shadowreview"
)

func TestReviewCreatesOneImmutableCanonicalRecordAndBindsKubernetesSubject(t *testing.T) {
	run := reviewRunJSON("jobmark-fix-427", "agw-runs", "11111111-1111-4111-8111-111111111111", "Accepted")
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: selfSubjectJSON("sohim", "user-1")},
		{stdout: run},
		{stdout: ""},
		{stdout: "configmap/agw-shadow-review-created\n"},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"review", "jobmark-fix-427", "--diff-good", "-n", "agw-runs"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("kubectl calls=%d, want auth/get/get/create: %#v", len(runner.calls), runner.calls)
	}
	if got := runner.calls[0].argv; len(got) < 5 || got[1] != "auth" || got[2] != "whoami" || got[3] != "--output" || got[4] != "json" {
		t.Fatalf("whoami argv=%#v", got)
	}
	if got := runner.calls[3].argv; len(got) < 2 || got[1] != "create" {
		t.Fatalf("create argv=%#v", got)
	}
	var configMap map[string]any
	if err := json.Unmarshal(runner.calls[3].stdin, &configMap); err != nil {
		t.Fatal(err)
	}
	if immutable, ok := configMap["immutable"].(bool); !ok || !immutable {
		t.Fatalf("ConfigMap immutable=%#v", configMap["immutable"])
	}
	data, ok := configMap["data"].(map[string]any)
	if !ok || data[shadowreview.ConfigMapDataKey] == nil {
		t.Fatalf("ConfigMap review data=%#v", configMap["data"])
	}
	reviewBody := []byte(data[shadowreview.ConfigMapDataKey].(string))
	review, err := shadowreview.ParseCanonicalBytes(reviewBody)
	if err != nil {
		t.Fatal(err)
	}
	if review.Reviewer.Username != "sohim" || review.DiffClassification != shadowreview.DiffGood || review.MachineVerdict != "Accepted" {
		t.Fatalf("review=%#v", review)
	}
	if !strings.Contains(stdout.String(), "created sha256:") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestReviewIsIdempotentButRefusesDifferentClassification(t *testing.T) {
	run := reviewRunJSON("jobmark-fix-427", "agw-runs", "22222222-2222-4222-8222-222222222222", "Rejected")
	reviewer := selfSubjectJSON("sohim", "user-1")
	var snapshot agentRunSnapshot
	if err := decodeBoundedJSON([]byte(run), &snapshot); err != nil {
		t.Fatal(err)
	}
	review, err := reviewFromRun(snapshot, string(shadowreview.DiffBad), shadowreview.Reviewer{Username: "sohim", UID: "user-1", Groups: []string{"system:authenticated"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := shadowreview.CanonicalBytes(review)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := shadowreview.Digest(review)
	if err != nil {
		t.Fatal(err)
	}
	name, err := shadowreview.ConfigMapName(review.RunUID)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := reviewConfigMap("agw-runs", name, body, digest)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: reviewer}, {stdout: run}, {stdout: string(existing)},
		{stdout: reviewer}, {stdout: run}, {stdout: string(existing)},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"review", "jobmark-fix-427", "--diff-bad"}); err != nil {
		t.Fatalf("identical review: %v", err)
	}
	if !strings.Contains(stdout.String(), "already exists (identical)") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if err := cli.Run(context.Background(), []string{"review", "jobmark-fix-427", "--diff-good"}); err == nil || !strings.Contains(err.Error(), "different immutable content") {
		t.Fatalf("different review error=%v", err)
	}
	if len(runner.calls) != 6 {
		t.Fatalf("calls=%d, want no create on existing records", len(runner.calls))
	}
}

func TestMatrixRevalidatesPayloadAgainstAgentRunAndShowsFalseAccepts(t *testing.T) {
	run := reviewRunJSON("jobmark-fix-427", "agw-runs", "33333333-3333-4333-8333-333333333333", "Accepted")
	var snapshot agentRunSnapshot
	if err := decodeBoundedJSON([]byte(run), &snapshot); err != nil {
		t.Fatal(err)
	}
	review, err := reviewFromRun(snapshot, string(shadowreview.DiffBad), shadowreview.Reviewer{Username: "sohim"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := shadowreview.CanonicalBytes(review)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := shadowreview.Digest(review)
	if err != nil {
		t.Fatal(err)
	}
	name, err := shadowreview.ConfigMapName(review.RunUID)
	if err != nil {
		t.Fatal(err)
	}
	configMap, err := reviewConfigMap("agw-runs", name, body, digest)
	if err != nil {
		t.Fatal(err)
	}
	var configMapObject map[string]any
	if err := json.Unmarshal(configMap, &configMapObject); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: mustJSON(map[string]any{"apiVersion": agentRunAPIVersion, "kind": "AgentRunList", "items": []any{mustJSONValue(run)}})},
		{stdout: mustJSON(map[string]any{"apiVersion": "v1", "kind": "ConfigMapList", "items": []any{configMapObject}})},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"matrix", "--output", "json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"acceptedBad":1`) || !strings.Contains(stdout.String(), `"falseAccepts":1`) {
		t.Fatalf("matrix=%q", stdout.String())
	}
}

func TestMatrixRejectsTamperedBindingEvenWhenDiscoveryLabelsLookRight(t *testing.T) {
	run := reviewRunJSON("jobmark-fix-427", "agw-runs", "44444444-4444-4444-8444-444444444444", "Accepted")
	var snapshot agentRunSnapshot
	if err := decodeBoundedJSON([]byte(run), &snapshot); err != nil {
		t.Fatal(err)
	}
	review, err := reviewFromRun(snapshot, string(shadowreview.DiffGood), shadowreview.Reviewer{Username: "sohim"})
	if err != nil {
		t.Fatal(err)
	}
	review.PatchDigest = "sha256:" + strings.Repeat("f", 64)
	body, err := shadowreview.CanonicalBytes(review)
	if err != nil {
		t.Fatal(err)
	}
	name, err := shadowreview.ConfigMapName(review.RunUID)
	if err != nil {
		t.Fatal(err)
	}
	configMap, err := reviewConfigMap("agw-runs", name, body, "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(configMap, &object); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: mustJSON(map[string]any{"apiVersion": agentRunAPIVersion, "kind": "AgentRunList", "items": []any{mustJSONValue(run)}})},
		{stdout: mustJSON(map[string]any{"apiVersion": "v1", "kind": "ConfigMapList", "items": []any{object}})},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	if err := cli.Run(context.Background(), []string{"matrix"}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("tampered matrix error=%v output=%q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "patch or report digest") {
		t.Fatalf("tampered matrix output=%q", stdout.String())
	}
}

func TestMatrixRejectsUnreviewedTerminalShadowRun(t *testing.T) {
	run := reviewRunJSON("jobmark-fix-428", "agw-runs", "55555555-5555-4555-8555-555555555555", "Rejected")
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: mustJSON(map[string]any{"apiVersion": agentRunAPIVersion, "kind": "AgentRunList", "items": []any{mustJSONValue(run)}})},
		{stdout: mustJSON(map[string]any{"apiVersion": "v1", "kind": "ConfigMapList", "items": []any{}})},
	}}
	var stdout bytes.Buffer
	cli := NewCLI(Config{Runner: runner, Stdout: &stdout, Stderr: io.Discard})
	err := cli.Run(context.Background(), []string{"matrix"})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("unreviewed matrix error=%v output=%q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "has no shadow review record") {
		t.Fatalf("unreviewed matrix output=%q", stdout.String())
	}
}

func reviewRunJSON(name, namespace, uid, verdict string) string {
	digest := "sha256:" + strings.Repeat("a", 64)
	return mustJSON(map[string]any{
		"apiVersion": agentRunAPIVersion, "kind": agentRunKind,
		"metadata": map[string]any{"name": name, "namespace": namespace, "uid": uid},
		"spec":     map[string]any{"gateRef": "go-default", "source": map[string]any{"repo": "github.com/Astatide1337/jobmark"}},
		"status": map[string]any{
			"phase": "Succeeded", "specDigest": digest, "baseSHA": strings.Repeat("b", 40),
			"patch": map[string]any{"ref": map[string]any{"digest": "sha256:" + strings.Repeat("c", 64)}},
			"gate":  map[string]any{"name": "go-default", "uid": "gate-uid", "generation": 7, "mode": "shadow", "verdict": verdict, "reportRef": map[string]any{"digest": "sha256:" + strings.Repeat("d", 64)}},
		},
	})
}

func selfSubjectJSON(username, uid string) string {
	return mustJSON(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": map[string]any{"username": username, "uid": uid, "groups": []string{"system:authenticated"}}}})
}

func mustJSONValue(encoded string) any {
	var value any
	if err := json.Unmarshal([]byte(encoded), &value); err != nil {
		panic(err)
	}
	return value
}
