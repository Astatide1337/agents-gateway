// Package contextartifact turns a verified ContextPack volume into a small,
// deterministic evidence artifact. The artifact contains identities and file
// descriptors only; it never copies task text, skills, policy bodies,
// credentials, model requests, or model transcripts.
package contextartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	SchemaVersion = "agents.astatide.com/context-artifact/v1"
	MediaType     = "application/vnd.agw.context-pack+json"
	Kind          = "context-pack"
	Name          = "context-pack.json"
	ManifestPath  = ".agw/context/manifest.json"

	MaxBundleBytes   = 512 << 10
	MaxManifestBytes = 8 << 20
	MaxFiles         = 4096
)

var (
	ErrInvalid  = errors.New("contextartifact: invalid artifact")
	ErrTampered = errors.New("contextartifact: context pack is tampered or incomplete")
	ErrMissing  = errors.New("contextartifact: immutable artifact is missing")
	ErrStore    = errors.New("contextartifact: immutable artifact store unavailable")
	ErrConflict = errors.New("contextartifact: immutable artifact conflicts with existing content")
	ErrStoreURI = errors.New("contextartifact: immutable artifact URI is unavailable")
)

// Store is the existing immutable object-store contract. Put must implement
// create-if-absent semantics; a false result is accepted only after an exact
// byte comparison of the existing object.
type Store interface {
	Put(context.Context, string, []byte, string) (created bool, uri string, err error)
	Get(context.Context, string) ([]byte, error)
}

// URIResolver is implemented by the production object-store adapter. A
// controller must be able to reconstruct the provider URI after a restart;
// failing closed is safer than projecting a guessed or logical URI.
type URIResolver interface {
	URI(string) (string, error)
}

// FileDescriptor is the exact descriptor copied from the verified pack
// manifest. It contains no file contents.
type FileDescriptor struct {
	Path          string `json:"path"`
	Digest        string `json:"digest"`
	SizeBytes     int64  `json:"sizeBytes"`
	TokenEstimate int64  `json:"tokenEstimate"`
	Kind          string `json:"kind"`
}

// Bundle is the credential-free, bounded evidence object. Strategies and
// omissions are included for reproducibility, while policy/skill descriptors
// and all generated file bodies are intentionally excluded.
type Bundle struct {
	SchemaVersion          string                           `json:"schemaVersion"`
	RunUID                 string                           `json:"runUID"`
	ResolvedSpecDigest     string                           `json:"resolvedSpecDigest"`
	BaseSHA                string                           `json:"baseSHA"`
	PolicyContractDigest   string                           `json:"policyContractDigest,omitempty"`
	PolicyContractManifest []byte                           `json:"policyContractManifest,omitempty"`
	ContextPack            PackIdentity                     `json:"contextPack"`
	Files                  []FileDescriptor                 `json:"files"`
	Strategies             []contextpack.StrategyDescriptor `json:"strategies,omitempty"`
	Omissions              []contextpack.Omission           `json:"omissions,omitempty"`
	Budgets                contextpack.Budgets              `json:"budgets"`
	Usage                  contextpack.BudgetUsage          `json:"usage"`
}

type PackIdentity struct {
	SchemaVersion      string `json:"schemaVersion"`
	RunUID             string `json:"runUID"`
	ResolvedSpecDigest string `json:"resolvedSpecDigest"`
	BaseSHA            string `json:"baseSHA"`
	ManifestPath       string `json:"manifestPath"`
	Digest             string `json:"digest"`
}

// Build verifies the read-only ContextPack volume and returns its canonical
// evidence bytes. The reference marker is read first only to discover the
// expected manifest digest; VerifyForRun then validates every file and the
// exact tree against all caller-supplied identities.
func Build(root, runUID, specDigest, baseSHA string) (Bundle, []byte, error) {
	if !validRunUID(runUID) || !canonical.ValidDigest(specDigest) || !resolved.ValidBaseSHA(baseSHA) || !validDirectory(root) {
		return Bundle{}, nil, ErrInvalid
	}
	ref, err := contextmaterializer.ReadReference(root)
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("%w: read context-pack-ref: %w", ErrTampered, err)
	}
	if ref.RunUID != runUID || ref.ResolvedSpecDigest != specDigest || ref.BaseSHA != baseSHA {
		return Bundle{}, nil, fmt.Errorf("%w: reference identity mismatch", ErrTampered)
	}
	if _, err := contextmaterializer.VerifyForRun(root, runUID, ref.Digest, specDigest, baseSHA); err != nil {
		return Bundle{}, nil, err
	}

	manifestBody, err := readRegular(filepath.Join(root, filepath.FromSlash(ManifestPath)), MaxManifestBytes)
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("%w: read manifest: %v", ErrTampered, err)
	}
	manifest, err := decodeManifest(manifestBody)
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("%w: decode manifest: %v", ErrTampered, err)
	}
	if manifest.SchemaVersion != contextpack.SchemaVersion || manifest.BaseSHA != baseSHA || manifest.ResolvedSpecDigest != specDigest || digest(manifestBody) != ref.Digest || len(manifest.Files) == 0 || len(manifest.Files) > MaxFiles {
		return Bundle{}, nil, fmt.Errorf("%w: manifest identity or bounds mismatch", ErrTampered)
	}
	var policyManifest []byte
	if manifest.PolicyContractDigest != "" {
		policyManifest, err = readRegular(filepath.Join(root, filepath.FromSlash(policycontract.ManifestPath)), policycontract.MaxManifestBytes)
		if err != nil || digest(policyManifest) != manifest.PolicyContractDigest {
			return Bundle{}, nil, fmt.Errorf("%w: policy contract manifest is missing or has the wrong digest", ErrTampered)
		}
		compiled, decodeErr := policycontract.DecodeManifest(policyManifest)
		if decodeErr != nil {
			return Bundle{}, nil, fmt.Errorf("%w: policy contract manifest is invalid: %v", ErrTampered, decodeErr)
		}
		policyIdentity := compiled.Manifest()
		if compiled.Digest() != manifest.PolicyContractDigest || policyIdentity.BaseSHA != baseSHA || policyIdentity.ResolvedSpecDigest != specDigest {
			return Bundle{}, nil, fmt.Errorf("%w: policy contract identity is invalid", ErrTampered)
		}
	}

	files := make([]FileDescriptor, 0, len(manifest.Files))
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		if !safePath(file.Path) || file.Path == ManifestPath || !canonical.ValidDigest(file.Digest) || file.SizeBytes < 0 || file.TokenEstimate < 0 || file.Kind == "" {
			return Bundle{}, nil, fmt.Errorf("%w: manifest file descriptor is invalid", ErrTampered)
		}
		if _, ok := seen[file.Path]; ok {
			return Bundle{}, nil, fmt.Errorf("%w: duplicate manifest file descriptor", ErrTampered)
		}
		seen[file.Path] = struct{}{}
		files = append(files, FileDescriptor{Path: file.Path, Digest: file.Digest, SizeBytes: file.SizeBytes, TokenEstimate: file.TokenEstimate, Kind: file.Kind})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	bundle := Bundle{
		SchemaVersion: SchemaVersion, RunUID: runUID, ResolvedSpecDigest: specDigest, BaseSHA: baseSHA,
		PolicyContractDigest: manifest.PolicyContractDigest, PolicyContractManifest: append([]byte(nil), policyManifest...),
		ContextPack: PackIdentity{SchemaVersion: ref.SchemaVersion, RunUID: ref.RunUID, ResolvedSpecDigest: ref.ResolvedSpecDigest, BaseSHA: ref.BaseSHA, ManifestPath: ref.ManifestPath, Digest: ref.Digest},
		Files:       files, Strategies: cloneStrategies(manifest.Strategies), Omissions: cloneOmissions(manifest.Omissions),
		Budgets: manifest.Budgets, Usage: manifest.Usage,
	}
	body, err := encode(bundle)
	if err != nil {
		return Bundle{}, nil, err
	}
	return bundle, body, nil
}

// Publish verifies the volume, then creates the deterministic immutable
// evidence object under the run prefix. Reconciliation-safe retries compare
// existing bytes and reject every conflict or ambiguous store result.
func Publish(ctx context.Context, store Store, root, runUID, specDigest, baseSHA string) (v1alpha1.ArtifactRef, error) {
	var zero v1alpha1.ArtifactRef
	if ctx == nil || store == nil {
		return zero, ErrInvalid
	}
	_, body, err := Build(root, runUID, specDigest, baseSHA)
	if err != nil {
		return zero, err
	}
	key, err := ObjectKey(runUID, specDigest)
	if err != nil {
		return zero, err
	}
	created, uri, err := store.Put(ctx, key, append([]byte(nil), body...), MediaType)
	if err != nil {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return zero, fmt.Errorf("%w: put context artifact: %v", ErrStore, err)
	}
	if !safeURI(uri) {
		return zero, fmt.Errorf("%w: store returned unsafe URI", ErrStore)
	}
	if !created {
		existing, getErr := store.Get(ctx, key)
		if getErr != nil {
			return zero, fmt.Errorf("%w: verify existing context artifact: %v", ErrStore, getErr)
		}
		if !bytes.Equal(existing, body) {
			return zero, ErrConflict
		}
	}
	digest := digest(body)
	return v1alpha1.ArtifactRef{URI: uri, Digest: digest, Kind: Kind, Name: Name, MediaType: MediaType, SizeBytes: int64(len(body))}, nil
}

// ObjectKey is shared by broker publication and controller reads. The key is
// deterministic from the immutable run/spec identity and cannot be selected
// by the agent.
func ObjectKey(runUID, specDigest string) (string, error) {
	if !validRunUID(runUID) || !canonical.ValidDigest(specDigest) {
		return "", ErrInvalid
	}
	return "runs/" + runUID + "/context-pack/" + strings.TrimPrefix(specDigest, canonical.DigestPrefix) + ".json", nil
}

// Load reads only the deterministic object. It deliberately does not decode
// it; the controller calls Decode itself after this independent store read.
func Load(ctx context.Context, store Store, runUID, specDigest string) ([]byte, string, error) {
	if ctx == nil || store == nil {
		return nil, "", ErrInvalid
	}
	key, err := ObjectKey(runUID, specDigest)
	if err != nil {
		return nil, "", err
	}
	body, err := store.Get(ctx, key)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: get context artifact: %v", ErrStore, err)
	}
	if len(body) == 0 || len(body) > MaxBundleBytes {
		return nil, "", ErrTampered
	}
	resolver, ok := store.(URIResolver)
	if !ok {
		return nil, "", ErrStoreURI
	}
	uri, err := resolver.URI(key)
	if err != nil || !safeURI(uri) {
		return nil, "", ErrStoreURI
	}
	return append([]byte(nil), body...), uri, nil
}

// Decode independently validates an immutable object and creates the API
// reference only from the validated body and store-derived URI.
func Decode(body []byte, uri, runUID, specDigest, baseSHA string) (Bundle, v1alpha1.ArtifactRef, error) {
	var zero v1alpha1.ArtifactRef
	if len(body) == 0 || len(body) > MaxBundleBytes || !safeURI(uri) || !validRunUID(runUID) || !canonical.ValidDigest(specDigest) || !resolved.ValidBaseSHA(baseSHA) {
		return Bundle{}, zero, ErrTampered
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) || strictjson.ValidateObject(body) != nil {
		return Bundle{}, zero, ErrTampered
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, zero, ErrTampered
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Bundle{}, zero, ErrTampered
	}
	if bundle.SchemaVersion != SchemaVersion || bundle.RunUID != runUID || bundle.ResolvedSpecDigest != specDigest || bundle.BaseSHA != baseSHA || bundle.ContextPack.RunUID != runUID || bundle.ContextPack.ResolvedSpecDigest != specDigest || bundle.ContextPack.BaseSHA != baseSHA || bundle.ContextPack.ManifestPath != ManifestPath || bundle.ContextPack.SchemaVersion != contextmaterializer.RefSchemaVersion || !canonical.ValidDigest(bundle.ContextPack.Digest) || len(bundle.Files) == 0 || len(bundle.Files) > MaxFiles {
		return Bundle{}, zero, ErrTampered
	}
	if bundle.PolicyContractDigest == "" {
		if len(bundle.PolicyContractManifest) != 0 {
			return Bundle{}, zero, ErrTampered
		}
	} else {
		if !canonical.ValidDigest(bundle.PolicyContractDigest) || len(bundle.PolicyContractManifest) == 0 || digest(bundle.PolicyContractManifest) != bundle.PolicyContractDigest {
			return Bundle{}, zero, ErrTampered
		}
		compiled, err := policycontract.DecodeManifest(bundle.PolicyContractManifest)
		if err != nil {
			return Bundle{}, zero, fmt.Errorf("%w: policy contract manifest is invalid: %v", ErrTampered, err)
		}
		policyIdentity := compiled.Manifest()
		if compiled.Digest() != bundle.PolicyContractDigest || policyIdentity.BaseSHA != baseSHA || policyIdentity.ResolvedSpecDigest != specDigest {
			return Bundle{}, zero, ErrTampered
		}
	}
	seen := make(map[string]struct{}, len(bundle.Files))
	for _, file := range bundle.Files {
		if !safePath(file.Path) || file.Path == ManifestPath || !canonical.ValidDigest(file.Digest) || file.SizeBytes < 0 || file.TokenEstimate < 0 || file.Kind == "" {
			return Bundle{}, zero, ErrTampered
		}
		if _, ok := seen[file.Path]; ok {
			return Bundle{}, zero, ErrTampered
		}
		seen[file.Path] = struct{}{}
	}
	if got := digest(body); got == "" {
		return Bundle{}, zero, ErrTampered
	}
	return bundle, v1alpha1.ArtifactRef{URI: uri, Digest: digest(body), Kind: Kind, Name: Name, MediaType: MediaType, SizeBytes: int64(len(body))}, nil
}

func encode(bundle Bundle) ([]byte, error) {
	bundle.SchemaVersion = SchemaVersion
	body, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("%w: encode bundle", ErrInvalid)
	}
	body, err = strictjson.Normalize(body)
	if err != nil || strictjson.ValidateObject(body) != nil || len(body) > MaxBundleBytes {
		return nil, fmt.Errorf("%w: bundle is oversized or non-canonical", ErrInvalid)
	}
	return body, nil
}

func decodeManifest(body []byte) (contextpack.Manifest, error) {
	if len(body) == 0 || len(body) > MaxManifestBytes || strictjson.ValidateObject(body) != nil {
		return contextpack.Manifest{}, ErrTampered
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest contextpack.Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return contextpack.Manifest{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return contextpack.Manifest{}, ErrTampered
	}
	return manifest, nil
}

func readRegular(name string, max int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > max {
		return nil, ErrTampered
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(body)) > max || !utf8.Valid(body) {
		return nil, ErrTampered
	}
	return body, nil
}

func cloneStrategies(input []contextpack.StrategyDescriptor) []contextpack.StrategyDescriptor {
	return append([]contextpack.StrategyDescriptor(nil), input...)
}

func cloneOmissions(input []contextpack.Omission) []contextpack.Omission {
	return append([]contextpack.Omission(nil), input...)
}

func validDirectory(value string) bool {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return false
	}
	info, err := os.Lstat(value)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func validRunUID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func safePath(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsRune(value, 0) || strings.ContainsRune(value, '\\') || strings.HasPrefix(value, "/") || value == "." || strings.HasPrefix(value, "../") || strings.Contains(value, "/../") || filepath.ToSlash(filepath.Clean(value)) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func safeURI(value string) bool {
	return (strings.HasPrefix(value, "s3://") || strings.HasPrefix(value, "https://")) && !strings.ContainsAny(value, "\x00\r\n") && !strings.Contains(value, "@") && !strings.Contains(value, "?") && !strings.Contains(value, "#")
}

func digest(body []byte) string {
	hash := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(hash[:])
}
