// Package cosignattestation is the opt-in boundary between the Agents Gateway
// Gate evidence contract and the external cosign CLI.
//
// This package deliberately does not implement DSSE, Sigstore bundles, KMS
// clients, or signature verification. It authenticates the existing Gate
// report with the trusted Ed25519 key already owned by Agents Gateway, builds
// the canonical in-toto statement through evidenceattestation, and invokes a
// pinned external cosign binary with a fixed argument shape.
package cosignattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

const (
	// DefaultTimeout is deliberately finite. A cosign command must never wait
	// forever on KMS, a registry, or an ambient credential provider.
	DefaultTimeout = 3 * time.Minute

	DefaultMaxStdoutBytes = 64 << 10
	DefaultMaxStderrBytes = 256 << 10
	DefaultMaxBundleBytes = 16 << 20

	maxOutputLimit = 1 << 20
	maxBundleLimit = 32 << 20
	maxPathBytes   = 4096
	maxKeyRefBytes = 2048
	maxCosignBytes = 1024

	sha256Algorithm = "sha256"

	// These are the fixed bundle values emitted by the pinned cosign v3
	// contract. The adapter checks the envelope shape, while cosign remains the
	// authority for the cryptographic signature and claim verification.
	sigstoreBundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	dssePayloadType         = evidenceattestation.DSSEPayloadType

	// temporaryStatementArg is only used by dry-run rendering. It is never
	// handed to a real process.
	temporaryStatementArg = "<temporary-statement.json>"
)

var (
	ErrInvalidConfig    = errors.New("cosignattestation: invalid configuration")
	ErrInvalidInput     = errors.New("cosignattestation: invalid input")
	ErrCommandFailed    = errors.New("cosignattestation: cosign command failed")
	ErrOutputLimit      = errors.New("cosignattestation: cosign output exceeded bounds")
	ErrUnexpectedOutput = errors.New("cosignattestation: unexpected cosign output")
	ErrBundleInvalid    = errors.New("cosignattestation: invalid cosign bundle")
	ErrDryRun           = errors.New("cosignattestation: dry-run plan")
	ErrRetryable        = errors.New("cosignattestation: retryable operation failure")
)

// Config defines the explicit external boundary. No field is populated from
// an environment variable by this package. In particular, KeyRef is required
// and keyless/Fulcio operation is never selected implicitly.
type Config struct {
	// CosignPath is the path or executable name of the operator-selected cosign
	// v3 binary. Pinning the binary is a deployment concern; the adapter never
	// downloads or replaces it.
	CosignPath string
	// KeyRef is an external cosign key reference, normally a KMS URI or a path
	// managed outside this process. Private key bytes are never accepted here.
	KeyRef string
	// VerifyKeyRef is the public verification key or KMS URI. It defaults to
	// KeyRef because KMS plugins commonly expose signing and verification
	// through one URI. File-backed deployments should set it to the public-key
	// path so the private key is never passed to a verification command.
	VerifyKeyRef string

	// PredicateType and MediaType must be the exact AGW values. They are
	// explicit so a caller cannot accidentally attest a different predicate or
	// silently change the unsigned statement media contract.
	PredicateType string
	MediaType     string

	Timeout        time.Duration
	MaxStdoutBytes int64
	MaxStderrBytes int64
	MaxBundleBytes int64

	// TempDir is optional. When set, it is used only for the private temporary
	// statement directory. Bundle output is created in its destination
	// directory so the final rename is atomic.
	TempDir string

	// Runner is a test seam. A nil Runner uses ExecRunner, which invokes
	// exec.CommandContext directly and never starts a shell.
	Runner Runner

	// DryRun builds and returns a redacted plan without creating files or
	// invoking cosign.
	DryRun bool
}

// normalized validates and fills only bounded, non-secret defaults.
func (c Config) normalized() (Config, error) {
	if c.CosignPath == "" || len(c.CosignPath) > maxCosignBytes || strings.IndexByte(c.CosignPath, 0) >= 0 || strings.ContainsAny(c.CosignPath, "\r\n") {
		return Config{}, fmt.Errorf("%w: cosign path is required", ErrInvalidConfig)
	}
	if err := validateKeyRef(c.KeyRef); err != nil {
		return Config{}, err
	}
	if c.VerifyKeyRef == "" {
		// A local signing-key path is normally a private key. Never pass it to
		// the verification command by default; file-backed deployments must
		// explicitly provide the separate public verification key. URI-backed
		// KMS providers may intentionally resolve signing and verification from
		// the same provider reference.
		if filepath.IsAbs(c.KeyRef) {
			return Config{}, fmt.Errorf("%w: file-backed signing keys require an explicit verification key", ErrInvalidConfig)
		}
		c.VerifyKeyRef = c.KeyRef
	}
	if err := validateKeyRef(c.VerifyKeyRef); err != nil {
		return Config{}, err
	}
	if filepath.IsAbs(c.KeyRef) && filepath.IsAbs(c.VerifyKeyRef) && filepath.Clean(c.KeyRef) == filepath.Clean(c.VerifyKeyRef) {
		return Config{}, fmt.Errorf("%w: file-backed signing and verification references must differ", ErrInvalidConfig)
	}
	if c.PredicateType != evidenceattestation.PredicateType {
		return Config{}, fmt.Errorf("%w: predicate type must be %q", ErrInvalidConfig, evidenceattestation.PredicateType)
	}
	if c.MediaType != evidenceattestation.MediaType {
		return Config{}, fmt.Errorf("%w: media type must be %q", ErrInvalidConfig, evidenceattestation.MediaType)
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Timeout < 0 || c.Timeout > time.Hour {
		return Config{}, fmt.Errorf("%w: timeout is out of bounds", ErrInvalidConfig)
	}
	if c.MaxStdoutBytes == 0 {
		c.MaxStdoutBytes = DefaultMaxStdoutBytes
	}
	if c.MaxStderrBytes == 0 {
		c.MaxStderrBytes = DefaultMaxStderrBytes
	}
	if c.MaxBundleBytes == 0 {
		c.MaxBundleBytes = DefaultMaxBundleBytes
	}
	if c.MaxStdoutBytes < 0 || c.MaxStdoutBytes > maxOutputLimit || c.MaxStderrBytes < 0 || c.MaxStderrBytes > maxOutputLimit {
		return Config{}, fmt.Errorf("%w: command output limits are out of bounds", ErrInvalidConfig)
	}
	if c.MaxBundleBytes < 1 || c.MaxBundleBytes > maxBundleLimit {
		return Config{}, fmt.Errorf("%w: bundle limit is out of bounds", ErrInvalidConfig)
	}
	if c.TempDir != "" {
		if len(c.TempDir) > maxPathBytes || strings.IndexByte(c.TempDir, 0) >= 0 || strings.ContainsAny(c.TempDir, "\r\n") {
			return Config{}, fmt.Errorf("%w: temporary directory path is invalid", ErrInvalidConfig)
		}
	}
	return c, nil
}

func validateKeyRef(value string) error {
	if value == "" || len(value) > maxKeyRefBytes || strings.IndexByte(value, 0) >= 0 || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: explicit cosign key reference is required", ErrInvalidConfig)
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%w: cosign key reference contains whitespace", ErrInvalidConfig)
		}
	}
	lower := strings.ToLower(value)
	// env:// would make the actual signing key an implicit environment secret.
	// HTTP(S) key material would make trust depend on an unpinned remote fetch.
	// Both are intentionally outside this adapter's trust boundary.
	forbiddenPrefixes := []string{"env://", "http://", "https://"}
	for _, prefix := range forbiddenPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("%w: key reference scheme %q is not allowed", ErrInvalidConfig, prefix[:len(prefix)-3])
		}
	}
	if value == "-" || strings.HasPrefix(value, "--") {
		return fmt.Errorf("%w: key reference is ambiguous", ErrInvalidConfig)
	}
	// A file-backed trust root must be anchored to an absolute path. Relative
	// paths depend on the process working directory, which is not a stable
	// production identity and can change across restarts or container images.
	// Provider/KMS references remain URI-shaped and are resolved by cosign's
	// explicitly selected provider.
	if !strings.Contains(value, "://") && !filepath.IsAbs(value) {
		return fmt.Errorf("%w: file-backed key reference must be absolute", ErrInvalidConfig)
	}
	return nil
}

func validatePath(path string, name string) error {
	if path == "" || path == "-" || len(path) > maxPathBytes || strings.IndexByte(path, 0) >= 0 || strings.ContainsAny(path, "\r\n") {
		return fmt.Errorf("%w: %s path is invalid", ErrInvalidInput, name)
	}
	return nil
}

// Runner executes one already-constructed argv. Implementations must not
// interpret args as shell source.
type Runner interface {
	Run(ctx context.Context, executable string, args []string, stdoutLimit, stderrLimit int64) CommandResult
}

// CommandResult is intentionally small. Stdout and stderr are bounded by the
// Runner and checked again by the adapter. Their contents are never included
// in adapter errors because cosign diagnostics can contain sensitive context.
type CommandResult struct {
	Stdout []byte
	Stderr []byte

	ExitCode int
	Err      error

	StdoutTruncated bool
	StderrTruncated bool
}

// ExecRunner invokes an external executable directly through os/exec. It does
// not inherit stdin and does not invoke a shell.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args []string, stdoutLimit, stderrLimit int64) CommandResult {
	command := exec.CommandContext(ctx, executable, args...)
	stdout := &boundedBuffer{limit: stdoutLimit}
	stderr := &boundedBuffer{limit: stderrLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	// A signing command must never pause for an interactive prompt. A nil
	// Stdin makes os/exec connect the child to the null device.
	command.Stdin = nil
	err := command.Run()
	exitCode := -1
	if err == nil {
		exitCode = 0
	} else {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	return CommandResult{
		Stdout:          append([]byte(nil), stdout.Bytes()...),
		Stderr:          append([]byte(nil), stderr.Bytes()...),
		ExitCode:        exitCode,
		Err:             err,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}
}

type boundedBuffer struct {
	data      bytes.Buffer
	limit     int64
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit < 0 {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - int64(b.data.Len())
	if remaining > 0 {
		count := len(p)
		if int64(count) > remaining {
			count = int(remaining)
		}
		_, _ = b.data.Write(p[:count])
		if count != len(p) {
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	// Always drain the child pipe. Returning an error here could leave cosign
	// blocked on a full pipe before its context deadline fires.
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.data.Bytes() }

// Plan is the exact argv plan. Args contains the real key reference for the
// caller that needs to execute it; RedactedArgs is safe for human rendering.
type Plan struct {
	Executable   string
	Args         []string
	RedactedArgs []string
}

func (p Plan) clone() Plan {
	return Plan{
		Executable:   p.Executable,
		Args:         append([]string(nil), p.Args...),
		RedactedArgs: append([]string(nil), p.RedactedArgs...),
	}
}

// Argv returns a defensive copy containing the executable followed by the
// actual arguments. Callers must not log this value when KeyRef is sensitive.
func (p Plan) Argv() []string {
	argv := make([]string, 0, len(p.Args)+1)
	argv = append(argv, p.Executable)
	argv = append(argv, p.Args...)
	return argv
}

// RedactedArgv returns a defensive copy suitable for logs or dry-run output.
func (p Plan) RedactedArgv() []string {
	argv := make([]string, 0, len(p.RedactedArgs)+1)
	argv = append(argv, p.Executable)
	argv = append(argv, p.RedactedArgs...)
	return argv
}

// Render writes a shell-like, quoted display of the redacted argv. It is only
// a renderer; it is never fed back to a shell.
func (p Plan) Render(w io.Writer) error {
	if w == nil {
		return fmt.Errorf("%w: nil render writer", ErrInvalidInput)
	}
	for index, value := range p.RedactedArgv() {
		if index > 0 {
			if _, err := io.WriteString(w, " "); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, strconv.Quote(value)); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// Request is the common input for Attest and Verify. The signed report is the
// existing canonical Gate envelope, not a cosign bundle.
type Request struct {
	SignedReport         []byte
	TrustedGatePublicKey ed25519.PublicKey
	EvidenceOptions      evidenceattestation.Options
	BundlePath           string
	StatementOutputPath  string
}

// AttestResult describes a successful local bundle creation. The bundle is
// created only after the Gate report and statement have been independently
// validated.
type AttestResult struct {
	Statement       []byte
	StatementDigest string
	SubjectDigest   string
	PredicateType   string
	MediaType       string
	BundlePath      string
	Plan            Plan
	DryRun          bool
}

// VerifyResult describes a successful cosign verification command. The
// statement fields are reconstructed from the independently trusted Gate
// report; cosign additionally checks the bundle's DSSE signature, subject
// digest, and predicate type.
type VerifyResult struct {
	StatementDigest string
	SubjectDigest   string
	PredicateType   string
	MediaType       string
	BundlePath      string
	Plan            Plan
	DryRun          bool
}

// Adapter is an opt-in cosign integration. Construct it with New.
type Adapter struct {
	config Config
	runner Runner
}

// New validates the explicit configuration. It performs no external calls.
func New(config Config) (*Adapter, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	runner := config.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Adapter{config: config, runner: runner}, nil
}

// Config returns a copy of the validated configuration. It never exposes
// mutable internal slices or private key material (none is stored here).
func (a *Adapter) Config() Config {
	if a == nil {
		return Config{}
	}
	return a.config
}

// PlanAttest builds the exact argv for a complete statement. It does not read
// or write either path.
func (a *Adapter) PlanAttest(statementPath, bundlePath string) (Plan, error) {
	if a == nil {
		return Plan{}, fmt.Errorf("%w: nil adapter", ErrInvalidConfig)
	}
	if err := validatePath(statementPath, "statement"); err != nil {
		return Plan{}, err
	}
	if err := validatePath(bundlePath, "bundle"); err != nil {
		return Plan{}, err
	}
	if filepath.Clean(statementPath) == filepath.Clean(bundlePath) {
		return Plan{}, fmt.Errorf("%w: statement and bundle paths must differ", ErrInvalidInput)
	}
	args := []string{
		"attest-blob",
		"--statement", statementPath,
		"--bundle", bundlePath,
		"--key", a.config.KeyRef,
		"--type", a.config.PredicateType,
		"--new-bundle-format=true",
		"--use-signing-config=false",
		"--tlog-upload=false",
		"--yes",
	}
	redacted := append([]string(nil), args...)
	for index := range redacted {
		if index > 0 && args[index-1] == "--key" {
			redacted[index] = "<key-ref-redacted>"
		}
	}
	return Plan{Executable: a.config.CosignPath, Args: args, RedactedArgs: redacted}, nil
}

// PlanVerify builds the exact argv for verification against a subject digest.
// subjectDigest must be the report's canonical sha256:... value.
func (a *Adapter) PlanVerify(bundlePath, subjectDigest string) (Plan, error) {
	if a == nil {
		return Plan{}, fmt.Errorf("%w: nil adapter", ErrInvalidConfig)
	}
	if err := validatePath(bundlePath, "bundle"); err != nil {
		return Plan{}, err
	}
	hexDigest, err := subjectHex(subjectDigest)
	if err != nil {
		return Plan{}, err
	}
	args := []string{
		"verify-blob-attestation",
		"--bundle", bundlePath,
		"--key", a.config.VerifyKeyRef,
		"--type", a.config.PredicateType,
		"--digest", hexDigest,
		"--digestAlg", sha256Algorithm,
		"--check-claims=true",
		"--new-bundle-format=true",
		"--insecure-ignore-tlog",
	}
	redacted := append([]string(nil), args...)
	for index := range redacted {
		if index > 0 && args[index-1] == "--key" {
			redacted[index] = "<key-ref-redacted>"
		}
	}
	return Plan{Executable: a.config.CosignPath, Args: args, RedactedArgs: redacted}, nil
}

// Attest verifies the existing Gate report, creates the canonical statement,
// invokes cosign attest-blob, validates the bounded local bundle, and commits
// it to the requested output path with an atomic rename. No registry upload is
// performed by the adapter.
func (a *Adapter) Attest(ctx context.Context, request Request) (AttestResult, error) {
	if a == nil {
		return AttestResult{}, fmt.Errorf("%w: nil adapter", ErrInvalidConfig)
	}
	if err := validContext(ctx); err != nil {
		return AttestResult{}, err
	}
	if err := validateRequestPaths(request, true); err != nil {
		return AttestResult{}, err
	}
	verified, statementBytes, statementDigest, subjectDigest, err := a.prepare(request)
	if err != nil {
		return AttestResult{}, err
	}
	_ = verified
	if a.config.DryRun {
		plan, err := a.PlanAttest(temporaryStatementArg, request.BundlePath)
		if err != nil {
			return AttestResult{}, err
		}
		return AttestResult{
			Statement:       append([]byte(nil), statementBytes...),
			StatementDigest: statementDigest,
			SubjectDigest:   subjectDigest,
			PredicateType:   a.config.PredicateType,
			MediaType:       a.config.MediaType,
			BundlePath:      filepath.Clean(request.BundlePath),
			Plan:            plan.clone(),
			DryRun:          true,
		}, nil
	}

	temporaryDir, err := os.MkdirTemp(a.config.TempDir, "agw-attest-")
	if err != nil {
		return AttestResult{}, fmt.Errorf("%w: create private temporary directory", ErrInvalidInput)
	}
	defer os.RemoveAll(temporaryDir)
	statementPath := filepath.Join(temporaryDir, "statement.json")
	if err := writePrivateFile(statementPath, statementBytes); err != nil {
		return AttestResult{}, err
	}
	bundleTemp, cleanupBundle, err := secureOutputTemp(request.BundlePath)
	if err != nil {
		return AttestResult{}, err
	}
	defer cleanupBundle()
	plan, err := a.PlanAttest(statementPath, bundleTemp)
	if err != nil {
		return AttestResult{}, err
	}
	if err := a.run(ctx, plan); err != nil {
		return AttestResult{}, err
	}
	if _, err := validateBundleFile(bundleTemp, a.config.MaxBundleBytes); err != nil {
		return AttestResult{}, err
	}
	if err := os.Rename(bundleTemp, request.BundlePath); err != nil {
		return AttestResult{}, fmt.Errorf("%w: commit bundle output", ErrBundleInvalid)
	}
	if request.StatementOutputPath != "" {
		if filepath.Clean(request.StatementOutputPath) == filepath.Clean(request.BundlePath) {
			return AttestResult{}, fmt.Errorf("%w: statement output and bundle output must differ", ErrInvalidInput)
		}
		if err := writeAtomicPrivateFile(request.StatementOutputPath, statementBytes); err != nil {
			return AttestResult{}, err
		}
	}
	return AttestResult{
		Statement:       append([]byte(nil), statementBytes...),
		StatementDigest: statementDigest,
		SubjectDigest:   subjectDigest,
		PredicateType:   a.config.PredicateType,
		MediaType:       a.config.MediaType,
		BundlePath:      filepath.Clean(request.BundlePath),
		Plan:            plan.clone(),
	}, nil
}

// Verify reconstructs the expected statement from the trusted Gate report,
// bounds the input bundle, then invokes cosign verify-blob-attestation with
// exact subject/predicate bindings. The command's successful exit status is
// the cryptographic result; this package does not reimplement DSSE.
func (a *Adapter) Verify(ctx context.Context, request Request) (VerifyResult, error) {
	if a == nil {
		return VerifyResult{}, fmt.Errorf("%w: nil adapter", ErrInvalidConfig)
	}
	if err := validContext(ctx); err != nil {
		return VerifyResult{}, err
	}
	if err := validateRequestPaths(request, false); err != nil {
		return VerifyResult{}, err
	}
	_, statementBytes, statementDigest, subjectDigest, err := a.prepare(request)
	if err != nil {
		return VerifyResult{}, err
	}
	_ = statementBytes
	plan, err := a.PlanVerify(request.BundlePath, subjectDigest)
	if err != nil {
		return VerifyResult{}, err
	}
	if a.config.DryRun {
		return VerifyResult{
			StatementDigest: statementDigest,
			SubjectDigest:   subjectDigest,
			PredicateType:   a.config.PredicateType,
			MediaType:       a.config.MediaType,
			BundlePath:      filepath.Clean(request.BundlePath),
			Plan:            plan.clone(),
			DryRun:          true,
		}, nil
	}
	if _, err := validateBundleFile(request.BundlePath, a.config.MaxBundleBytes); err != nil {
		return VerifyResult{}, err
	}
	if err := a.run(ctx, plan); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		StatementDigest: statementDigest,
		SubjectDigest:   subjectDigest,
		PredicateType:   a.config.PredicateType,
		MediaType:       a.config.MediaType,
		BundlePath:      filepath.Clean(request.BundlePath),
		Plan:            plan.clone(),
	}, nil
}

func validContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	return nil
}

func validateRequestPaths(request Request, attest bool) error {
	if len(request.SignedReport) == 0 || len(request.SignedReport) > gate.MaxReportBytes+1024 {
		return fmt.Errorf("%w: signed Gate report is out of bounds", ErrInvalidInput)
	}
	if err := validatePath(request.BundlePath, "bundle"); err != nil {
		return err
	}
	if attest && request.StatementOutputPath != "" {
		if err := validatePath(request.StatementOutputPath, "statement output"); err != nil {
			return err
		}
		if filepath.Clean(request.StatementOutputPath) == filepath.Clean(request.BundlePath) {
			return fmt.Errorf("%w: statement output and bundle output must differ", ErrInvalidInput)
		}
	}
	return nil
}

func (a *Adapter) prepare(request Request) (evidenceattestation.VerifiedReport, []byte, string, string, error) {
	verified, err := evidenceattestation.VerifySignedReportBytes(request.SignedReport, request.TrustedGatePublicKey)
	if err != nil {
		return evidenceattestation.VerifiedReport{}, nil, "", "", err
	}
	statement, err := evidenceattestation.StatementFromVerifiedReport(verified, request.EvidenceOptions)
	if err != nil {
		return evidenceattestation.VerifiedReport{}, nil, "", "", err
	}
	statementBytes, err := evidenceattestation.StatementBytes(statement)
	if err != nil {
		return evidenceattestation.VerifiedReport{}, nil, "", "", err
	}
	// Keep the media-type check explicit at this boundary even though the
	// statement constructor uses the same fixed predicate contract.
	if err := evidenceattestation.VerifyArtifact(a.config.MediaType, statementBytes, verified, request.EvidenceOptions); err != nil {
		return evidenceattestation.VerifiedReport{}, nil, "", "", err
	}
	statementDigest, err := evidenceattestation.StatementDigest(statement)
	if err != nil {
		return evidenceattestation.VerifiedReport{}, nil, "", "", err
	}
	return verified, statementBytes, statementDigest, verified.Report().PatchDigest, nil
}

func subjectHex(subjectDigest string) (string, error) {
	if len(subjectDigest) != len("sha256:")+64 || !strings.HasPrefix(subjectDigest, "sha256:") {
		return "", fmt.Errorf("%w: subject digest must be a canonical sha256 digest", ErrInvalidInput)
	}
	hexDigest := strings.TrimPrefix(subjectDigest, "sha256:")
	for _, r := range hexDigestRunes(hexDigest) {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return "", fmt.Errorf("%w: subject digest is not lowercase hexadecimal", ErrInvalidInput)
		}
	}
	return hexDigest, nil
}

func hexDigestRunes(value string) string { return value }

func (a *Adapter) run(ctx context.Context, plan Plan) error {
	commandCtx := ctx
	if a.config.Timeout > 0 {
		var cancel context.CancelFunc
		commandCtx, cancel = context.WithTimeout(ctx, a.config.Timeout)
		defer cancel()
	}
	result := a.runner.Run(commandCtx, plan.Executable, plan.Args, a.config.MaxStdoutBytes, a.config.MaxStderrBytes)
	if result.StdoutTruncated || result.StderrTruncated || int64(len(result.Stdout)) > a.config.MaxStdoutBytes || int64(len(result.Stderr)) > a.config.MaxStderrBytes {
		return ErrOutputLimit
	}
	if result.Err != nil {
		if errors.Is(result.Err, context.DeadlineExceeded) || errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return errors.Join(ErrCommandFailed, ErrRetryable, context.DeadlineExceeded)
		}
		if errors.Is(result.Err, context.Canceled) || errors.Is(commandCtx.Err(), context.Canceled) {
			return errors.Join(ErrCommandFailed, context.Canceled)
		}
		return ErrCommandFailed
	}
	if result.ExitCode < 0 || result.ExitCode > 255 {
		return fmt.Errorf("%w: cosign exit status is unknown", ErrCommandFailed)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%w: cosign exited with status %d", ErrCommandFailed, result.ExitCode)
	}
	// Cosign v3 writes the generated DSSE envelope and a bounded informational
	// line to stdout even when --bundle names an output file. Stdout is captured
	// and never logged or trusted. The bounded bundle file plus the subsequent
	// verify-blob-attestation command are the authoritative outputs.
	return nil
}

func writePrivateFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("%w: create private statement file", ErrInvalidInput)
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return fmt.Errorf("%w: write private statement file", ErrInvalidInput)
	}
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("%w: secure private statement file", ErrInvalidInput)
	}
	return nil
}

func writeAtomicPrivateFile(destination string, body []byte) error {
	if err := validatePath(destination, "statement output"); err != nil {
		return err
	}
	destination = filepath.Clean(destination)
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".agw-statement-*")
	if err != nil {
		return fmt.Errorf("%w: create statement output", ErrInvalidInput)
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryPath) }
	defer cleanup()
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("%w: secure statement output", ErrInvalidInput)
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("%w: write statement output", ErrInvalidInput)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("%w: flush statement output", ErrInvalidInput)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("%w: close statement output", ErrInvalidInput)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("%w: commit statement output", ErrInvalidInput)
	}
	return nil
}

func secureOutputTemp(destination string) (string, func(), error) {
	if err := validatePath(destination, "bundle"); err != nil {
		return "", func() {}, err
	}
	destination = filepath.Clean(destination)
	directory := filepath.Dir(destination)
	base := filepath.Base(destination)
	temporary, err := os.CreateTemp(directory, "."+base+".agw-cosign-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("%w: create secure bundle output", ErrBundleInvalid)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", func() {}, fmt.Errorf("%w: secure bundle output", ErrBundleInvalid)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", func() {}, fmt.Errorf("%w: prepare bundle output", ErrBundleInvalid)
	}
	cleanup := func() { _ = os.Remove(temporaryPath) }
	return temporaryPath, cleanup, nil
}

func validateBundleFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open bundle", ErrBundleInvalid)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: bundle is not a regular file", ErrBundleInvalid)
	}
	if info.Size() <= 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("%w: bundle size is out of bounds", ErrBundleInvalid)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes || int64(len(body)) != info.Size() {
		return nil, fmt.Errorf("%w: bundle read exceeded bounds", ErrBundleInvalid)
	}
	if current, statErr := file.Stat(); statErr != nil || current.Size() != int64(len(body)) {
		return nil, fmt.Errorf("%w: bundle changed while being read", ErrBundleInvalid)
	}
	if !validBundleShape(body) {
		return nil, ErrBundleInvalid
	}
	return body, nil
}

type bundleShape struct {
	MediaType            string          `json:"mediaType"`
	VerificationMaterial json.RawMessage `json:"verificationMaterial"`
	DSSEEnvelope         struct {
		PayloadType string            `json:"payloadType"`
		Payload     string            `json:"payload"`
		Signatures  []json.RawMessage `json:"signatures"`
	} `json:"dsseEnvelope"`
}

func validBundleShape(body []byte) bool {
	if !json.Valid(body) || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return false
	}
	var bundle bundleShape
	if err := json.Unmarshal(body, &bundle); err != nil {
		return false
	}
	return bundle.MediaType == sigstoreBundleMediaType &&
		len(bundle.VerificationMaterial) > 0 &&
		bundle.DSSEEnvelope.PayloadType == dssePayloadType &&
		bundle.DSSEEnvelope.Payload != "" &&
		len(bundle.DSSEEnvelope.Signatures) > 0
}

// IsRetryable reports only failures that are safe to retry because the
// command deadline or caller-provided context prevented a determinate result.
// Non-zero cosign exits remain unclassified: they may be a bad key, a bad
// statement, or a transient provider response and must not be retried blindly.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

// IsPermanent reports failures that a retry of the same immutable input cannot
// repair. Unknown provider failures deliberately return false so callers can
// apply their normal fail-closed/inspection path.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrInvalidInput) ||
		errors.Is(err, ErrOutputLimit) || errors.Is(err, ErrBundleInvalid)
}

// WriteRenderedPlan renders a redacted plan without exposing KeyRef.
func WriteRenderedPlan(w io.Writer, plan Plan) error { return plan.Render(w) }
