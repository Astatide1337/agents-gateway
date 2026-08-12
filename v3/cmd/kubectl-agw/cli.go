package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

type Config struct {
	Runner        CommandRunner
	KubectlBinary string
	Stdout        io.Writer
	Stderr        io.Writer
}

type CLI struct {
	runner        CommandRunner
	kubectlBinary string
	stdout        io.Writer
	stderr        io.Writer
}

func NewCLI(config Config) *CLI {
	runner := config.Runner
	if runner == nil {
		runner = ExecCommandRunner{}
	}
	binary := config.KubectlBinary
	if binary == "" {
		binary = defaultKubectlBinary
	}
	stdout := config.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := config.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return &CLI{runner: runner, kubectlBinary: binary, stdout: stdout, stderr: stderr}
}

func (c *CLI) Run(ctx context.Context, args []string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s", boundedError(err.Error()))
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	parsed, err := parseInvocation(args)
	if err != nil {
		return err
	}
	switch parsed.verb {
	case "run":
		options, err := parseRunOptions(parsed.args)
		if err != nil {
			return err
		}
		data, err := readAndValidateManifest(options.file, options.namespace)
		if err != nil {
			return err
		}
		return c.run(ctx, parsed.globalArgs, options, data)
	case "logs":
		options, err := parseLogsOptions(parsed.args)
		if err != nil {
			return err
		}
		return c.logs(ctx, parsed.globalArgs, options)
	case "cancel":
		options, err := parseCancelOptions(parsed.args)
		if err != nil {
			return err
		}
		return c.cancel(ctx, parsed.globalArgs, options)
	case "review":
		options, err := parseReviewOptions(parsed.args)
		if err != nil {
			return err
		}
		return c.review(ctx, parsed.globalArgs, options)
	case "matrix":
		options, err := parseMatrixOptions(parsed.args)
		if err != nil {
			return err
		}
		return c.matrix(ctx, parsed.globalArgs, options)
	default:
		return &usageError{message: fmt.Sprintf("unknown subcommand %q", parsed.verb)}
	}
}

func (c *CLI) run(ctx context.Context, globals []string, options runOptions, data []byte) error {
	_, err := c.invoke(ctx, globals, "create AgentRun", data, "create", "--filename", "-", "--namespace", options.namespace)
	return err
}

func (c *CLI) cancel(ctx context.Context, globals []string, options cancelOptions) error {
	patch := []byte(`{"spec":{"cancelRequested":true}}`)
	_, err := c.invoke(ctx, globals, "cancel AgentRun", nil, "patch", agentRunResource, options.run, "--namespace", options.namespace, "--type", "merge", "--patch", string(patch), "--output", "name")
	return err
}

func (c *CLI) logs(ctx context.Context, globals []string, options logsOptions) error {
	podName, err := c.resolvePodForLogs(ctx, globals, options.namespace, options.run, options.container)
	if err != nil {
		return err
	}
	arguments := []string{"logs", podName, "--namespace", options.namespace, "--container", options.container}
	if options.follow {
		arguments = append(arguments, "--follow")
	}
	_, err = c.invoke(ctx, globals, "logs AgentRun", nil, arguments...)
	return err
}

func (c *CLI) capture(ctx context.Context, globals []string, action string, arguments ...string) ([]byte, error) {
	stdout := newBoundedBuffer(maxJSONOutputBytes)
	_, err := c.invokeWithWriters(ctx, globals, action, nil, stdout, arguments...)
	if err != nil {
		return nil, err
	}
	if stdout.truncated {
		return nil, fmt.Errorf("%s: kubectl output exceeds %d bytes", action, maxJSONOutputBytes)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func (c *CLI) invoke(ctx context.Context, globals []string, action string, stdin []byte, arguments ...string) ([]byte, error) {
	stdout := c.stdout
	return c.invokeWithWriters(ctx, globals, action, stdin, stdout, arguments...)
}

func (c *CLI) invokeWithWriters(ctx context.Context, globals []string, action string, stdin []byte, stdout io.Writer, arguments ...string) ([]byte, error) {
	argv := make([]string, 0, 1+len(globals)+len(arguments))
	argv = append(argv, c.kubectlBinary)
	argv = append(argv, globals...)
	argv = append(argv, arguments...)
	stderr := newBoundedBuffer(maxCommandErrorBytes)
	if stdout == nil {
		stdout = io.Discard
	}
	if err := c.runner.Run(ctx, argv, stdin, stdout, stderr); err != nil {
		return nil, commandFailure(action, err, stderr)
	}
	return nil, nil
}

func main() {
	cli := NewCLI(Config{Stdout: os.Stdout, Stderr: os.Stderr})
	if err := cli.Run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kubectl agw:", err)
		os.Exit(1)
	}
}
