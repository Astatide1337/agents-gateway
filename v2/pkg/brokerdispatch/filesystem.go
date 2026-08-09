package brokerdispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"golang.org/x/sys/unix"
)

type clientConfig struct {
	SessionID       string `json:"session_id"`
	BearerToken     string `json:"bearer_token"`
	PolicyDigest    string `json:"policy_digest"`
	AllowedModel    string `json:"allowed_model"`
	ModelURL        string `json:"model_url"`
	ToolsURL        string `json:"tools_url"`
	ArtifactURL     string `json:"artifact_url"`
	ToolsEnabled    bool   `json:"tools_enabled"`
	ArtifactEnabled bool   `json:"artifact_enabled"`
}

func validatePrivateRoot(root string) error {
	if root == "" || strings.ContainsRune(root, 0) || !filepath.IsAbs(root) {
		return ErrInvalidConfig
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) || root == "." {
		return ErrInvalidConfig
	}
	if err := ensurePrivatePath(root, true); err != nil {
		return fmt.Errorf("%w: broker root", ErrUnsafePath)
	}
	return nil
}

func ensurePrivateRoot(root string) error {
	root = filepath.Clean(root)
	if root == "" || root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return ErrInvalidConfig
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		if err := mkdirPrivatePath(root); err != nil {
			return ErrInvalidConfig
		}
	} else if err != nil {
		return ErrUnsafePath
	}
	return validatePrivateRoot(root)
}

func mkdirPrivatePath(root string) error {
	root = filepath.Clean(root)
	if root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return ErrInvalidConfig
	}
	// Walk and create one component at a time so an existing symlink is never
	// silently followed by MkdirAll.
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
	}
	return os.Chmod(root, 0700)
}

func ensurePrivatePath(path string, requireFinal bool) error {
	path = filepath.Clean(path)
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	last := -1
	for i := range parts {
		if parts[i] != "" {
			last = i
		}
	}
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && !requireFinal && index == last {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
		if index == last {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint32(stat.Uid) != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
				return ErrUnsafePath
			}
		}
	}
	return nil
}

func createSessionDirectory(root string, sessionID string) (string, os.FileInfo, error) {
	if err := runner.ValidateBrokerSessionID(sessionID); err != nil {
		return "", nil, ErrUnsafePath
	}
	dir := filepath.Join(root, sessionID)
	if filepath.Clean(dir) != dir || filepath.Dir(dir) != filepath.Clean(root) {
		return "", nil, ErrUnsafePath
	}
	err := os.Mkdir(dir, 0700)
	if err != nil {
		return "", nil, fmt.Errorf("%w: create session directory", ErrInvalidConfig)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.Remove(dir)
		return "", nil, fmt.Errorf("%w: harden session directory", ErrInvalidConfig)
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		_ = os.Remove(dir)
		return "", nil, ErrUnsafePath
	}
	return dir, info, nil
}

func writeClientConfig(dir string, config clientConfig) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("%w: encode client config", ErrInvalidConfig)
	}
	temporary, err := os.CreateTemp(dir, ".client-*")
	if err != nil {
		return fmt.Errorf("%w: create client config", ErrInvalidConfig)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return fmt.Errorf("%w: prepare client config", ErrInvalidConfig)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("%w: write client config", ErrInvalidConfig)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("%w: sync client config", ErrInvalidConfig)
	}
	if err := temporary.Chmod(clientFileMode); err != nil {
		return fmt.Errorf("%w: protect client config", ErrInvalidConfig)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("%w: close client config", ErrInvalidConfig)
	}
	if err := os.Rename(temporaryName, filepath.Join(dir, "client.json")); err != nil {
		return fmt.Errorf("%w: publish client config", ErrInvalidConfig)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%w: open client directory", ErrInvalidConfig)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("%w: sync client directory", ErrInvalidConfig)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("%w: close client directory", ErrInvalidConfig)
	}
	removeTemporary = false
	return nil
}

func removeOwnedDirectory(path string, created os.FileInfo) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return ErrUnsafePath
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafePath
	}
	if created != nil && !os.SameFile(created, info) {
		return ErrUnsafePath
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(stat.Uid) != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return ErrUnsafePath
	}
	if err := makeOwnedTreeRemovable(path); err != nil {
		return ErrUnsafePath
	}
	if err := os.RemoveAll(path); err != nil {
		return ErrUnsafePath
	}
	return nil
}

func makeOwnedTreeRemovable(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint32(stat.Uid) != uint32(os.Geteuid()) {
			return ErrUnsafePath
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0700); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && info.Mode()&os.ModeSocket == 0 {
			return ErrUnsafePath
		}
		return nil
	})
}

func lockIsHeld(path string) (held bool, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false, nil
}

func sessionFingerprint(input workflow.ScheduleRunnerTaskInput) (string, string, error) {
	keyMaterial := struct {
		OrganizationID string
		ProjectID      string
		RunID          string
		WorkflowName   string
		StepID         string
		IdempotencyKey string
	}{input.OrganizationID, input.ProjectID, input.RunID, input.WorkflowName, input.StepID, input.IdempotencyKey}
	keyBytes, err := json.Marshal(keyMaterial)
	if err != nil {
		return "", "", err
	}
	base := input
	base.Execution.BrokerSessionID = ""
	fingerprintBytes, err := json.Marshal(base)
	if err != nil {
		return "", "", err
	}
	keyHash := sha256.Sum256(keyBytes)
	fingerprintHash := sha256.Sum256(fingerprintBytes)
	return hex.EncodeToString(keyHash[:]), hex.EncodeToString(fingerprintHash[:]), nil
}
