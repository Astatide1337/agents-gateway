// Package bridge implements the small compatibility boundary between Argo
// Workflows and the Agent Sandbox condition API.
//
// Agent Sandbox exposes lifecycle state as a condition array. This package
// deliberately does not rely on array order or on Argo's label-selector
// resource wait language. It resolves conditions by their semantic type and
// treats ambiguous, malformed, or terminally unsuccessful state as an error.
package bridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ConditionReady    = "Ready"
	ConditionFinished = "Finished"
	FinishedSucceeded = "PodSucceeded"
	FinishedFailed    = "PodFailed"
	MarkerVersion     = "agents.astatide.com/phase0-marker/v1"
	maxResponseBytes  = 1 << 20
)

// ErrSandboxMissing is terminal for a lifecycle wait. A deterministic
// Sandbox that disappeared must not be treated like a temporary API outage:
// recreating or waiting for a same-name replacement could authorize a
// different object than the one whose UID the lifecycle started with.
var ErrSandboxMissing = errors.New("Sandbox is missing")

// Condition is the subset of metav1.Condition needed by the bridge.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// Sandbox is the subset of an Agent Sandbox object read by the bridge.
type Sandbox struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   struct {
		Name       string `json:"name,omitempty"`
		Namespace  string `json:"namespace,omitempty"`
		UID        string `json:"uid,omitempty"`
		Generation int64  `json:"generation,omitempty"`
	} `json:"metadata"`
	Status struct {
		Conditions []Condition `json:"conditions,omitempty"`
	} `json:"status"`
}

// Decision is the result of evaluating one lifecycle target.
type Decision string

const (
	Pending   Decision = "pending"
	Satisfied Decision = "satisfied"
	Failed    Decision = "failed"
)

// Evaluation is a fail-closed interpretation of a Sandbox object.
type Evaluation struct {
	Decision Decision
	Reason   string
	UID      string
}

// Marker records the exact Sandbox identity for which a lifecycle condition
// was observed. The UID prevents a stale PVC marker from authorizing a newly
// recreated Sandbox with the same deterministic name.
type Marker struct {
	Version   string `json:"version"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Condition string `json:"condition"`
	Observed  string `json:"observedAt"`
}

// Config controls one wait operation.
type Config struct {
	Namespace          string
	Name               string
	Condition          string
	Timeout            time.Duration
	PollInterval       time.Duration
	MarkerPath         string
	RequiredMarkerPath string
}

// Client is a minimal Kubernetes REST client. It intentionally has no
// controller-runtime dependency because the bridge is shipped as a tiny,
// hermetic container and is also easy to unit test with httptest.Server.
type Client struct {
	HTTPClient *http.Client
	ServerURL  *url.URL
	Token      string
}

// NewClient creates a client for a Kubernetes API server. The server must use
// HTTPS; unit tests may construct Client directly with an httptest server.
func NewClient(server, token string, caPEM []byte) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(server), "/"))
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("Kubernetes API URL must be an https URL")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("Kubernetes bearer token is required")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(caPEM) == 0 || !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("Kubernetes CA bundle is missing or invalid")
	}
	return &Client{
		HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		}},
		ServerURL: parsed,
		Token:     token,
	}, nil
}

// Get retrieves one named Sandbox. HTTP failures are classified so Wait can
// retry only transient API failures and fail immediately on authorization,
// missing-resource, or malformed-resource errors.
func (c *Client) Get(ctx context.Context, namespace, name string) (Sandbox, error) {
	if c == nil || c.HTTPClient == nil || c.ServerURL == nil {
		return Sandbox{}, fmt.Errorf("Kubernetes client is not configured")
	}
	if namespace == "" || name == "" {
		return Sandbox{}, fmt.Errorf("Sandbox namespace and name are required")
	}
	path := fmt.Sprintf("/apis/agents.x-k8s.io/v1beta1/namespaces/%s/sandboxes/%s", url.PathEscape(namespace), url.PathEscape(name))
	requestURL := *c.ServerURL
	requestURL.Path = strings.TrimRight(c.ServerURL.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return Sandbox{}, fmt.Errorf("create Kubernetes request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agw-phase0-sandbox-wait/1")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Sandbox{}, transientError{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Sandbox{}, transientError{err: fmt.Errorf("read Kubernetes response: %w", err)}
	}
	if len(body) > maxResponseBytes {
		return Sandbox{}, fmt.Errorf("Kubernetes response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode == http.StatusNotFound {
		return Sandbox{}, fmt.Errorf("%w: Kubernetes API returned HTTP 404: %s", ErrSandboxMissing, compact(body))
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return Sandbox{}, transientError{err: fmt.Errorf("Kubernetes API returned HTTP %d: %s", resp.StatusCode, compact(body))}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Sandbox{}, fmt.Errorf("Kubernetes API authorization failed with HTTP %d: %s", resp.StatusCode, compact(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Sandbox{}, fmt.Errorf("Kubernetes API returned HTTP %d: %s", resp.StatusCode, compact(body))
	}
	var sandbox Sandbox
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&sandbox); err != nil {
		return Sandbox{}, fmt.Errorf("decode Sandbox response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Sandbox{}, fmt.Errorf("decode Sandbox response: trailing JSON value")
		}
		return Sandbox{}, fmt.Errorf("decode Sandbox response: trailing data: %w", err)
	}
	return sandbox, nil
}

type transientError struct{ err error }

func (e transientError) Error() string { return e.err.Error() }
func (e transientError) Unwrap() error { return e.err }

func compact(body []byte) string {
	message := strings.TrimSpace(string(body))
	if len(message) > 256 {
		return message[:256] + "..."
	}
	return message
}

// Evaluate resolves a lifecycle condition by exact type. It never treats a
// missing, Unknown, or False condition as success. Finished=True is only a
// successful terminal state when its reason is exactly PodSucceeded.
func Evaluate(sandbox Sandbox, target string) (Evaluation, error) {
	if target != ConditionReady && target != ConditionFinished {
		return Evaluation{Decision: Failed}, fmt.Errorf("unsupported condition %q", target)
	}
	if sandbox.Metadata.UID == "" {
		return Evaluation{Decision: Failed}, fmt.Errorf("Sandbox metadata.uid is missing")
	}
	if sandbox.Metadata.Generation <= 0 {
		return Evaluation{Decision: Failed, UID: sandbox.Metadata.UID}, fmt.Errorf("Sandbox metadata.generation is missing or invalid")
	}
	byType := make(map[string]Condition, len(sandbox.Status.Conditions))
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == "" {
			return Evaluation{Decision: Failed}, fmt.Errorf("Sandbox contains a condition without a type")
		}
		if _, exists := byType[condition.Type]; exists {
			return Evaluation{Decision: Failed}, fmt.Errorf("Sandbox contains duplicate %q conditions", condition.Type)
		}
		if condition.Status != "True" && condition.Status != "False" && condition.Status != "Unknown" {
			return Evaluation{Decision: Failed}, fmt.Errorf("Sandbox condition %q has invalid status %q", condition.Type, condition.Status)
		}
		if condition.ObservedGeneration != sandbox.Metadata.Generation {
			return Evaluation{Decision: Failed, UID: sandbox.Metadata.UID}, fmt.Errorf("Sandbox condition %q observed generation %d, want %d", condition.Type, condition.ObservedGeneration, sandbox.Metadata.Generation)
		}
		byType[condition.Type] = condition
	}

	finished, hasFinished := byType[ConditionFinished]
	if hasFinished && finished.Status == "True" {
		if finished.Reason != FinishedSucceeded {
			if finished.Reason == FinishedFailed {
				return Evaluation{Decision: Failed, UID: sandbox.Metadata.UID}, fmt.Errorf("Sandbox finished unsuccessfully: %s", finished.Message)
			}
			return Evaluation{Decision: Failed, UID: sandbox.Metadata.UID}, fmt.Errorf("Sandbox Finished=True has unrecognized reason %q", finished.Reason)
		}
		if target == ConditionFinished {
			return Evaluation{Decision: Satisfied, Reason: "PodSucceeded", UID: sandbox.Metadata.UID}, nil
		}
		// Agent Sandbox v0.5.4 deliberately transitions Ready to False with
		// reason PodSucceeded after a successful pod exits. A caller waiting
		// for Ready must not manufacture evidence after that transition; the
		// separate Ready marker is the historical proof used by Finished.
		return Evaluation{Decision: Failed, UID: sandbox.Metadata.UID}, fmt.Errorf("Sandbox completed before Ready=True was observed")
	}

	condition, exists := byType[target]
	if !exists {
		return Evaluation{Decision: Pending, Reason: target + " condition is not present", UID: sandbox.Metadata.UID}, nil
	}
	if condition.Status == "True" {
		if target == ConditionFinished {
			// Finished=True was handled above; reaching this branch means the
			// condition has an invalid combination that must not succeed.
			return Evaluation{Decision: Failed, UID: sandbox.Metadata.UID}, fmt.Errorf("Sandbox Finished=True did not satisfy the success contract")
		}
		return Evaluation{Decision: Satisfied, Reason: "Ready=True", UID: sandbox.Metadata.UID}, nil
	}
	return Evaluation{Decision: Pending, Reason: target + "=" + condition.Status, UID: sandbox.Metadata.UID}, nil
}

// Wait polls the named Sandbox until the target condition is satisfied or the
// bounded context expires. It writes an identity-bound marker only after a
// successful evaluation. A required marker is checked against the current UID
// before the target can succeed.
func Wait(ctx context.Context, client *Client, config Config, logf func(string, ...any)) error {
	if client == nil {
		return fmt.Errorf("Kubernetes client is required")
	}
	if config.Namespace == "" || config.Name == "" {
		return fmt.Errorf("Sandbox namespace and name are required")
	}
	if config.Condition != ConditionReady && config.Condition != ConditionFinished {
		return fmt.Errorf("unsupported condition %q", config.Condition)
	}
	if config.Condition == ConditionFinished && config.RequiredMarkerPath == "" {
		return fmt.Errorf("Finished waits require an identity-bound Ready marker")
	}
	if config.Timeout <= 0 || config.PollInterval <= 0 {
		return fmt.Errorf("timeout and poll interval must be positive")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	var lastTransient error
	for attempt := 1; ; attempt++ {
		sandbox, err := client.Get(ctx, config.Namespace, config.Name)
		if err != nil {
			var transient transientError
			if !errors.As(err, &transient) {
				return err
			}
			lastTransient = err
			logf("transient Sandbox read failure attempt=%d error=%v", attempt, err)
		} else {
			if sandbox.Metadata.Name != config.Name || sandbox.Metadata.Namespace != config.Namespace {
				return fmt.Errorf("Kubernetes response identity does not match %s/%s", config.Namespace, config.Name)
			}
			if config.RequiredMarkerPath != "" {
				if err := verifyMarker(config.RequiredMarkerPath, config.Namespace, config.Name, sandbox.Metadata.UID, ConditionReady); err != nil {
					return err
				}
			}
			evaluation, evalErr := Evaluate(sandbox, config.Condition)
			if evalErr != nil {
				return evalErr
			}
			logf("Sandbox %s/%s condition=%s decision=%s reason=%s", config.Namespace, config.Name, config.Condition, evaluation.Decision, evaluation.Reason)
			switch evaluation.Decision {
			case Satisfied:
				if config.MarkerPath != "" {
					if err := writeMarker(config.MarkerPath, Marker{
						Version: MarkerVersion, Namespace: config.Namespace, Name: config.Name,
						UID: evaluation.UID, Condition: config.Condition,
						Observed: time.Now().UTC().Format(time.RFC3339Nano),
					}); err != nil {
						return err
					}
				}
				return nil
			case Failed:
				return fmt.Errorf("Sandbox condition evaluation failed")
			}
		}

		timer := time.NewTimer(config.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if lastTransient != nil {
				return fmt.Errorf("Sandbox wait timed out after %s: %w", config.Timeout, lastTransient)
			}
			return fmt.Errorf("Sandbox wait timed out after %s: %w", config.Timeout, ctx.Err())
		case <-timer.C:
		}
	}
}

func writeMarker(path string, marker Marker) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create marker directory: %w", err)
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode marker: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		defer file.Close()
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("write marker: %w", err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create marker: %w", err)
	}
	return verifyMarker(path, marker.Namespace, marker.Name, marker.UID, marker.Condition)
}

func verifyMarker(path, namespace, name, uid, condition string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read required marker %q: %w", path, err)
	}
	var marker Marker
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return fmt.Errorf("decode marker %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("marker %q contains trailing JSON data", path)
	}
	if marker.Version != MarkerVersion || marker.Namespace != namespace || marker.Name != name || marker.UID != uid || marker.Condition != condition {
		return fmt.Errorf("marker %q does not identify the current Sandbox %s/%s UID %s condition %s", path, namespace, name, uid, condition)
	}
	if marker.Observed == "" {
		return fmt.Errorf("marker %q has no observation timestamp", path)
	}
	if _, err := time.Parse(time.RFC3339Nano, marker.Observed); err != nil {
		return fmt.Errorf("marker %q has an invalid observation timestamp: %w", path, err)
	}
	return nil
}
