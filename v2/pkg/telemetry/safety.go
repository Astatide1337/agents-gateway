package telemetry

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

const (
	// Redacted is the stable value emitted for a recognized secret attribute.
	Redacted              = "[REDACTED]"
	defaultAttributeLimit = 1024
)

var secretKeyPattern = regexp.MustCompile(`(?i)(pass(word)?|secret|token|api[-_]?key|authorization|cookie|credential|private[-_]?key|client[-_]?secret|signature|session)`)

// SafeString returns an attribute whose key is normalized and whose value is
// bounded. Secret-looking keys are retained for useful schema visibility but
// their values are replaced with Redacted.
func SafeString(key, value string) attribute.KeyValue {
	key = normalizeKey(key)
	if secretKeyPattern.MatchString(key) {
		value = Redacted
	}
	return attribute.String(key, truncate(value, defaultAttributeLimit))
}

// SafeAttributes filters malformed, nil, unsupported, and sensitive values.
// It accepts common scalar types and slices. Callers should use this helper
// for user-controlled or request-derived attributes before adding them to a
// span, metric, resource, or log record.
func SafeAttributes(attrs ...attribute.KeyValue) []attribute.KeyValue {
	result := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		key := normalizeKey(string(attr.Key))
		if key == "" || !validKey(key) {
			continue
		}
		if secretKeyPattern.MatchString(key) {
			result = append(result, attribute.String(key, Redacted))
			continue
		}
		value, ok := safeValue(attr.Value)
		if ok {
			result = append(result, attribute.KeyValue{Key: attribute.Key(key), Value: value})
		}
	}
	return result
}

// SafeAny converts a supported scalar value to a bounded OTel attribute. It
// returns false for unsupported values rather than stringifying arbitrary
// objects, which prevents accidental serialization of credentials or bodies.
func SafeAny(key string, value any) (attribute.KeyValue, bool) {
	key = normalizeKey(key)
	if key == "" || !validKey(key) {
		return attribute.KeyValue{}, false
	}
	if secretKeyPattern.MatchString(key) {
		return attribute.String(key, Redacted), true
	}
	switch typed := value.(type) {
	case string:
		return attribute.String(key, truncate(typed, defaultAttributeLimit)), true
	case bool:
		return attribute.Bool(key, typed), true
	case int:
		return attribute.Int(key, typed), true
	case int64:
		return attribute.Int64(key, typed), true
	case float64:
		return attribute.Float64(key, typed), true
	case []string:
		bounded := make([]string, 0, len(typed))
		for _, item := range typed {
			bounded = append(bounded, truncate(item, defaultAttributeLimit))
		}
		return attribute.StringSlice(key, bounded), true
	default:
		return attribute.KeyValue{}, false
	}
}

// LogRecord creates a correlated OTel log record. The OTel log API does not
// automatically copy span context from context.Context, so this helper does
// that explicitly and applies the same safety filtering as span attributes.
func LogRecord(ctx context.Context, body string, severity log.Severity, attrs ...attribute.KeyValue) log.Record {
	var record log.Record
	record.SetTimestamp(time.Now())
	record.SetSeverity(severity)
	record.SetSeverityText(severity.String())
	record.SetBody(attribute.StringValue(truncate(body, defaultAttributeLimit)))
	correlation := make([]attribute.KeyValue, 0, len(attrs)+3)
	correlation = append(correlation, attrs...)
	if ctx != nil {
		spanContext := trace.SpanContextFromContext(ctx)
		if spanContext.IsValid() {
			// The current stable Logs API does not expose dedicated trace/span
			// fields on log.Record. These correlation attributes are exported
			// with the record and understood by common telemetry backends.
			correlation = append(correlation,
				attribute.String("trace_id", spanContext.TraceID().String()),
				attribute.String("span_id", spanContext.SpanID().String()),
				attribute.String("trace_flags", fmt.Sprintf("%02x", byte(spanContext.TraceFlags()))),
			)
		}
	}
	record.AddAttributes(SafeAttributes(correlation...)...)
	return record
}

// Emit emits a correlated, redacted log record through logger.
func Emit(ctx context.Context, logger log.Logger, body string, severity log.Severity, attrs ...attribute.KeyValue) {
	if logger == nil {
		return
	}
	logger.Emit(ctx, LogRecord(ctx, body, severity, attrs...))
}

func safeValue(value attribute.Value) (attribute.Value, bool) {
	switch value.Type() {
	case attribute.BOOL:
		return attribute.BoolValue(value.AsBool()), true
	case attribute.INT64:
		return attribute.Int64Value(value.AsInt64()), true
	case attribute.FLOAT64:
		return attribute.Float64Value(value.AsFloat64()), true
	case attribute.STRING:
		return attribute.StringValue(truncate(value.AsString(), defaultAttributeLimit)), true
	case attribute.BOOLSLICE:
		return attribute.BoolSliceValue(value.AsBoolSlice()), true
	case attribute.INT64SLICE:
		return attribute.Int64SliceValue(value.AsInt64Slice()), true
	case attribute.FLOAT64SLICE:
		return attribute.Float64SliceValue(value.AsFloat64Slice()), true
	case attribute.STRINGSLICE:
		values := value.AsStringSlice()
		bounded := make([]string, 0, len(values))
		for _, item := range values {
			bounded = append(bounded, truncate(item, defaultAttributeLimit))
		}
		return attribute.StringSliceValue(bounded), true
	default:
		return attribute.Value{}, false
	}
}

func normalizeKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

func validKey(key string) bool {
	if len(key) > 128 {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}
