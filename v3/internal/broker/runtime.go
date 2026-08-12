package broker

import (
	"context"
	"errors"
	"io"

	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

// RuntimeStore is the immutable object-store seam required by the v3 runtime
// event collector. It is an alias for the existing runtimeevents contract so
// the broker does not duplicate persistence semantics.
type RuntimeStore = runtimeevents.Store

// RuntimeConfig binds supervision to the immutable run identity. BaseSHA is
// mandatory and is copied into the completion record by runtimeevents; a
// process exit can never manufacture success without the exact base revision.
type RuntimeConfig struct {
	RunUID     string
	SpecDigest string
	BaseSHA    string
	Store      RuntimeStore
	Options    runtimeevents.Options
}

// RuntimeSupervisor is the narrow broker-side owner of the JSONL completion
// boundary. It accepts only runtimeproto-v1 event frames and finalizes only
// after a terminal event plus a known process exit.
type RuntimeSupervisor struct {
	collector  *runtimeevents.Collector
	repo       *runtimeevents.Repository
	runUID     string
	specDigest string
	baseSHA    string
}

func NewRuntimeSupervisor(config RuntimeConfig) (*RuntimeSupervisor, error) {
	collector, err := runtimeevents.NewCollector(config.RunUID, config.SpecDigest, config.BaseSHA, config.Store, config.Options)
	if err != nil {
		return nil, ErrRuntimeUnavailable
	}
	repo, err := runtimeevents.NewRepository(config.Store)
	if err != nil {
		return nil, ErrRuntimeUnavailable
	}
	return &RuntimeSupervisor{collector: collector, repo: repo, runUID: config.RunUID, specDigest: config.SpecDigest, baseSHA: config.BaseSHA}, nil
}

// AcceptRuntimeLine durably records one JSONL event. Runtime event errors are
// returned as-is because the runtimeevents package uses fixed, non-secret
// sentinel text and never embeds event payloads.
func (s *RuntimeSupervisor) AcceptRuntimeLine(ctx context.Context, line []byte) error {
	if s == nil || s.collector == nil || ctx == nil {
		return ErrRuntimeUnavailable
	}
	return s.collector.Accept(ctx, line)
}

// Supervise reads the runtime's JSONL stream through the strict decoder,
// durably accepts each event, then applies the process-side wait result. A
// malformed stream or unknown process result is never converted into success.
func (s *RuntimeSupervisor) Supervise(ctx context.Context, reader io.Reader, exit runtimeevents.ProcessExit) (runtimeevents.CompletionRecord, error) {
	var zero runtimeevents.CompletionRecord
	if s == nil || s.collector == nil || reader == nil || ctx == nil {
		return zero, ErrRuntimeUnavailable
	}
	decoder := runtimeproto.NewDecoder(reader)
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		frame, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return zero, err
		}
		line, err := runtimeproto.EncodeLine(frame)
		if err != nil {
			return zero, err
		}
		if err := s.collector.Accept(ctx, line); err != nil {
			return zero, err
		}
	}
	return s.ProcessExit(ctx, exit)
}

func (s *RuntimeSupervisor) ProcessExit(ctx context.Context, exit runtimeevents.ProcessExit) (runtimeevents.CompletionRecord, error) {
	var zero runtimeevents.CompletionRecord
	if s == nil || s.collector == nil || ctx == nil {
		return zero, ErrRuntimeUnavailable
	}
	return s.collector.ProcessExit(ctx, exit)
}

func (s *RuntimeSupervisor) Finish(ctx context.Context, exitCode int) (runtimeevents.CompletionRecord, error) {
	var zero runtimeevents.CompletionRecord
	if s == nil || s.collector == nil || ctx == nil {
		return zero, ErrRuntimeUnavailable
	}
	return s.collector.Finish(ctx, exitCode)
}

func (s *RuntimeSupervisor) Completion() (runtimeevents.CompletionRecord, error) {
	var zero runtimeevents.CompletionRecord
	if s == nil || s.collector == nil {
		return zero, ErrRuntimeUnavailable
	}
	record, err := s.collector.Completion()
	if err != nil {
		return zero, err
	}
	if record.RunUID != s.runUID || record.SpecDigest != s.specDigest || record.BaseSHA != s.baseSHA {
		return zero, ErrRuntimeUnavailable
	}
	return record, nil
}

// LoadCompletion is the restart/reconcile seam. It reads the immutable
// record from storage and verifies all three identity fields against this
// supervisor's exact contract.
func (s *RuntimeSupervisor) LoadCompletion(ctx context.Context) (runtimeevents.CompletionRecord, error) {
	var zero runtimeevents.CompletionRecord
	if s == nil || s.repo == nil || ctx == nil {
		return zero, ErrRuntimeUnavailable
	}
	record, err := s.repo.LoadCompletion(ctx, s.runUID, s.specDigest)
	if err != nil {
		return zero, err
	}
	if record.BaseSHA != s.baseSHA {
		return zero, ErrRuntimeUnavailable
	}
	return record, nil
}
