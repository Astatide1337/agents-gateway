package telemetry

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// FromEnv maps standard OTLP variables plus the small AGW safety controls to
// Config. Header values follow the standard URL-encoded comma-separated form.
func FromEnv(serviceName string) (Config, error) {
	protocol := ProtocolHTTP
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))) {
	case "", "http", "http/protobuf":
	case "grpc":
		protocol = ProtocolGRPC
	default:
		return Config{}, errors.New("telemetry: unsupported OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	headers, err := parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	if err != nil {
		return Config{}, err
	}
	sampling := 0.0
	if raw := strings.TrimSpace(os.Getenv("AGW_OTEL_SAMPLING_RATIO")); raw != "" {
		sampling, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, errors.New("telemetry: invalid AGW_OTEL_SAMPLING_RATIO")
		}
	}
	return Config{
		Disabled:              strings.EqualFold(strings.TrimSpace(os.Getenv("AGW_TELEMETRY_DISABLED")), "true"),
		ServiceName:           serviceName,
		ServiceVersion:        strings.TrimSpace(os.Getenv("AGW_VERSION")),
		ServiceNamespace:      "agents-gateway",
		DeploymentEnvironment: strings.TrimSpace(os.Getenv("AGW_ENVIRONMENT")),
		ServiceInstanceID:     strings.TrimSpace(os.Getenv("AGW_INSTANCE_ID")),
		Endpoint:              strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		Protocol:              protocol,
		Headers:               headers,
		Insecure:              strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")), "true"),
		InsecureHosts:         splitComma(os.Getenv("AGW_OTEL_INSECURE_HOSTS")),
		Sampling:              sampling,
	}, nil
}

func parseHeaders(raw string) (map[string]string, error) {
	result := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, errors.New("telemetry: invalid OTLP headers")
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil || strings.ContainsAny(key+decoded, "\r\n") {
			return nil, errors.New("telemetry: invalid OTLP headers")
		}
		result[strings.TrimSpace(key)] = decoded
	}
	return result, nil
}

func splitComma(raw string) []string {
	var result []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}
