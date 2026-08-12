package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrInvalidApplierConfig = errors.New("retention: invalid applier configuration")
	ErrLifecycleNotAttested = errors.New("retention: deletion is disabled until PVC lifecycle attestation")
	ErrUnsupportedAction    = errors.New("retention: unsupported deletion action")
	ErrObjectNotFound       = errors.New("retention: object-store object not found")
	ErrFenceConflict        = errors.New("retention: deletion fence conflict")
)

// KubernetesDeleteClient is deliberately delete-only. Retention never patches
// finalizers, status, labels, or owner references.
type KubernetesDeleteClient interface {
	Delete(context.Context, client.Object, ...client.DeleteOption) error
}

type ApplierConfig struct {
	ObjectPrefix      string
	LedgerPrefix      string
	MaxActions        int
	Enforce           bool
	DryRun            bool
	LifecycleAttested bool
}

type ApplyResult struct {
	Action Action
	Result string
	Reason string
}

type ApplyReport struct {
	WouldDelete int
	Deleted     int
	Skipped     int
	Failed      int
	Guarded     bool
	Results     []ApplyResult
}

const (
	ApplyResultWouldDelete    = "would-delete"
	ApplyResultDeleted        = "deleted"
	ApplyResultAlreadyAbsent  = "already-absent"
	ApplyResultSkipped        = "skipped"
	ApplyResultFenceConflict  = "fence-conflict"
	ApplyResultFailed         = "failed"
	ApplyReasonDryRun         = "dry-run"
	ApplyReasonEnforcementOff = "enforcement-disabled"
	ApplyReasonLifecycleGuard = "lifecycle-attestation-missing"
	ApplyReasonFinalizerSafe  = "finalizer-safe-delete"
	ApplyReasonInvalidAction  = "invalid-action"
)

func (c ApplierConfig) normalized() (ApplierConfig, error) {
	if err := validateObjectKey(strings.TrimSuffix(c.ObjectPrefix, "/"), false); err != nil {
		return ApplierConfig{}, fmt.Errorf("%w: object prefix: %v", ErrInvalidApplierConfig, err)
	}
	if c.LedgerPrefix == "" {
		c.LedgerPrefix = DefaultPolicy().LedgerPrefix
	}
	if err := validateObjectKey(strings.TrimSuffix(c.LedgerPrefix, "/"), false); err != nil {
		return ApplierConfig{}, fmt.Errorf("%w: ledger prefix: %v", ErrInvalidApplierConfig, err)
	}
	if c.MaxActions == 0 {
		c.MaxActions = DefaultMaxActionsPerPlan
	}
	if c.MaxActions < 1 {
		return ApplierConfig{}, fmt.Errorf("%w: max actions must be positive", ErrInvalidApplierConfig)
	}
	if c.Enforce && c.DryRun {
		return ApplierConfig{}, fmt.Errorf("%w: enforce and dry-run cannot both be enabled", ErrInvalidApplierConfig)
	}
	return c, nil
}

// Apply executes an already complete, deterministic plan. It re-checks the
// exact run prefix and uses Kubernetes UID preconditions/S3 ETags. A missing
// object is an idempotent success; a stale fence is recorded and never
// converted into a force delete.
func Apply(ctx context.Context, plan Plan, kube KubernetesDeleteClient, objects ObjectInventory, config ApplierConfig) (ApplyReport, error) {
	var report ApplyReport
	if ctx == nil || kube == nil || objects == nil {
		return report, ErrInvalidApplierConfig
	}
	normalized, err := config.normalized()
	if err != nil {
		return report, err
	}
	if len(plan.Actions) > normalized.MaxActions {
		return report, fmt.Errorf("%w: %d actions exceed %d", ErrActionLimit, len(plan.Actions), normalized.MaxActions)
	}
	if len(plan.Actions) == 0 && len(plan.LedgerPairs) == 0 {
		return report, nil
	}
	if err := validateLedgerPlan(plan, normalized); err != nil {
		return report, err
	}
	if !normalized.LifecycleAttested && normalized.Enforce {
		report.Guarded = true
	}

	var failures []error
	for _, action := range plan.Actions {
		if action.PairKey != "" {
			continue
		}
		result := ApplyResult{Action: action}
		switch {
		case plan.DryRun || normalized.DryRun:
			result.Result, result.Reason = ApplyResultSkipped, ApplyReasonDryRun
			report.WouldDelete++
			report.Skipped++
		case !normalized.Enforce:
			result.Result, result.Reason = ApplyResultSkipped, ApplyReasonEnforcementOff
			report.WouldDelete++
			report.Skipped++
		case !normalized.LifecycleAttested:
			result.Result, result.Reason = ApplyResultSkipped, ApplyReasonLifecycleGuard
			report.WouldDelete++
			report.Skipped++
		case action.Mode != ActionDelete:
			result.Result, result.Reason = ApplyResultSkipped, ApplyReasonInvalidAction
			report.Skipped++
		default:
			if err := applyAction(ctx, action, normalized, kube, objects); err != nil {
				if apierrors.IsNotFound(err) || errors.Is(err, ErrObjectNotFound) {
					result.Result, result.Reason = ApplyResultAlreadyAbsent, "idempotent-absent"
					report.Deleted++
				} else if apierrors.IsConflict(err) || errors.Is(err, ErrFenceConflict) {
					result.Result, result.Reason = ApplyResultFenceConflict, "uid-or-etag-fence-conflict"
					report.Skipped++
				} else {
					result.Result, result.Reason = ApplyResultFailed, "delete-error"
					report.Failed++
					failures = append(failures, err)
				}
			} else {
				result.Result, result.Reason = ApplyResultDeleted, ApplyReasonFinalizerSafe
				report.Deleted++
			}
		}
		report.Results = append(report.Results, result)
	}
	for _, pair := range plan.LedgerPairs {
		pairReport, err := applyLedgerPair(ctx, pair, plan, normalized, objects)
		report.WouldDelete += pairReport.WouldDelete
		report.Deleted += pairReport.Deleted
		report.Skipped += pairReport.Skipped
		report.Failed += pairReport.Failed
		report.Results = append(report.Results, pairReport.Results...)
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return report, errors.Join(failures...)
	}
	return report, nil
}

type pairApplyReport struct {
	WouldDelete int
	Deleted     int
	Skipped     int
	Failed      int
	Results     []ApplyResult
}

func validateLedgerPlan(plan Plan, config ApplierConfig) error {
	if len(plan.LedgerPairs) == 0 {
		for _, action := range plan.Actions {
			if action.PairKey != "" {
				return fmt.Errorf("%w: ledger action has no pair", ErrUnsupportedAction)
			}
		}
		return nil
	}
	actions := make(map[string]map[string]Action, len(plan.LedgerPairs))
	for _, action := range plan.Actions {
		if action.PairKey == "" {
			continue
		}
		if action.Role != "ledger-claim" && action.Role != "ledger-outcome" {
			return fmt.Errorf("%w: invalid ledger action role", ErrUnsupportedAction)
		}
		if actions[action.PairKey] == nil {
			actions[action.PairKey] = make(map[string]Action, 2)
		}
		if _, exists := actions[action.PairKey][action.Role]; exists {
			return fmt.Errorf("%w: duplicate ledger action", ErrUnsupportedAction)
		}
		actions[action.PairKey][action.Role] = action
	}
	pairs := make(map[string]struct{}, len(plan.LedgerPairs))
	for _, pair := range plan.LedgerPairs {
		if _, exists := pairs[pair.EffectKey]; exists {
			return fmt.Errorf("%w: duplicate ledger pair", ErrUnsupportedAction)
		}
		pairs[pair.EffectKey] = struct{}{}
		if !canonical.ValidDigest(pair.EffectKey) || pair.RunUID == "" || pair.Claim.EffectKey != pair.EffectKey || pair.Outcome.EffectKey != pair.EffectKey || pair.Claim.RequestDigest != pair.Outcome.RequestDigest || pair.Claim.RunUID != pair.RunUID || pair.Outcome.RunUID != pair.RunUID || (pair.Outcome.State != effects.OutcomeSucceeded && pair.Outcome.State != effects.OutcomeFailed) || pair.ClaimETag == "" || pair.OutcomeETag == "" || len(pair.TombstoneBody) == 0 || len(pair.TombstoneBody) > effects.MaxLedgerBodyBytes {
			return fmt.Errorf("%w: malformed ledger pair %s", ErrUnsupportedAction, pair.EffectKey)
		}
		if segment, key, err := parseLedgerObjectKey(config.ObjectPrefix, config.LedgerPrefix, pair.ClaimKey); err != nil || segment != LedgerClaimsSegment || key != pair.EffectKey {
			return fmt.Errorf("%w: invalid ledger claim key", ErrUnsupportedAction)
		}
		if segment, key, err := parseLedgerObjectKey(config.ObjectPrefix, config.LedgerPrefix, pair.OutcomeKey); err != nil || segment != LedgerOutcomesSegment || key != pair.EffectKey {
			return fmt.Errorf("%w: invalid ledger outcome key", ErrUnsupportedAction)
		}
		if segment, key, err := parseLedgerObjectKey(config.ObjectPrefix, config.LedgerPrefix, pair.TombstoneKey); err != nil || segment != LedgerTombstonesSegment || key != pair.EffectKey {
			return fmt.Errorf("%w: invalid ledger tombstone key", ErrUnsupportedAction)
		}
		tombstone, err := effects.DecodeTombstone(pair.TombstoneBody)
		if err != nil || !sameTombstoneIdentity(tombstone, pair.Tombstone) || tombstone.State != pair.Outcome.State || tombstone.EffectKey != pair.EffectKey || tombstone.RequestDigest != pair.Claim.RequestDigest || tombstone.Operation != pair.Claim.Operation || tombstone.RunUID != pair.RunUID {
			return fmt.Errorf("%w: invalid ledger tombstone body", ErrUnsupportedAction)
		}
		pairActions := actions[pair.EffectKey]
		claimAction, claimOK := pairActions["ledger-claim"]
		outcomeAction, outcomeOK := pairActions["ledger-outcome"]
		if !claimOK || !outcomeOK || len(pairActions) != 2 || claimAction.Mode != plan.actionMode() || outcomeAction.Mode != plan.actionMode() || claimAction.RunUID != pair.RunUID || outcomeAction.RunUID != pair.RunUID || claimAction.PairKey != pair.EffectKey || outcomeAction.PairKey != pair.EffectKey || claimAction.Target.ObjectKey != pair.ClaimKey || claimAction.Target.ETag != pair.ClaimETag || outcomeAction.Target.ObjectKey != pair.OutcomeKey || outcomeAction.Target.ETag != pair.OutcomeETag {
			return fmt.Errorf("%w: ledger pair actions are incomplete", ErrUnsupportedAction)
		}
	}
	for effectKey := range actions {
		if _, exists := pairs[effectKey]; !exists {
			return fmt.Errorf("%w: ledger action has no pair", ErrUnsupportedAction)
		}
	}
	return nil
}

func applyLedgerPair(ctx context.Context, pair LedgerPairPlan, plan Plan, config ApplierConfig, objects ObjectInventory) (pairApplyReport, error) {
	result := pairApplyReport{}
	actions := []Action{
		{Mode: plan.actionMode(), RunUID: pair.RunUID, Role: "ledger-outcome", PairKey: pair.EffectKey, Reason: "terminal effect outcome exceeded retention window", Target: ActionTarget{Kind: ObjectStoreKind, ObjectKey: pair.OutcomeKey, ETag: pair.OutcomeETag}},
		{Mode: plan.actionMode(), RunUID: pair.RunUID, Role: "ledger-claim", PairKey: pair.EffectKey, Reason: "terminal effect claim exceeded retention window", Target: ActionTarget{Kind: ObjectStoreKind, ObjectKey: pair.ClaimKey, ETag: pair.ClaimETag}},
	}
	if plan.DryRun || config.DryRun {
		for _, action := range actions {
			result.Results = append(result.Results, ApplyResult{Action: action, Result: ApplyResultSkipped, Reason: ApplyReasonDryRun})
			result.WouldDelete++
			result.Skipped++
		}
		return result, nil
	}
	if !config.Enforce {
		for _, action := range actions {
			result.Results = append(result.Results, ApplyResult{Action: action, Result: ApplyResultSkipped, Reason: ApplyReasonEnforcementOff})
			result.WouldDelete++
			result.Skipped++
		}
		return result, nil
	}
	if !config.LifecycleAttested {
		for _, action := range actions {
			result.Results = append(result.Results, ApplyResult{Action: action, Result: ApplyResultSkipped, Reason: ApplyReasonLifecycleGuard})
			result.WouldDelete++
			result.Skipped++
		}
		return result, nil
	}
	if actions[0].Mode != ActionDelete || actions[1].Mode != ActionDelete {
		return result, ErrUnsupportedAction
	}
	writer, ok := objects.(ObjectPairWriter)
	if !ok {
		for _, action := range actions {
			result.Results = append(result.Results, ApplyResult{Action: action, Result: ApplyResultFailed, Reason: "ledger-fence-store-unavailable"})
			result.Failed++
		}
		return result, fmt.Errorf("%w: object store cannot create and read ledger tombstones", ErrUnsupportedAction)
	}
	if err := ensureLedgerTombstone(ctx, pair, writer); err != nil {
		for _, action := range actions {
			result.Results = append(result.Results, ApplyResult{Action: action, Result: ApplyResultFailed, Reason: "ledger-fence-failed"})
			result.Failed++
		}
		return result, err
	}

	err := objects.Delete(ctx, pair.OutcomeKey, pair.OutcomeETag)
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		if errors.Is(err, ErrFenceConflict) {
			result.Results = append(result.Results, ApplyResult{Action: actions[0], Result: ApplyResultFenceConflict, Reason: "uid-or-etag-fence-conflict"}, ApplyResult{Action: actions[1], Result: ApplyResultSkipped, Reason: "ledger-outcome-not-retired"})
			result.Skipped += 2
			return result, nil
		}
		result.Results = append(result.Results, ApplyResult{Action: actions[0], Result: ApplyResultFailed, Reason: "delete-error"}, ApplyResult{Action: actions[1], Result: ApplyResultSkipped, Reason: "ledger-outcome-not-retired"})
		result.Failed++
		result.Skipped++
		return result, err
	}
	if errors.Is(err, ErrObjectNotFound) {
		result.Results = append(result.Results, ApplyResult{Action: actions[0], Result: ApplyResultAlreadyAbsent, Reason: "idempotent-absent"})
	} else {
		result.Results = append(result.Results, ApplyResult{Action: actions[0], Result: ApplyResultDeleted, Reason: ApplyReasonFinalizerSafe})
	}
	result.Deleted++

	claimErr := objects.Delete(ctx, pair.ClaimKey, pair.ClaimETag)
	if claimErr != nil && !errors.Is(claimErr, ErrObjectNotFound) {
		if errors.Is(claimErr, ErrFenceConflict) {
			result.Results = append(result.Results, ApplyResult{Action: actions[1], Result: ApplyResultFenceConflict, Reason: "uid-or-etag-fence-conflict"})
			result.Skipped++
			return result, nil
		}
		result.Results = append(result.Results, ApplyResult{Action: actions[1], Result: ApplyResultFailed, Reason: "delete-error"})
		result.Failed++
		return result, claimErr
	}
	if errors.Is(claimErr, ErrObjectNotFound) {
		result.Results = append(result.Results, ApplyResult{Action: actions[1], Result: ApplyResultAlreadyAbsent, Reason: "idempotent-absent"})
	} else {
		result.Results = append(result.Results, ApplyResult{Action: actions[1], Result: ApplyResultDeleted, Reason: ApplyReasonFinalizerSafe})
	}
	result.Deleted++
	return result, nil
}

func ensureLedgerTombstone(ctx context.Context, pair LedgerPairPlan, writer ObjectPairWriter) error {
	if pair.TombstoneExists {
		body, err := writer.Get(ctx, pair.TombstoneKey)
		if err != nil {
			return fmt.Errorf("%w: existing tombstone cannot be proven", err)
		}
		tombstone, err := effects.DecodeTombstone(body)
		if err != nil || !sameTombstoneIdentity(tombstone, pair.Tombstone) {
			return fmt.Errorf("%w: existing tombstone identity mismatch", ErrFenceConflict)
		}
		return nil
	}
	created, err := writer.Create(ctx, pair.TombstoneKey, pair.TombstoneBody, "application/json")
	if err != nil {
		return fmt.Errorf("create ledger tombstone: %w", err)
	}
	if created {
		return nil
	}
	body, err := writer.Get(ctx, pair.TombstoneKey)
	if err != nil {
		return fmt.Errorf("%w: conditional tombstone create was ambiguous", ErrFenceConflict)
	}
	tombstone, err := effects.DecodeTombstone(body)
	if err != nil || !sameTombstoneIdentity(tombstone, pair.Tombstone) {
		return fmt.Errorf("%w: conditional tombstone identity mismatch", ErrFenceConflict)
	}
	return nil
}

func applyAction(ctx context.Context, action Action, config ApplierConfig, kube KubernetesDeleteClient, objects ObjectInventory) error {
	if action.RunUID == "" {
		return ErrUnsupportedAction
	}
	switch action.Target.Kind {
	case SecretKind:
		if action.Target.Namespace == "" || action.Target.Name == "" || !validUID(action.Target.UID) {
			return ErrUnsupportedAction
		}
		uid := types.UID(action.Target.UID)
		object := &corev1.Secret{}
		object.Namespace, object.Name = action.Target.Namespace, action.Target.Name
		return kube.Delete(ctx, object,
			client.Preconditions{UID: &uid},
			client.PropagationPolicy(metav1.DeletePropagationBackground),
		)
	case PersistentVolumeKind:
		if action.Target.Namespace == "" || action.Target.Name == "" || !validUID(action.Target.UID) {
			return ErrUnsupportedAction
		}
		uid := types.UID(action.Target.UID)
		object := &corev1.PersistentVolumeClaim{}
		object.Namespace, object.Name = action.Target.Namespace, action.Target.Name
		return kube.Delete(ctx, object,
			client.Preconditions{UID: &uid},
			client.PropagationPolicy(metav1.DeletePropagationBackground),
		)
	case ObjectStoreKind:
		if action.Target.ObjectKey == "" || action.Target.ETag == "" || action.Target.ObjectKey != runObjectKey(config.ObjectPrefix, action.RunUID, action.Target.ObjectKey) {
			return ErrUnsupportedAction
		}
		return objects.Delete(ctx, action.Target.ObjectKey, action.Target.ETag)
	default:
		return ErrUnsupportedAction
	}
}

func runObjectKey(prefix, runUID, key string) string {
	if validateArtifactKey(prefix, runUID, key) != nil {
		return ""
	}
	return key
}
