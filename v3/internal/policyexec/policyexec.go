// Package policyexec executes the deterministic scripts projected by the
// immutable policy contract. It is deliberately shared by the broker and the
// independent verifier so the two consumers cannot grow different script
// semantics.
package policyexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
)

var (
	ErrInvalidConfig = errors.New("policyexec: invalid configuration")
	ErrScript        = errors.New("policyexec: script could not be verified")
)

// PathResolver returns the exact sealed script path for one descriptor. The
// broker resolves ContextPackOutputPath; the verifier resolves
// ScriptSourcePath from its pristine checkout. Neither caller may substitute
// a path from the request.
type PathResolver func(policycontract.CheckDescriptor) (string, error)

// Run executes checks in descriptor order and returns one bounded observation
// per descriptor. A non-zero script exit is an observation, not a Go error;
// missing/tampered scripts are returned as observations without an exit code
// and also surface as an infrastructure error so the caller can fail closed.
func Run(ctx context.Context, workingDir, contractDigest string, checks []policycontract.CheckDescriptor, resolve PathResolver) ([]policycontract.CheckObservation, error) {
	if ctx == nil || !canonical.ValidDigest(contractDigest) || !validDirectory(workingDir) || resolve == nil || len(checks) > policycontract.MaxTotalRules {
		return nil, ErrInvalidConfig
	}
	observations := make([]policycontract.CheckObservation, 0, len(checks))
	seen := make(map[string]struct{}, len(checks))
	var firstErr error
	for _, check := range checks {
		if check.RuleID == "" || check.ContractDigest != contractDigest || check.ContractDigest == "" {
			return nil, fmt.Errorf("%w: descriptor identity does not match contract", ErrInvalidConfig)
		}
		if _, exists := seen[check.RuleID]; exists {
			return nil, fmt.Errorf("%w: duplicate rule %q", ErrInvalidConfig, check.RuleID)
		}
		seen[check.RuleID] = struct{}{}
		observation := policycontract.CheckObservation{ContractDigest: contractDigest, RuleID: check.RuleID}
		if !check.HasScript {
			observation.Skipped = true
			observations = append(observations, observation)
			continue
		}
		if check.Kind != policycontract.CheckKindScript || check.Expect != policycontract.ExpectExit0 || check.ExpectedExitCode != policycontract.ExpectedExitCode || check.ScriptDigest == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: rule %q has an invalid executable descriptor", ErrScript, check.RuleID)
			}
			observations = append(observations, observation)
			continue
		}
		scriptPath, err := resolve(check)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: resolve rule %q: %v", ErrScript, check.RuleID, err)
			}
			observations = append(observations, observation)
			continue
		}
		body, err := readScript(scriptPath)
		if err != nil || digest(body) != check.ScriptDigest {
			if firstErr == nil {
				if err != nil {
					firstErr = fmt.Errorf("%w: read rule %q: %v", ErrScript, check.RuleID, err)
				} else {
					firstErr = fmt.Errorf("%w: digest mismatch for rule %q", ErrScript, check.RuleID)
				}
			}
			observations = append(observations, observation)
			continue
		}
		observation.ScriptDigest = digest(body)
		command := exec.CommandContext(ctx, policycontract.ScriptRunner, scriptPath)
		command.Dir = workingDir
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C"}
		command.Stdin = nil
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		err = command.Run()
		if err == nil {
			code := int32(0)
			observation.ExitCode = &code
		} else if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ProcessState != nil {
			code := int32(exitErr.ProcessState.ExitCode())
			observation.ExitCode = &code
		} else if firstErr == nil {
			firstErr = fmt.Errorf("%w: execute rule %q: %v", ErrScript, check.RuleID, err)
		}
		observations = append(observations, observation)
	}
	return observations, firstErr
}

func readScript(name string) ([]byte, error) {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, ErrScript
	}
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > policycontract.MaxScriptBytes {
		return nil, ErrScript
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, policycontract.MaxScriptBytes+1))
	if err != nil || len(body) == 0 || len(body) > policycontract.MaxScriptBytes || strings.IndexByte(string(body), 0) >= 0 {
		return nil, ErrScript
	}
	return body, nil
}

func validDirectory(name string) bool {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" {
		return false
	}
	info, err := os.Lstat(name)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}
