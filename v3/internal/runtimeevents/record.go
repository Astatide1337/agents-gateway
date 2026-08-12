package runtimeevents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

type frameIndex struct {
	SchemaVersion  int    `json:"schemaVersion"`
	IdentityDigest string `json:"identityDigest"`
	RunUID         string `json:"runUID"`
	SpecDigest     string `json:"specDigest"`
	Seq            uint64 `json:"seq"`
	LineDigest     string `json:"lineDigest"`
	SizeBytes      uint64 `json:"sizeBytes"`
}

type streamManifest struct {
	SchemaVersion  int          `json:"schemaVersion"`
	IdentityDigest string       `json:"identityDigest"`
	RunUID         string       `json:"runUID"`
	SpecDigest     string       `json:"specDigest"`
	FrameCount     uint64       `json:"frameCount"`
	SizeBytes      uint64       `json:"sizeBytes"`
	StreamDigest   string       `json:"streamDigest"`
	TerminalType   TerminalType `json:"terminalType"`
	TerminalSeq    uint64       `json:"terminalSeq"`
}

func maxFrameStorageBytes() int {
	// The protocol's frame limit excludes the newline that makes it JSONL.
	return runtimeproto.MaxFrameBytes + 1
}

func validateArtifactRef(ref ArtifactRef) error {
	if !safeURI(ref.URI) || !canonical.ValidDigest(ref.Digest) || ref.SizeBytes <= 0 || uint64(ref.SizeBytes) > MaxStreamBytes {
		return ErrInvalid
	}
	if ref.Kind == "" || len(ref.Kind) > maxArtifactKind || !safeDescriptor(ref.Kind) {
		return ErrInvalid
	}
	if ref.Name == "" || len(ref.Name) > maxArtifactName || !safeDescriptor(ref.Name) {
		return ErrInvalid
	}
	if ref.MediaType == "" || len(ref.MediaType) > maxMediaType || strings.ContainsAny(ref.MediaType, "\x00\r\n") {
		return ErrInvalid
	}
	return nil
}

func validateCompletion(record CompletionRecord) error {
	if record.SchemaVersion != schemaVersion || !validRunUID(record.RunUID) || !canonical.ValidDigest(record.SpecDigest) || !validBaseSHA(record.BaseSHA) || record.TerminalSeq == 0 || !validTerminalType(record.TerminalType) {
		return ErrInvalid
	}
	if err := validateArtifactRef(record.EventStream); err != nil {
		return err
	}
	if len(record.Summary.ErrorCode) > maxSummaryBytes || len(record.Summary.EffectID) > maxSummaryBytes || !safeSummaryIdentifier(record.Summary.ErrorCode) || !safeSummaryIdentifier(record.Summary.EffectID) {
		return ErrInvalid
	}
	if record.TerminalType != TerminalFailed && record.Summary.ErrorCode != "" {
		return ErrInvalid
	}
	if record.TerminalType != TerminalUnknownEffect && record.Summary.EffectID != "" {
		return ErrInvalid
	}
	if record.TerminalType != TerminalCompleted && record.Summary.OutputPresent {
		return ErrInvalid
	}
	return nil
}

func validateFrameIndex(index frameIndex) error {
	if index.SchemaVersion != schemaVersion || !validRunUID(index.RunUID) || !canonical.ValidDigest(index.SpecDigest) || !validIdentityDigest(index.IdentityDigest) || index.IdentityDigest != identityDigestHex(index.RunUID, index.SpecDigest) || index.Seq == 0 || index.Seq > maxStreamFrames || !canonical.ValidDigest(index.LineDigest) || index.SizeBytes == 0 || index.SizeBytes > uint64(maxFrameStorageBytes()) {
		return ErrInvalid
	}
	return nil
}

func validateManifest(manifest streamManifest) error {
	if manifest.SchemaVersion != schemaVersion || !validRunUID(manifest.RunUID) || !canonical.ValidDigest(manifest.SpecDigest) || !validIdentityDigest(manifest.IdentityDigest) || manifest.IdentityDigest != identityDigestHex(manifest.RunUID, manifest.SpecDigest) || manifest.FrameCount == 0 || manifest.FrameCount > maxStreamFrames || manifest.SizeBytes == 0 || manifest.SizeBytes > MaxStreamBytes || !canonical.ValidDigest(manifest.StreamDigest) || !validTerminalType(manifest.TerminalType) || manifest.TerminalSeq != manifest.FrameCount {
		return ErrInvalid
	}
	return nil
}

func validIdentityDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validTerminalType(value TerminalType) bool {
	switch value {
	case TerminalCompleted, TerminalFailed, TerminalCancelled, TerminalUnknownEffect:
		return true
	default:
		return false
	}
}

func safeDescriptor(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return false
	}
	return true
}

func safeSummaryIdentifier(value string) bool {
	if value == "" {
		return true
	}
	return safeDescriptor(value) && len(value) <= maxSummaryBytes
}

func terminalType(eventType string) (TerminalType, bool) {
	switch eventType {
	case proto.EventRunCompleted:
		return TerminalCompleted, true
	case proto.EventRunFailed:
		return TerminalFailed, true
	case proto.EventRunCancelled:
		return TerminalCancelled, true
	case proto.EventUnknownEffect:
		return TerminalUnknownEffect, true
	default:
		return "", false
	}
}

func terminalSummary(frame proto.Envelope) (TerminalSummary, error) {
	if _, ok := terminalType(frame.Type); !ok || !frame.Terminal {
		return TerminalSummary{}, ErrInvalidFrame
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frame.Data, &fields); err != nil || fields == nil {
		return TerminalSummary{}, ErrInvalidFrame
	}
	var summary TerminalSummary
	switch frame.Type {
	case proto.EventRunCompleted:
		_, summary.OutputPresent = fields["output"]
	case proto.EventRunFailed:
		errorValue := fields["error"]
		if len(bytes.TrimSpace(errorValue)) > 0 && bytes.TrimSpace(errorValue)[0] == '{' {
			var details struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(errorValue, &details); err != nil {
				return TerminalSummary{}, ErrInvalidFrame
			}
			summary.ErrorCode = details.Code
		}
	case proto.EventUnknownEffect:
		if err := json.Unmarshal(fields["effect_id"], &summary.EffectID); err != nil {
			return TerminalSummary{}, ErrInvalidFrame
		}
	}
	if len(summary.ErrorCode) > maxSummaryBytes || len(summary.EffectID) > maxSummaryBytes || !safeSummaryIdentifier(summary.ErrorCode) || !safeSummaryIdentifier(summary.EffectID) {
		return TerminalSummary{}, ErrInvalidFrame
	}
	return summary, nil
}

func parseStoredLine(body []byte) (proto.Envelope, error) {
	if len(body) == 0 || !bytes.HasSuffix(body, []byte{'\n'}) {
		return proto.Envelope{}, ErrCorrupt
	}
	frame, err := runtimeproto.ParseLine(body)
	if err != nil {
		return proto.Envelope{}, ErrCorrupt
	}
	return frame, nil
}

func canonicalLine(frame proto.Envelope) ([]byte, error) {
	encoded, err := runtimeproto.EncodeLine(frame)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(encoded)
	normalized, err := strictjson.Normalize(trimmed)
	if err != nil {
		return nil, err
	}
	return append(normalized, '\n'), nil
}

func canonicalRecord(record CompletionRecord) ([]byte, error) {
	if err := validateCompletion(record); err != nil {
		return nil, err
	}
	return canonicalBytes(record)
}

func canonicalIndex(index frameIndex) ([]byte, error) {
	if err := validateFrameIndex(index); err != nil {
		return nil, err
	}
	return canonicalBytes(index)
}

func canonicalManifest(manifest streamManifest) ([]byte, error) {
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	return canonicalBytes(manifest)
}

func invalidField(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
