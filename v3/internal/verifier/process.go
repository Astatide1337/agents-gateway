package verifier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

const (
	commandPollInterval = 5 * time.Millisecond
	maxExitCode         = 255
)

type commandRun struct {
	Observation CommandEvidence
	Stdout      []byte
	Stderr      []byte
	TimedOut    bool
	Oversized   bool
	Operational bool
}

type outputCapture struct {
	mu       sync.Mutex
	limit    int64
	total    int64
	overflow bool
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

type streamWriter struct {
	capture *outputCapture
	stdout  bool
}

func (w streamWriter) Write(body []byte) (int, error) {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	if w.capture.overflow {
		return len(body), nil
	}
	remaining := w.capture.limit - w.capture.total
	if remaining <= 0 {
		w.capture.overflow = true
		return len(body), nil
	}
	keep := int64(len(body))
	if keep > remaining {
		keep = remaining
		w.capture.overflow = true
	}
	if keep > 0 {
		if w.stdout {
			_, _ = w.capture.stdout.Write(body[:keep])
		} else {
			_, _ = w.capture.stderr.Write(body[:keep])
		}
		w.capture.total += keep
	}
	return len(body), nil
}

func (c *outputCapture) snapshot() (stdout, stderr []byte, overflow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.stdout.Bytes()...), append([]byte(nil), c.stderr.Bytes()...), c.overflow
}

func executeCommands(ctx context.Context, cfg Config, cwd string, commands []v1alpha1.VerifyCommand) ([]commandRun, bool) {
	results := make([]commandRun, len(commands))
	operationalFailure := false
	for index, command := range commands {
		results[index].Observation.Index = index
		if ctx == nil || ctx.Err() != nil {
			operationalFailure = true
			continue
		}
		result := executeCommand(ctx, cfg.MaxOutputBytes, cwd, index, command)
		results[index] = result
		if result.Operational {
			operationalFailure = true
			// A timeout, output breach, or inability to execute invalidates
			// the observation contract. Do not run later trusted commands
			// after the verifier has lost its bounded execution guarantee.
			for skipped := index + 1; skipped < len(results); skipped++ {
				results[skipped].Observation.Index = skipped
			}
			break
		}
	}
	return results, operationalFailure
}

func executeCommand(ctx context.Context, maxOutputBytes int64, cwd string, index int, command v1alpha1.VerifyCommand) commandRun {
	return executeCommandWithEnv(ctx, maxOutputBytes, cwd, index, command, nil)
}

func executeCommandWithEnv(ctx context.Context, maxOutputBytes int64, cwd string, index int, command v1alpha1.VerifyCommand, extraEnv map[string]string) commandRun {
	result := commandRun{Observation: CommandEvidence{Index: index}}
	if ctx == nil || maxOutputBytes <= 0 || cwd == "" {
		result.Operational = true
		return result
	}

	argv, err := trustedArgv(command)
	if err != nil {
		result.Operational = true
		return result
	}
	process := exec.Command(argv[0], argv[1:]...)
	process.Dir = cwd
	process.Env = cleanEnvironment(extraEnv)
	process.Stdin = nil
	capture := &outputCapture{limit: maxOutputBytes}
	process.Stdout = streamWriter{capture: capture, stdout: true}
	process.Stderr = streamWriter{capture: capture, stdout: false}
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	started := time.Now()
	if err := process.Start(); err != nil {
		result.Operational = true
		result.Observation.DurationMillis = durationMillis(time.Since(started))
		return result
	}

	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	ticker := time.NewTicker(commandPollInterval)
	defer ticker.Stop()
	var waitErr error
	timedOut := false
	oversized := false
	for {
		select {
		case waitErr = <-done:
			goto finished
		case <-ctx.Done():
			timedOut = true
			killProcessGroup(process)
			waitErr = <-done
			goto finished
		case <-ticker.C:
			_, _, overflow := capture.snapshot()
			if overflow {
				oversized = true
				killProcessGroup(process)
				waitErr = <-done
				goto finished
			}
		}
	}

finished:
	var overflow bool
	result.Stdout, result.Stderr, overflow = capture.snapshot()
	result.TimedOut = timedOut
	result.Oversized = oversized || overflow
	result.Observation.DurationMillis = durationMillis(time.Since(started))
	if timedOut || result.Oversized {
		result.Operational = true
		return result
	}
	if waitErr == nil {
		code := int32(0)
		result.Observation.ExitCode = &code
	} else {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) && exitError.ProcessState != nil {
			code := exitError.ProcessState.ExitCode()
			if code >= 0 && code <= maxExitCode {
				code32 := int32(code)
				result.Observation.ExitCode = &code32
			}
		} else {
			result.Operational = true
			return result
		}
	}
	result.Observation.EvidenceDigest = digestCommandOutput(index, result.Stdout, result.Stderr)
	return result
}

func trustedArgv(command v1alpha1.VerifyCommand) ([]string, error) {
	if len(command.Argv) > 0 {
		return append([]string(nil), command.Argv...), nil
	}
	if command.Shell == nil || strings.TrimSpace(*command.Shell) == "" {
		return nil, ErrInvalidConfig
	}
	// Gate shell commands are trusted policy, not repository input. They are
	// still executed through this fixed shell with a fixed argv[0] and no
	// inherited environment. The caller must not expose this field to an
	// untrusted submitter without an admission policy change.
	return []string{"/bin/sh", "-c", *command.Shell, "agw-gate"}, nil
}

func cleanEnvironment(extra map[string]string) []string {
	environment := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"TERM=dumb",
		"CI=true",
		"NO_COLOR=1",
		"PAGER=cat",
		"GIT_PAGER=cat",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_SSH_VARIANT=ssh",
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"all_proxy=",
		"NO_PROXY=*",
		"no_proxy=*",
	}
	for name, value := range extra {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			continue
		}
		prefix := name + "="
		for index := range environment {
			if strings.HasPrefix(environment[index], prefix) {
				environment[index] = prefix + value
				goto next
			}
		}
		environment = append(environment, prefix+value)
	next:
	}
	return environment
}

func killProcessGroup(process *exec.Cmd) {
	if process == nil || process.Process == nil {
		return
	}
	_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
	_ = process.Process.Kill()
}

func durationMillis(value time.Duration) *int64 {
	if value < 0 {
		value = 0
	}
	result := value.Milliseconds()
	if result > 24*60*60*1000 {
		result = 24 * 60 * 60 * 1000
	}
	return &result
}

func digestCommandOutput(index int, stdout, stderr []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("AGW_COMMAND_EVIDENCE_V1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(index))
	_, _ = hash.Write(length[:])
	binary.BigEndian.PutUint64(length[:], uint64(len(stdout)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(stdout)
	binary.BigEndian.PutUint64(length[:], uint64(len(stderr)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(stderr)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}
