package runtimeevents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"strings"
	"sync"

	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

const (
	DefaultWindowFrames = 32
	DefaultWindowBytes  = 256 << 10
	maxWindowFrames     = 128
	maxWindowBytes      = 1 << 20
)

// Options controls only the bounded projection. It cannot increase the
// protocol frame or persistence limits. The default redactor always runs;
// Redactor is an additional application-specific redaction hook.
type Options struct {
	WindowFrames int
	WindowBytes  int
	Redactor     runtimeproto.RedactionHook
}

// FrameProjection is safe to expose in status/log views. It contains no
// payload, message, argument, error, or diagnostic text.
type FrameProjection struct {
	Seq      uint64
	Type     string
	Bytes    uint64
	Terminal bool
}

// Projection is the bounded current view of a collector. The stream itself is
// never represented here.
type Projection struct {
	RunUID         string
	SpecDigest     string
	AcceptedFrames uint64
	AcceptedBytes  uint64
	NextSeq        uint64
	TerminalType   TerminalType
	TerminalSeq    uint64
	ProcessExited  bool
	Finalized      bool
	Window         []FrameProjection
}

// ProcessExit carries the process-side observation. Known must be true when
// the supervisor has a definitive wait result. An unknown process outcome is
// never allowed to create a completion record.
type ProcessExit struct {
	Code  int
	Known bool
}

// Collector is safe for concurrent callers. Store calls are serialized with
// stream state transitions so a failed/ambiguous write can never be mistaken
// for an accepted event.
type Collector struct {
	mu sync.Mutex

	repo       *Repository
	store      Store
	runUID     string
	specDigest string
	baseSHA    string
	identity   string
	window     []FrameProjection
	windowSize uint64
	windowMax  int
	windowByte int
	redactor   runtimeproto.RedactionHook

	nextSeq        uint64
	sequence       runtimeproto.SequenceValidator
	acceptedFrames uint64
	acceptedBytes  uint64
	streamHash     hash.Hash
	terminalType   TerminalType
	terminalSeq    uint64
	terminalSum    TerminalSummary
	processExited  bool
	completion     *CompletionRecord
	terminalError  error
	fatal          error
}

func NewCollector(runUID, specDigest, baseSHA string, store Store, options Options) (*Collector, error) {
	if !validRunUID(runUID) || !isValidSpecDigest(specDigest) || !validBaseSHA(baseSHA) || store == nil {
		return nil, ErrInvalid
	}
	windowFrames := options.WindowFrames
	if windowFrames == 0 {
		windowFrames = DefaultWindowFrames
	}
	windowBytes := options.WindowBytes
	if windowBytes == 0 {
		windowBytes = DefaultWindowBytes
	}
	if windowFrames < 1 || windowFrames > maxWindowFrames || windowBytes < 1 || windowBytes > maxWindowBytes {
		return nil, ErrInvalid
	}
	return &Collector{
		repo:       &Repository{store: store},
		store:      store,
		runUID:     runUID,
		specDigest: specDigest,
		baseSHA:    baseSHA,
		identity:   identityDigestHex(runUID, specDigest),
		windowMax:  windowFrames,
		windowByte: windowBytes,
		redactor:   composeRedactors(options.Redactor),
		nextSeq:    1,
		streamHash: sha256.New(),
	}, nil
}

// Accept parses and durably records one JSONL event. Any invalid frame,
// sequence violation, wrong run, or ambiguous persistence result permanently
// rejects the collector. This is intentional: the caller cannot safely resume
// a stream after losing the persistence boundary.
func (c *Collector) Accept(ctx context.Context, line []byte) error {
	if c == nil {
		return ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal != nil {
		return c.fatal
	}
	if c.processExited {
		return c.rejectLocked(ErrStreamClosed)
	}
	if c.terminalType != "" {
		return c.rejectLocked(ErrSequence)
	}
	if err := ctx.Err(); err != nil {
		return c.rejectLocked(ErrAmbiguous)
	}

	frame, err := runtimeproto.ParseLine(line)
	if err != nil {
		return c.rejectLocked(fmt.Errorf("%w: protocol parse failed", ErrInvalidFrame))
	}
	if frame.Kind != proto.KindEvent {
		return c.rejectLocked(ErrInvalidFrame)
	}
	if frame.RunID != c.runUID {
		return c.rejectLocked(ErrWrongRun)
	}
	if err := c.sequence.Accept(frame); err != nil {
		return c.rejectLocked(fmt.Errorf("%w: %v", ErrSequence, err))
	}
	frame, err = runtimeproto.ApplyRedaction(frame, c.redactor)
	if err != nil {
		return c.rejectLocked(fmt.Errorf("%w: redaction failed", ErrInvalidFrame))
	}
	if frame.RunID != c.runUID {
		return c.rejectLocked(ErrWrongRun)
	}
	lineBytes, err := canonicalLine(frame)
	if err != nil || len(lineBytes) > maxFrameStorageBytes() {
		return c.rejectLocked(fmt.Errorf("%w: canonical frame failed", ErrInvalidFrame))
	}
	if c.acceptedBytes > MaxStreamBytes-uint64(len(lineBytes)) {
		return c.rejectLocked(ErrStreamTooLarge)
	}
	lineDigest := digestBytes(lineBytes)
	if err := c.persistFrameLocked(ctx, frame, lineBytes, lineDigest); err != nil {
		return c.rejectLocked(err)
	}
	if _, err := c.streamHash.Write(lineBytes); err != nil {
		return c.rejectLocked(ErrAmbiguous)
	}
	c.acceptedFrames++
	c.acceptedBytes += uint64(len(lineBytes))
	c.nextSeq++
	c.appendProjectionLocked(FrameProjection{Seq: frame.Seq, Type: frame.Type, Bytes: uint64(len(lineBytes)), Terminal: frame.Terminal})
	if frame.Terminal {
		kind, ok := terminalType(frame.Type)
		if !ok {
			return c.rejectLocked(ErrInvalidFrame)
		}
		summary, err := terminalSummary(frame)
		if err != nil {
			return c.rejectLocked(err)
		}
		c.terminalType = kind
		c.terminalSeq = frame.Seq
		c.terminalSum = summary
	}
	return nil
}

func (c *Collector) persistFrameLocked(ctx context.Context, frame proto.Envelope, lineBytes []byte, lineDigest string) error {
	if _, err := putImmutable(ctx, c.store, frameKey(c.runUID, lineDigest), lineBytes, "application/x-ndjson", maxFrameStorageBytes()); err != nil {
		return err
	}
	index := frameIndex{
		SchemaVersion:  schemaVersion,
		IdentityDigest: c.identity,
		RunUID:         c.runUID,
		SpecDigest:     c.specDigest,
		Seq:            frame.Seq,
		LineDigest:     lineDigest,
		SizeBytes:      uint64(len(lineBytes)),
	}
	body, err := canonicalIndex(index)
	if err != nil {
		return ErrInvalid
	}
	_, err = putImmutable(ctx, c.store, indexKey(c.runUID, c.identity, frame.Seq), body, "application/json", MaxIndexBytes)
	return err
}

// ProcessExit closes the input boundary and creates a completion record only
// after a terminal event and a definitive process observation both exist.
// Terminal failure/cancellation/unknown-effect events remain valid terminal
// outcomes even when their process exits non-zero; a run.completed event with a
// non-zero exit is rejected because the two signals contradict one another.
func (c *Collector) ProcessExit(ctx context.Context, exit ProcessExit) (CompletionRecord, error) {
	var zero CompletionRecord
	if c == nil {
		return zero, ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.completion != nil {
		return *c.completion, nil
	}
	if c.fatal != nil {
		return zero, c.fatal
	}
	if c.processExited {
		if c.terminalError != nil {
			return zero, c.terminalError
		}
		return zero, ErrStreamClosed
	}
	c.processExited = true
	if c.terminalType == "" {
		c.terminalError = ErrNoTerminal
		return zero, ErrNoTerminal
	}
	if !exit.Known {
		c.terminalError = ErrProcessExitUnknown
		return zero, ErrProcessExitUnknown
	}
	if c.terminalType == TerminalCompleted && exit.Code != 0 {
		c.terminalError = ErrProcessExitFailed
		return zero, ErrProcessExitFailed
	}
	record, err := c.finalizeLocked(ctx)
	if err != nil {
		return zero, c.rejectLocked(err)
	}
	c.completion = &record
	return record, nil
}

// Finish is a convenience for supervisors with a definitive exit code.
func (c *Collector) Finish(ctx context.Context, exitCode int) (CompletionRecord, error) {
	return c.ProcessExit(ctx, ProcessExit{Code: exitCode, Known: true})
}

func (c *Collector) finalizeLocked(ctx context.Context) (CompletionRecord, error) {
	streamDigest := formatDigest(c.streamHash.Sum(nil))
	manifest := streamManifest{
		SchemaVersion:  schemaVersion,
		IdentityDigest: c.identity,
		RunUID:         c.runUID,
		SpecDigest:     c.specDigest,
		FrameCount:     c.acceptedFrames,
		SizeBytes:      c.acceptedBytes,
		StreamDigest:   streamDigest,
		TerminalType:   c.terminalType,
		TerminalSeq:    c.terminalSeq,
	}
	manifestBody, err := canonicalManifest(manifest)
	if err != nil {
		return CompletionRecord{}, ErrInvalid
	}
	manifestURI, err := putImmutable(ctx, c.store, streamKey(c.runUID, streamDigest), manifestBody, "application/vnd.agw.runtime-stream+json", MaxManifestBytes)
	if err != nil {
		return CompletionRecord{}, err
	}
	record := CompletionRecord{
		SchemaVersion: schemaVersion,
		RunUID:        c.runUID,
		SpecDigest:    c.specDigest,
		BaseSHA:       c.baseSHA,
		TerminalType:  c.terminalType,
		TerminalSeq:   c.terminalSeq,
		EventStream: ArtifactRef{
			URI:       manifestURI,
			Digest:    streamDigest,
			Kind:      "runtime-event-stream",
			Name:      "events.jsonl",
			MediaType: "application/x-ndjson",
			SizeBytes: int64(c.acceptedBytes),
		},
		Summary: c.terminalSum,
	}
	recordBody, err := canonicalRecord(record)
	if err != nil {
		return CompletionRecord{}, ErrInvalid
	}
	key, err := CompletionKey(c.runUID, c.specDigest)
	if err != nil {
		return CompletionRecord{}, err
	}
	if _, err := putImmutable(ctx, c.store, key, recordBody, "application/json", MaxCompletionBytes); err != nil {
		return CompletionRecord{}, err
	}
	return record, nil
}

// Completion returns the local immutable record if one was created. It is
// ErrNotReady before ProcessExit; a process exit without a terminal is
// reported as ErrNoTerminal so callers can distinguish the two states.
func (c *Collector) Completion() (CompletionRecord, error) {
	var zero CompletionRecord
	if c == nil {
		return zero, ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.completion != nil {
		return *c.completion, nil
	}
	if c.terminalError != nil {
		return zero, c.terminalError
	}
	if c.fatal != nil {
		return zero, c.fatal
	}
	return zero, ErrNotReady
}

func (c *Collector) Projection() Projection {
	if c == nil {
		return Projection{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	window := append([]FrameProjection(nil), c.window...)
	return Projection{RunUID: c.runUID, SpecDigest: c.specDigest, AcceptedFrames: c.acceptedFrames, AcceptedBytes: c.acceptedBytes, NextSeq: c.nextSeq, TerminalType: c.terminalType, TerminalSeq: c.terminalSeq, ProcessExited: c.processExited, Finalized: c.completion != nil, Window: window}
}

func (c *Collector) appendProjectionLocked(frame FrameProjection) {
	if frame.Bytes > uint64(c.windowByte) {
		c.window = nil
		c.windowSize = 0
		return
	}
	c.window = append(c.window, frame)
	c.windowSize += frame.Bytes
	for len(c.window) > c.windowMax || c.windowSize > uint64(c.windowByte) {
		c.windowSize -= c.window[0].Bytes
		c.window = c.window[1:]
	}
}

func (c *Collector) rejectLocked(err error) error {
	if c.fatal != nil {
		return c.fatal
	}
	if err == nil {
		err = ErrRejected
	}
	c.fatal = fmt.Errorf("%w: %w", ErrRejected, err)
	return c.fatal
}

func isValidSpecDigest(value string) bool {
	return len(value) == len("sha256:")+sha256.Size*2 && strings.HasPrefix(value, "sha256:") && canonicalDigestHex(value[len("sha256:"):])
}

func canonicalDigestHex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func composeRedactors(extra runtimeproto.RedactionHook) runtimeproto.RedactionHook {
	return func(path string, value json.RawMessage) (json.RawMessage, bool, error) {
		if sensitivePath(path) {
			return json.RawMessage(`"[REDACTED]"`), true, nil
		}
		if extra == nil {
			return nil, false, nil
		}
		return extra(path, value)
	}
}

func sensitivePath(path string) bool {
	part := path
	if index := strings.LastIndex(part, "."); index >= 0 {
		part = part[index+1:]
	}
	if index := strings.LastIndex(part, "]"); index >= 0 {
		part = part[index+1:]
	}
	part = strings.ToLower(strings.Trim(part, " \t\""))
	switch part {
	case "token", "access_token", "refresh_token", "api_key", "apikey", "password", "secret", "client_secret", "private_key", "authorization", "cookie", "credential", "credentials":
		return true
	default:
		return false
	}
}
