// Package runnerdispatch implements the narrow mTLS control-plane connection
// used by Temporal activities. Runtime sockets and upstream credentials never
// cross this boundary.
package runnerdispatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const maxResponseBytes int64 = 1 << 20

type Config struct {
	Endpoint       string
	ServerName     string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	Timeout        time.Duration
}

type Client struct {
	endpoint string
	http     *http.Client
}

func New(config Config) (*Client, error) {
	endpoint, err := validateEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	if config.ServerName == "" || config.CAFile == "" || config.ClientCertFile == "" || config.ClientKeyFile == "" {
		return nil, errors.New("runner mTLS server name, CA, client certificate, and client key are required")
	}
	caPEM, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read runner CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("runner CA contains no certificates")
	}
	certificate, err := tls.LoadX509KeyPair(config.ClientCertFile, config.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load runner client certificate: %w", err)
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			ServerName:   config.ServerName,
			RootCAs:      roots,
			Certificates: []tls.Certificate{certificate},
		},
	}
	return &Client{endpoint: endpoint, http: &http.Client{
		Transport: otelhttp.NewTransport(transport),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("runner redirects are forbidden")
		},
	}}, nil
}

// NewUnix creates the standalone runner client. The transport can only dial
// the configured Unix socket and therefore cannot silently fall back to TCP.
// Access to the socket is enforced by its owning directory and file mode.
func NewUnix(socketPath string, timeout time.Duration) (*Client, error) {
	socketPath = filepath.Clean(strings.TrimSpace(socketPath))
	if socketPath == "." || socketPath == string(filepath.Separator) || !filepath.IsAbs(socketPath) {
		return nil, errors.New("runner Unix socket must be a non-root absolute path")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{endpoint: "http://agw-runner", http: &http.Client{
		Transport: otelhttp.NewTransport(transport),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("runner redirects are forbidden")
		},
	}}, nil
}

func validateEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("runner endpoint must be an HTTPS origin without credentials, path, query, or fragment")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func (c *Client) ScheduleRunnerTask(ctx context.Context, input agentworkflow.ScheduleRunnerTaskInput) (agentworkflow.ScheduleRunnerTaskResult, error) {
	var result agentworkflow.ScheduleRunnerTaskResult
	if err := c.call(ctx, "/v1/tasks/schedule", input, &result, http.StatusOK); err != nil {
		return agentworkflow.ScheduleRunnerTaskResult{}, err
	}
	return result, nil
}

func (c *Client) CancelRunnerTask(ctx context.Context, input agentworkflow.CancelRunnerTaskInput) error {
	return c.call(ctx, "/v1/tasks/cancel", input, nil, http.StatusNoContent)
}

func (c *Client) ResumeRunnerTask(ctx context.Context, input agentworkflow.ResumeRunnerTaskInput) error {
	return c.call(ctx, "/v1/tasks/resume", input, nil, http.StatusNoContent)
}

func (c *Client) StatusRunnerTask(ctx context.Context, input agentworkflow.StatusRunnerTaskInput) (agentworkflow.StatusRunnerTaskResult, error) {
	var result agentworkflow.StatusRunnerTaskResult
	if err := c.call(ctx, "/v1/tasks/status", input, &result, http.StatusOK); err != nil {
		return agentworkflow.StatusRunnerTaskResult{}, err
	}
	return result, nil
}

// Ready verifies that the configured runner transport is reachable and that
// the runner's own host prerequisite report is healthy. It intentionally uses
// the same authenticated transport as task dispatch, including SO_PEERCRED on
// the standalone Unix socket.
func (c *Client) Ready(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/readyz", nil)
	if err != nil {
		return errors.New("create runner readiness request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("runner readiness unavailable: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("read runner readiness response")
	}
	if int64(len(data)) > maxResponseBytes {
		return errors.New("runner readiness response exceeds limit")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("runner is not ready (status %d)", response.StatusCode)
	}
	return nil
}

func (c *Client) call(ctx context.Context, path string, input, output any, expected int) error {
	body, err := json.Marshal(input)
	if err != nil {
		return errors.New("encode runner request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("create runner request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		// Whether the runner accepted a state-changing request may be unknown.
		// The stable idempotency key lets a subsequent Temporal activity safely
		// ask the runner again without creating a second task or command.
		return fmt.Errorf("runner request outcome unknown: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return errors.New("read runner response")
	}
	if int64(len(data)) > maxResponseBytes {
		return errors.New("runner response exceeds limit")
	}
	if response.StatusCode != expected {
		return fmt.Errorf("runner rejected request with status %d", response.StatusCode)
	}
	if output == nil {
		if len(bytes.TrimSpace(data)) != 0 {
			return errors.New("runner returned an unexpected response body")
		}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.New("runner returned invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("runner returned multiple JSON values")
	}
	return nil
}

var _ agentworkflow.RunnerActivities = (*Client)(nil)
