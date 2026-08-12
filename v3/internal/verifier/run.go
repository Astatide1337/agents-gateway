package verifier

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/policyexec"
)

// Run executes one bounded offline verification attempt and writes one and
// only one evidence frame when identity and patch bytes are available. A
// returned error describes verifier infrastructure/configuration failure; it
// is never converted into an accepting observation. Gate command failures are
// represented by their observed non-zero ExitCode and do not, by themselves,
// make Run fail.
func Run(ctx context.Context, getenv func(string) (string, bool), stdout io.Writer) error {
	if ctx == nil || stdout == nil {
		return ErrInvalidConfig
	}
	cfg, err := ConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	analysis, analysisErr := Analyze(cfg.RepoPath, cfg.BasePath, cfg.PatchPath, cfg.MaxPatchBytes)
	if analysisErr != nil {
		if analysis.PatchDigest == "" {
			if patch, readErr := readBoundedRegular(cfg.PatchPath, cfg.MaxPatchBytes); readErr == nil {
				analysis.PatchDigest = digestBytes(patch)
			}
		}
		if analysis.PatchDigest == "" {
			return analysisErr
		}
		evidence := fallbackEvidence(cfg, analysis.PatchDigest, len(cfg.Commands))
		if emitErr := emit(stdout, evidence); emitErr != nil {
			return emitErr
		}
		return analysisErr
	}
	if cfg.ExpectedPatchDigest != "" && cfg.ExpectedPatchDigest != analysis.PatchDigest {
		evidence := fallbackEvidence(cfg, analysis.PatchDigest, len(cfg.Commands))
		if emitErr := emit(stdout, evidence); emitErr != nil {
			return emitErr
		}
		return fmt.Errorf("%w: patch digest does not match expected identity", ErrInvalidIdentity)
	}

	deadline, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	commandRuns, operationalFailure := executeCommands(deadline, cfg, cfg.RepoPath, cfg.Commands)
	policyObservations, policyErr := runPolicyChecks(deadline, cfg)

	evidence := MachineEvidence{
		SchemaVersion:  EvidenceSchemaVersion,
		RunUID:         cfg.RunUID,
		SpecDigest:     cfg.SpecDigest,
		BaseSHA:        cfg.BaseSHA,
		PatchDigest:    analysis.PatchDigest,
		ChangedPaths:   append([]string{}, analysis.ChangedPaths...),
		FilesChanged:   int64Ptr(analysis.FilesChanged),
		LinesChanged:   int64Ptr(analysis.LinesChanged),
		HasBinaryFiles: boolPtr(analysis.HasBinaryFiles),
		Commands:       commandObservations(commandRuns),
		PolicyChecks:   policyObservations,
	}

	coverageErr := error(nil)
	if cfg.CoverageDelta != "" {
		var coverage string
		coverage, coverageErr = computeCoverageDelta(deadline, cfg)
		if coverage != "" {
			evidence.CoverageDelta = stringPtr(coverage)
		}
	}

	testStrengthErr := error(nil)
	if cfg.TestStrength == "newTestsMustFailOnBase" {
		if !operationalFailure {
			passed, err := verifyNewTestsFailOnBase(deadline, cfg, analysis)
			if passed {
				evidence.NewTestsFailOnBase = boolPtr(true)
			} else {
				evidence.NewTestsFailOnBase = boolPtr(false)
			}
			testStrengthErr = err
		} else {
			evidence.NewTestsFailOnBase = boolPtr(false)
			testStrengthErr = ErrTestStrength
		}
	}

	if emitErr := emit(stdout, evidence); emitErr != nil {
		fallback := fallbackEvidence(cfg, analysis.PatchDigest, len(cfg.Commands))
		if fallbackErr := emit(stdout, fallback); fallbackErr != nil {
			return emitErr
		}
		return emitErr
	}
	if operationalFailure {
		return fmt.Errorf("%w: command execution exceeded a verifier bound", ErrAnalysis)
	}
	if coverageErr != nil {
		return coverageErr
	}
	if testStrengthErr != nil {
		return testStrengthErr
	}
	if policyErr != nil {
		return policyErr
	}
	return nil
}

func runPolicyChecks(ctx context.Context, cfg Config) ([]policycontract.CheckObservation, error) {
	if len(cfg.PolicyChecks) == 0 {
		return nil, nil
	}
	checks := make([]policycontract.CheckDescriptor, 0, len(cfg.PolicyChecks))
	for _, policyCheck := range cfg.PolicyChecks {
		checks = append(checks, policyCheck.CheckDescriptor)
	}
	contractDigest := checks[0].ContractDigest
	return policyexec.Run(ctx, cfg.RepoPath, contractDigest, checks, func(check policycontract.CheckDescriptor) (string, error) {
		if check.ScriptSourcePath == "" {
			return "", fmt.Errorf("policy rule %q has no source script", check.RuleID)
		}
		path := filepath.Join(cfg.BasePath, filepath.FromSlash(check.ScriptSourcePath))
		rel, err := filepath.Rel(cfg.BasePath, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Clean(path) != path {
			return "", fmt.Errorf("policy rule %q has an unsafe source path", check.RuleID)
		}
		return path, nil
	})
}

func commandObservations(results []commandRun) []CommandEvidence {
	output := make([]CommandEvidence, len(results))
	for index, result := range results {
		output[index] = result.Observation
		output[index].Index = index
		if output[index].ExitCode == nil {
			output[index].EvidenceDigest = ""
			output[index].DurationMillis = nil
		}
	}
	return output
}

func normalizeDecimal(value string) string {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	parts := strings.SplitN(value, ".", 2)
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	if fraction == "" {
		return integer
	}
	output := integer + "." + fraction
	if negative && output != "0" {
		return "-" + output
	}
	return output
}

func emit(stdout io.Writer, evidence MachineEvidence) error {
	frame, err := EncodeEvidenceFrame(evidence)
	if err != nil {
		return err
	}
	written, err := stdout.Write(frame)
	if err != nil {
		return fmt.Errorf("verifier: write evidence frame: %w", err)
	}
	if written != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// Main is exposed for the tiny command wrapper and keeps process-global I/O
// policy in one place for tests.
func Main() int {
	if err := Run(context.Background(), os.LookupEnv, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agw-verifier: verification failed")
		return 1
	}
	return 0
}
