package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/policyexec"
)

// PolicyCheck executes the broker projection of the same compiled Policy
// contract used to build AGENTS.md and the independent Gate. Rule IDs are an
// allowlisted subset; an empty list runs every executable/advisory rule.
func (b *Broker) PolicyCheck(ctx context.Context, ruleIDs []string) ([]policycontract.CheckObservation, error) {
	if b == nil || ctx == nil || b.contextRoot == "" || b.workspaceRoot == "" || b.policyContract.Digest() == "" {
		return nil, ErrInvalidConfig
	}
	checks, err := selectSelfChecks(b.policyContract.SelfChecks(), ruleIDs)
	if err != nil {
		return nil, err
	}
	return policyexec.Run(ctx, b.workspaceRoot, b.policyContract.Digest(), checks, func(check policycontract.CheckDescriptor) (string, error) {
		if check.ContextPackOutputPath == "" {
			return "", fmt.Errorf("rule %q has no ContextPack script", check.RuleID)
		}
		return filepath.Join(b.contextRoot, filepath.FromSlash(check.ContextPackOutputPath)), nil
	})
}

func selectSelfChecks(checks []policycontract.SelfCheckDescriptor, ruleIDs []string) ([]policycontract.CheckDescriptor, error) {
	if len(ruleIDs) == 0 {
		output := make([]policycontract.CheckDescriptor, len(checks))
		copy(output, checks)
		return output, nil
	}
	byID := make(map[string]policycontract.CheckDescriptor, len(checks))
	for _, check := range checks {
		byID[check.RuleID] = check
	}
	output := make([]policycontract.CheckDescriptor, 0, len(ruleIDs))
	seen := make(map[string]struct{}, len(ruleIDs))
	for _, id := range ruleIDs {
		if _, ok := seen[id]; ok {
			return nil, ErrInvalidRequest
		}
		check, ok := byID[id]
		if !ok {
			return nil, ErrDenied
		}
		seen[id] = struct{}{}
		output = append(output, check)
	}
	return output, nil
}

// policyCheckToolResult adapts the bounded observations to MCP's standard
// CallToolResult shape without exposing script bodies or workspace paths.
func policyCheckToolResult(observations []policycontract.CheckObservation) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"contractDigest": observationsContractDigest(observations),
		"observations":   observations,
	})
	if err != nil {
		return nil, err
	}
	content, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(body)}},
		"isError": false,
	})
	if err != nil {
		return nil, err
	}
	return content, nil
}

func observationsContractDigest(observations []policycontract.CheckObservation) string {
	if len(observations) == 0 {
		return ""
	}
	return observations[0].ContractDigest
}
