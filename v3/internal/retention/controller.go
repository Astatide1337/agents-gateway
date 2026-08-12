package retention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	retentionCycles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agw_retention_cycles_total", Help: "Retention inventory cycles by outcome.",
	}, []string{"outcome"})
	retentionInventoryItems = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agw_retention_inventory_items", Help: "Items in the last complete retention inventory.",
	}, []string{"kind"})
	retentionActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agw_retention_actions_total", Help: "Retention actions by resource kind and result.",
	}, []string{"kind", "result"})
	retentionSkips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agw_retention_skips_total", Help: "Retention skips by stable reason code.",
	}, []string{"reason"})
	retentionLastComplete = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agw_retention_last_complete_inventory_timestamp_seconds", Help: "Unix timestamp of the last complete retention inventory.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(retentionCycles, retentionInventoryItems, retentionActions, retentionSkips, retentionLastComplete)
}

type EventSink interface {
	Record(context.Context, Run, string, string)
}

type ControllerConfig struct {
	Policy            Policy
	Interval          time.Duration
	InventoryLimits   InventoryLimits
	Enforce           bool
	DryRun            bool
	LifecycleAttested bool
	MaxEventsPerCycle int
}

func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{
		Policy:            DefaultPolicy(),
		Interval:          15 * time.Minute,
		InventoryLimits:   DefaultInventoryLimits(),
		DryRun:            true,
		MaxEventsPerCycle: 32,
	}
}

func (c ControllerConfig) normalized() (ControllerConfig, error) {
	if c.Interval <= 0 || c.MaxEventsPerCycle <= 0 {
		return ControllerConfig{}, fmt.Errorf("%w: interval and event cap must be positive", ErrInvalidApplierConfig)
	}
	limits, err := c.InventoryLimits.normalized()
	if err != nil {
		return ControllerConfig{}, err
	}
	policy, err := c.Policy.normalized()
	if err != nil {
		return ControllerConfig{}, err
	}
	if c.Enforce && c.DryRun {
		return ControllerConfig{}, fmt.Errorf("%w: enforce and dry-run cannot both be enabled", ErrInvalidApplierConfig)
	}
	policy.DryRun = c.DryRun
	c.Policy, c.InventoryLimits = policy, limits
	return c, nil
}

type CycleReport struct {
	Inventory Inventory
	Plan      Plan
	Apply     ApplyReport
	Outcome   string
}

type Controller struct {
	source  *KubernetesSource
	kube    KubernetesDeleteClient
	objects ObjectInventory
	config  ControllerConfig
	events  EventSink
	clock   func() time.Time
	logger  logr.Logger
}

func NewController(source *KubernetesSource, kube KubernetesDeleteClient, objects ObjectInventory, config ControllerConfig, events EventSink, logger logr.Logger) (*Controller, error) {
	if source == nil || kube == nil || objects == nil {
		return nil, ErrInvalidApplierConfig
	}
	normalized, err := config.normalized()
	if err != nil {
		return nil, err
	}
	if source.ObjectPrefix != normalized.Policy.ObjectPrefix {
		return nil, fmt.Errorf("%w: inventory and planner object prefixes differ", ErrInvalidApplierConfig)
	}
	if source.LedgerPrefix == "" {
		source.LedgerPrefix = normalized.Policy.LedgerPrefix
	}
	if source.LedgerPrefix != normalized.Policy.LedgerPrefix {
		return nil, fmt.Errorf("%w: inventory and planner ledger prefixes differ", ErrInvalidApplierConfig)
	}
	if source.Limits != normalized.InventoryLimits {
		return nil, fmt.Errorf("%w: inventory limits differ from controller limits", ErrInvalidApplierConfig)
	}
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}
	return &Controller{source: source, kube: kube, objects: objects, config: normalized, events: events, clock: time.Now, logger: logger}, nil
}

// NeedLeaderElection keeps only the elected operator instance mutating the
// cluster or object store when controller-runtime leader election is enabled.
func (c *Controller) NeedLeaderElection() bool { return true }

// Start implements controller-runtime's periodic Runnable contract. A cycle
// runs immediately and then at a bounded interval; a transient inventory error
// is logged and retried on the next tick without ever applying a partial plan.
func (c *Controller) Start(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrInvalidApplierConfig
	}
	if _, err := c.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.logger.Error(err, "retention inventory cycle failed", "reason", "inventory-cycle-error")
	}
	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := c.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logger.Error(err, "retention inventory cycle failed", "reason", "inventory-cycle-error")
			}
		}
	}
}

func (c *Controller) RunOnce(ctx context.Context) (CycleReport, error) {
	var report CycleReport
	if c == nil || ctx == nil {
		return report, ErrInvalidApplierConfig
	}
	now := c.clock().UTC()
	inventory, err := c.source.Collect(ctx)
	if err != nil {
		retentionCycles.WithLabelValues("incomplete").Inc()
		c.logger.Error(err, "retention inventory rejected", "reason", "inventory-incomplete")
		return report, err
	}
	report.Inventory = inventory
	retentionInventoryItems.WithLabelValues(AgentRunKind).Set(float64(len(inventory.Runs)))
	retentionInventoryItems.WithLabelValues("Secret+PVC").Set(float64(len(inventory.Resources)))
	retentionInventoryItems.WithLabelValues(ObjectStoreKind).Set(float64(len(inventory.Artifacts) + len(inventory.Ledger)))
	policy := c.config.Policy
	policy.DryRun = c.config.DryRun
	plan, err := policy.Plan(now, inventory)
	if err != nil {
		retentionCycles.WithLabelValues("planner-error").Inc()
		c.logger.Error(err, "retention plan rejected", "reason", "plan-invalid")
		return report, err
	}
	report.Plan = plan
	for _, skipped := range plan.Skipped {
		retentionSkips.WithLabelValues(string(skipped.Reason)).Inc()
	}
	applyReport, err := Apply(ctx, plan, c.kube, c.objects, ApplierConfig{
		ObjectPrefix: policy.ObjectPrefix, LedgerPrefix: policy.LedgerPrefix, MaxActions: policy.MaxActionsPerPlan, Enforce: c.config.Enforce, DryRun: c.config.DryRun,
		LifecycleAttested: c.config.LifecycleAttested,
	})
	report.Apply = applyReport
	for _, result := range applyReport.Results {
		retentionActions.WithLabelValues(result.Action.Target.Kind, result.Result).Inc()
	}
	if applyReport.Guarded {
		retentionCycles.WithLabelValues("guarded").Inc()
	} else if applyReport.Failed > 0 {
		retentionCycles.WithLabelValues("apply-error").Inc()
	} else if c.config.DryRun || !c.config.Enforce {
		retentionCycles.WithLabelValues("dry-run").Inc()
	} else {
		retentionCycles.WithLabelValues("applied").Inc()
	}
	retentionLastComplete.Set(float64(now.Unix()))
	c.emitEvents(ctx, inventory, plan, applyReport)
	c.logger.Info("retention cycle complete", "reason", "inventory-complete", "runs", len(inventory.Runs), "resources", len(inventory.Resources), "objects", len(inventory.Artifacts)+len(inventory.Ledger), "actions", len(plan.Actions), "skipped", len(plan.Skipped), "deleted", applyReport.Deleted, "dryRun", c.config.DryRun, "enforce", c.config.Enforce)
	if err != nil {
		return report, err
	}
	return report, nil
}

func (c *Controller) emitEvents(ctx context.Context, inventory Inventory, plan Plan, applied ApplyReport) {
	if c.events == nil {
		return
	}
	runs := make(map[string]Run, len(inventory.Runs))
	for _, run := range inventory.Runs {
		runs[run.UID] = run
	}
	reasons := make(map[string]map[string]struct{})
	for _, skipped := range plan.Skipped {
		if skipped.RunUID == "" {
			continue
		}
		if reasons[skipped.RunUID] == nil {
			reasons[skipped.RunUID] = make(map[string]struct{})
		}
		reasons[skipped.RunUID][string(skipped.Reason)] = struct{}{}
	}
	for _, result := range applied.Results {
		if result.Action.RunUID == "" {
			continue
		}
		if reasons[result.Action.RunUID] == nil {
			reasons[result.Action.RunUID] = make(map[string]struct{})
		}
		if result.Reason != "" && result.Result != ApplyResultDeleted && result.Result != ApplyResultAlreadyAbsent {
			reasons[result.Action.RunUID][result.Reason] = struct{}{}
		}
	}
	keys := make([]string, 0, len(reasons))
	for key := range reasons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > c.config.MaxEventsPerCycle {
		keys = keys[:c.config.MaxEventsPerCycle]
	}
	for _, runUID := range keys {
		run, ok := runs[runUID]
		if !ok {
			continue
		}
		codes := make([]string, 0, len(reasons[runUID]))
		for reason := range reasons[runUID] {
			codes = append(codes, reason)
		}
		sort.Strings(codes)
		if len(codes) > 8 {
			codes = codes[:8]
		}
		c.events.Record(ctx, run, "RetentionSkipped", "retention reason codes: "+joinReasonCodes(codes))
	}
}

func joinReasonCodes(values []string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}

var _ interface {
	Start(context.Context) error
	NeedLeaderElection() bool
} = (*Controller)(nil)
