// agw-clone performs the network-enabled setup side of the execution airlock.
// It checks out the controller-pinned commit, creates an immutable pristine
// base copy, and exits before any agent-authored code is allowed to run.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxTokenBytes = 4096

var (
	repoPattern = regexp.MustCompile(`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern  = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
)

type config struct {
	repo, baseSHA, workspace, tokenFile string
	depth                               int
}

func main() {
	if err := run(context.Background(), os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "agw-clone: setup failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	cfg, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	token, err := readToken(cfg.tokenFile)
	if err != nil {
		return err
	}
	defer clear(token)

	repoDir := filepath.Join(cfg.workspace, "repo")
	baseDir := filepath.Join(cfg.workspace, "base")
	if err := requireAbsent(repoDir, baseDir); err != nil {
		return err
	}
	askpass, err := writeAskpass(cfg.workspace)
	if err != nil {
		return err
	}
	defer os.Remove(askpass)
	env := []string{
		"HOME=/tmp", "PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + askpass, "AGW_GIT_TOKEN_FILE=" + cfg.tokenFile,
		"AGW_GIT_USERNAME=x-access-token", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C",
	}
	remote := "https://" + cfg.repo + ".git"
	commands := [][]string{
		{"init", "--quiet", repoDir},
		{"-C", repoDir, "remote", "add", "origin", remote},
		{"-C", repoDir, "fetch", "--quiet", "--no-tags", "--depth=" + strconv.Itoa(cfg.depth), "origin", cfg.baseSHA},
		{"-C", repoDir, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, args := range commands {
		if err := git(ctx, env, args...); err != nil {
			return err
		}
	}
	got, err := gitOutput(ctx, env, "-C", repoDir, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(got) != cfg.baseSHA {
		return errors.New("checked out revision does not match the controller-pinned SHA")
	}
	if err := git(ctx, env, "clone", "--quiet", "--no-local", "--no-hardlinks", repoDir, baseDir); err != nil {
		return err
	}
	if err := git(ctx, env, "-C", baseDir, "checkout", "--quiet", "--detach", cfg.baseSHA); err != nil {
		return err
	}
	if err := chownTree(repoDir, 1000, 1000); err != nil {
		return errors.New("could not assign worktree ownership")
	}
	if err := makeTreeReadOnly(baseDir); err != nil {
		return errors.New("could not seal pristine base")
	}
	return nil
}

func configFromEnv(getenv func(string) string) (config, error) {
	if getenv == nil {
		return config{}, errors.New("environment reader is required")
	}
	cfg := config{repo: getenv("AGW_REPO"), baseSHA: getenv("AGW_BASE_SHA"), workspace: getenv("AGW_WORKSPACE"), tokenFile: getenv("AGW_GIT_TOKEN_FILE")}
	if !repoPattern.MatchString(cfg.repo) || !shaPattern.MatchString(cfg.baseSHA) {
		return config{}, errors.New("repository or base SHA is invalid")
	}
	depth, err := strconv.Atoi(getenv("AGW_DEPTH"))
	if err != nil || depth < 1 || depth > 100 {
		return config{}, errors.New("clone depth must be in 1..100")
	}
	cfg.depth = depth
	for _, path := range []string{cfg.workspace, cfg.tokenFile} {
		if !filepath.IsAbs(path) || path == "/" || strings.ContainsRune(path, 0) {
			return config{}, errors.New("clone paths must be absolute and bounded")
		}
	}
	if filepath.Clean(cfg.tokenFile) != cfg.tokenFile || filepath.Clean(cfg.workspace) != cfg.workspace {
		return config{}, errors.New("clone paths must be canonical")
	}
	return cfg, nil
}

func readToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxTokenBytes {
		return nil, errors.New("clone credential file is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("clone credential cannot be opened")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil || len(body) > maxTokenBytes {
		return nil, errors.New("clone credential cannot be read safely")
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 || strings.ContainsAny(string(body), "\x00\r\n") {
		return nil, errors.New("clone credential is malformed")
	}
	return body, nil
}

func writeAskpass(workspace string) (string, error) {
	file, err := os.CreateTemp(workspace, ".agw-askpass-")
	if err != nil {
		return "", errors.New("cannot create credential helper")
	}
	path := file.Name()
	script := "#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' \"$AGW_GIT_USERNAME\";; *) exec cat \"$AGW_GIT_TOKEN_FILE\";; esac\n"
	if _, err = io.WriteString(file, script); err != nil {
		file.Close()
		os.Remove(path)
		return "", errors.New("cannot write credential helper")
	}
	if err = file.Close(); err != nil || os.Chmod(path, 0700) != nil {
		os.Remove(path)
		return "", errors.New("cannot secure credential helper")
	}
	return path, nil
}

func requireAbsent(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return errors.New("workspace output already exists")
		}
	}
	return nil
}

func git(ctx context.Context, env []string, args ...string) error {
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", args...)
	cmd.Env = env
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("git operation failed")
	}
	return nil
}

func gitOutput(ctx context.Context, env []string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", args...)
	cmd.Env = env
	cmd.Stderr = io.Discard
	body, err := cmd.Output()
	if err != nil || len(body) > 256 {
		return "", errors.New("git proof failed")
	}
	return string(body), nil
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

func makeTreeReadOnly(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := info.Mode().Perm() &^ 0222
		if info.IsDir() {
			mode |= 0555
		}
		if err := os.Chown(path, 0, 0); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	})
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
