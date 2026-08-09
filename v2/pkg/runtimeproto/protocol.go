// Package runtimeproto implements the strict agw.runtime.v1 JSONL boundary.
package runtimeproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/proto"
)

const (
	MaxFrameBytes = 1 << 20
	MaxDataBytes  = 768 << 10

	maxProtocolStringBytes = 256
	maxTextBytes           = 64 << 10
	maxObjectFields        = 64
	maxArrayItems          = 256
	maxJSONDepth           = 32
)

var (
	ErrFrameTooLarge = errors.New("runtime protocol frame exceeds size limit")
	ErrBlankFrame    = errors.New("runtime protocol frame is blank")
	ErrTrailingData  = errors.New("runtime protocol frame contains trailing data")
)

type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

func invalid(path, format string, args ...any) error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// ParseLine parses one JSONL frame. It rejects unknown or duplicate top-level
// fields, malformed JSON, trailing values, oversized payloads, and invalid
// protocol fields.
func ParseLine(line []byte) (proto.Envelope, error) {
	var out proto.Envelope
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 || len(bytes.TrimSpace(line)) == 0 {
		return out, ErrBlankFrame
	}
	if len(line) > MaxFrameBytes {
		return out, ErrFrameTooLarge
	}
	if err := checkJSONShape(line); err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, invalid("json", "%v", err)
	}
	if err := ensureEOF(dec); err != nil {
		return out, err
	}
	if len(out.Data) > MaxDataBytes {
		return out, ErrFrameTooLarge
	}
	if err := ValidateEnvelope(out); err != nil {
		return out, err
	}
	return out, nil
}

// Decoder reads newline-delimited frames. A final line without a newline is
// accepted, as is customary for JSONL files.
type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10)}
}

func (d *Decoder) Next() (proto.Envelope, error) {
	var zero proto.Envelope
	var line []byte
	for {
		chunk, err := d.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxFrameBytes+1 {
			return zero, ErrFrameTooLarge
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return zero, io.EOF
			}
			break
		}
		return zero, err
	}
	frame, parseErr := ParseLine(line)
	if parseErr != nil {
		return zero, parseErr
	}
	return frame, nil
}

func EncodeLine(frame proto.Envelope) ([]byte, error) {
	if err := ValidateEnvelope(frame); err != nil {
		return nil, err
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return append(data, '\n'), nil
}

func ValidateEnvelope(frame proto.Envelope) error {
	if frame.Protocol != proto.ProtocolVersion {
		return invalid("protocol", "must equal %q", proto.ProtocolVersion)
	}
	if frame.Kind != proto.KindEvent && frame.Kind != proto.KindRequest {
		return invalid("kind", "unsupported kind %q", frame.Kind)
	}
	if frame.Type == "" {
		return invalid("type", "is required")
	}
	if len(frame.Type) > maxProtocolStringBytes {
		return invalid("type", "exceeds %d bytes", maxProtocolStringBytes)
	}
	if strings.TrimSpace(frame.RunID) == "" {
		return invalid("run_id", "is required")
	}
	if len(frame.RunID) > maxProtocolStringBytes {
		return invalid("run_id", "exceeds %d bytes", maxProtocolStringBytes)
	}
	if frame.Seq == 0 {
		return invalid("seq", "must be greater than zero")
	}
	trimmedData := bytes.TrimSpace(frame.Data)
	if len(trimmedData) == 0 || bytes.Equal(trimmedData, []byte("null")) {
		return invalid("data", "must be a JSON object")
	}
	if trimmedData[0] != '{' || !json.Valid(trimmedData) {
		return invalid("data", "must be a JSON object")
	}
	if frame.Kind == proto.KindEvent {
		return validateEvent(frame)
	}
	return validateRequest(frame)
}

func validateEvent(frame proto.Envelope) error {
	terminal := isTerminalEvent(frame.Type)
	if frame.Terminal != terminal {
		return invalid("terminal", "must be %t for event %q", terminal, frame.Type)
	}
	if !knownEvent(frame.Type) {
		return invalid("type", "unsupported event %q", frame.Type)
	}
	return validateEventData(frame.Type, frame.Data)
}

func validateRequest(frame proto.Envelope) error {
	if frame.Terminal {
		return invalid("terminal", "must be false for requests")
	}
	if !knownRequest(frame.Type) {
		return invalid("type", "unsupported request %q", frame.Type)
	}
	return validateRequestData(frame.Type, frame.Data)
}

func knownEvent(t string) bool {
	switch t {
	case proto.EventRunStarted, proto.EventHeartbeat, proto.EventAssistantMessage,
		proto.EventModelRequested, proto.EventModelCompleted, proto.EventToolRequested,
		proto.EventToolCompleted, proto.EventApprovalRequested, proto.EventApprovalResolved,
		proto.EventArtifactCreated, proto.EventRunCompleted, proto.EventRunFailed,
		proto.EventRunCancelled, proto.EventUnknownEffect:
		return true
	default:
		return false
	}
}

func knownRequest(t string) bool {
	switch t {
	case proto.RequestRunStart, proto.RequestInput, proto.RequestToolResult,
		proto.RequestApproval, proto.RequestCancel, proto.RequestPing:
		return true
	default:
		return false
	}
}

func isTerminalEvent(t string) bool {
	switch t {
	case proto.EventRunCompleted, proto.EventRunFailed, proto.EventRunCancelled,
		proto.EventUnknownEffect:
		return true
	default:
		return false
	}
}

// fieldRule is deliberately local to this package. v1 has no unversioned
// extension field: adding a field to any payload requires a protocol revision
// (or a separately versioned nested object) rather than silently widening the
// accepted input.
type fieldRule struct {
	required bool
	check    func(string, json.RawMessage) error
}

type objectSchema map[string]fieldRule

func validateEventData(typ string, raw json.RawMessage) error {
	var schema objectSchema
	switch typ {
	case proto.EventRunStarted:
		schema = objectSchema{
			"agent_id":   requiredField(validateIdentifier),
			"sandbox_id": requiredField(validateIdentifier),
		}
	case proto.EventHeartbeat:
		schema = objectSchema{}
	case proto.EventAssistantMessage:
		schema = objectSchema{
			"message": optionalField(validateText),
			"content": optionalField(validateText),
			"role":    optionalField(validateIdentifier),
		}
	case proto.EventModelRequested:
		schema = objectSchema{"model": requiredField(validateIdentifier)}
	case proto.EventModelCompleted:
		schema = objectSchema{
			"model":         optionalField(validateIdentifier),
			"status":        optionalField(validateIdentifier),
			"input_tokens":  optionalField(validateUint),
			"output_tokens": optionalField(validateUint),
			"response":      optionalField(validateBoundedJSON),
		}
	case proto.EventToolRequested:
		schema = objectSchema{
			"request_id": requiredField(validateIdentifier),
			"tool":       requiredField(validateIdentifier),
			"arguments":  optionalField(validateJSONObjectValue),
			"effect_key": optionalField(validateIdentifier),
		}
	case proto.EventToolCompleted:
		schema = objectSchema{
			"request_id": requiredField(validateIdentifier),
			"status":     requiredField(validateIdentifier),
			"result":     optionalField(validateBoundedJSON),
			"error":      optionalField(validateError),
		}
	case proto.EventApprovalRequested:
		schema = objectSchema{
			"approval_id": requiredField(validateIdentifier),
			"reason":      optionalField(validateText),
			"effect_key":  optionalField(validateIdentifier),
		}
	case proto.EventApprovalResolved:
		schema = objectSchema{
			"approval_id": requiredField(validateIdentifier),
			"decision":    requiredField(validateIdentifier),
			"actor":       optionalField(validateIdentifier),
		}
	case proto.EventArtifactCreated:
		schema = objectSchema{
			"artifact_id": requiredField(validateIdentifier),
			"artifact":    optionalField(validateArtifact),
		}
	case proto.EventRunCompleted:
		schema = objectSchema{
			"result":             requiredField(validateResultSummary),
			"output":             optionalField(validateArtifact),
			"sandbox_acceptance": optionalField(validateIdentifier),
		}
	case proto.EventRunFailed:
		schema = objectSchema{"error": requiredField(validateError)}
	case proto.EventRunCancelled:
		schema = objectSchema{"reason": requiredField(validateText)}
	case proto.EventUnknownEffect:
		schema = objectSchema{
			"effect_id": requiredField(validateIdentifier),
			"reason":    optionalField(validateText),
		}
	default:
		return invalid("type", "unsupported event %q", typ)
	}
	if err := validateObject(raw, "data", schema); err != nil {
		return err
	}
	if typ == proto.EventAssistantMessage {
		fields, err := objectFields(raw, "data")
		if err != nil {
			return err
		}
		if _, message := fields["message"]; message == false {
			if _, content := fields["content"]; content == false {
				return invalid("data", "requires message or content")
			}
		}
	}
	return nil
}

func validateRequestData(typ string, raw json.RawMessage) error {
	switch typ {
	case proto.RequestRunStart:
		return validateRunStart(raw)
	case proto.RequestInput:
		return validateObject(raw, "data", objectSchema{
			"reference": requiredField(validateReference),
		})
	case proto.RequestToolResult:
		return validateObject(raw, "data", objectSchema{
			"request_id": requiredField(validateIdentifier),
			"status":     requiredField(validateIdentifier),
			"result":     optionalField(validateBoundedJSON),
			"error":      optionalField(validateError),
		})
	case proto.RequestApproval:
		return validateObject(raw, "data", objectSchema{
			"approval_id": requiredField(validateIdentifier),
			"decision":    requiredField(validateIdentifier),
		})
	case proto.RequestCancel:
		return validateObject(raw, "data", objectSchema{
			"reason":   requiredField(validateText),
			"deadline": optionalField(validateDeadline),
		})
	case proto.RequestPing:
		return validateObject(raw, "data", objectSchema{})
	default:
		return invalid("type", "unsupported request %q", typ)
	}
}

func validateRunStart(raw json.RawMessage) error {
	// This is cmd/agw-runner.RunContract, not ScheduleRunnerTaskInput. Its
	// execution field is workflow.ExecutionContract; the runner's SandboxSpec
	// travels separately in the runner request and is never part of run.start.
	return validateObject(raw, "data", objectSchema{
		"organization_id":   requiredField(validateIdentifier),
		"project_id":        requiredField(validateIdentifier),
		"run_id":            requiredField(validateIdentifier),
		"workflow_name":     requiredField(validateIdentifier),
		"step_id":           requiredField(validateIdentifier),
		"agent_ref":         requiredField(validateReference),
		"input_ref":         optionalField(validateReference),
		"dependency_output": optionalField(validateArtifactArray),
		"execution":         requiredField(validateExecutionContract),
	})
}

func validateExecutionContract(path string, raw json.RawMessage) error {
	return validateObject(raw, path, objectSchema{
		"agent":            requiredField(validateRevision),
		"sandbox_profile":  requiredField(validateRevision),
		"skill_set":        optionalField(validateRevision),
		"tool_set":         optionalField(validateRevision),
		"model_route":      optionalField(validateRevision),
		"inline_skills":    optionalField(validatePinnedReferenceArray),
		"instructions_ref": requiredField(validateReference),
		"instructions":     requiredField(validateNonEmptyText),
		"verification_ref": optionalField(validateReference),
		"verification":     optionalField(validateStringArray),
		"environment":      optionalField(validateEnvironmentArray),
	})
}

func validateRevision(path string, raw json.RawMessage) error {
	return validateObject(raw, path, objectSchema{
		"kind":   requiredField(validateIdentifier),
		"name":   requiredField(validateIdentifier),
		"digest": requiredField(validateDigest),
	})
}

func validatePinnedReferenceArray(path string, raw json.RawMessage) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return invalid(path, "must be an array")
	}
	if len(values) > maxArrayItems {
		return invalid(path, "contains more than %d items", maxArrayItems)
	}
	for index, value := range values {
		if err := validateObject(value, fmt.Sprintf("%s[%d]", path, index), objectSchema{
			"ref":    requiredField(validateReference),
			"digest": requiredField(validateDigest),
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateStringArray(path string, raw json.RawMessage) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return invalid(path, "must be an array")
	}
	if len(values) > maxArrayItems {
		return invalid(path, "contains more than %d items", maxArrayItems)
	}
	for index, value := range values {
		if err := validateNonEmptyText(fmt.Sprintf("%s[%d]", path, index), value); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvironmentArray(path string, raw json.RawMessage) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return invalid(path, "must be an array")
	}
	if len(values) > maxArrayItems {
		return invalid(path, "contains more than %d items", maxArrayItems)
	}
	for index, value := range values {
		if err := validateObject(value, fmt.Sprintf("%s[%d]", path, index), objectSchema{
			"name": requiredField(validateIdentifier),
			"ref":  requiredField(validateReference),
		}); err != nil {
			return err
		}
	}
	return nil
}

func requiredField(check func(string, json.RawMessage) error) fieldRule {
	return fieldRule{required: true, check: check}
}

func optionalField(check func(string, json.RawMessage) error) fieldRule {
	return fieldRule{check: check}
}

func validateObject(raw json.RawMessage, path string, schema objectSchema) error {
	if err := validateJSONShape(raw, path); err != nil {
		return err
	}
	fields, err := objectFields(raw, path)
	if err != nil {
		return err
	}
	unknown := make([]string, 0)
	for name := range fields {
		if _, ok := schema[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) != 0 {
		sort.Strings(unknown)
		return invalid(path+"."+unknown[0], "unknown field")
	}
	for name, rule := range schema {
		value, present := fields[name]
		trimmed := bytes.TrimSpace(value)
		if !present || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			if rule.required {
				return invalid(path+"."+name, "is required and must not be null")
			}
			if present {
				return invalid(path+"."+name, "must not be null")
			}
			continue
		}
		if rule.check != nil {
			if err := rule.check(path+"."+name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func objectFields(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, invalid(path, "must be a JSON object")
	}
	return fields, nil
}

func validateIdentifier(path string, raw json.RawMessage) error {
	return validateString(path, raw, maxProtocolStringBytes, true)
}

func validateReference(path string, raw json.RawMessage) error {
	return validateString(path, raw, maxTextBytes, true)
}

func validateText(path string, raw json.RawMessage) error {
	return validateString(path, raw, maxTextBytes, false)
}

func validateString(path string, raw json.RawMessage, max int, nonEmpty bool) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return invalid(path, "must be a string")
	}
	if len(value) > max {
		return invalid(path, "exceeds %d bytes", max)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return invalid(path, "must not contain NUL")
	}
	if nonEmpty && strings.TrimSpace(value) == "" {
		return invalid(path, "must be non-empty")
	}
	return nil
}

func validateNonEmptyText(path string, raw json.RawMessage) error {
	return validateString(path, raw, maxTextBytes, true)
}

func validateBool(path string, raw json.RawMessage) error {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return invalid(path, "must be a boolean")
	}
	return nil
}

func validateFloat(path string, raw json.RawMessage) error {
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return invalid(path, "must be a number")
	}
	return nil
}

func validateUint(path string, raw json.RawMessage) error {
	var value uint64
	if err := json.Unmarshal(raw, &value); err != nil {
		return invalid(path, "must be an unsigned integer")
	}
	return nil
}

func validateDeadline(path string, raw json.RawMessage) error {
	if err := validateString(path, raw, maxProtocolStringBytes, true); err != nil {
		return err
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return invalid(path, "must be an RFC3339 timestamp")
	}
	return nil
}

func validateResultSummary(path string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return invalid(path, "must be a non-null string or object")
	}
	if trimmed[0] != '"' && trimmed[0] != '{' {
		return invalid(path, "must be a string or object")
	}
	return validateBoundedJSON(path, raw)
}

func validateError(path string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return invalid(path, "must be a non-null string or error object")
	}
	if trimmed[0] == '"' {
		return validateString(path, raw, maxTextBytes, true)
	}
	if trimmed[0] != '{' {
		return invalid(path, "must be a string or error object")
	}
	return validateObject(raw, path, objectSchema{
		"code":    requiredField(validateIdentifier),
		"message": requiredField(validateText),
	})
}

func validateArtifact(path string, raw json.RawMessage) error {
	return validateObject(raw, path, objectSchema{
		"id":         requiredField(validateIdentifier),
		"uri":        requiredField(validateReference),
		"digest":     requiredField(validateDigest),
		"size_bytes": requiredField(validateNonNegativeInt),
		"media_type": optionalField(validateIdentifier),
	})
}

func validateDigest(path string, raw json.RawMessage) error {
	if err := validateString(path, raw, maxProtocolStringBytes, true); err != nil {
		return err
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return invalid(path, "must be a sha256 digest")
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return invalid(path, "must be a lowercase sha256 digest")
		}
	}
	return nil
}

func validateNonNegativeInt(path string, raw json.RawMessage) error {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		return invalid(path, "must be a non-negative integer")
	}
	return nil
}

func validateArtifactArray(path string, raw json.RawMessage) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return invalid(path, "must be an array")
	}
	if len(values) > maxArrayItems {
		return invalid(path, "contains more than %d items", maxArrayItems)
	}
	for index, value := range values {
		if err := validateArtifact(fmt.Sprintf("%s[%d]", path, index), value); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONObjectValue(path string, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return invalid(path, "must be an object")
	}
	return validateBoundedJSON(path, raw)
}

func validateBoundedJSON(path string, raw json.RawMessage) error {
	if err := validateJSONShape(raw, path); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalid(path, "must not be null")
	}
	return nil
}

func validateJSONShape(raw json.RawMessage, path string) error {
	if err := checkJSONShape(raw); err != nil {
		return invalid(path, "%v", err)
	}
	return nil
}

// SequenceValidator enforces one ordered event stream per run.
type SequenceValidator struct {
	runID    string
	lastSeq  uint64
	started  bool
	terminal bool
}

func (v *SequenceValidator) Accept(frame proto.Envelope) error {
	if frame.Kind != proto.KindEvent {
		return invalid("kind", "sequence validation accepts events only")
	}
	if err := ValidateEnvelope(frame); err != nil {
		return err
	}
	if v.runID == "" {
		v.runID = frame.RunID
	}
	if frame.RunID != v.runID {
		return invalid("run_id", "does not match stream run %q", v.runID)
	}
	if v.terminal {
		return invalid("sequence", "event received after terminal event")
	}
	if frame.Seq != v.lastSeq+1 {
		return invalid("seq", "expected %d, got %d", v.lastSeq+1, frame.Seq)
	}
	if !v.started && frame.Type != proto.EventRunStarted {
		return invalid("type", "first event must be %q", proto.EventRunStarted)
	}
	if v.started && frame.Type == proto.EventRunStarted {
		return invalid("type", "run.started may only occur once")
	}
	v.lastSeq = frame.Seq
	if frame.Type == proto.EventRunStarted {
		v.started = true
	}
	if frame.Terminal {
		v.terminal = true
	}
	return nil
}

// RedactionHook can replace a leaf or object value before a frame is logged.
// Returning redact=false preserves the original value. Hooks should be
// deterministic and must not mutate the supplied bytes.
type RedactionHook func(path string, value json.RawMessage) (replacement json.RawMessage, redact bool, err error)

func ApplyRedaction(frame proto.Envelope, hook RedactionHook) (proto.Envelope, error) {
	if hook == nil {
		return frame, nil
	}
	if err := ValidateEnvelope(frame); err != nil {
		return proto.Envelope{}, err
	}
	data, err := redactValue(frame.Data, "data", hook)
	if err != nil {
		return proto.Envelope{}, err
	}
	frame.Data = data
	if len(data) > MaxDataBytes {
		return proto.Envelope{}, ErrFrameTooLarge
	}
	// Redaction is a transformation at a trusted boundary, but hooks are
	// caller-supplied. Re-run the complete schema after transformation so a
	// hook cannot accidentally turn a valid frame into a protocol-invalid one.
	if err := ValidateEnvelope(frame); err != nil {
		return proto.Envelope{}, invalid("data", "redaction produced invalid payload: %v", err)
	}
	return frame, nil
}

func redactValue(raw json.RawMessage, path string, hook RedactionHook) (json.RawMessage, error) {
	if replacement, redact, err := hook(path, append(json.RawMessage(nil), raw...)); err != nil {
		return nil, err
	} else if redact {
		if !json.Valid(replacement) {
			return nil, invalid(path, "redaction replacement is invalid JSON")
		}
		return replacement, nil
	}
	trimmed := bytes.TrimSpace(raw)
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		for key, value := range obj {
			child, err := redactValue(value, path+"."+key, hook)
			if err != nil {
				return nil, err
			}
			obj[key] = child
		}
		return json.Marshal(obj)
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, err
		}
		for i, value := range values {
			child, err := redactValue(value, fmt.Sprintf("%s[%d]", path, i), hook)
			if err != nil {
				return nil, err
			}
			values[i] = child
		}
		return json.Marshal(values)
	default:
		return trimmed, nil
	}
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err == nil {
		return ErrTrailingData
	} else {
		return invalid("json", "%v", err)
	}
}

// checkJSONShape detects duplicate keys, which encoding/json otherwise
// silently accepts. It also proves that the entire input is one JSON value.
func checkJSONShape(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := walkValue(dec, "", 0); err != nil {
		return invalid("json", "%v", err)
	}
	if err := ensureEOF(dec); err != nil {
		return err
	}
	return nil
}

func walkValue(dec *json.Decoder, path string, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels at %s", maxJSONDepth, path)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch value := tok.(type) {
	case json.Delim:
		switch value {
		case '{':
			keys := map[string]struct{}{}
			for dec.More() {
				if len(keys) >= maxObjectFields {
					return fmt.Errorf("object exceeds %d fields at %s", maxObjectFields, path)
				}
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := keys[key]; exists {
					return fmt.Errorf("duplicate object key %q at %s", key, path)
				}
				keys[key] = struct{}{}
				if err := walkValue(dec, path+"."+key, depth+1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			index := 0
			for dec.More() {
				if index >= maxArrayItems {
					return fmt.Errorf("array exceeds %d items at %s", maxArrayItems, path)
				}
				if err := walkValue(dec, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
					return err
				}
				index++
			}
			_, err = dec.Token()
			return err
		default:
			return fmt.Errorf("unexpected delimiter %q", value)
		}
	default:
		if stringValue, ok := tok.(string); ok && len(stringValue) > maxTextBytes {
			return fmt.Errorf("string exceeds %d bytes at %s", maxTextBytes, path)
		}
		return nil
	}
}
