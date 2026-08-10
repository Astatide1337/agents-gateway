// Package telemetry provides the process-wide OpenTelemetry setup used by
// Agents Gateway. It deliberately owns exporter construction and lifecycle so
// callers only need to configure one OTLP endpoint and call Setup once.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Protocol selects the OTLP transport.
type Protocol string

const (
	// ProtocolHTTP sends protobuf telemetry over HTTPS. It is the recommended
	// transport when the collector is reached through a public endpoint.
	ProtocolHTTP Protocol = "http"
	// ProtocolGRPC sends protobuf telemetry over TLS-protected gRPC.
	ProtocolGRPC Protocol = "grpc"
)

// Config controls Setup. Endpoint may be omitted only when
// OTEL_EXPORTER_OTLP_ENDPOINT is set. An enabled configuration without an
// endpoint is an error; it never silently degrades to a no-op provider.
type Config struct {
	// Disabled explicitly disables instrumentation. This is the only setting
	// that permits Setup to return without configuring exporters.
	Disabled bool

	ServiceName           string
	ServiceVersion        string
	ServiceNamespace      string
	DeploymentEnvironment string
	ServiceInstanceID     string
	// ResourceAttributes are added after the standard service attributes.
	// Sensitive keys are rejected by SafeAttributes before they reach OTel.
	ResourceAttributes map[string]string

	Endpoint string
	Protocol Protocol
	Headers  map[string]string
	// Insecure permits plaintext OTLP only to loopback/localhost endpoints.
	// It is intended for a local collector in development, never for a public
	// telemetry endpoint.
	Insecure bool
	// InsecureHosts is an explicit allowlist for non-loopback plaintext
	// collectors, typically a single Compose-internal service name. Insecure
	// alone remains loopback-only.
	InsecureHosts []string

	Timeout        time.Duration
	ExportInterval time.Duration
	BatchTimeout   time.Duration
	// Sampling is a parent-based trace sampling ratio in [0, 1]. The default
	// is 1.0 so server-side sampling remains the source of truth unless a
	// caller explicitly configures a lower ratio.
	Sampling float64
	// The default enables all three signals. Set DisableAllSignals only when a
	// caller has an explicit operational reason to construct no signal SDKs.
	DisableAllSignals bool
}

// Shutdown flushes and closes all exporters created by Setup. It is safe to
// call more than once and restores the providers that were installed before
// Setup.
type Shutdown func(context.Context) error

// Setup configures the global OTel providers and W3C trace context/baggage
// propagation. It returns a lifecycle function for graceful shutdown.
func Setup(ctx context.Context, cfg Config) (Shutdown, error) {
	if ctx == nil {
		return nil, errors.New("telemetry: nil context")
	}
	if cfg.Disabled {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: service name is required")
	}
	if cfg.DisableAllSignals {
		return nil, errors.New("telemetry: all signals disabled; set Disabled to explicitly opt out")
	}
	if cfg.Protocol == "" {
		cfg.Protocol = ProtocolHTTP
	}
	if cfg.Protocol != ProtocolHTTP && cfg.Protocol != ProtocolGRPC {
		return nil, fmt.Errorf("telemetry: unsupported protocol %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	endpoint, err := validateEndpoint(cfg.Endpoint, cfg.Protocol, cfg.Insecure, cfg.InsecureHosts)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.ExportInterval <= 0 {
		cfg.ExportInterval = 15 * time.Second
	}
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = 10 * time.Second
	}
	if cfg.Sampling == 0 {
		cfg.Sampling = 1
	}
	if cfg.Sampling < 0 || cfg.Sampling > 1 {
		return nil, errors.New("telemetry: sampling must be between 0 and 1")
	}
	for key := range cfg.Headers {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
			return nil, errors.New("telemetry: invalid OTLP header name")
		}
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	previous := previousProviders{
		tracer:     otel.GetTracerProvider(),
		meter:      otel.GetMeterProvider(),
		logger:     global.GetLoggerProvider(),
		propagator: otel.GetTextMapPropagator(),
	}
	created := &providers{}
	cleanup := func(shutdownCtx context.Context) error {
		return created.shutdown(shutdownCtx)
	}

	if err := setupTraces(ctx, cfg, endpoint, res, created); err != nil {
		_ = cleanup(context.Background())
		return nil, fmt.Errorf("telemetry: setup traces: %w", err)
	}
	if err := setupMetrics(ctx, cfg, endpoint, res, created); err != nil {
		_ = cleanup(context.Background())
		return nil, fmt.Errorf("telemetry: setup metrics: %w", err)
	}
	if err := setupLogs(ctx, cfg, endpoint, res, created); err != nil {
		_ = cleanup(context.Background())
		return nil, fmt.Errorf("telemetry: setup logs: %w", err)
	}

	otel.SetTracerProvider(created.tracer)
	otel.SetMeterProvider(created.meter)
	global.SetLoggerProvider(created.logger)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	var once sync.Once
	return func(shutdownCtx context.Context) error {
		var shutdownErr error
		once.Do(func() {
			shutdownErr = cleanup(shutdownCtx)
			// Restoring the old providers matters to tests, embedded processes,
			// and applications that manage more than one lifecycle.
			otel.SetTracerProvider(previous.tracer)
			otel.SetMeterProvider(previous.meter)
			global.SetLoggerProvider(previous.logger)
			otel.SetTextMapPropagator(previous.propagator)
		})
		return shutdownErr
	}, nil
}

type providers struct {
	tracer     *sdktrace.TracerProvider
	meter      *metric.MeterProvider
	logger     *log.LoggerProvider
	propagator propagation.TextMapPropagator
}

type previousProviders struct {
	tracer     oteltrace.TracerProvider
	meter      otelmetric.MeterProvider
	logger     otellog.LoggerProvider
	propagator propagation.TextMapPropagator
}

func (p *providers) shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var errs []error
	if p.logger != nil {
		errs = append(errs, p.logger.Shutdown(ctx))
	}
	if p.meter != nil {
		errs = append(errs, p.meter.Shutdown(ctx))
	}
	if p.tracer != nil {
		errs = append(errs, p.tracer.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func buildResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName)}
	for key, value := range map[string]string{
		"service.version":             cfg.ServiceVersion,
		"service.namespace":           cfg.ServiceNamespace,
		"deployment.environment.name": cfg.DeploymentEnvironment,
		"service.instance.id":         cfg.ServiceInstanceID,
	} {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	for key, value := range cfg.ResourceAttributes {
		attrs = append(attrs, SafeString(key, value))
	}
	// Process/runtime/SDK attributes are safe and useful for a centralized
	// telemetry backend. Environment-provided arbitrary resource attributes are
	// intentionally not imported automatically because that could export a
	// secret accidentally placed in an environment variable.
	return resource.New(ctx,
		resource.WithAttributes(SafeAttributes(attrs...)...),
		resource.WithProcessRuntimeName(),
		resource.WithProcessRuntimeVersion(),
		resource.WithTelemetrySDK(),
	)
}

func setupTraces(ctx context.Context, cfg Config, endpoint string, res *resource.Resource, p *providers) error {
	var exporter sdktrace.SpanExporter
	var err error
	if cfg.Protocol == ProtocolHTTP {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(signalEndpoint(endpoint, "v1/traces")), otlptracehttp.WithHeaders(copyHeaders(cfg.Headers)), otlptracehttp.WithTimeout(cfg.Timeout)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	} else {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(grpcTarget(endpoint)), otlptracegrpc.WithHeaders(copyHeaders(cfg.Headers)), otlptracegrpc.WithTimeout(cfg.Timeout)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	}
	if err != nil {
		return err
	}
	p.tracer = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Sampling))),
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(cfg.BatchTimeout)),
	)
	return nil
}

func setupMetrics(ctx context.Context, cfg Config, endpoint string, res *resource.Resource, p *providers) error {
	var exporter metric.Exporter
	var err error
	if cfg.Protocol == ProtocolHTTP {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(signalEndpoint(endpoint, "v1/metrics")), otlpmetrichttp.WithHeaders(copyHeaders(cfg.Headers)), otlpmetrichttp.WithTimeout(cfg.Timeout)}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exporter, err = otlpmetrichttp.New(ctx, opts...)
	} else {
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(grpcTarget(endpoint)), otlpmetricgrpc.WithHeaders(copyHeaders(cfg.Headers)), otlpmetricgrpc.WithTimeout(cfg.Timeout)}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exporter, err = otlpmetricgrpc.New(ctx, opts...)
	}
	if err != nil {
		return err
	}
	p.meter = metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter, metric.WithInterval(cfg.ExportInterval), metric.WithTimeout(cfg.BatchTimeout))),
	)
	return nil
}

func setupLogs(ctx context.Context, cfg Config, endpoint string, res *resource.Resource, p *providers) error {
	var exporter log.Exporter
	var err error
	if cfg.Protocol == ProtocolHTTP {
		opts := []otlploghttp.Option{otlploghttp.WithEndpointURL(signalEndpoint(endpoint, "v1/logs")), otlploghttp.WithHeaders(copyHeaders(cfg.Headers)), otlploghttp.WithTimeout(cfg.Timeout)}
		if cfg.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		exporter, err = otlploghttp.New(ctx, opts...)
	} else {
		opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(grpcTarget(endpoint)), otlploggrpc.WithHeaders(copyHeaders(cfg.Headers)), otlploggrpc.WithTimeout(cfg.Timeout)}
		if cfg.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		exporter, err = otlploggrpc.New(ctx, opts...)
	}
	if err != nil {
		return err
	}
	p.logger = log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter, log.WithExportInterval(cfg.ExportInterval), log.WithExportTimeout(cfg.BatchTimeout))),
	)
	return nil
}

// ValidateEndpoint validates the endpoint before an exporter can be created.
// HTTPS is required for non-local endpoints. gRPC endpoints cannot contain a
// path because gRPC addressing is host-based; HTTP endpoints may contain a
// collector base path such as /otlp.
func ValidateEndpoint(raw string, protocol Protocol, insecure bool) (string, error) {
	return validateEndpoint(raw, protocol, insecure, nil)
}

func validateEndpoint(raw string, protocol Protocol, insecure bool, insecureHosts []string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("telemetry: OTLP endpoint is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("telemetry: OTLP endpoint must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("telemetry: OTLP endpoint must not contain credentials, query, or fragment")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("telemetry: OTLP endpoint scheme %q is unsupported", u.Scheme)
	}
	if protocol != ProtocolHTTP && protocol != ProtocolGRPC {
		return "", fmt.Errorf("telemetry: unsupported protocol %q", protocol)
	}
	allowedInsecureHost := isLocalHost(u.Hostname())
	for _, host := range insecureHosts {
		if strings.EqualFold(strings.TrimSpace(host), u.Hostname()) {
			allowedInsecureHost = true
			break
		}
	}
	if u.Scheme == "http" && (!insecure || !allowedInsecureHost) {
		return "", errors.New("telemetry: plaintext OTLP is permitted only for loopback/localhost with Insecure=true")
	}
	if protocol == ProtocolGRPC && strings.Trim(u.Path, "/") != "" {
		return "", errors.New("telemetry: gRPC OTLP endpoint must not contain a path")
	}
	if u.Hostname() == "" {
		return "", errors.New("telemetry: OTLP endpoint host is required")
	}
	return u.String(), nil
}

func signalEndpoint(base, signal string) string {
	u, _ := url.Parse(base)
	basePath := strings.TrimSuffix(u.Path, "/")
	u.Path = basePath + "/" + signal
	return u.String()
}

func grpcTarget(raw string) string {
	u, _ := url.Parse(raw)
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "4317")
	}
	return net.JoinHostPort(u.Hostname(), u.Port())
}

func isLocalHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func copyHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	copy := make(map[string]string, len(headers))
	for key, value := range headers {
		copy[key] = value
	}
	return copy
}
