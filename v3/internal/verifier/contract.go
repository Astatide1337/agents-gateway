// Package verifier implements the trusted, offline Gate verifier that runs
// after the Sandbox network airlock has been installed.
//
// The package deliberately emits observations, never a Gate verdict. The
// controller's independent Gate engine evaluates the evidence later. This
// keeps a process exit, a mutable environment, or a verifier bug from being
// mistaken for acceptance.
package verifier

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	EvidenceSchemaVersion = "agents.astatide.com/verification-evidence/v1alpha1"
	EvidenceFramePrefix   = "AGW_VERIFY_EVIDENCE_V1 "

	CommandEnvelopeVersion      = 1
	RequirementsEnvelopeVersion = 2

	DefaultMaxOutputBytes = 64 << 10
	HardMaxOutputBytes    = 256 << 10
	DefaultMaxPatchBytes  = 128 << 20
	HardMaxPatchBytes     = 1 << 30
	DefaultMaxCopyBytes   = 1 << 30
	HardMaxCopyBytes      = 4 << 30
	MaxCommandBytes       = 4096
	MaxPolicyChecksBytes  = 512 << 10
	MaxPathBytes          = gate.MaxPathBytes
	MaxDuration           = 24 * time.Hour
)

var (
	ErrInvalidConfig   = errors.New("verifier: invalid configuration")
	ErrInvalidIdentity = errors.New("verifier: invalid evidence identity")
	ErrUnsafePath      = errors.New("verifier: unsafe repository path")
	ErrSymlink         = errors.New("verifier: symlink encountered")
	ErrOutputLimit     = errors.New("verifier: command output limit exceeded")
	ErrCommandTimeout  = errors.New("verifier: command timed out")
	ErrPatchLimit      = errors.New("verifier: patch size limit exceeded")
	ErrAnalysis        = errors.New("verifier: repository evidence analysis failed")
	ErrTestStrength    = errors.New("verifier: new-tests-on-base evidence failed")
	ErrCoverage        = errors.New("verifier: coverage evidence is unavailable")

	sha256Pattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	baseSHAPattern = regexp.MustCompile(`^[a-f0-9]{40}$|^[a-f0-9]{64}$`)
	decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
)

// MachineEvidence is intentionally field-for-field compatible with
// verifycontroller.MachineEvidence. Do not add a verdict or a success field.
// A missing pointer is meaningful: it tells the Gate that a fact was not
// independently observed.
type MachineEvidence struct {
	SchemaVersion string `json:"schemaVersion"`
	RunUID        string `json:"runUID"`
	SpecDigest    string `json:"specDigest"`
	BaseSHA       string `json:"baseSHA"`
	PatchDigest   string `json:"patchDigest"`

	ChangedPaths       []string                          `json:"changedPaths"`
	FilesChanged       *int64                            `json:"filesChanged,omitempty"`
	LinesChanged       *int64                            `json:"linesChanged,omitempty"`
	HasBinaryFiles     *bool                             `json:"hasBinaryFiles,omitempty"`
	CoverageDelta      *string                           `json:"coverageDelta,omitempty"`
	NewTestsFailOnBase *bool                             `json:"newTestsFailOnBase,omitempty"`
	Commands           []CommandEvidence                 `json:"commands"`
	PolicyChecks       []policycontract.CheckObservation `json:"policyChecks,omitempty"`
}

// CommandEvidence is the bounded observation consumed by the Gate engine.
// An absent ExitCode/EvidenceDigest is an explicit missing observation, not a
// successful command.
type CommandEvidence struct {
	Index          int    `json:"index"`
	ExitCode       *int32 `json:"exitCode,omitempty"`
	EvidenceDigest string `json:"evidenceDigest,omitempty"`
	DurationMillis *int64 `json:"durationMillis,omitempty"`
}

// Config is the immutable process configuration loaded from the environment.
// Gate and sandbox manifests are trusted controller inputs; repository text is
// not. Commands are trusted Gate policy and are still executed with a fixed
// shell, clean environment, bounded cwd, and no inherited file descriptors.
type Config struct {
	Workspace string
	RepoPath  string
	BasePath  string
	PatchPath string

	RunUID              string
	SpecDigest          string
	BaseSHA             string
	ExpectedPatchDigest string
	InputDigest         string

	Timeout        time.Duration
	MaxOutputBytes int64
	MaxPatchBytes  int64
	MaxCopyBytes   int64

	Commands        []v1alpha1.VerifyCommand
	BaseTestCommand *v1alpha1.VerifyCommand
	PolicyChecks    []policycontract.GateCheckDescriptor

	Adapter       v1alpha1.GateAdapter
	TestStrength  v1alpha1.TestStrength
	CoverageDelta string
}

type commandEnvelope struct {
	Version      int                      `json:"version"`
	Commands     []v1alpha1.VerifyCommand `json:"commands"`
	Requirements *requirementsEnvelope    `json:"requirements,omitempty"`
}

type requirementsEnvelope struct {
	Version         int                     `json:"version"`
	Adapter         v1alpha1.GateAdapter    `json:"adapter,omitempty"`
	TestStrength    v1alpha1.TestStrength   `json:"testStrength"`
	BaseTestCommand *v1alpha1.VerifyCommand `json:"baseTestCommand,omitempty"`
	CoverageDelta   string                  `json:"coverageDelta,omitempty"`
}

// ConfigFromEnv loads only the documented AGW_VERIFY_* variables. The
// getenv seam keeps parser tests deterministic and prevents the package from
// accidentally observing a developer's ambient proxy or credential env.
func ConfigFromEnv(getenv func(string) (string, bool)) (Config, error) {
	if getenv == nil {
		return Config{}, ErrInvalidConfig
	}
	get := func(name string) string {
		value, _ := getenv(name)
		return value
	}

	cfg := Config{
		Workspace:   get("AGW_WORKSPACE"),
		RepoPath:    get("AGW_REPO_PATH"),
		BasePath:    get("AGW_BASE_PATH"),
		PatchPath:   get("AGW_PATCH_PATH"),
		RunUID:      get("AGW_RUN_UID"),
		SpecDigest:  get("AGW_SPEC_DIGEST"),
		BaseSHA:     get("AGW_BASE_SHA"),
		InputDigest: get("AGW_VERIFY_INPUT_DIGEST"),
	}
	if network := get("AGW_VERIFY_NETWORK"); network != "" && network != "disabled" {
		return Config{}, fmt.Errorf("%w: verifier network mode must be disabled", ErrInvalidConfig)
	}
	if expected := get("AGW_PATCH_DIGEST"); expected != "" {
		cfg.ExpectedPatchDigest = expected
	}
	if err := validateIdentity(&cfg); err != nil {
		return Config{}, err
	}
	if err := validatePaths(&cfg); err != nil {
		return Config{}, err
	}

	timeoutText := get("AGW_VERIFY_TIMEOUT")
	timeout, err := time.ParseDuration(timeoutText)
	if err != nil || timeout <= 0 || timeout > MaxDuration {
		return Config{}, fmt.Errorf("%w: timeout must be in (0,%s]", ErrInvalidConfig, MaxDuration)
	}
	cfg.Timeout = timeout

	cfg.MaxOutputBytes, err = boundedEnvInt(get, "AGW_VERIFY_MAX_OUTPUT_BYTES", DefaultMaxOutputBytes, 1, HardMaxOutputBytes)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxPatchBytes, err = boundedEnvInt(get, "AGW_VERIFY_MAX_PATCH_BYTES", DefaultMaxPatchBytes, 1, HardMaxPatchBytes)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxCopyBytes, err = boundedEnvInt(get, "AGW_VERIFY_MAX_COPY_BYTES", DefaultMaxCopyBytes, 1, HardMaxCopyBytes)
	if err != nil {
		return Config{}, err
	}

	commandJSON := get("AGW_VERIFY_COMMANDS_JSON")
	envelope, err := decodeCommandEnvelope([]byte(commandJSON))
	if err != nil {
		return Config{}, err
	}
	cfg.Commands = envelope.Commands
	policyChecks, err := decodePolicyChecks([]byte(get("AGW_VERIFY_POLICY_CHECKS_JSON")))
	if err != nil {
		return Config{}, err
	}
	cfg.PolicyChecks = policyChecks
	if envelope.Requirements != nil {
		if err := applyRequirements(&cfg, *envelope.Requirements); err != nil {
			return Config{}, err
		}
	}
	if requirementsJSON := get("AGW_VERIFY_REQUIREMENTS_JSON"); requirementsJSON != "" {
		requirements, err := decodeRequirements([]byte(requirementsJSON))
		if err != nil {
			return Config{}, err
		}
		if envelope.Requirements != nil {
			return Config{}, fmt.Errorf("%w: requirements were supplied twice", ErrInvalidConfig)
		}
		if err := applyRequirements(&cfg, requirements); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func decodePolicyChecks(body []byte) ([]policycontract.GateCheckDescriptor, error) {
	if len(body) == 0 {
		return nil, nil
	}
	if len(body) > MaxPolicyChecksBytes {
		return nil, fmt.Errorf("%w: policy check contract exceeds %d bytes", ErrInvalidConfig, MaxPolicyChecksBytes)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) || len(body) == 0 || body[0] != '[' {
		return nil, fmt.Errorf("%w: policy checks must be canonical JSON array", ErrInvalidConfig)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var checks []policycontract.GateCheckDescriptor
	if err := decoder.Decode(&checks); err != nil {
		return nil, fmt.Errorf("%w: policy checks cannot be decoded", ErrInvalidConfig)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: policy checks contain trailing JSON", ErrInvalidConfig)
	}
	if err := policycontract.ValidateGateChecks(checks); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return checks, nil
}

func validateIdentity(cfg *Config) error {
	if cfg.RunUID == "" || len(cfg.RunUID) > gate.MaxRunUIDBytes || !utf8.ValidString(cfg.RunUID) || strings.ContainsAny(cfg.RunUID, "\x00\r\n\t ") {
		return fmt.Errorf("%w: run UID is invalid", ErrInvalidIdentity)
	}
	if !sha256Pattern.MatchString(cfg.SpecDigest) || !canonical.ValidDigest(cfg.SpecDigest) {
		return fmt.Errorf("%w: spec digest is invalid", ErrInvalidIdentity)
	}
	if !baseSHAPattern.MatchString(cfg.BaseSHA) {
		return fmt.Errorf("%w: base SHA is invalid", ErrInvalidIdentity)
	}
	for name, value := range map[string]string{"input": cfg.InputDigest, "patch": cfg.ExpectedPatchDigest} {
		if value != "" && (!sha256Pattern.MatchString(value) || !canonical.ValidDigest(value)) {
			return fmt.Errorf("%w: %s digest is invalid", ErrInvalidIdentity, name)
		}
	}
	return nil
}

func validatePaths(cfg *Config) error {
	if cfg.Workspace == "" || !filepath.IsAbs(cfg.Workspace) || cfg.Workspace == "/" || filepath.Clean(cfg.Workspace) != cfg.Workspace {
		return fmt.Errorf("%w: workspace must be an absolute canonical directory", ErrInvalidConfig)
	}
	for name, value := range map[string]string{"repo": cfg.RepoPath, "base": cfg.BasePath, "patch": cfg.PatchPath} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsRune(value, 0) || !pathWithin(cfg.Workspace, value) {
			return fmt.Errorf("%w: %s path is outside the workspace", ErrInvalidConfig, name)
		}
		if value == cfg.Workspace {
			return fmt.Errorf("%w: %s path cannot be the workspace root", ErrInvalidConfig, name)
		}
	}
	if cfg.RepoPath == cfg.BasePath || cfg.RepoPath == cfg.PatchPath || cfg.BasePath == cfg.PatchPath {
		return fmt.Errorf("%w: repository, base, and patch paths must be distinct", ErrInvalidConfig)
	}
	return nil
}

func pathWithin(root, value string) bool {
	rel, err := filepath.Rel(root, value)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func boundedEnvInt(get func(string) string, name string, fallback, minimum, maximum int64) (int64, error) {
	value := get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%w: %s must be in %d..%d", ErrInvalidConfig, name, minimum, maximum)
	}
	return parsed, nil
}

func decodeCommandEnvelope(body []byte) (commandEnvelope, error) {
	var output commandEnvelope
	if len(body) == 0 || strictjson.ValidateObject(body) != nil {
		return output, fmt.Errorf("%w: commands must be a strict JSON object", ErrInvalidConfig)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return commandEnvelope{}, fmt.Errorf("%w: commands cannot be decoded", ErrInvalidConfig)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return commandEnvelope{}, fmt.Errorf("%w: commands contain trailing JSON", ErrInvalidConfig)
	}
	if output.Version != CommandEnvelopeVersion || len(output.Commands) == 0 || len(output.Commands) > gate.MaxCommands {
		return commandEnvelope{}, fmt.Errorf("%w: unsupported command envelope", ErrInvalidConfig)
	}
	for index, command := range output.Commands {
		if err := validateCommand(command, index); err != nil {
			return commandEnvelope{}, err
		}
	}
	return output, nil
}

func decodeRequirements(body []byte) (requirementsEnvelope, error) {
	var output requirementsEnvelope
	if len(body) == 0 || strictjson.ValidateObject(body) != nil {
		return output, fmt.Errorf("%w: requirements must be a strict JSON object", ErrInvalidConfig)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return requirementsEnvelope{}, fmt.Errorf("%w: requirements cannot be decoded", ErrInvalidConfig)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return requirementsEnvelope{}, fmt.Errorf("%w: requirements contain trailing JSON", ErrInvalidConfig)
	}
	if output.Version != RequirementsEnvelopeVersion {
		return requirementsEnvelope{}, fmt.Errorf("%w: unsupported requirements envelope", ErrInvalidConfig)
	}
	return output, nil
}

func applyRequirements(cfg *Config, requirements requirementsEnvelope) error {
	if cfg == nil {
		return ErrInvalidConfig
	}
	if requirements.TestStrength != "" && requirements.TestStrength != v1alpha1.TestStrengthNone && requirements.TestStrength != v1alpha1.TestStrengthNewTestsFailOnBase {
		return fmt.Errorf("%w: unsupported test strength", ErrInvalidConfig)
	}
	if requirements.Adapter != "" && requirements.Adapter != v1alpha1.GateAdapterGo {
		return fmt.Errorf("%w: unsupported verifier adapter %q", ErrInvalidConfig, requirements.Adapter)
	}
	if requirements.CoverageDelta != "" && !validCoverageRequirement(requirements.CoverageDelta) {
		return fmt.Errorf("%w: coverage requirement is malformed", ErrInvalidConfig)
	}
	if requirements.TestStrength == v1alpha1.TestStrengthNewTestsFailOnBase {
		if requirements.Adapter != v1alpha1.GateAdapterGo || requirements.BaseTestCommand == nil {
			return fmt.Errorf("%w: newTestsMustFailOnBase requires the go adapter and one explicit baseTestCommand", ErrInvalidConfig)
		}
		if err := validateCommand(*requirements.BaseTestCommand, 0); err != nil {
			return err
		}
		if err := validateGoTestCommand(*requirements.BaseTestCommand); err != nil {
			return fmt.Errorf("%w: baseTestCommand: %v", ErrInvalidConfig, err)
		}
	} else if requirements.BaseTestCommand != nil {
		return fmt.Errorf("%w: baseTestCommand requires newTestsMustFailOnBase", ErrInvalidConfig)
	}
	if requirements.CoverageDelta != "" && requirements.Adapter != v1alpha1.GateAdapterGo {
		return fmt.Errorf("%w: coverageDelta requires the go verifier adapter", ErrInvalidConfig)
	}

	cfg.TestStrength = requirements.TestStrength
	if cfg.TestStrength == "" {
		cfg.TestStrength = v1alpha1.TestStrengthNone
	}
	cfg.Adapter = requirements.Adapter
	cfg.CoverageDelta = requirements.CoverageDelta
	if requirements.BaseTestCommand != nil {
		cfg.BaseTestCommand = &v1alpha1.VerifyCommand{Argv: append([]string(nil), requirements.BaseTestCommand.Argv...)}
		if requirements.BaseTestCommand.Shell != nil {
			value := *requirements.BaseTestCommand.Shell
			cfg.BaseTestCommand.Shell = &value
		}
	}
	return nil
}

func validateGoTestCommand(command v1alpha1.VerifyCommand) error {
	if len(command.Argv) < 2 || command.Argv[0] != "go" || command.Argv[1] != "test" || command.Shell != nil {
		return errors.New("must be an argv command beginning with go test")
	}
	return nil
}

func validateCommand(command v1alpha1.VerifyCommand, index int) error {
	hasArgv := len(command.Argv) > 0
	hasShell := command.Shell != nil && strings.TrimSpace(*command.Shell) != ""
	if hasArgv == hasShell {
		return fmt.Errorf("%w: command %d must select exactly one execution form", ErrInvalidConfig, index)
	}
	if len(command.Argv) > 64 {
		return fmt.Errorf("%w: command %d has too many argv entries", ErrInvalidConfig, index)
	}
	for argIndex, arg := range command.Argv {
		if arg == "" || len(arg) > MaxCommandBytes || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("%w: command %d argv[%d] is invalid", ErrInvalidConfig, index, argIndex)
		}
	}
	if hasShell && (len(*command.Shell) > MaxCommandBytes || !utf8.ValidString(*command.Shell) || strings.ContainsRune(*command.Shell, 0)) {
		return fmt.Errorf("%w: command %d shell is invalid", ErrInvalidConfig, index)
	}
	return nil
}

func validCoverageRequirement(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, operator := range []string{"<=", ">=", "==", "<", ">"} {
		if strings.HasPrefix(value, operator) {
			number := strings.TrimSpace(strings.TrimPrefix(value, operator))
			return decimalPattern.MatchString(number)
		}
	}
	return false
}

// EncodeEvidenceFrame emits exactly the stdout line consumed by
// verifycontroller. RawStdEncoding is intentional: padding is forbidden by
// the contract and surrounding logs are never allowed.
func EncodeEvidenceFrame(evidence MachineEvidence) ([]byte, error) {
	if err := validateEvidence(evidence); err != nil {
		return nil, err
	}
	body, err := json.Marshal(evidence)
	if err != nil || len(body) == 0 || len(body) > 1<<20 || strictjson.ValidateObject(body) != nil {
		return nil, fmt.Errorf("%w: evidence JSON is invalid", ErrInvalidConfig)
	}
	encoded := base64.RawStdEncoding.EncodeToString(body)
	frame := make([]byte, 0, len(EvidenceFramePrefix)+len(encoded)+1)
	frame = append(frame, EvidenceFramePrefix...)
	frame = append(frame, encoded...)
	frame = append(frame, '\n')
	return frame, nil
}

func validateEvidence(evidence MachineEvidence) error {
	if evidence.SchemaVersion != EvidenceSchemaVersion || !validIdentityFields(evidence) || evidence.ChangedPaths == nil || evidence.Commands == nil {
		return ErrInvalidIdentity
	}
	if len(evidence.ChangedPaths) > gate.MaxChangedPaths || len(evidence.Commands) > gate.MaxCommandEvidence {
		return fmt.Errorf("%w: evidence bounds exceeded", ErrInvalidConfig)
	}
	for index, path := range evidence.ChangedPaths {
		if !gate.ValidateRepoPath(path) || (index > 0 && evidence.ChangedPaths[index-1] >= path) {
			return fmt.Errorf("%w: changed paths are not sorted and unique", ErrInvalidConfig)
		}
	}
	if evidence.FilesChanged != nil && (*evidence.FilesChanged < 0 || *evidence.FilesChanged > gate.MaxObservedFiles || *evidence.FilesChanged != int64(len(evidence.ChangedPaths))) {
		return fmt.Errorf("%w: file count is inconsistent", ErrInvalidConfig)
	}
	if evidence.LinesChanged != nil && (*evidence.LinesChanged < 0 || *evidence.LinesChanged > gate.MaxObservedLines) {
		return fmt.Errorf("%w: line count is invalid", ErrInvalidConfig)
	}
	if evidence.CoverageDelta != nil && !decimalPattern.MatchString(*evidence.CoverageDelta) {
		return fmt.Errorf("%w: coverage delta is invalid", ErrInvalidConfig)
	}
	for index, command := range evidence.Commands {
		if command.Index != index {
			return fmt.Errorf("%w: command indexes are not deterministic", ErrInvalidConfig)
		}
		if command.ExitCode == nil {
			if command.EvidenceDigest != "" || command.DurationMillis != nil {
				return fmt.Errorf("%w: incomplete command evidence contains partial fields", ErrInvalidConfig)
			}
			continue
		}
		if *command.ExitCode < -1 || *command.ExitCode > 255 || !sha256Pattern.MatchString(command.EvidenceDigest) || command.DurationMillis == nil || *command.DurationMillis < 0 || *command.DurationMillis > int64(MaxDuration/time.Millisecond) {
			return fmt.Errorf("%w: command evidence is invalid", ErrInvalidConfig)
		}
	}
	return nil
}

func validIdentityFields(evidence MachineEvidence) bool {
	return evidence.RunUID != "" && utf8.ValidString(evidence.RunUID) && !strings.ContainsAny(evidence.RunUID, "\x00\r\n\t ") && sha256Pattern.MatchString(evidence.SpecDigest) && baseSHAPattern.MatchString(evidence.BaseSHA) && sha256Pattern.MatchString(evidence.PatchDigest)
}

func fallbackEvidence(cfg Config, patchDigest string, commandCount int) MachineEvidence {
	commands := make([]CommandEvidence, commandCount)
	for index := range commands {
		commands[index].Index = index
	}
	return MachineEvidence{
		SchemaVersion:      EvidenceSchemaVersion,
		RunUID:             cfg.RunUID,
		SpecDigest:         cfg.SpecDigest,
		BaseSHA:            cfg.BaseSHA,
		PatchDigest:        patchDigest,
		ChangedPaths:       []string{},
		HasBinaryFiles:     boolPtr(true),
		NewTestsFailOnBase: boolPtr(false),
		Commands:           commands,
	}
}

func boolPtr(value bool) *bool       { return &value }
func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }
