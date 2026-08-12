package runtimeevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

const testSpecDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testBaseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type memoryStore struct {
	mu       sync.Mutex
	values   map[string][]byte
	contents map[string]string
	uri      string
	putErr   error
	getErr   error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: map[string][]byte{}, contents: map[string]string{}, uri: "s3://test-bucket"}
}

func (s *memoryStore) Put(_ context.Context, key string, body []byte, contentType string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return false, "", s.putErr
	}
	if _, exists := s.values[key]; exists {
		return false, s.uri + "/" + key, nil
	}
	s.values[key] = append([]byte(nil), body...)
	s.contents[key] = contentType
	return true, s.uri + "/" + key, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	body, exists := s.values[key]
	if !exists {
		return nil, ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

func (s *memoryStore) set(key string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = append([]byte(nil), body...)
}

func newTestCollector(t *testing.T, store Store, options Options) *Collector {
	t.Helper()
	collector, err := NewCollector("run-uid", testSpecDigest, testBaseSHA, store, options)
	if err != nil {
		t.Fatal(err)
	}
	return collector
}

func eventLine(t *testing.T, runUID string, seq uint64, eventType string, terminal bool, data string) []byte {
	t.Helper()
	line, err := runtimeproto.EncodeLine(proto.Envelope{
		Protocol: proto.ProtocolVersion,
		Kind:     proto.KindEvent,
		Type:     eventType,
		RunID:    runUID,
		Seq:      seq,
		Terminal: terminal,
		Data:     []byte(data),
	})
	if err != nil {
		t.Fatalf("encode %s: %v", eventType, err)
	}
	return line
}

func heartbeat(t *testing.T, runUID string, seq uint64) []byte {
	return eventLine(t, runUID, seq, proto.EventHeartbeat, false, `{}`)
}

func started(t *testing.T, runUID string) []byte {
	return eventLine(t, runUID, 1, proto.EventRunStarted, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)
}

func terminalLine(t *testing.T, runUID string, seq uint64, kind TerminalType) []byte {
	switch kind {
	case TerminalCompleted:
		return eventLine(t, runUID, seq, string(kind), true, `{"result":"ok","output":{"id":"out","uri":"s3://bucket/out","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size_bytes":1}}`)
	case TerminalFailed:
		return eventLine(t, runUID, seq, string(kind), true, `{"error":{"code":"E_TEST","message":"private failure detail"}}`)
	case TerminalCancelled:
		return eventLine(t, runUID, seq, string(kind), true, `{"reason":"private cancellation detail"}`)
	case TerminalUnknownEffect:
		return eventLine(t, runUID, seq, string(kind), true, `{"effect_id":"effect-1","reason":"private effect detail"}`)
	default:
		t.Fatalf("unknown terminal %q", kind)
		return nil
	}
}

func accept(t *testing.T, collector *Collector, line []byte) {
	t.Helper()
	if err := collector.Accept(context.Background(), line); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestCollectorPersistsAndReplaysBoundedStream(t *testing.T) {
	store := newMemoryStore()
	collector := newTestCollector(t, store, Options{WindowFrames: 2, WindowBytes: 4096})
	first := started(t, "run-uid")
	second := terminalLine(t, "run-uid", 2, TerminalCompleted)
	accept(t, collector, first)
	accept(t, collector, second)
	record, err := collector.Finish(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if record.TerminalType != TerminalCompleted || record.TerminalSeq != 2 || !record.Summary.OutputPresent {
		t.Fatalf("unexpected completion record: %#v", record)
	}
	loaded, err := (&Repository{store: store}).LoadCompletion(context.Background(), "run-uid", testSpecDigest)
	if err != nil || loaded != record {
		t.Fatalf("loaded=%#v err=%v want=%#v", loaded, err, record)
	}
	var replay bytes.Buffer
	info, err := (&Repository{store: store}).WriteJSONL(context.Background(), record.RunUID, record.EventStream, &replay)
	if err != nil {
		t.Fatal(err)
	}
	if info.RunUID != "run-uid" || info.SpecDigest != testSpecDigest || info.FrameCount != 2 || info.TerminalType != TerminalCompleted {
		t.Fatalf("unexpected stream info: %#v", info)
	}
	if got := replay.String(); got == "" || !strings.Contains(got, `"seq":1`) || !strings.Contains(got, `"seq":2`) {
		t.Fatalf("unexpected replay: %s", got)
	}
	projection := collector.Projection()
	if projection.AcceptedFrames != 2 || projection.NextSeq != 3 || !projection.Finalized || len(projection.Window) != 2 {
		t.Fatalf("unexpected projection: %#v", projection)
	}
	if _, err := collector.Completion(); err != nil {
		t.Fatalf("completion should be ready: %v", err)
	}
}

func TestRepositoryFinalizesOnlyWithControllerObservedExit(t *testing.T) {
	store := newMemoryStore()
	collector := newTestCollector(t, store, Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	if _, err := (&Repository{store: store}).LoadCompletion(context.Background(), "run-uid", testSpecDigest); !errors.Is(err, ErrMissing) {
		t.Fatalf("runtime terminal manufactured completion before process observation: %v", err)
	}

	repository := &Repository{store: store}
	record, err := repository.FinalizeObservedExit(context.Background(), "run-uid", testSpecDigest, testBaseSHA, ProcessExit{Known: true, Code: 0})
	if err != nil {
		t.Fatal(err)
	}
	if record.TerminalType != TerminalCompleted || record.TerminalSeq != 2 || record.BaseSHA != testBaseSHA {
		t.Fatalf("unexpected completion: %#v", record)
	}
	replayed, err := repository.FinalizeObservedExit(context.Background(), "run-uid", testSpecDigest, testBaseSHA, ProcessExit{Known: true, Code: 0})
	if err != nil || replayed != record {
		t.Fatalf("idempotent finalize=%#v err=%v", replayed, err)
	}
}

func TestRepositoryObservedExitFailsClosed(t *testing.T) {
	t.Run("completed event with nonzero exit", func(t *testing.T) {
		store := newMemoryStore()
		collector := newTestCollector(t, store, Options{})
		accept(t, collector, started(t, "run-uid"))
		accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
		_, err := (&Repository{store: store}).FinalizeObservedExit(context.Background(), "run-uid", testSpecDigest, testBaseSHA, ProcessExit{Known: true, Code: 17})
		if !errors.Is(err, ErrProcessExitFailed) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("exit without terminal", func(t *testing.T) {
		store := newMemoryStore()
		collector := newTestCollector(t, store, Options{})
		accept(t, collector, started(t, "run-uid"))
		_, err := (&Repository{store: store}).FinalizeObservedExit(context.Background(), "run-uid", testSpecDigest, testBaseSHA, ProcessExit{Known: true, Code: 0})
		if !errors.Is(err, ErrNoTerminal) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("unknown process observation", func(t *testing.T) {
		_, err := (&Repository{store: newMemoryStore()}).FinalizeObservedExit(context.Background(), "run-uid", testSpecDigest, testBaseSHA, ProcessExit{})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCollectorEnforcesExactRunAndContiguousSequence(t *testing.T) {
	tests := []struct {
		name string
		line []byte
		want error
	}{
		{name: "wrong run", line: heartbeat(t, "other-run", 1), want: ErrWrongRun},
		{name: "gap", line: heartbeat(t, "run-uid", 2), want: ErrSequence},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := newTestCollector(t, newMemoryStore(), Options{})
			err := collector.Accept(context.Background(), test.line)
			if !errors.Is(err, ErrRejected) || !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want rejection containing %v", err, test.want)
			}
		})
	}

	collector := newTestCollector(t, newMemoryStore(), Options{})
	accept(t, collector, started(t, "run-uid"))
	err := collector.Accept(context.Background(), heartbeat(t, "run-uid", 1))
	if !errors.Is(err, ErrSequence) {
		t.Fatalf("replay error=%v", err)
	}
}

func TestCollectorRejectsDataAfterAndDuplicateTerminal(t *testing.T) {
	for _, name := range []string{"trailing event", "trailing terminal"} {
		t.Run(name, func(t *testing.T) {
			collector := newTestCollector(t, newMemoryStore(), Options{})
			accept(t, collector, started(t, "run-uid"))
			accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
			var trailing []byte
			if name == "trailing event" {
				trailing = heartbeat(t, "run-uid", 3)
			} else {
				trailing = terminalLine(t, "run-uid", 3, TerminalFailed)
			}
			err := collector.Accept(context.Background(), trailing)
			if !errors.Is(err, ErrRejected) || !errors.Is(err, ErrSequence) {
				t.Fatalf("error=%v, want terminal rejection", err)
			}
			if _, err := collector.Finish(context.Background(), 0); !errors.Is(err, ErrRejected) {
				t.Fatalf("rejected stream became complete: %v", err)
			}
		})
	}

	collector := newTestCollector(t, newMemoryStore(), Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	if _, err := collector.Finish(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := collector.Accept(context.Background(), terminalLine(t, "run-uid", 2, TerminalCompleted)); !errors.Is(err, ErrRejected) {
		t.Fatalf("duplicate terminal accepted: %v", err)
	}
}

func TestAllTerminalKindsAreDistinguished(t *testing.T) {
	for _, kind := range []TerminalType{TerminalCompleted, TerminalFailed, TerminalCancelled, TerminalUnknownEffect} {
		t.Run(string(kind), func(t *testing.T) {
			collector := newTestCollector(t, newMemoryStore(), Options{})
			accept(t, collector, started(t, "run-uid"))
			accept(t, collector, terminalLine(t, "run-uid", 2, kind))
			record, err := collector.Finish(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if record.TerminalType != kind || record.TerminalSeq != 2 {
				t.Fatalf("record=%#v", record)
			}
			switch kind {
			case TerminalFailed:
				if record.Summary.ErrorCode != "E_TEST" || record.Summary.EffectID != "" {
					t.Fatalf("failed summary=%#v", record.Summary)
				}
			case TerminalUnknownEffect:
				if record.Summary.EffectID != "effect-1" || record.Summary.ErrorCode != "" {
					t.Fatalf("unknown effect summary=%#v", record.Summary)
				}
			case TerminalCancelled:
				if record.Summary != (TerminalSummary{}) {
					t.Fatalf("cancelled summary=%#v", record.Summary)
				}
			}
		})
	}
}

func TestProcessExitWithoutTerminalIsNeverSuccess(t *testing.T) {
	store := newMemoryStore()
	collector := newTestCollector(t, store, Options{})
	accept(t, collector, started(t, "run-uid"))
	if _, err := collector.Finish(context.Background(), 0); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("finish error=%v", err)
	}
	if _, err := collector.Completion(); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("completion error=%v", err)
	}
	repository := &Repository{store: store}
	if _, err := repository.LoadCompletion(context.Background(), "run-uid", testSpecDigest); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing completion error=%v", err)
	}
	if err := collector.Accept(context.Background(), heartbeat(t, "run-uid", 2)); !errors.Is(err, ErrRejected) {
		t.Fatalf("stream accepted data after process exit: %v", err)
	}
}

func TestUnknownAndContradictoryProcessExitFailClosed(t *testing.T) {
	collector := newTestCollector(t, newMemoryStore(), Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	if _, err := collector.ProcessExit(context.Background(), ProcessExit{}); !errors.Is(err, ErrProcessExitUnknown) {
		t.Fatalf("unknown exit error=%v", err)
	}
	if _, err := collector.Completion(); !errors.Is(err, ErrProcessExitUnknown) {
		t.Fatalf("unknown exit completion=%v", err)
	}

	collector = newTestCollector(t, newMemoryStore(), Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	if _, err := collector.Finish(context.Background(), 7); !errors.Is(err, ErrProcessExitFailed) {
		t.Fatalf("contradictory exit error=%v", err)
	}
}

func TestOversizedFrameRejectsCollector(t *testing.T) {
	collector := newTestCollector(t, newMemoryStore(), Options{})
	line := []byte(strings.Repeat("x", runtimeproto.MaxFrameBytes+1))
	err := collector.Accept(context.Background(), line)
	if !errors.Is(err, ErrRejected) || !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversized error=%v", err)
	}
}

func TestRedactionPreventsCredentialMaterialInPersistedStream(t *testing.T) {
	store := newMemoryStore()
	collector := newTestCollector(t, store, Options{})
	accept(t, collector, started(t, "run-uid"))
	tool := eventLine(t, "run-uid", 2, proto.EventToolRequested, false, `{"request_id":"req","tool":"github","arguments":{"token":"super-secret","apiKey":"also-secret","safe":"ok"}}`)
	accept(t, collector, tool)
	accept(t, collector, terminalLine(t, "run-uid", 3, TerminalCompleted))
	record, err := collector.Finish(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for key, body := range store.values {
		if bytes.Contains(body, []byte("super-secret")) || bytes.Contains(body, []byte("also-secret")) {
			t.Fatalf("credential material persisted in %s: %s", key, body)
		}
	}
	var replay bytes.Buffer
	if _, err := (&Repository{store: store}).WriteJSONL(context.Background(), record.RunUID, record.EventStream, &replay); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replay.String(), `[REDACTED]`) || !strings.Contains(replay.String(), `"safe":"ok"`) {
		t.Fatalf("redacted replay=%s", replay.String())
	}
	if strings.Contains(replay.String(), "private failure detail") {
		t.Fatal("unexpected diagnostic in replay")
	}
}

func TestBoundedProjectionDoesNotGrowWithStream(t *testing.T) {
	collector := newTestCollector(t, newMemoryStore(), Options{WindowFrames: 3, WindowBytes: 512})
	for seq := uint64(1); seq <= 40; seq++ {
		if seq == 1 {
			accept(t, collector, started(t, "run-uid"))
		} else {
			accept(t, collector, heartbeat(t, "run-uid", seq))
		}
	}
	projection := collector.Projection()
	if projection.AcceptedFrames != 40 || projection.NextSeq != 41 || len(projection.Window) > 3 {
		t.Fatalf("projection=%#v", projection)
	}
	var bytesInWindow uint64
	for _, frame := range projection.Window {
		bytesInWindow += frame.Bytes
	}
	if bytesInWindow > 512 {
		t.Fatalf("window bytes=%d", bytesInWindow)
	}
	encoded, err := json.Marshal(projection)
	if err != nil || strings.Contains(string(encoded), "message") {
		t.Fatalf("unsafe projection=%s err=%v", encoded, err)
	}
}

func TestExactReplayAndConflicts(t *testing.T) {
	store := newMemoryStore()
	for attempt := 0; attempt < 2; attempt++ {
		collector := newTestCollector(t, store, Options{})
		accept(t, collector, started(t, "run-uid"))
		accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
		if _, err := collector.Finish(context.Background(), 0); err != nil {
			t.Fatalf("idempotent attempt %d: %v", attempt, err)
		}
	}

	conflicting := newMemoryStore()
	key, err := CompletionKey("run-uid", testSpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	conflicting.set(key, []byte(`{"schemaVersion":1}`))
	collector := newTestCollector(t, conflicting, Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	if _, err := collector.Finish(context.Background(), 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("completion conflict error=%v", err)
	}

	ambiguous := newMemoryStore()
	ambiguous.putErr = errors.New("timeout")
	collector = newTestCollector(t, ambiguous, Options{})
	if err := collector.Accept(context.Background(), started(t, "run-uid")); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous put error=%v", err)
	}
}

func TestCanonicalTamperingAndMissingObjectsAreDistinct(t *testing.T) {
	store := newMemoryStore()
	collector := newTestCollector(t, store, Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	record, err := collector.Finish(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	repository := &Repository{store: store}
	completionKey, _ := CompletionKey("run-uid", testSpecDigest)
	original := append([]byte(nil), store.values[completionKey]...)
	store.set(completionKey, append(original, ' '))
	if _, err := repository.LoadCompletion(context.Background(), "run-uid", testSpecDigest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-canonical completion error=%v", err)
	}
	store.set(completionKey, []byte(`{"schemaVersion":1,"schemaVersion":1}`))
	if _, err := repository.LoadCompletion(context.Background(), "run-uid", testSpecDigest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("duplicate completion error=%v", err)
	}
	store.set(completionKey, original)

	manifestKey := streamKey(record.RunUID, record.EventStream.Digest)
	manifest := append([]byte(nil), store.values[manifestKey]...)
	var manifestObject map[string]any
	if err := json.Unmarshal(manifest, &manifestObject); err != nil {
		t.Fatal(err)
	}
	manifestObject["sizeBytes"] = float64(999)
	tamperedManifest, err := canonicalBytes(manifestObject)
	if err != nil {
		t.Fatal(err)
	}
	store.set(manifestKey, tamperedManifest)
	var output bytes.Buffer
	if _, err := repository.WriteJSONL(context.Background(), record.RunUID, record.EventStream, &output); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered manifest error=%v", err)
	}

	store.set(manifestKey, manifest)
	var indexObject streamManifest
	if err := json.Unmarshal(manifest, &indexObject); err != nil {
		t.Fatal(err)
	}
	indexBody := store.values[indexKey(record.RunUID, indexObject.IdentityDigest, 1)]
	var index frameIndex
	if err := json.Unmarshal(indexBody, &index); err != nil {
		t.Fatal(err)
	}
	frameKeyValue := frameKey(record.RunUID, index.LineDigest)
	store.set(frameKeyValue, []byte("{}\n"))
	if _, err := repository.WriteJSONL(context.Background(), record.RunUID, record.EventStream, &output); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered frame error=%v", err)
	}

	delete(store.values, frameKeyValue)
	if _, err := repository.WriteJSONL(context.Background(), record.RunUID, record.EventStream, &output); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing frame error=%v", err)
	}
}

func TestStoreUnavailableDoesNotBecomeCorruption(t *testing.T) {
	store := newMemoryStore()
	store.getErr = fmt.Errorf("backend unavailable")
	repository := &Repository{store: store}
	if _, err := repository.LoadCompletion(context.Background(), "run-uid", testSpecDigest); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("store error=%v", err)
	}
}

func TestEveryRuntimeObjectIsFencedBelowExactRunPrefix(t *testing.T) {
	store := newMemoryStore()
	collector := newTestCollector(t, store, Options{})
	accept(t, collector, started(t, "run-uid"))
	accept(t, collector, terminalLine(t, "run-uid", 2, TerminalCompleted))
	if _, err := collector.Finish(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	for key := range store.values {
		if !strings.HasPrefix(key, "runs/run-uid/runtime-events/") {
			t.Fatalf("runtime object escaped per-run prefix: %q", key)
		}
	}
	for _, unsafeUID := range []string{"run/escape", ".", "..", "run..uid"} {
		if _, err := CompletionKey(unsafeUID, testSpecDigest); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe run UID %q was accepted: %v", unsafeUID, err)
		}
	}
}

func FuzzAcceptDoesNotPanic(f *testing.F) {
	f.Add([]byte(`{"protocol":"agw.runtime.v1","kind":"event","type":"heartbeat","run_id":"run-uid","seq":1,"terminal":false,"data":{}}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, line []byte) {
		collector, err := NewCollector("run-uid", testSpecDigest, testBaseSHA, newMemoryStore(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		_ = collector.Accept(context.Background(), line)
		_ = collector.Projection()
	})
}

func FuzzCompletionBytesAreStrict(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schemaVersion":1,"runUID":"run-uid"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		store := newMemoryStore()
		key, err := CompletionKey("run-uid", testSpecDigest)
		if err != nil {
			t.Fatal(err)
		}
		store.set(key, body)
		_, _ = (&Repository{store: store}).LoadCompletion(context.Background(), "run-uid", testSpecDigest)
	})
}
