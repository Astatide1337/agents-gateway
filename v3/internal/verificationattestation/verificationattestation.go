// Package verificationattestation persists a cosign-authenticated envelope
// around an already authenticated Agents Gateway Gate report.
//
// The Gate report remains the authority for the verdict. This package adds a
// portable in-toto/cosign proof; it never evaluates checks or changes a Gate
// decision.
package verificationattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/cosignattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
)

const (
	BundleKind      = "verification-attestation-bundle"
	BundleName      = "verification-attestation.sigstore.json"
	BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	StatementKind   = "verification-attestation-statement"
	StatementName   = "verification-attestation.intoto.json"
)

var (
	ErrInvalidConfig = errors.New("verificationattestation: invalid configuration")
	ErrInvalidInput  = errors.New("verificationattestation: invalid input")
	ErrConflict      = errors.New("verificationattestation: immutable artifact conflict")
	ErrRetryable     = errors.New("verificationattestation: retryable reconciliation failure")
)

// Cosign is the exact cryptographic adapter surface used by the lifecycle.
// The production implementation is cosignattestation.Adapter; tests can use
// a deterministic implementation without weakening this boundary.
type Cosign interface {
	Attest(context.Context, cosignattestation.Request) (cosignattestation.AttestResult, error)
	Verify(context.Context, cosignattestation.Request) (cosignattestation.VerifyResult, error)
}

type Config struct {
	Cosign               Cosign
	Store                artifacts.Store
	TrustedGatePublicKey ed25519.PublicKey
	TempDir              string
	MaxBundleBytes       int64
}

type Lifecycle struct {
	cosign     Cosign
	store      artifacts.Store
	trustedKey ed25519.PublicKey
	tempDir    string
	maxBundle  int64
}

func New(config Config) (*Lifecycle, error) {
	if config.Cosign == nil || config.Store == nil || len(config.TrustedGatePublicKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidConfig
	}
	if config.MaxBundleBytes == 0 {
		config.MaxBundleBytes = cosignattestation.DefaultMaxBundleBytes
	}
	if config.MaxBundleBytes <= 0 || config.MaxBundleBytes > cosignattestation.DefaultMaxBundleBytes*2 {
		return nil, ErrInvalidConfig
	}
	if config.TempDir != "" {
		clean := filepath.Clean(config.TempDir)
		if !filepath.IsAbs(clean) || clean == string(filepath.Separator) || strings.ContainsRune(clean, 0) {
			return nil, ErrInvalidConfig
		}
		config.TempDir = clean
	}
	return &Lifecycle{
		cosign: config.Cosign, store: config.Store,
		trustedKey: append(ed25519.PublicKey(nil), config.TrustedGatePublicKey...),
		tempDir:    config.TempDir, maxBundle: config.MaxBundleBytes,
	}, nil
}

// Input contains the immutable identities needed to store the attestation.
// SignedReport must be the exact bytes referenced by ReportRef.
type Input struct {
	RunUID          string
	SpecDigest      string
	PatchDigest     string
	ReportRef       v1alpha1.ArtifactRef
	SignedReport    []byte
	EvidenceOptions evidenceattestation.Options
}

type Result struct {
	StatementRef v1alpha1.ArtifactRef
	BundleRef    v1alpha1.ArtifactRef
}

func (l *Lifecycle) Attest(ctx context.Context, input Input) (Result, error) {
	if l == nil || l.cosign == nil || l.store == nil || ctx == nil {
		return Result{}, ErrInvalidConfig
	}
	if err := validateInput(input); err != nil {
		return Result{}, err
	}
	reportDigest := digest(input.SignedReport)
	if reportDigest != input.ReportRef.Digest || input.ReportRef.SizeBytes != int64(len(input.SignedReport)) {
		return Result{}, fmt.Errorf("%w: signed report does not match report reference", ErrInvalidInput)
	}
	// The artifact key is derived from caller-supplied lifecycle identities, so
	// bind those identities to the authenticated Gate report before invoking
	// the external attestation adapter. Otherwise a valid report for run A
	// could be stored under the immutable path for run B.
	verifiedReport, err := evidenceattestation.VerifySignedReportBytes(input.SignedReport, l.trustedKey)
	if err != nil {
		return Result{}, fmt.Errorf("%w: Gate report is not trusted", ErrInvalidInput)
	}
	report := verifiedReport.Report()
	if report.RunUID != input.RunUID || report.SpecDigest != input.SpecDigest || report.PatchDigest != input.PatchDigest {
		return Result{}, fmt.Errorf("%w: lifecycle identities do not match Gate report", ErrInvalidInput)
	}
	directory, err := os.MkdirTemp(l.tempDir, "agw-verification-attestation-")
	if err != nil {
		return Result{}, fmt.Errorf("%w: create private working directory", ErrInvalidInput)
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0700); err != nil {
		return Result{}, fmt.Errorf("%w: secure private working directory", ErrInvalidInput)
	}
	bundlePath := filepath.Join(directory, "bundle.sigstore.json")
	statementPath := filepath.Join(directory, "statement.intoto.json")
	request := cosignattestation.Request{
		SignedReport:         append([]byte(nil), input.SignedReport...),
		TrustedGatePublicKey: append(ed25519.PublicKey(nil), l.trustedKey...),
		EvidenceOptions:      cloneOptions(input.EvidenceOptions),
		BundlePath:           bundlePath, StatementOutputPath: statementPath,
	}
	attested, err := l.cosign.Attest(ctx, request)
	if err != nil {
		return Result{}, fmt.Errorf("create cosign verification attestation: %w", err)
	}
	if attested.DryRun {
		return Result{}, fmt.Errorf("%w: dry-run attestation cannot enter lifecycle status", ErrInvalidInput)
	}
	statement, err := readRegular(statementPath, evidenceattestation.MaxStatementBytes)
	if err != nil || !bytes.Equal(statement, attested.Statement) || digest(statement) != attested.StatementDigest {
		return Result{}, fmt.Errorf("%w: statement output does not match authenticated result", ErrInvalidInput)
	}
	if err := evidenceattestation.VerifyStatement(statement, verifiedReport, input.EvidenceOptions); err != nil {
		return Result{}, fmt.Errorf("%w: statement is not bound to the Gate report", ErrInvalidInput)
	}
	bundle, err := readRegular(bundlePath, l.maxBundle)
	if err != nil {
		return Result{}, err
	}
	verified, err := l.cosign.Verify(ctx, request)
	if err != nil {
		return Result{}, fmt.Errorf("verify cosign verification attestation: %w", err)
	}
	if verified.DryRun || verified.StatementDigest != attested.StatementDigest || verified.SubjectDigest != input.PatchDigest || verified.PredicateType != evidenceattestation.PredicateType || verified.MediaType != evidenceattestation.MediaType {
		return Result{}, fmt.Errorf("%w: cosign verification result binding mismatch", ErrInvalidInput)
	}
	statementRef, err := l.persist(ctx, input, statement, StatementKind, StatementName, evidenceattestation.MediaType, ".intoto.json")
	if err != nil {
		return Result{}, err
	}
	bundleRef, err := l.persist(ctx, input, bundle, BundleKind, BundleName, BundleMediaType, ".sigstore.json")
	if err != nil {
		return Result{}, err
	}
	return Result{StatementRef: statementRef, BundleRef: bundleRef}, nil
}

func (l *Lifecycle) persist(ctx context.Context, input Input, body []byte, kind, name, mediaType, extension string) (v1alpha1.ArtifactRef, error) {
	d := digest(body)
	key := "runs/" + input.RunUID + "/verification/" + trim(input.SpecDigest) + "/" + trim(input.PatchDigest) + "/attestations/" + trim(d) + extension
	_, uri, err := l.store.Put(ctx, key, append([]byte(nil), body...), mediaType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("persist %s: %w", kind, err)
	}
	// A successful create is not sufficient evidence that the bytes now
	// addressed by the immutable key are the bytes we authenticated. Read the
	// object back for both fresh and replayed writes. This makes retries
	// idempotent and fails closed on a broken provider, a misbehaving adapter, or
	// a stale/incorrect URI response instead of projecting a false artifact ref.
	existing, getErr := l.store.Get(ctx, key)
	if getErr != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("persist %s: read-after-write failed: %w", kind, classifyStoreError(getErr))
	}
	if len(existing) == 0 {
		// A successful conditional write followed by a missing read is not a
		// byte conflict. It can be eventual visibility or an ambiguous provider
		// response, and the content-addressed key makes a bounded retry safe.
		return v1alpha1.ArtifactRef{}, fmt.Errorf("persist %s: object is not yet readable: %w", kind, ErrRetryable)
	}
	if !bytes.Equal(existing, body) {
		return v1alpha1.ArtifactRef{}, ErrConflict
	}
	if !safeURI(uri) {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: unsafe artifact URI", ErrInvalidInput)
	}
	return v1alpha1.ArtifactRef{URI: uri, Digest: d, Kind: kind, Name: name, MediaType: mediaType, SizeBytes: int64(len(body))}, nil
}

func validateInput(input Input) error {
	if !safeSegment(input.RunUID) || !canonical.ValidDigest(input.SpecDigest) || !canonical.ValidDigest(input.PatchDigest) || len(input.SignedReport) == 0 || len(input.SignedReport) > evidenceattestation.MaxStatementBytes {
		return ErrInvalidInput
	}
	if !canonical.ValidDigest(input.ReportRef.Digest) || input.ReportRef.Kind != "verification-report" || input.ReportRef.Name != "verification-report.json" || input.ReportRef.MediaType != "application/json" || input.ReportRef.SizeBytes <= 0 || !safeURI(input.ReportRef.URI) {
		return ErrInvalidInput
	}
	return nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: output is missing, non-regular, or out of bounds", ErrInvalidInput)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("%w: output is missing, non-regular, or out of bounds", ErrInvalidInput)
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) != info.Size() || int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: bounded output read failed", ErrInvalidInput)
	}
	if current, statErr := file.Stat(); statErr != nil || current.Size() != int64(len(body)) {
		return nil, fmt.Errorf("%w: output changed while being read", ErrInvalidInput)
	}
	return body, nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func trim(value string) string { return strings.TrimPrefix(value, canonical.DigestPrefix) }

func safeSegment(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func safeURI(value string) bool {
	if len(value) == 0 || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") || strings.ContainsAny(value, "@?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path == "" {
		return false
	}
	return parsed.Scheme == "s3" || parsed.Scheme == "https"
}

func classifyStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrRetryable, err)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) {
		return errors.Join(ErrRetryable, err)
	}
	return err
}

// IsRetryable identifies only bounded transport/deadline failures and the
// missing-after-write case. Unknown provider errors remain unclassified so a
// caller cannot turn an ambiguous result into an automatic retry policy.
func IsRetryable(err error) bool {
	if errors.Is(err, ErrRetryable) || errors.Is(err, context.DeadlineExceeded) || cosignattestation.IsRetryable(err) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary())
}

// IsPermanent identifies malformed input, immutable conflicts, and known
// cosign/configuration failures. An unknown provider failure is neither class
// and must remain fail-closed for operator inspection.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrInvalidInput) ||
		errors.Is(err, ErrConflict) || cosignattestation.IsPermanent(err)
}

func cloneOptions(input evidenceattestation.Options) evidenceattestation.Options {
	return evidenceattestation.Options{
		EvidenceArtifactDigests: append([]string(nil), input.EvidenceArtifactDigests...),
		CheckSeverities:         append([]evidenceattestation.CheckSeverityBinding(nil), input.CheckSeverities...),
	}
}
