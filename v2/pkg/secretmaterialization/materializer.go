// Package secretmaterialization resolves opaque host-owned secret references
// and creates short-lived environment files for one sandbox run.
//
// The package intentionally has no dependency on the control-plane store or
// resource manifests. A runner receives only an opaque reference, resolves it
// on the host, and exposes the resulting value to the runtime through a
// private per-run file. Materialized values are never returned in a contract,
// event, error, or log value.
package secretmaterialization

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	maxReferenceBytes = 4096
	maxSecretBytes    = 64 << 10
	maxEnvironment    = 256
	secretScheme      = "secret"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	secretIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Reference is the only input needed to select one host-side secret. Value is
// deliberately absent so a caller cannot accidentally put a secret into a
// manifest-shaped object.
type Reference struct {
	Name string
	Ref  string
}

// Resolver retrieves a secret on the trusted runner host. Implementations
// must not return errors containing the secret value.
type Resolver interface {
	Resolve(context.Context, string) ([]byte, error)
}

// Materialization contains only a private path and a cleanup operation. It
// never exposes resolved values to callers.
type Materialization struct {
	path       string
	root       string
	cleanup    sync.Once
	cleanupErr error
}

func (m *Materialization) EnvironmentFile() string {
	if m == nil {
		return ""
	}
	return m.path
}

// Cleanup removes the complete per-run directory. It is idempotent.
func (m *Materialization) Cleanup() error {
	if m == nil || m.path == "" {
		return nil
	}
	m.cleanup.Do(func() {
		if err := os.RemoveAll(m.root); err != nil {
			m.cleanupErr = errors.New("remove materialized environment")
		}
	})
	return m.cleanupErr
}

// FileResolver maps secret://<opaque-id> to one exact file below Root. The
// root and files must be private to the runner process owner. It does not
// follow symlinks and does not accept paths, queries, fragments, or encoded
// path separators.
type FileResolver struct {
	Root string
}

func NewFileResolver(root string) (*FileResolver, error) {
	root, err := privateRoot(root)
	if err != nil {
		return nil, err
	}
	return &FileResolver{Root: root}, nil
}

func (r *FileResolver) Resolve(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := parseSecretReference(ref)
	if err != nil {
		return nil, err
	}
	root, err := privateRoot(r.Root)
	if err != nil {
		return nil, err
	}
	directoryFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("secret reference is unavailable")
	}
	defer unix.Close(directoryFD)
	var rootStat unix.Stat_t
	if err := unix.Fstat(directoryFD, &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Mode&0777 != 0700 || int(rootStat.Uid) != os.Geteuid() || int(rootStat.Gid) != os.Getegid() {
		return nil, errors.New("secret directory is not private")
	}
	secretFD, err := unix.Openat(directoryFD, id, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("secret reference is unavailable")
	}
	file := os.NewFile(uintptr(secretFD), "runner-secret")
	if file == nil {
		_ = unix.Close(secretFD)
		return nil, errors.New("secret reference is unavailable")
	}
	defer file.Close()
	var secretStat unix.Stat_t
	if err := unix.Fstat(secretFD, &secretStat); err != nil || secretStat.Mode&unix.S_IFMT != unix.S_IFREG || secretStat.Mode&0777 != 0600 || int(secretStat.Uid) != os.Geteuid() || int(secretStat.Gid) != os.Getegid() || secretStat.Size < 1 || secretStat.Size > maxSecretBytes {
		return nil, errors.New("secret file is not private and bounded")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil || len(value) == 0 || len(value) > maxSecretBytes {
		zero(value)
		return nil, errors.New("secret value is unavailable or oversized")
	}
	if bytes.IndexByte(value, 0) >= 0 || bytes.IndexByte(value, '\r') >= 0 || bytes.IndexByte(value, '\n') >= 0 {
		zero(value)
		return nil, errors.New("secret value contains unsupported characters")
	}
	return value, nil
}

// Materializer resolves references and writes one private env file. Root is
// created private to the current runner user and each run gets a separate
// randomly named directory.
type Materializer struct {
	Root     string
	Resolver Resolver
}

func New(root string, resolver Resolver) (*Materializer, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return nil, errors.New("materialization root must be a non-root absolute path")
	}
	if resolver == nil {
		return nil, errors.New("secret resolver is required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, errors.New("create materialization root")
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, err
	}
	return &Materializer{Root: root, Resolver: resolver}, nil
}

func (m *Materializer) Materialize(ctx context.Context, runID string, references []Reference) (*Materialization, error) {
	if m == nil || m.Resolver == nil {
		return nil, errors.New("secret materializer is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(references) == 0 {
		return nil, nil
	}
	if len(references) > maxEnvironment {
		return nil, errors.New("too many environment references")
	}
	if strings.TrimSpace(runID) == "" || len(runID) > maxReferenceBytes || strings.ContainsAny(runID, "/\\\x00\r\n") {
		return nil, errors.New("run identifier is invalid")
	}
	root := filepath.Clean(strings.TrimSpace(m.Root))
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, err
	}
	runRoot, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return nil, errors.New("create secret materialization directory")
	}
	if err := os.Chmod(runRoot, 0700); err != nil {
		_ = os.RemoveAll(runRoot)
		return nil, errors.New("harden secret materialization directory")
	}
	materialized := &Materialization{path: filepath.Join(runRoot, "env"), root: runRoot}
	failed := true
	defer func() {
		if failed {
			_ = materialized.Cleanup()
		}
	}()

	seenNames := make(map[string]struct{}, len(references))
	file, err := os.OpenFile(materialized.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("create materialized environment")
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	for _, reference := range references {
		if err := validateEnvironmentReference(reference, seenNames); err != nil {
			return nil, err
		}
		value, err := m.Resolver.Resolve(ctx, reference.Ref)
		if err != nil {
			return nil, errors.New("resolve environment secret")
		}
		if len(value) == 0 || len(value) > maxSecretBytes || bytes.IndexByte(value, 0) >= 0 || bytes.IndexByte(value, '\r') >= 0 || bytes.IndexByte(value, '\n') >= 0 {
			zero(value)
			return nil, errors.New("resolved secret is invalid")
		}
		line := append([]byte(reference.Name+"="), value...)
		line = append(line, '\n')
		_, writeErr := file.Write(line)
		zero(value)
		zero(line)
		if writeErr != nil {
			return nil, errors.New("write materialized environment")
		}
	}
	if err := file.Sync(); err != nil {
		return nil, errors.New("sync materialized environment")
	}
	if err := file.Close(); err != nil {
		closeFile = false
		return nil, errors.New("close materialized environment")
	}
	closeFile = false
	if err := verifyPrivateFile(materialized.path); err != nil {
		return nil, err
	}
	failed = false
	return materialized, nil
}

func validateEnvironmentReference(reference Reference, seen map[string]struct{}) error {
	if !environmentNamePattern.MatchString(reference.Name) {
		return errors.New("environment name is invalid")
	}
	if _, exists := seen[reference.Name]; exists {
		return errors.New("environment name is duplicated")
	}
	seen[reference.Name] = struct{}{}
	if _, err := parseSecretReference(reference.Ref); err != nil {
		return err
	}
	return nil
}

func parseSecretReference(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > maxReferenceBytes || !strings.HasPrefix(ref, secretScheme+"://") {
		return "", errors.New("environment secret reference must be an opaque secret URI")
	}
	if strings.ContainsAny(ref, "\r\n\x00") {
		return "", errors.New("environment secret reference is invalid")
	}
	id := strings.TrimPrefix(ref, secretScheme+"://")
	if !secretIDPattern.MatchString(id) || strings.ContainsAny(id, "/\\?#%:@") {
		return "", errors.New("environment secret reference is not an opaque identifier")
	}
	return id, nil
}

func privateRoot(root string) (string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return "", errors.New("secret root must be a non-root absolute path")
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return "", err
	}
	return root, nil
}

func ensurePrivateDirectory(path string) error {
	info, err := lstatNoSymlink(path)
	if err != nil {
		return errors.New("secret directory is unavailable")
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return errors.New("secret directory is not private")
	}
	return nil
}

func verifyPrivateFile(path string) error {
	info, err := lstatNoSymlink(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
		return errors.New("materialized environment is not private")
	}
	return nil
}

func lstatNoSymlink(path string) (os.FileInfo, error) {
	clean := filepath.Clean(path)
	if clean == "." || !filepath.IsAbs(clean) {
		return nil, errors.New("path must be absolute")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("path contains a symlink")
		}
	}
	return os.Lstat(clean)
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && int(stat.Gid) == os.Getegid()
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ Resolver = (*FileResolver)(nil)
