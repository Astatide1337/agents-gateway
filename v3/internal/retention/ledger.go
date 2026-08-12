package retention

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

const (
	ledgerPublishOperation = "publish/pr"
	ledgerMCPWrite         = "mcp-write"
	ledgerMCPPublish       = "mcp-publish"
)

type ledgerRecord[T any] struct {
	Object LedgerObject
	Value  T
}

type ledgerPair struct {
	EffectKey string
	Claim     *ledgerRecord[effects.Claim]
	Outcome   *ledgerRecord[effects.Outcome]
	Tombstone *ledgerRecord[effects.Tombstone]
}

type ledgerIndex struct {
	Pairs []ledgerPair
}

// LedgerPairPlan is the only plan shape that can retire effect records. The
// claim and terminal outcome remain one logical action: Apply installs or
// validates the permanent tombstone, then deletes outcome before claim.
type LedgerPairPlan struct {
	EffectKey       string
	RunUID          string
	Claim           effects.Claim
	Outcome         effects.Outcome
	ClaimKey        string
	OutcomeKey      string
	ClaimETag       string
	OutcomeETag     string
	Tombstone       effects.Tombstone
	TombstoneKey    string
	TombstoneBody   []byte
	TombstoneExists bool
}

func indexLedger(policy Policy, objects []LedgerObject) (ledgerIndex, error) {
	claims := make(map[string]ledgerRecord[effects.Claim])
	outcomes := make(map[string]ledgerRecord[effects.Outcome])
	tombstones := make(map[string]ledgerRecord[effects.Tombstone])
	for _, object := range objects {
		if len(object.Body) == 0 || len(object.Body) > effects.MaxLedgerBodyBytes {
			return ledgerIndex{}, fmt.Errorf("%w: ledger object %q exceeds bounded body contract", ErrLedgerInventory, object.Key)
		}
		segment, effectKey, err := parseLedgerObjectKey(policy.ObjectPrefix, policy.LedgerPrefix, object.Key)
		if err != nil {
			return ledgerIndex{}, err
		}
		switch segment {
		case LedgerClaimsSegment:
			if _, exists := claims[effectKey]; exists {
				return ledgerIndex{}, fmt.Errorf("%w: duplicate claim for %s", ErrLedgerInventory, effectKey)
			}
			claim, err := effects.DecodeClaim(object.Body)
			if err != nil || claim.EffectKey != effectKey {
				return ledgerIndex{}, fmt.Errorf("%w: claim %q is malformed or mismatched", ErrLedgerInventory, object.Key)
			}
			claims[effectKey] = ledgerRecord[effects.Claim]{Object: object, Value: claim}
		case LedgerOutcomesSegment:
			if _, exists := outcomes[effectKey]; exists {
				return ledgerIndex{}, fmt.Errorf("%w: duplicate outcome for %s", ErrLedgerInventory, effectKey)
			}
			outcome, err := effects.DecodeOutcome(object.Body)
			if err != nil || outcome.EffectKey != effectKey {
				return ledgerIndex{}, fmt.Errorf("%w: outcome %q is malformed or mismatched", ErrLedgerInventory, object.Key)
			}
			outcomes[effectKey] = ledgerRecord[effects.Outcome]{Object: object, Value: outcome}
		case LedgerTombstonesSegment:
			if _, exists := tombstones[effectKey]; exists {
				return ledgerIndex{}, fmt.Errorf("%w: duplicate tombstone for %s", ErrLedgerInventory, effectKey)
			}
			tombstone, err := effects.DecodeTombstone(object.Body)
			if err != nil || tombstone.EffectKey != effectKey {
				return ledgerIndex{}, fmt.Errorf("%w: tombstone %q is malformed or mismatched", ErrLedgerInventory, object.Key)
			}
			tombstones[effectKey] = ledgerRecord[effects.Tombstone]{Object: object, Value: tombstone}
		}
	}

	keys := make(map[string]struct{}, len(claims)+len(outcomes))
	for key := range claims {
		keys[key] = struct{}{}
	}
	for key := range outcomes {
		keys[key] = struct{}{}
	}
	result := ledgerIndex{Pairs: make([]ledgerPair, 0, len(keys))}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		pair := ledgerPair{EffectKey: key}
		if claim, ok := claims[key]; ok {
			claimCopy := claim
			pair.Claim = &claimCopy
		}
		if outcome, ok := outcomes[key]; ok {
			outcomeCopy := outcome
			pair.Outcome = &outcomeCopy
		}
		if tombstone, ok := tombstones[key]; ok {
			tombstoneCopy := tombstone
			pair.Tombstone = &tombstoneCopy
		}
		result.Pairs = append(result.Pairs, pair)
	}
	return result, nil
}

func parseLedgerObjectKey(objectPrefix, ledgerPrefix, key string) (string, string, error) {
	if err := validateObjectKey(key, false); err != nil {
		return "", "", fmt.Errorf("%w: invalid ledger key %q", ErrLedgerInventory, key)
	}
	root := strings.TrimSuffix(objectPrefix, "/") + "/" + strings.Trim(ledgerPrefix, "/") + "/"
	if !strings.HasPrefix(key, root) {
		return "", "", fmt.Errorf("%w: ledger key %q is outside configured prefix", ErrLedgerInventory, key)
	}
	parts := strings.Split(strings.TrimPrefix(key, root), "/")
	if len(parts) != 2 || parts[1] == "" || !strings.HasSuffix(parts[1], ".json") {
		return "", "", fmt.Errorf("%w: ledger key %q has an invalid shape", ErrLedgerInventory, key)
	}
	segment := parts[0]
	if segment != LedgerClaimsSegment && segment != LedgerOutcomesSegment && segment != LedgerTombstonesSegment {
		return "", "", fmt.Errorf("%w: ledger key %q has an unknown segment", ErrLedgerInventory, key)
	}
	hex := strings.TrimSuffix(parts[1], ".json")
	if len(hex) != 64 || strings.ToLower(hex) != hex || !isLowerHex(hex) {
		return "", "", fmt.Errorf("%w: ledger key %q has an invalid effect digest", ErrLedgerInventory, key)
	}
	return segment, canonical.DigestPrefix + hex, nil
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func ledgerRoot(objectPrefix, ledgerPrefix string) string {
	return strings.TrimSuffix(objectPrefix, "/") + "/" + strings.Trim(ledgerPrefix, "/") + "/"
}

func isLedgerObjectKey(objectPrefix, ledgerPrefix, key string) bool {
	return strings.HasPrefix(key, ledgerRoot(objectPrefix, ledgerPrefix))
}

func (p *Plan) ledgerPair(pair ledgerPair, run Run, policy Policy, inventory Inventory, now time.Time) {
	if pair.Claim == nil || pair.Outcome == nil {
		p.skipLedger(pair, SkipLedgerIncomplete)
		return
	}
	claim := pair.Claim.Value
	outcome := pair.Outcome.Value
	if outcome.State == effects.OutcomeUnknown {
		p.skipLedger(pair, SkipLedgerUnknown)
		return
	}
	if outcome.State != effects.OutcomeSucceeded && outcome.State != effects.OutcomeFailed {
		p.skipLedger(pair, SkipLedgerUnknown)
		return
	}
	if outcome.RunUID == "" || claim.RunUID != run.UID || outcome.RunUID != run.UID || claim.EffectKey != pair.EffectKey || outcome.EffectKey != pair.EffectKey || claim.RequestDigest != outcome.RequestDigest || claim.Operation == "" {
		p.skipLedger(pair, SkipLedgerIdentityMismatch)
		return
	}
	if run.EffectKey != "" && run.EffectKey != pair.EffectKey {
		p.skipLedger(pair, SkipLedgerIdentityMismatch)
		return
	}
	if ok, reason := terminalEligible(run, now, 0); !ok {
		p.skipLedger(pair, reason)
		return
	}
	if !run.LedgerFlushed {
		p.skipLedger(pair, SkipLedgerNotFlushed)
		return
	}
	if ok, reason := evidenceReferencesBound(run, policy.ObjectPrefix); !ok {
		p.skipLedger(pair, reason)
		return
	}
	if run.CompletedAt.After(p.LedgerCutoff) || outcome.RecordedAt.After(p.LedgerCutoff) || claim.CreatedAt.After(now) || outcome.RecordedAt.After(now) || claim.CreatedAt.After(outcome.RecordedAt) {
		p.skipLedger(pair, SkipLedgerYoung)
		return
	}
	if !resolved.ValidBaseSHA(run.BaseSHA) || !ledgerEffectKeyMatches(run, claim) {
		p.skipLedger(pair, SkipLedgerIdentityMismatch)
		return
	}
	if hasRetainedArtifact(policy.ObjectPrefix, run.UID, inventory.Artifacts) {
		p.skipLedger(pair, SkipLedgerArtifacts)
		return
	}
	if hasRetainedWorkspace(run, inventory.Resources) {
		p.skipLedger(pair, SkipLedgerWorkspace)
		return
	}
	if pair.Claim.Object.ETag == "" || pair.Outcome.Object.ETag == "" {
		p.skipLedger(pair, SkipLedgerETagMissing)
		return
	}

	tombstone := effects.Tombstone{
		SchemaVersion: effects.SchemaVersion,
		EffectKey:     pair.EffectKey, RequestDigest: claim.RequestDigest, Operation: claim.Operation,
		RunUID: run.UID, State: outcome.State, RetiredAt: now.UTC(),
	}
	tombstoneBody, err := effects.EncodeTombstone(tombstone)
	if err != nil {
		p.skipLedger(pair, SkipLedgerIdentityMismatch)
		return
	}
	if pair.Tombstone != nil {
		if !sameTombstoneIdentity(pair.Tombstone.Value, tombstone) {
			p.skipLedger(pair, SkipLedgerTombstone)
			return
		}
	}
	planned := LedgerPairPlan{
		EffectKey: pair.EffectKey, RunUID: run.UID, Claim: claim, Outcome: outcome,
		ClaimKey: pair.Claim.Object.Key, OutcomeKey: pair.Outcome.Object.Key,
		ClaimETag: pair.Claim.Object.ETag, OutcomeETag: pair.Outcome.Object.ETag,
		Tombstone: tombstone, TombstoneKey: ledgerRoot(policy.ObjectPrefix, policy.LedgerPrefix) + LedgerTombstonesSegment + "/" + strings.TrimPrefix(pair.EffectKey, canonical.DigestPrefix) + ".json",
		TombstoneBody: tombstoneBody, TombstoneExists: pair.Tombstone != nil,
	}
	p.LedgerPairs = append(p.LedgerPairs, planned)
	mode := p.actionMode()
	p.Actions = append(p.Actions,
		Action{Mode: mode, RunUID: run.UID, Role: "ledger-outcome", PairKey: pair.EffectKey, Reason: "terminal effect outcome exceeded retention window", Target: ActionTarget{Kind: ObjectStoreKind, ObjectKey: planned.OutcomeKey, ETag: planned.OutcomeETag}},
		Action{Mode: mode, RunUID: run.UID, Role: "ledger-claim", PairKey: pair.EffectKey, Reason: "terminal effect claim exceeded retention window", Target: ActionTarget{Kind: ObjectStoreKind, ObjectKey: planned.ClaimKey, ETag: planned.ClaimETag}},
	)
}

func ledgerEffectKeyMatches(run Run, claim effects.Claim) bool {
	var digest string
	switch claim.Operation {
	case ledgerPublishOperation:
		if !canonical.ValidDigest(run.PatchDigest) || run.PatchDigest != claim.RequestDigest {
			return false
		}
		digest = run.PatchDigest
	case ledgerMCPWrite, ledgerMCPPublish:
		digest = claim.RequestDigest
	default:
		return false
	}
	expected, err := effects.EffectKey(run.UID, run.BaseSHA, digest, claim.Operation)
	return err == nil && expected == claim.EffectKey
}

func sameTombstoneIdentity(left, right effects.Tombstone) bool {
	return left.SchemaVersion == right.SchemaVersion && left.EffectKey == right.EffectKey && left.RequestDigest == right.RequestDigest && left.Operation == right.Operation && left.RunUID == right.RunUID && left.State == right.State && !left.RetiredAt.IsZero() && !right.RetiredAt.IsZero()
}

func hasRetainedArtifact(prefix, runUID string, artifacts []Artifact) bool {
	want := runPrefix(prefix, runUID)
	for _, artifact := range artifacts {
		if artifact.RunUID == runUID || strings.HasPrefix(artifact.Key, want) {
			return true
		}
	}
	return false
}

func hasRetainedWorkspace(run Run, resources []Resource) bool {
	childUIDs := make(map[string]struct{})
	childNames := make(map[string]struct{})
	for _, child := range run.Children {
		if child.Role == WorkRole {
			childUIDs[child.UID] = struct{}{}
			childNames[child.Name] = struct{}{}
		}
	}
	for _, resource := range resources {
		if resource.Kind != PersistentVolumeKind {
			continue
		}
		if resource.Labels[RunUIDLabelKey] == run.UID {
			return true
		}
		for _, owner := range resource.OwnerReferences {
			if string(owner.UID) == run.UID || (owner.Kind == SandboxKind && (owner.Name != "" && hasString(childNames, owner.Name) || string(owner.UID) != "" && hasString(childUIDs, string(owner.UID)))) {
				return true
			}
		}
	}
	return false
}

func hasString(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}

func (p *Plan) skipLedger(pair ledgerPair, reason SkipReason) {
	runUID := ""
	if pair.Claim != nil {
		runUID = pair.Claim.Value.RunUID
	}
	p.Skipped = append(p.Skipped, Skip{RunUID: runUID, ObjectKey: pair.EffectKey, Reason: reason})
}
