package runtimeproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/proto"
)

func event(typ string, seq uint64, terminal bool, data string) proto.Envelope {
	return proto.Envelope{
		Protocol: proto.ProtocolVersion,
		Kind:     proto.KindEvent,
		Type:     typ,
		RunID:    "run-1",
		Seq:      seq,
		Terminal: terminal,
		Data:     []byte(data),
	}
}

func TestParseLineStrictAndRoundTrip(t *testing.T) {
	frame := event(proto.EventRunStarted, 1, false, `{"agent_id":"a","sandbox_id":"s"}`)
	line, err := EncodeLine(frame)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != frame.RunID || got.Type != frame.Type || string(got.Data) != string(frame.Data) {
		t.Fatalf("round trip mismatch: %#v", got)
	}

	for name, input := range map[string]string{
		"unknown top-level field":   `{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"terminal":false,"data":{},"extra":1}`,
		"duplicate top-level field": `{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"seq":2,"terminal":false,"data":{}}`,
		"trailing value":            `{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"terminal":false,"data":{}} {}`,
		"non-object data":           `{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"terminal":false,"data":[]}`,
		"malformed data":            `{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"terminal":false,"data":{"x":}}`,
		"whitespace data":           `{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"terminal":false,"data":   }`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLine([]byte(input)); err == nil {
				t.Fatal("expected strict parse error")
			}
		})
	}
}

func TestParseLineSizeLimit(t *testing.T) {
	line := []byte(`{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"r","seq":1,"terminal":false,"data":{"x":"` + strings.Repeat("x", MaxFrameBytes) + `"}}`)
	if _, err := ParseLine(line); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected frame size error, got %v", err)
	}
}

func TestEventValidationAndTerminalState(t *testing.T) {
	validator := new(SequenceValidator)
	frames := []proto.Envelope{
		event(proto.EventRunStarted, 1, false, `{"agent_id":"a","sandbox_id":"s"}`),
		event(proto.EventToolRequested, 2, false, `{"request_id":"q","tool":"read"}`),
		event(proto.EventRunCompleted, 3, true, `{"result":{}}`),
	}
	for _, frame := range frames {
		if err := validator.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := validator.Accept(event(proto.EventHeartbeat, 4, false, `{}`)); err == nil {
		t.Fatal("expected post-terminal event rejection")
	}

	badTerminal := event(proto.EventHeartbeat, 1, true, `{}`)
	if err := ValidateEnvelope(badTerminal); err == nil {
		t.Fatal("expected non-terminal event to reject terminal=true")
	}
	if err := (&SequenceValidator{}).Accept(event(proto.EventHeartbeat, 1, false, `{}`)); err == nil {
		t.Fatal("expected stream to require run.started")
	}
	if err := (&SequenceValidator{}).Accept(event(proto.EventRunStarted, 2, false, `{"agent_id":"a","sandbox_id":"s"}`)); err == nil {
		t.Fatal("expected first sequence number to be one")
	}
}

func TestRunCompletedAcceptsBoundedSandboxAcceptanceMarker(t *testing.T) {
	frame := event(proto.EventRunCompleted, 1, true, `{"result":"completed","sandbox_acceptance":"pass"}`)
	if err := ValidateEnvelope(frame); err != nil {
		t.Fatalf("sandbox acceptance marker was rejected: %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	request := proto.Envelope{
		Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestApproval,
		RunID: "run-1", Seq: 1, Data: []byte(`{"approval_id":"a","decision":"approve"}`),
	}
	if err := ValidateEnvelope(request); err != nil {
		t.Fatal(err)
	}
	request.Data = []byte(`{"decision":"approve"}`)
	if err := ValidateEnvelope(request); err == nil {
		t.Fatal("expected approval_id requirement")
	}
}

func TestStrictSchemasAcceptEverySupportedKind(t *testing.T) {
	artifact := `{"id":"artifact-1","uri":"artifact://one","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":0,"media_type":"application/json"}`
	events := []struct {
		typ      string
		terminal bool
		data     string
	}{
		{proto.EventRunStarted, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`},
		{proto.EventHeartbeat, false, `{}`},
		{proto.EventAssistantMessage, false, `{"message":"hello","role":"assistant"}`},
		{proto.EventModelRequested, false, `{"model":"model-1"}`},
		{proto.EventModelCompleted, false, `{"model":"model-1","status":"ok","input_tokens":2,"output_tokens":3,"response":{"ok":true}}`},
		{proto.EventToolRequested, false, `{"request_id":"request-1","tool":"files.read","arguments":{"path":"README.md"},"effect_key":"effect-1"}`},
		{proto.EventToolCompleted, false, `{"request_id":"request-1","status":"ok","result":{"content":[{"type":"text","text":"ok"}]}}`},
		{proto.EventApprovalRequested, false, `{"approval_id":"approval-1","reason":"write effect","effect_key":"effect-1"}`},
		{proto.EventApprovalResolved, false, `{"approval_id":"approval-1","decision":"approved","actor":"operator"}`},
		{proto.EventArtifactCreated, false, `{"artifact_id":"artifact-1","artifact":` + artifact + `}`},
		{proto.EventRunCompleted, true, `{"result":"completed","output":` + artifact + `}`},
		{proto.EventRunFailed, true, `{"error":{"code":"adapter_failed","message":"adapter failed"}}`},
		{proto.EventRunCancelled, true, `{"reason":"operator requested cancellation"}`},
		{proto.EventUnknownEffect, true, `{"effect_id":"effect-1","reason":"upstream outcome ambiguous"}`},
	}
	for sequence, test := range events {
		t.Run(test.typ, func(t *testing.T) {
			if err := ValidateEnvelope(event(test.typ, uint64(sequence+1), test.terminal, test.data)); err != nil {
				t.Fatalf("valid %s rejected: %v", test.typ, err)
			}
		})
	}

	requests := []struct {
		typ  string
		data string
	}{
		{proto.RequestRunStart, validRunStartData(t)},
		{proto.RequestInput, `{"reference":"input://run-1"}`},
		{proto.RequestToolResult, `{"request_id":"request-1","status":"ok","result":{"ok":true}}`},
		{proto.RequestApproval, `{"approval_id":"approval-1","decision":"approved"}`},
		{proto.RequestCancel, `{"reason":"operator requested cancellation","deadline":"2026-08-09T12:00:00Z"}`},
		{proto.RequestPing, `{}`},
	}
	for _, test := range requests {
		t.Run(test.typ, func(t *testing.T) {
			frame := proto.Envelope{
				Protocol: proto.ProtocolVersion,
				Kind:     proto.KindRequest,
				Type:     test.typ,
				RunID:    "run-1",
				Seq:      1,
				Data:     json.RawMessage(test.data),
			}
			if err := ValidateEnvelope(frame); err != nil {
				t.Fatalf("valid %s rejected: %v", test.typ, err)
			}
		})
	}
}

func TestStrictSchemasRejectAdversarialPayloads(t *testing.T) {
	artifact := `{"id":"artifact-1","uri":"artifact://one","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":0}`
	cases := []struct {
		name  string
		frame proto.Envelope
	}{
		{
			name:  "required string has wrong type",
			frame: event(proto.EventRunStarted, 1, false, `{"agent_id":true,"sandbox_id":"sandbox"}`),
		},
		{
			name:  "required string is null",
			frame: event(proto.EventModelRequested, 1, false, `{"model":null}`),
		},
		{
			name:  "unknown event field",
			frame: event(proto.EventHeartbeat, 1, false, `{"timestamp":"now"}`),
		},
		{
			name:  "unknown nested artifact field",
			frame: event(proto.EventRunCompleted, 1, true, `{"result":"ok","output":`+strings.TrimSuffix(artifact, "}")+`,"extra":true}}`),
		},
		{
			name:  "artifact size is negative",
			frame: event(proto.EventArtifactCreated, 1, false, `{"artifact_id":"artifact-1","artifact":{"id":"artifact-1","uri":"artifact://one","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":-1}}`),
		},
		{
			name:  "tool arguments must be object",
			frame: event(proto.EventToolRequested, 1, false, `{"request_id":"request-1","tool":"files.read","arguments":[]}`),
		},
		{
			name:  "error object rejects unknown nested field",
			frame: event(proto.EventRunFailed, 1, true, `{"error":{"code":"failed","message":"no","details":{}}}`),
		},
		{
			name:  "result rejects scalar number",
			frame: event(proto.EventRunCompleted, 1, true, `{"result":42}`),
		},
		{
			name:  "request rejects unknown field",
			frame: proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestApproval, RunID: "run-1", Seq: 1, Data: []byte(`{"approval_id":"approval-1","decision":"approved","actor":"operator"}`)},
		},
		{
			name:  "request string has wrong type",
			frame: proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestInput, RunID: "run-1", Seq: 1, Data: []byte(`{"reference":7}`)},
		},
		{
			name:  "cancel deadline is not timestamp",
			frame: proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestCancel, RunID: "run-1", Seq: 1, Data: []byte(`{"reason":"stop","deadline":"tomorrow"}`)},
		},
		{
			name:  "run start rejects unknown nested execution field",
			frame: proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: "run-1", Seq: 1, Data: []byte(strings.Replace(validRunStartData(t), `"instructions":`, `"unexpected":true,"instructions":`, 1))},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateEnvelope(test.frame); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestStrictSchemasRejectBoundariesAndDuplicateNestedKeys(t *testing.T) {
	tooLong := strings.Repeat("x", maxTextBytes+1)
	if err := ValidateEnvelope(event(proto.EventAssistantMessage, 1, false, fmt.Sprintf(`{"message":%q}`, tooLong))); err == nil {
		t.Fatal("expected oversized message rejection")
	}
	if err := ValidateEnvelope(event(proto.EventToolRequested, 1, false, `{"request_id":"request-1","tool":"files.read","arguments":{"path":"ok"},"arguments":{"path":"duplicate"}}`)); err == nil {
		t.Fatal("expected duplicate nested field rejection")
	}

	deep := `"leaf"`
	for index := 0; index < maxJSONDepth+2; index++ {
		deep = `{"nested":` + deep + `}`
	}
	if err := ValidateEnvelope(event(proto.EventToolCompleted, 1, false, `{"request_id":"request-1","status":"ok","result":`+deep+`}`)); err == nil {
		t.Fatal("expected excessive nesting rejection")
	}

	items := make([]string, maxArrayItems+1)
	for index := range items {
		items[index] = `{"id":"a","uri":"artifact://a","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":0}`
	}
	data := `{"organization_id":"org","project_id":"project","run_id":"run-1","workflow_name":"workflow","step_id":"step","agent_ref":"agent","execution":` + validContractData() + `,"dependency_output":[` + strings.Join(items, ",") + `]}`
	frame := proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: "run-1", Seq: 1, Data: []byte(data)}
	if err := ValidateEnvelope(frame); err == nil {
		t.Fatal("expected oversized dependency_output rejection")
	}
}

func validRunStartData(t *testing.T) string {
	t.Helper()
	return `{"organization_id":"org","project_id":"project","run_id":"run-1","workflow_name":"workflow","step_id":"step","agent_ref":"agent","input_ref":"input://run-1","dependency_output":[],"execution":` + validContractData() + `}`
}

func validContractData() string {
	return `{"agent":{"kind":"agent","name":"agent","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"},"sandbox_profile":{"kind":"sandbox","name":"sandbox","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"},"instructions_ref":"instructions://run-1","instructions":"run the task","verification_ref":"verification://run-1","verification":["true"],"environment":[{"name":"MODE","ref":"secret://mode"}]}`
}

func TestApplyRedaction(t *testing.T) {
	frame := event(proto.EventToolRequested, 1, false, `{"request_id":"q","tool":"github","arguments":{"token":"secret","repo":"public"}}`)
	redacted, err := ApplyRedaction(frame, func(path string, value json.RawMessage) (json.RawMessage, bool, error) {
		if strings.HasSuffix(path, ".token") {
			return []byte(`"[REDACTED]"`), true, nil
		}
		return nil, false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(redacted.Data, []byte("secret")) || !bytes.Contains(redacted.Data, []byte("[REDACTED]")) {
		t.Fatalf("unexpected redaction result: %s", redacted.Data)
	}
}

func TestDecoderAcceptsFinalLineAndRejectsOversizeLine(t *testing.T) {
	valid, err := EncodeLine(event(proto.EventHeartbeat, 1, false, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	decoder := NewDecoder(bytes.NewReader(bytes.TrimSuffix(valid, []byte("\n"))))
	if _, err := decoder.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}

	decoder = NewDecoder(bytes.NewReader(append(bytes.Repeat([]byte{'x'}, MaxFrameBytes+2), '\n')))
	if _, err := decoder.Next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected oversize error, got %v", err)
	}
	decoder = NewDecoder(bytes.NewReader(bytes.Repeat([]byte{'x'}, MaxFrameBytes+2)))
	if _, err := decoder.Next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected oversize unterminated error, got %v", err)
	}
}
