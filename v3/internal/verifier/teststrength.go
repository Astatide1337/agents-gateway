package verifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

// verifyNewTestsFailOnBase implements the Gate's test-strength rule. The
// second tree starts from the pristine base and receives only changed test
// files from the applied tree. No application source file, artifact, skill,
// or agent workspace state is copied into it.
func verifyNewTestsFailOnBase(ctx context.Context, cfg Config, analysis Analysis) (bool, error) {
	if cfg.TestStrength != v1alpha1.TestStrengthNewTestsFailOnBase || cfg.Adapter != v1alpha1.GateAdapterGo {
		return false, ErrTestStrength
	}
	testPaths := make([]string, 0)
	for _, path := range analysis.ChangedPaths {
		if isGoTestFile(path) {
			testPaths = append(testPaths, path)
		}
	}
	if len(testPaths) == 0 {
		return false, fmt.Errorf("%w: no changed test files", ErrTestStrength)
	}
	if cfg.BaseTestCommand == nil {
		return false, fmt.Errorf("%w: no explicit baseTestCommand", ErrTestStrength)
	}
	if err := validateGoTestCommand(*cfg.BaseTestCommand); err != nil {
		return false, fmt.Errorf("%w: baseTestCommand: %v", ErrTestStrength, err)
	}
	temp, err := os.MkdirTemp(cfg.Workspace, ".agw-pristine-base-")
	if err != nil {
		return false, fmt.Errorf("%w: create pristine base: %v", ErrTestStrength, err)
	}
	defer os.RemoveAll(temp)
	if err := copyTree(cfg.BasePath, temp, cfg.MaxCopyBytes); err != nil {
		return false, fmt.Errorf("%w: copy pristine base: %v", ErrTestStrength, err)
	}
	for _, path := range testPaths {
		if err := copyRelativeFile(cfg.RepoPath, temp, path); err != nil {
			return false, fmt.Errorf("%w: overlay test file: %v", ErrTestStrength, err)
		}
	}
	result := executeCommandWithEnv(ctx, cfg.MaxOutputBytes, temp, 0, *cfg.BaseTestCommand, map[string]string{
		"GOCACHE":     filepath.Join(temp, ".agw-go-cache"),
		"GOMODCACHE":  filepath.Join(temp, ".agw-go-modcache"),
		"GOTOOLCHAIN": "local",
	})
	if result.Operational {
		return false, fmt.Errorf("%w: base test command did not produce bounded evidence", ErrTestStrength)
	}
	if result.Observation.ExitCode == nil {
		return false, fmt.Errorf("%w: base test command evidence is missing", ErrTestStrength)
	}
	if *result.Observation.ExitCode == 0 {
		return false, fmt.Errorf("%w: patched tests pass on the pristine base", ErrTestStrength)
	}
	return true, nil
}

func isGoTestFile(path string) bool {
	if !gate.ValidateRepoPath(path) {
		return false
	}
	base := strings.ToLower(filepath.Base(filepath.FromSlash(path)))
	return strings.HasSuffix(base, "_test.go")
}

func copyTree(source, destination string, maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > HardMaxCopyBytes {
		return ErrInvalidConfig
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source is not a regular directory")
	}
	if existing, err := os.Lstat(destination); err == nil {
		if !existing.IsDir() || existing.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		entries, err := os.ReadDir(destination)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("copy destination is not empty")
		}
	} else if os.IsNotExist(err) {
		if err := os.Mkdir(destination, 0700); err != nil {
			return err
		}
	} else {
		return err
	}
	var copied int64
	var files int
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		normalized := filepath.ToSlash(rel)
		if normalized == ".git" || strings.HasPrefix(normalized, ".git/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destinationPath := filepath.Join(destination, filepath.FromSlash(normalized))
		if entry.IsDir() {
			if err := ensureSafeParents(destination, normalized); err != nil {
				return err
			}
			return os.Mkdir(destinationPath, entryMode(entry, 0700))
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return ErrSymlink
		}
		if !gate.ValidateRepoPath(normalized) {
			return ErrUnsafePath
		}
		if files >= maxTreeFiles {
			return errors.New("pristine base file count exceeds bound")
		}
		if err := copyRegularFile(path, destinationPath, &copied, maxBytes, entryMode(entry, 0600)); err != nil {
			return err
		}
		files++
		return nil
	})
}

func copyRelativeFile(sourceRoot, destinationRoot, relative string) error {
	if !gate.ValidateRepoPath(relative) {
		return ErrUnsafePath
	}
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	destination := filepath.Join(destinationRoot, filepath.FromSlash(relative))
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrSymlink
	}
	if err := ensureSafeParents(destinationRoot, relative); err != nil {
		return err
	}
	var copied int64
	if err := copyRegularFile(source, destination, &copied, maxFileBytes, info.Mode().Perm()); err != nil {
		return err
	}
	return nil
}

func copyRegularFile(source, destination string, copied *int64, maxBytes int64, mode fs.FileMode) error {
	if copied == nil {
		return ErrInvalidConfig
	}
	parent := filepath.Dir(destination)
	if err := ensureDirectoryChain(parent, parent); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	// O_NOFOLLOW prevents a later symlink substitution from turning an
	// overlay into a write outside the second pristine tree on Linux.
	flags |= syscall.O_NOFOLLOW
	output, err := os.OpenFile(destination, flags, mode.Perm())
	if err != nil {
		return err
	}
	defer output.Close()
	written, err := io.CopyN(output, input, maxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if written > maxBytes {
		return errors.New("copied file exceeds bound")
	}
	if *copied > maxBytes-written {
		return errors.New("pristine base copy exceeds bound")
	}
	*copied += written
	return output.Sync()
}

func ensureSafeParents(root, relative string) error {
	if root == "" {
		return ErrInvalidConfig
	}
	if relative == "" {
		return ensureDirectoryChain(root, root)
	}
	return ensureDirectoryChain(root, filepath.Join(root, filepath.Dir(filepath.FromSlash(relative))))
}

func ensureDirectoryChain(root, target string) error {
	if root == "" || target == "" {
		return ErrInvalidConfig
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrUnsafePath
	}
	current := root
	if info, err := os.Lstat(current); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafePath
	}
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return ErrUnsafePath
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return ErrUnsafePath
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(current, 0700); err != nil {
			return err
		}
	}
	return nil
}

func entryMode(entry fs.DirEntry, fallback fs.FileMode) fs.FileMode {
	info, err := entry.Info()
	if err != nil {
		return fallback
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		return fallback
	}
	return mode
}
