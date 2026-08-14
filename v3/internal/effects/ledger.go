// Package effects implements the durable, fail-closed protocol used before
// externally visible mutations. Kubernetes status is only a projection; the
// object store remains authoritative.
package effects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	SchemaVersion      = 1
	MaxLedgerBodyBytes = 64 << 10
	TombstonesSegment  = "tombstones"
)

var (
	ErrInvalidInput = errors.New("invalid effect-ledger input")
	ErrConflict     = errors.New("effect-ledger conflict")
	ErrUnknown      = errors.New("effect outcome is unknown")
	ErrCorrupt      = errors.New("effect-ledger object is corrupt")
	ErrNotFound     = errors.New("effect-ledger object not found")
)

// ObjectStore is the minimum production object-store contract. Create must be
// a strong conditional create: created=false means an object already existed,
// never that the backend guessed after a timeout.
type ObjectStore interface {
	Create(ctx context.Context, key string, body []byte, contentType string) (created bool, err error)
	Get(ctx context.Context, key string) ([]byte, error)
}

type Claim struct {
	SchemaVersion int       `json:"schemaVersion"`
	EffectKey     string    `json:"effectKey"`
	RequestDigest string    `json:"requestDigest"`
	Operation     string    `json:"operation"`
	RunUID        string    `json:"runUID"`
	CreatedAt     time.Time `json:"createdAt"`
}

type OutcomeState string

const (
	OutcomeSucceeded OutcomeState = "succeeded"
	OutcomeFailed    OutcomeState = "failed"
	OutcomeUnknown   OutcomeState = "unknown"
)

type Outcome struct {
	SchemaVersion int    `json:"schemaVersion"`
	EffectKey     string `json:"effectKey"`
	RequestDigest string `json:"requestDigest"`
	// RunUID was added without changing SchemaVersion so old terminal outcomes
	// remain replay-readable. New commits always populate it; retention refuses
	// to retire an outcome that does not carry the run identity in its body.
	RunUID       string       `json:"runUID,omitempty"`
	State        OutcomeState `json:"state"`
	ResultDigest string       `json:"resultDigest,omitempty"`
	ResultRef    string       `json:"resultRef,omitempty"`
	RecordedAt   time.Time    `json:"recordedAt"`
}

// Tombstone is a permanent replay fence written before a terminal ledger pair
// is retired. Object stores do not provide an atomic delete for two keys, so
// the fence is intentionally retained forever and checked by Claim/Commit.
type Tombstone struct {
	SchemaVersion int          `json:"schemaVersion"`
	EffectKey     string       `json:"effectKey"`
	RequestDigest string       `json:"requestDigest"`
	Operation     string       `json:"operation"`
	RunUID        string       `json:"runUID"`
	State         OutcomeState `json:"state"`
	RetiredAt     time.Time    `json:"retiredAt"`
}

type ClaimDecision struct {
	Execute bool
	Unknown bool
	Claim   Claim
}

type Ledger struct {
	store  ObjectStore
	prefix string
	clock  func() time.Time
}

func New(store ObjectStore, prefix string, clock func() time.Time) (*Ledger, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: object store is required", ErrInvalidInput)
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		prefix = "effects"
	}
	for _, segment := range strings.Split(prefix, "/") {
		if !safeSegment(segment) {
			return nil, fmt.Errorf("%w: invalid ledger prefix", ErrInvalidInput)
		}
	}
	if clock == nil {
		clock = time.Now
	}
	return &Ledger{store: store, prefix: prefix, clock: clock}, nil
}

// EffectKey binds one external operation to a specific run and immutable
// request. Never derive this from a mutable display name alone.
func EffectKey(runUID, baseSHA, patchDigest, operation string) (string, error) {
	if !safeIdentifier(runUID, 128) || !safeIdentifier(baseSHA, 128) ||
		!canonical.ValidDigest(patchDigest) || !safeIdentifier(operation, 128) {
		return "", ErrInvalidInput
	}
	hash := sha256.Sum256([]byte(runUID + "\x00" + baseSHA + "\x00" + patchDigest + "\x00" + operation))
	return canonical.DigestPrefix + hex.EncodeToString(hash[:]), nil
}

// Claim creates the durable intent before an external call. Only the caller
// that receives Execute=true may perform the mutation. An existing matching
// claim with no recorded outcome is ambiguous after reconciliation and is
// therefore returned as Unknown rather than retried.
func (l *Ledger) Claim(ctx context.Context, claim Claim) (ClaimDecision, error) {
	if l == nil || l.store == nil || !validClaim(claim) {
		return ClaimDecision{}, ErrInvalidInput
	}
	if tombstoned, err := l.tombstoneExists(ctx, claim.EffectKey); err != nil {
		return ClaimDecision{}, err
	} else if tombstoned {
		return ClaimDecision{Unknown: true, Claim: claim}, ErrUnknown
	}
	claim.SchemaVersion = SchemaVersion
	claim.CreatedAt = l.clock().UTC()
	if claim.CreatedAt.IsZero() {
		return ClaimDecision{}, ErrInvalidInput
	}
	body, err := canonicalJSON(claim)
	if err != nil {
		return ClaimDecision{}, err
	}
	key := l.claimKey(claim.EffectKey)
	created, err := l.store.Create(ctx, key, body, "application/json")
	if err != nil {
		// A timeout cannot prove whether the claim exists. The caller must not
		// proceed to the external effect.
		return ClaimDecision{}, fmt.Errorf("persist effect claim: %w", err)
	}
	if created {
		// Retention may have installed the fence after the conditional claim
		// create. Leave the claim in place and refuse to grant execution.
		if tombstoned, err := l.tombstoneExists(ctx, claim.EffectKey); err != nil {
			return ClaimDecision{}, err
		} else if tombstoned {
			return ClaimDecision{Unknown: true, Claim: claim}, ErrUnknown
		}
		return ClaimDecision{Execute: true, Claim: claim}, nil
	}
	existing, err := l.readClaim(ctx, key)
	if err != nil {
		return ClaimDecision{}, err
	}
	if existing.EffectKey != claim.EffectKey || existing.RequestDigest != claim.RequestDigest ||
		existing.Operation != claim.Operation || existing.RunUID != claim.RunUID {
		return ClaimDecision{}, ErrConflict
	}
	if tombstoned, err := l.tombstoneExists(ctx, claim.EffectKey); err != nil {
		return ClaimDecision{}, err
	} else if tombstoned {
		return ClaimDecision{Unknown: true, Claim: existing}, ErrUnknown
	}
	existingOutcome, err := l.ReadOutcome(ctx, claim.EffectKey, claim.RequestDigest)
	if err == nil {
		// The outcome read and the tombstone read are separate object-store
		// operations. Re-check the fence after the read so a retirement that
		// completed in between cannot be reported as an ordinary replay.
		if tombstoned, err := l.tombstoneExists(ctx, claim.EffectKey); err != nil {
			return ClaimDecision{}, err
		} else if tombstoned {
			return ClaimDecision{Unknown: true, Claim: existing}, ErrUnknown
		}
		if existingOutcome.State == OutcomeUnknown {
			return ClaimDecision{Unknown: true, Claim: existing}, ErrUnknown
		}
		return ClaimDecision{Claim: existing}, nil
	} else if !isNotFound(err) {
		return ClaimDecision{}, err
	}
	return ClaimDecision{Unknown: true, Claim: existing}, ErrUnknown
}

// Commit records exactly one terminal outcome. A conflicting outcome is never
// overwritten, while an identical replay is accepted as idempotent.
func (l *Ledger) Commit(ctx context.Context, outcome Outcome) error {
	if l == nil || l.store == nil || !validOutcome(outcome) {
		return ErrInvalidInput
	}
	if tombstoned, err := l.tombstoneExists(ctx, outcome.EffectKey); err != nil {
		return err
	} else if tombstoned {
		return ErrUnknown
	}
	claim, err := l.readClaim(ctx, l.claimKey(outcome.EffectKey))
	if err != nil {
		return err
	}
	if claim.RequestDigest != outcome.RequestDigest {
		return ErrConflict
	}
	if outcome.RunUID != "" && outcome.RunUID != claim.RunUID {
		return ErrConflict
	}
	outcome.RunUID = claim.RunUID
	outcome.SchemaVersion = SchemaVersion
	outcome.RecordedAt = l.clock().UTC()
	if outcome.RecordedAt.IsZero() {
		return ErrInvalidInput
	}
	body, err := canonicalJSON(outcome)
	if err != nil {
		return err
	}
	key := l.outcomeKey(outcome.EffectKey)
	created, err := l.store.Create(ctx, key, body, "application/json")
	if err != nil {
		return fmt.Errorf("persist effect outcome: %w", err)
	}
	if created {
		// A pair-retirement fence can only be installed after an outcome was
		// already present, but rechecking keeps this boundary fail-closed if a
		// different store implementation races the write.
		if tombstoned, err := l.tombstoneExists(ctx, outcome.EffectKey); err != nil {
			return err
		} else if tombstoned {
			return ErrUnknown
		}
		return nil
	}
	existing, err := l.ReadOutcome(ctx, outcome.EffectKey, outcome.RequestDigest)
	if err != nil {
		return err
	}
	// A legacy outcome without RunUID remains readable, but it is not a
	// sufficient binding for an idempotent commit. Treat both legacy and
	// conflicting run identities as a conflict rather than accepting a record
	// that could belong to another run.
	if existing.RunUID != claim.RunUID {
		return ErrConflict
	}
	if existing.State != outcome.State || existing.ResultDigest != outcome.ResultDigest || existing.ResultRef != outcome.ResultRef {
		return ErrConflict
	}
	// The outcome may have been read just before retention installed its
	// permanent fence. A committed replay must observe that fence as unknown.
	if tombstoned, err := l.tombstoneExists(ctx, outcome.EffectKey); err != nil {
		return err
	} else if tombstoned {
		return ErrUnknown
	}
	return nil
}

func (l *Ledger) ReadOutcome(ctx context.Context, effectKey, requestDigest string) (Outcome, error) {
	if l == nil || !canonical.ValidDigest(effectKey) || !canonical.ValidDigest(requestDigest) {
		return Outcome{}, ErrInvalidInput
	}
	body, err := l.store.Get(ctx, l.outcomeKey(effectKey))
	if err != nil {
		return Outcome{}, err
	}
	outcome, err := DecodeOutcome(body)
	if err != nil || outcome.EffectKey != effectKey || outcome.RequestDigest != requestDigest {
		return Outcome{}, ErrCorrupt
	}
	return outcome, nil
}

func (l *Ledger) readClaim(ctx context.Context, key string) (Claim, error) {
	body, err := l.store.Get(ctx, key)
	if err != nil {
		return Claim{}, err
	}
	claim, err := DecodeClaim(body)
	if err != nil {
		return Claim{}, ErrCorrupt
	}
	return claim, nil
}

func (l *Ledger) claimKey(effectKey string) string {
	return l.prefix + "/claims/" + digestHex(effectKey) + ".json"
}
func (l *Ledger) outcomeKey(effectKey string) string {
	return l.prefix + "/outcomes/" + digestHex(effectKey) + ".json"
}

func (l *Ledger) tombstoneKey(effectKey string) string {
	return l.prefix + "/" + TombstonesSegment + "/" + digestHex(effectKey) + ".json"
}

func (l *Ledger) tombstoneExists(ctx context.Context, effectKey string) (bool, error) {
	if l == nil || l.store == nil || !canonical.ValidDigest(effectKey) {
		return false, ErrInvalidInput
	}
	body, err := l.store.Get(ctx, l.tombstoneKey(effectKey))
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tombstone, err := DecodeTombstone(body)
	if err != nil || tombstone.EffectKey != effectKey {
		return false, ErrCorrupt
	}
	return true, nil
}

// isNotFound accepts both this package's sentinel and the provider-neutral
// marker used by object-store adapters. The latter keeps the ledger boundary
// independent of a concrete S3 SDK error type while still distinguishing an
// absent object from an ambiguous read failure.
func isNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var marker interface{ NotFound() bool }
	return errors.As(err, &marker) && marker.NotFound()
}

func validClaim(claim Claim) bool {
	return canonical.ValidDigest(claim.EffectKey) && canonical.ValidDigest(claim.RequestDigest) &&
		safeIdentifier(claim.Operation, 128) && safeIdentifier(claim.RunUID, 128)
}

func validOutcome(outcome Outcome) bool {
	if !canonical.ValidDigest(outcome.EffectKey) || !canonical.ValidDigest(outcome.RequestDigest) {
		return false
	}
	switch outcome.State {
	case OutcomeSucceeded, OutcomeFailed, OutcomeUnknown:
	default:
		return false
	}
	return outcome.ResultDigest == "" || canonical.ValidDigest(outcome.ResultDigest)
}

// DecodeClaim validates a bounded, strict, canonical claim body. It is shared
// by retention so object metadata can never stand in for the authoritative
// ledger record.
func DecodeClaim(body []byte) (Claim, error) {
	var claim Claim
	if err := decodeStrict(body, &claim); err != nil || claim.SchemaVersion != SchemaVersion || !validClaim(claim) || claim.CreatedAt.IsZero() {
		return Claim{}, ErrCorrupt
	}
	canonicalBody, err := canonicalJSON(claim)
	if err != nil || !bytes.Equal(body, canonicalBody) {
		return Claim{}, ErrCorrupt
	}
	return claim, nil
}

// DecodeOutcome validates a bounded, strict, canonical terminal or unknown
// outcome body. Legacy v1 outcomes without RunUID remain readable for replay;
// retention deliberately requires the field before retiring a pair.
func DecodeOutcome(body []byte) (Outcome, error) {
	var outcome Outcome
	if err := decodeStrict(body, &outcome); err != nil || outcome.SchemaVersion != SchemaVersion || !validOutcome(outcome) || outcome.RecordedAt.IsZero() {
		return Outcome{}, ErrCorrupt
	}
	canonicalBody, err := canonicalJSON(outcome)
	if err != nil || !bytes.Equal(body, canonicalBody) {
		return Outcome{}, ErrCorrupt
	}
	return outcome, nil
}

// EncodeTombstone returns the exact canonical body used for a permanent replay
// fence. Callers must provide the terminal identity that was just validated.
func EncodeTombstone(tombstone Tombstone) ([]byte, error) {
	if tombstone.SchemaVersion == 0 {
		tombstone.SchemaVersion = SchemaVersion
	}
	if tombstone.SchemaVersion != SchemaVersion || !validTombstone(tombstone) {
		return nil, ErrCorrupt
	}
	return canonicalJSON(tombstone)
}

// DecodeTombstone validates a permanent replay fence.
func DecodeTombstone(body []byte) (Tombstone, error) {
	var tombstone Tombstone
	if err := decodeStrict(body, &tombstone); err != nil || tombstone.SchemaVersion != SchemaVersion || !validTombstone(tombstone) {
		return Tombstone{}, ErrCorrupt
	}
	canonicalBody, err := canonicalJSON(tombstone)
	if err != nil || !bytes.Equal(body, canonicalBody) {
		return Tombstone{}, ErrCorrupt
	}
	return tombstone, nil
}

func validTombstone(tombstone Tombstone) bool {
	if !canonical.ValidDigest(tombstone.EffectKey) || !canonical.ValidDigest(tombstone.RequestDigest) ||
		!safeIdentifier(tombstone.Operation, 128) || !safeIdentifier(tombstone.RunUID, 128) || tombstone.RetiredAt.IsZero() {
		return false
	}
	return tombstone.State == OutcomeSucceeded || tombstone.State == OutcomeFailed
}

func canonicalJSON(value any) ([]byte, error) {
	body, err := canonical.CanonicalizeResolvedSpec(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize effect record: %w", err)
	}
	return body, nil
}

func decodeStrict(body []byte, into any) error {
	if len(body) == 0 || len(body) > MaxLedgerBodyBytes || strictjson.ValidateObject(body) != nil {
		return ErrCorrupt
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return ErrCorrupt
	}
	return nil
}

func digestHex(digest string) string { return strings.TrimPrefix(digest, canonical.DigestPrefix) }

func safeIdentifier(value string, max int) bool {
	return value != "" && len(value) <= max && !strings.ContainsAny(value, "\x00\r\n")
}

func safeSegment(value string) bool {
	if !safeIdentifier(value, 128) || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
