package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Plan is the canonical, lexicographically ordered representation of a
// manifest. Ordering is derived from IDs, never from input slice order.
type Plan struct {
	Name     string
	Revision string
	Steps    []Step
}

func CompileManifest(manifest Manifest) (Plan, error) {
	if strings.TrimSpace(manifest.Name) == "" || strings.TrimSpace(manifest.Revision) == "" {
		return Plan{}, errors.New("manifest name and revision are required")
	}
	if len(manifest.Steps) == 0 || len(manifest.Steps) > MaxManifestSteps {
		return Plan{}, fmt.Errorf("manifest must contain between 1 and %d steps", MaxManifestSteps)
	}
	if len(manifest.Name) > MaxReferenceSize || len(manifest.Revision) > MaxReferenceSize {
		return Plan{}, errors.New("manifest identity is not bounded")
	}

	steps := append([]Step(nil), manifest.Steps...)
	sort.Slice(steps, func(i, j int) bool { return steps[i].ID < steps[j].ID })
	byID := make(map[string]Step, len(steps))
	for _, step := range steps {
		if err := validateReference(step.ID, "step id"); err != nil {
			return Plan{}, err
		}
		if _, exists := byID[step.ID]; exists {
			return Plan{}, fmt.Errorf("duplicate step id %q", step.ID)
		}
		if (step.AgentRef == "") == (step.Approval == nil) {
			return Plan{}, fmt.Errorf("step %q must define exactly one agent or approval", step.ID)
		}
		if step.AgentRef != "" {
			if err := validateReference(step.AgentRef, "agent reference"); err != nil {
				return Plan{}, fmt.Errorf("step %q: %w", step.ID, err)
			}
		}
		if err := validateInputReference(step.InputRef, "input reference"); err != nil {
			return Plan{}, fmt.Errorf("step %q: %w", step.ID, err)
		}
		if step.Timeout < 0 {
			return Plan{}, fmt.Errorf("step %q: timeout must not be negative", step.ID)
		}
		if step.Retry.MaxAttempts < 0 || step.Retry.InitialInterval < 0 || step.Retry.MaximumInterval < 0 {
			return Plan{}, fmt.Errorf("step %q: retry values must not be negative", step.ID)
		}
		if step.Retry.BackoffCoefficient != 0 && step.Retry.BackoffCoefficient < 1 {
			return Plan{}, fmt.Errorf("step %q: retry backoff coefficient must be at least one", step.ID)
		}
		if step.Approval != nil {
			if err := validateReference(step.Approval.ID, "approval id"); err != nil {
				return Plan{}, fmt.Errorf("step %q: %w", step.ID, err)
			}
			if strings.TrimSpace(step.Approval.Reason) == "" || len(step.Approval.Reason) > MaxReferenceSize {
				return Plan{}, fmt.Errorf("step %q: approval reason must be bounded and non-empty", step.ID)
			}
		}
		byID[step.ID] = step
	}

	for i := range steps {
		needs := append([]string(nil), steps[i].Needs...)
		sort.Strings(needs)
		steps[i].Needs = needs
		seen := make(map[string]struct{}, len(needs))
		for _, need := range needs {
			if need == steps[i].ID {
				return Plan{}, fmt.Errorf("step %q cannot depend on itself", steps[i].ID)
			}
			if _, ok := byID[need]; !ok {
				return Plan{}, fmt.Errorf("step %q depends on unknown step %q", steps[i].ID, need)
			}
			if _, ok := seen[need]; ok {
				return Plan{}, fmt.Errorf("step %q repeats dependency %q", steps[i].ID, need)
			}
			seen[need] = struct{}{}
		}
	}

	if !acyclic(steps) {
		return Plan{}, errors.New("manifest contains a dependency cycle")
	}
	return Plan{Name: manifest.Name, Revision: manifest.Revision, Steps: steps}, nil
}

func acyclic(steps []Step) bool {
	indegree := make(map[string]int, len(steps))
	children := make(map[string][]string, len(steps))
	for _, step := range steps {
		indegree[step.ID] = len(step.Needs)
		for _, need := range step.Needs {
			children[need] = append(children[need], step.ID)
		}
	}
	queue := make([]string, 0, len(steps))
	for _, step := range steps {
		if indegree[step.ID] == 0 {
			queue = append(queue, step.ID)
		}
	}
	sort.Strings(queue)
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		kids := append([]string(nil), children[id]...)
		sort.Strings(kids)
		for _, child := range kids {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
				sort.Strings(queue)
			}
		}
	}
	return visited == len(steps)
}

type stepPhase string

const (
	phasePending   stepPhase = "pending"
	phaseRunning   stepPhase = "running"
	phaseSucceeded stepPhase = "succeeded"
)

func readySteps(plan Plan, phases map[string]stepPhase) []Step {
	ready := make([]Step, 0)
	for _, step := range plan.Steps {
		if phases[step.ID] != phasePending {
			continue
		}
		allSucceeded := true
		for _, need := range step.Needs {
			if phases[need] != phaseSucceeded {
				allSucceeded = false
				break
			}
		}
		if allSucceeded {
			ready = append(ready, step)
		}
	}
	return ready
}

func stepDispatchKey(runID, workflowName, stepID string) string {
	return strings.Join([]string{runID, workflowName, stepID}, "/")
}

func childWorkflowID(runID, stepID string) string {
	return strings.Join([]string{runID, "agent", stepID}, "/")
}
