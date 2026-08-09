// Package proto contains the wire types for the Agents Gateway runtime
// protocol. The package intentionally contains no transport or validation
// logic so callers can use the same representation for JSONL and other
// transports later.
package proto

import "encoding/json"

const (
	ProtocolVersion = "agw.runtime.v1"

	KindEvent   = "event"
	KindRequest = "request"
)

const (
	EventRunStarted        = "run.started"
	EventHeartbeat         = "heartbeat"
	EventAssistantMessage  = "assistant.message"
	EventModelRequested    = "model.requested"
	EventModelCompleted    = "model.completed"
	EventToolRequested     = "tool.requested"
	EventToolCompleted     = "tool.completed"
	EventApprovalRequested = "approval.requested"
	EventApprovalResolved  = "approval.resolved"
	EventArtifactCreated   = "artifact.created"
	EventRunCompleted      = "run.completed"
	EventRunFailed         = "run.failed"
	EventRunCancelled      = "run.cancelled"
	EventUnknownEffect     = "run.unknown_effect"
)

const (
	RequestRunStart   = "run.start"
	RequestInput      = "input"
	RequestToolResult = "tool.result"
	RequestApproval   = "approval.result"
	RequestCancel     = "run.cancel"
	RequestPing       = "ping"
)

// Envelope is one JSONL frame. Data is always a JSON object for v1. Keeping
// the payload raw lets runtime adapters evolve their typed payloads without
// making this transport package depend on every adapter.
type Envelope struct {
	Protocol string          `json:"protocol"`
	Kind     string          `json:"kind"`
	Type     string          `json:"type"`
	RunID    string          `json:"run_id"`
	Seq      uint64          `json:"seq"`
	Terminal bool            `json:"terminal"`
	Data     json.RawMessage `json:"data"`
}

// ErrorPayload is the stable error shape used by failed runtime frames.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
