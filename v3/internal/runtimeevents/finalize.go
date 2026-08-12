package runtimeevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

// FinalizeObservedExit reconstructs the durably accepted event stream and
// combines it with a definitive process exit observed by the Kubernetes
// controller. This is the production completion boundary: the runtime may
// author events, but it cannot author the container exit status that turns a
// terminal event into a CompletionRecord.
//
// The method is deterministic and safe to retry. Concurrent callers write
// identical immutable stream and completion objects; an ambiguous store
// outcome remains ambiguous and is never guessed to be success.
func (r *Repository) FinalizeObservedExit(ctx context.Context, runUID, specDigest, baseSHA string, exit ProcessExit) (CompletionRecord, error) {
	var zero CompletionRecord
	if r == nil || r.store == nil || ctx == nil || !validRunUID(runUID) || !isValidSpecDigest(specDigest) || !validBaseSHA(baseSHA) || !exit.Known {
		return zero, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	if existing, err := r.LoadCompletion(ctx, runUID, specDigest); err == nil {
		if existing.BaseSHA != baseSHA {
			return zero, ErrConflict
		}
		return existing, nil
	} else if !isMissing(err) {
		return zero, err
	}

	identity := identityDigestHex(runUID, specDigest)
	streamHash := sha256.New()
	var frameCount uint64
	var sizeBytes uint64
	var terminal TerminalType
	var terminalSeq uint64
	var summary TerminalSummary
	var sequence runtimeproto.SequenceValidator

	for seq := uint64(1); seq <= maxStreamFrames; seq++ {
		indexBody, err := r.get(ctx, indexKey(runUID, identity, seq))
		if err != nil {
			if isMissing(err) {
				return zero, ErrNoTerminal
			}
			return zero, err
		}
		var index frameIndex
		if err := decodeCanonicalObject(indexBody, MaxIndexBytes, &index); err != nil || validateFrameIndex(index) != nil || index.IdentityDigest != identity || index.RunUID != runUID || index.SpecDigest != specDigest || index.Seq != seq {
			return zero, fmt.Errorf("%w: frame index", ErrCorrupt)
		}
		frameBody, err := r.get(ctx, frameKey(runUID, index.LineDigest))
		if err != nil {
			return zero, err
		}
		if len(frameBody) == 0 || len(frameBody) > maxFrameStorageBytes() || !bytes.HasSuffix(frameBody, []byte{'\n'}) || digestBytes(frameBody) != index.LineDigest || uint64(len(frameBody)) != index.SizeBytes {
			return zero, fmt.Errorf("%w: frame bytes", ErrCorrupt)
		}
		frame, err := parseStoredLine(frameBody)
		if err != nil || frame.RunID != runUID || frame.Seq != seq || frame.Kind != "event" {
			return zero, fmt.Errorf("%w: frame envelope", ErrCorrupt)
		}
		if err := sequence.Accept(frame); err != nil {
			return zero, fmt.Errorf("%w: frame sequence: %v", ErrCorrupt, err)
		}
		canonical, err := canonicalLine(frame)
		if err != nil || !bytes.Equal(canonical, frameBody) {
			return zero, fmt.Errorf("%w: non-canonical frame", ErrCorrupt)
		}
		if sizeBytes > MaxStreamBytes-uint64(len(frameBody)) {
			return zero, ErrStreamTooLarge
		}
		_, _ = streamHash.Write(frameBody)
		frameCount = seq
		sizeBytes += uint64(len(frameBody))

		if !frame.Terminal {
			continue
		}
		var ok bool
		terminal, ok = terminalType(frame.Type)
		if !ok {
			return zero, fmt.Errorf("%w: terminal type", ErrCorrupt)
		}
		summary, err = terminalSummary(frame)
		if err != nil {
			return zero, fmt.Errorf("%w: terminal summary", ErrCorrupt)
		}
		terminalSeq = seq

		// A terminal frame must be the final accepted frame. An exact missing
		// next index proves the sequence ended; any store uncertainty remains a
		// retryable/ambiguous controller error.
		if _, nextErr := r.get(ctx, indexKey(runUID, identity, seq+1)); nextErr == nil {
			return zero, fmt.Errorf("%w: data after terminal", ErrCorrupt)
		} else if !isMissing(nextErr) {
			return zero, nextErr
		}
		break
	}

	if terminal == "" || terminalSeq == 0 || frameCount != terminalSeq {
		return zero, ErrNoTerminal
	}
	if terminal == TerminalCompleted && exit.Code != 0 {
		return zero, ErrProcessExitFailed
	}
	streamDigest := formatDigest(streamHash.Sum(nil))
	manifest := streamManifest{
		SchemaVersion: schemaVersion, IdentityDigest: identity,
		RunUID: runUID, SpecDigest: specDigest, FrameCount: frameCount,
		SizeBytes: sizeBytes, StreamDigest: streamDigest,
		TerminalType: terminal, TerminalSeq: terminalSeq,
	}
	manifestBody, err := canonicalManifest(manifest)
	if err != nil {
		return zero, ErrInvalid
	}
	manifestURI, err := putImmutable(ctx, r.store, streamKey(runUID, streamDigest), manifestBody, "application/vnd.agw.runtime-stream+json", MaxManifestBytes)
	if err != nil {
		return zero, err
	}
	record := CompletionRecord{
		SchemaVersion: schemaVersion, RunUID: runUID, SpecDigest: specDigest,
		BaseSHA: baseSHA, TerminalType: terminal, TerminalSeq: terminalSeq,
		EventStream: ArtifactRef{
			URI: manifestURI, Digest: streamDigest, Kind: "runtime-event-stream",
			Name: "events.jsonl", MediaType: "application/x-ndjson", SizeBytes: int64(sizeBytes),
		},
		Summary: summary,
	}
	recordBody, err := canonicalRecord(record)
	if err != nil {
		return zero, ErrInvalid
	}
	key, err := CompletionKey(runUID, specDigest)
	if err != nil {
		return zero, err
	}
	if _, err := putImmutable(ctx, r.store, key, recordBody, "application/json", MaxCompletionBytes); err != nil {
		return zero, err
	}
	return record, nil
}

func isMissing(err error) bool { return errors.Is(err, ErrMissing) }
