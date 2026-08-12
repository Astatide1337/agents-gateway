// Command agw-attest is the opt-in CLI for the Agents Gateway ADR-027 cosign
// adapter. It reads an existing signed Gate report, authenticates it with an
// explicitly supplied Ed25519 public key, and delegates DSSE/bundle signing or
// verification to an external cosign v3 binary.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/cosignattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

const maxKeyFileBytes = 4096

type cliOptions struct {
	cosignPath       string
	keyRef           string
	verifyKeyRef     string
	signedReportPath string
	gatePublicKey    string
	bundlePath       string
	statementOutput  string
	predicateType    string
	mediaType        string
	timeout          time.Duration
	maxStdout        int64
	maxStderr        int64
	maxBundle        int64
	dryRun           bool
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Keep diagnostics generic. In particular, do not print cosign stderr,
		// key references, environment values, or command argv.
		_, _ = fmt.Fprintln(os.Stderr, "agw-attest: operation failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if ctx == nil || stdout == nil || stderr == nil {
		return errors.New("invalid command streams")
	}
	if len(args) == 0 {
		writeUsage(stderr)
		return errors.New("operation is required")
	}
	operation := args[0]
	if operation != "attest" && operation != "verify" {
		writeUsage(stderr)
		return errors.New("unsupported operation")
	}
	options, err := parseOptions(operation, args[1:], stderr)
	if err != nil {
		return err
	}
	signedReport, err := readBoundedRegularFile(options.signedReportPath, gate.MaxReportBytes+1024, "signed Gate report")
	if err != nil {
		return err
	}
	trustedKey, err := readTrustedPublicKey(options.gatePublicKey)
	if err != nil {
		return err
	}
	adapter, err := cosignattestation.New(cosignattestation.Config{
		CosignPath:     options.cosignPath,
		KeyRef:         options.keyRef,
		VerifyKeyRef:   options.verifyKeyRef,
		PredicateType:  options.predicateType,
		MediaType:      options.mediaType,
		Timeout:        options.timeout,
		MaxStdoutBytes: options.maxStdout,
		MaxStderrBytes: options.maxStderr,
		MaxBundleBytes: options.maxBundle,
		DryRun:         options.dryRun,
	})
	if err != nil {
		return err
	}
	request := cosignattestation.Request{
		SignedReport:         signedReport,
		TrustedGatePublicKey: trustedKey,
		BundlePath:           options.bundlePath,
		StatementOutputPath:  options.statementOutput,
	}
	if operation == "attest" {
		result, err := adapter.Attest(ctx, request)
		if err != nil {
			return err
		}
		if result.DryRun {
			_, _ = io.WriteString(stdout, "dry-run ")
			if err := cosignattestation.WriteRenderedPlan(stdout, result.Plan); err != nil {
				return err
			}
			return nil
		}
		_, err = fmt.Fprintf(stdout, "attested statement=%s subject=%s bundle=%s\n", result.StatementDigest, result.SubjectDigest, result.BundlePath)
		return err
	}
	result, err := adapter.Verify(ctx, request)
	if err != nil {
		return err
	}
	if result.DryRun {
		_, _ = io.WriteString(stdout, "dry-run ")
		if err := cosignattestation.WriteRenderedPlan(stdout, result.Plan); err != nil {
			return err
		}
		return nil
	}
	_, err = fmt.Fprintf(stdout, "verified statement=%s subject=%s bundle=%s\n", result.StatementDigest, result.SubjectDigest, result.BundlePath)
	return err
}

func parseOptions(operation string, args []string, stderr io.Writer) (cliOptions, error) {
	options := cliOptions{
		cosignPath:    "cosign",
		predicateType: evidenceattestation.PredicateType,
		mediaType:     evidenceattestation.MediaType,
		timeout:       cosignattestation.DefaultTimeout,
		maxStdout:     cosignattestation.DefaultMaxStdoutBytes,
		maxStderr:     cosignattestation.DefaultMaxStderrBytes,
		maxBundle:     cosignattestation.DefaultMaxBundleBytes,
	}
	flags := flag.NewFlagSet("agw-attest "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.cosignPath, "cosign", options.cosignPath, "pinned cosign v3 executable")
	flags.StringVar(&options.keyRef, "key-ref", "", "explicit cosign KMS/key reference")
	flags.StringVar(&options.verifyKeyRef, "verify-key-ref", "", "optional explicit cosign verification key reference; defaults to --key-ref")
	flags.StringVar(&options.signedReportPath, "signed-report", "", "canonical signed Gate report path")
	flags.StringVar(&options.gatePublicKey, "gate-public-key", "", "trusted raw/base64 Ed25519 Gate public key path")
	flags.StringVar(&options.bundlePath, "bundle", "", "cosign bundle path (output for attest, input for verify)")
	flags.StringVar(&options.statementOutput, "statement-output", "", "optional canonical statement output path (attest only)")
	flags.StringVar(&options.predicateType, "predicate-type", options.predicateType, "expected AGW predicate type")
	flags.StringVar(&options.mediaType, "media-type", options.mediaType, "expected AGW statement media type")
	flags.DurationVar(&options.timeout, "timeout", options.timeout, "cosign command timeout")
	flags.Int64Var(&options.maxStdout, "max-stdout-bytes", options.maxStdout, "maximum captured cosign stdout")
	flags.Int64Var(&options.maxStderr, "max-stderr-bytes", options.maxStderr, "maximum captured cosign stderr")
	flags.Int64Var(&options.maxBundle, "max-bundle-bytes", options.maxBundle, "maximum cosign bundle size")
	flags.BoolVar(&options.dryRun, "dry-run", false, "render a redacted argv without invoking cosign or writing files")
	if err := flags.Parse(args); err != nil {
		_, _ = fmt.Fprintln(stderr, "agw-attest: invalid flags")
		return cliOptions{}, err
	}
	if flags.NArg() != 0 {
		return cliOptions{}, errors.New("unexpected positional argument")
	}
	if options.signedReportPath == "" || options.gatePublicKey == "" || options.bundlePath == "" || options.keyRef == "" {
		return cliOptions{}, errors.New("--signed-report, --gate-public-key, --bundle, and --key-ref are required")
	}
	if operation == "verify" && options.statementOutput != "" {
		return cliOptions{}, errors.New("--statement-output is only valid for attest")
	}
	return options, nil
}

func readTrustedPublicKey(path string) (ed25519.PublicKey, error) {
	body, err := readBoundedRegularFile(path, maxKeyFileBytes, "Gate public key")
	if err != nil {
		return nil, err
	}
	if len(body) == ed25519.PublicKeySize {
		return ed25519.PublicKey(append([]byte(nil), body...)), nil
	}
	text := strings.TrimSpace(string(body))
	if text == "" || strings.ContainsAny(text, "\r\n \t") {
		return nil, errors.New("Gate public key must be raw 32-byte or compact base64")
	}
	for _, decoder := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding} {
		decoded, decodeErr := decoder.DecodeString(text)
		if decodeErr == nil && len(decoded) == ed25519.PublicKeySize {
			return ed25519.PublicKey(append([]byte(nil), decoded...)), nil
		}
	}
	// Hex is accepted only as exactly 64 hexadecimal characters. It is useful
	// for operators copying a fingerprint-derived key, while still rejecting
	// PEM and arbitrary textual key formats.
	if len(text) == ed25519.PublicKeySize*2 {
		decoded, decodeErr := hex.DecodeString(text)
		if decodeErr == nil && len(decoded) == ed25519.PublicKeySize {
			return ed25519.PublicKey(append([]byte(nil), decoded...)), nil
		}
	}
	return nil, errors.New("Gate public key must be raw 32-byte, compact base64, or 64-character hex")
}

func readBoundedRegularFile(path string, maxBytes int64, description string) ([]byte, error) {
	if path == "" || path == "-" {
		return nil, fmt.Errorf("%s path is required", description)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%s must be a regular file", description)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("%s size is out of bounds", description)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes || int64(len(body)) != info.Size() {
		return nil, fmt.Errorf("%s read exceeded bounds", description)
	}
	if current, statErr := file.Stat(); statErr != nil || current.Size() != int64(len(body)) {
		return nil, fmt.Errorf("%s changed while being read", description)
	}
	return body, nil
}

func writeUsage(w io.Writer) {
	_, _ = io.WriteString(w, "usage: agw-attest <attest|verify> --signed-report FILE --gate-public-key FILE --bundle FILE --key-ref KMS_OR_KEY [--verify-key-ref PUBLIC_KEY_OR_KMS] [flags]\n")
}
