package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

func TestValidateEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		protocol Protocol
		insecure bool
		wantErr  bool
		want     string
	}{
		{name: "https http", raw: "https://collector.example/otlp", protocol: ProtocolHTTP, want: "https://collector.example/otlp"},
		{name: "https grpc", raw: "https://collector.example", protocol: ProtocolGRPC, want: "https://collector.example"},
		{name: "local http", raw: "http://127.0.0.1:4318", protocol: ProtocolHTTP, insecure: true, want: "http://127.0.0.1:4318"},
		{name: "public plaintext", raw: "http://collector.example", protocol: ProtocolHTTP, insecure: true, wantErr: true},
		{name: "grpc path", raw: "https://collector.example/otlp", protocol: ProtocolGRPC, wantErr: true},
		{name: "credentials", raw: "https://user:pass@collector.example", protocol: ProtocolHTTP, wantErr: true},
		{name: "query", raw: "https://collector.example?token=secret", protocol: ProtocolHTTP, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateEndpoint(tt.raw, tt.protocol, tt.insecure)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateEndpoint() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("ValidateEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetupRequiresExplicitDisableForNoOp(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{ServiceName: "agw"})
	if shutdown != nil || err == nil {
		t.Fatalf("Setup() = (%v, %v), expected an endpoint error", shutdown, err)
	}

	shutdown, err = Setup(context.Background(), Config{Disabled: true})
	if err != nil || shutdown == nil {
		t.Fatalf("disabled Setup() = (%v, %v)", shutdown, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("disabled shutdown: %v", err)
	}
}

func TestSetupInstallsAndRestoresProviders(t *testing.T) {
	previousTracer := otel.GetTracerProvider()
	previousMeter := otel.GetMeterProvider()
	shutdown, err := Setup(context.Background(), Config{
		ServiceName: "agents-gateway",
		Endpoint:    "https://collector.example/otlp",
		Protocol:    ProtocolHTTP,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if otel.GetTracerProvider() == previousTracer || otel.GetMeterProvider() == previousMeter {
		t.Fatal("Setup() did not install SDK providers")
	}
	// No collector is running in this unit test, so exporters may report a
	// flush error. Provider restoration must still happen and shutdown remains
	// responsible for returning that operational error to real callers.
	_ = shutdown(context.Background())
	if otel.GetTracerProvider() != previousTracer || otel.GetMeterProvider() != previousMeter {
		t.Fatal("shutdown() did not restore previous providers")
	}
}

func TestSignalEndpoint(t *testing.T) {
	if got := signalEndpoint("https://collector.example/otlp", "v1/traces"); got != "https://collector.example/otlp/v1/traces" {
		t.Fatalf("signalEndpoint() = %q", got)
	}
	if got := grpcTarget("https://[::1]"); got != "[::1]:4317" {
		t.Fatalf("grpcTarget() = %q", got)
	}
	if got := grpcTarget("https://collector.example:8443"); got != "collector.example:8443" {
		t.Fatalf("grpcTarget() = %q", got)
	}
}

func TestSafeAttributes(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz"
	attrs := SafeAttributes(
		attribute.String("Authorization", "bearer secret"),
		attribute.String(" request.id ", long),
		attribute.String("bad key", "ignored"),
		attribute.KeyValue{Key: "object", Value: attribute.Value{}},
	)
	if len(attrs) != 2 {
		t.Fatalf("SafeAttributes() returned %d attrs, want 2", len(attrs))
	}
	if attrs[0].Value.AsString() != Redacted {
		t.Fatalf("secret was not redacted: %q", attrs[0].Value.AsString())
	}
	if attrs[1].Key != "request.id" || attrs[1].Value.AsString() != long {
		t.Fatalf("safe attribute = %#v", attrs[1])
	}
}

func TestSafeStringAndAny(t *testing.T) {
	if got := SafeString("api_key", "secret").Value.AsString(); got != Redacted {
		t.Fatalf("SafeString() = %q", got)
	}
	if _, ok := SafeAny("payload", struct{ Secret string }{"secret"}); ok {
		t.Fatal("SafeAny accepted an arbitrary struct")
	}
	if got, ok := SafeAny("count", int64(42)); !ok || got.Value.AsInt64() != 42 {
		t.Fatalf("SafeAny integer = %#v, %v", got, ok)
	}
}

func TestLogRecordCorrelatesAndRedacts(t *testing.T) {
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	record := LogRecord(ctx, "request failed", otellog.SeverityError, attribute.String("token", "secret"))
	if record.Body().AsString() != "request failed" || record.Severity() != otellog.SeverityError {
		t.Fatalf("unexpected log record body/severity")
	}
	var found bool
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		if attr.Key == "token" {
			found = true
			if attr.Value.AsString() != Redacted {
				t.Errorf("token was not redacted")
			}
		}
		return true
	})
	if !found {
		t.Fatal("redacted token attribute missing")
	}
	var traceID, spanID string
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		switch attr.Key {
		case "trace_id":
			traceID = attr.Value.AsString()
		case "span_id":
			spanID = attr.Value.AsString()
		}
		return true
	})
	if traceID != spanContext.TraceID().String() || spanID != spanContext.SpanID().String() {
		t.Fatalf("log record lost span correlation: trace=%q span=%q", traceID, spanID)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{ServiceName: "", Endpoint: "https://collector.example"},
		{ServiceName: "agw", Endpoint: "https://collector.example", Sampling: 2},
		{ServiceName: "agw", Endpoint: "https://collector.example", DisableAllSignals: true},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := Setup(ctx, cfg)
		cancel()
		if err == nil {
			t.Fatalf("Setup(%+v) unexpectedly succeeded", cfg)
		}
	}
}

func TestExplicitInternalCollectorAndEnvironmentParsing(t *testing.T) {
	if _, err := validateEndpoint("http://otel-collector:4318", ProtocolHTTP, true, nil); err == nil {
		t.Fatal("non-loopback plaintext collector accepted without an explicit host allowlist")
	}
	if got, err := validateEndpoint("http://otel-collector:4318", ProtocolHTTP, true, []string{"otel-collector"}); err != nil || got != "http://otel-collector:4318" {
		t.Fatalf("explicit internal collector=%q err=%v", got, err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Basic%20abc")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("AGW_OTEL_SAMPLING_RATIO", "0.25")
	config, err := FromEnv("agw-server")
	if err != nil || config.Headers["Authorization"] != "Basic abc" || config.Sampling != 0.25 || config.ServiceName != "agw-server" {
		t.Fatalf("config=%#v err=%v", config, err)
	}
}
