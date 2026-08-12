package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	defaultKubectlBinary = "kubectl"
	maxCommandErrorBytes = 16 << 10
	maxJSONOutputBytes   = 1 << 20
)

// CommandRunner is the narrow transport seam for the plugin. argv[0] is the
// executable name and must be executed directly; implementations must not
// interpret argv through a shell.
type CommandRunner interface {
	Run(ctx context.Context, argv []string, stdin []byte, stdout, stderr io.Writer) error
}

// ExecCommandRunner executes kubectl without a shell.
type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, argv []string, stdin []byte, stdout, stderr io.Writer) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("empty command argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit < 0 {
		return 0, fmt.Errorf("negative output limit")
	}
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = b.Buffer.Write(p)
		} else {
			_, _ = b.Buffer.Write(p[:remaining])
		}
	}
	if len(p) > remaining {
		b.truncated = true
	}
	// Returning len(p) keeps the child process running long enough for the
	// caller to report a bounded diagnostic instead of getting a broken pipe.
	return len(p), nil
}

// bytes.Buffer promotes WriteString when embedded. Override it so callers
// such as io.WriteString cannot bypass the bound above.
func (b *boundedBuffer) WriteString(value string) (int, error) {
	return b.Write([]byte(value))
}

func (b *boundedBuffer) StringWithMarker() string {
	value := strings.TrimSpace(b.String())
	if b.truncated {
		if value == "" {
			return "[output truncated]"
		}
		return value + " [output truncated]"
	}
	return value
}

func boundedError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxCommandErrorBytes {
		return value
	}
	marker := " [error truncated]"
	if strings.Contains(value, "[output truncated]") {
		marker = " [output truncated]"
	}
	if len(marker) >= maxCommandErrorBytes {
		return marker[:maxCommandErrorBytes]
	}
	return value[:maxCommandErrorBytes-len(marker)] + marker
}

func commandFailure(action string, err error, stderr *boundedBuffer) error {
	parts := []string{action}
	if err != nil {
		parts = append(parts, boundedError(err.Error()))
	}
	if stderr != nil {
		if diagnostic := stderr.StringWithMarker(); diagnostic != "" {
			parts = append(parts, diagnostic)
		}
	}
	message := strings.Join(parts, ": ")
	if len(message) > maxCommandErrorBytes {
		marker := " [error truncated]"
		if stderr != nil && stderr.truncated {
			marker = " [output truncated]"
		}
		message = truncateWithMarker(message, marker, maxCommandErrorBytes)
	}
	return fmt.Errorf("%s", message)
}

func truncateWithMarker(value, marker string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if len(marker) >= limit {
		return marker[:limit]
	}
	return value[:limit-len(marker)] + marker
}
