package main

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const defaultNamespace = "agw-runs"

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message + "\n\n" + usageText() }

type invocation struct {
	verb       string
	globalArgs []string
	args       []string
}

type runOptions struct {
	file      string
	namespace string
}

type logsOptions struct {
	run       string
	namespace string
	container string
	follow    bool
}

type cancelOptions struct {
	run       string
	namespace string
}

type reviewOptions struct {
	run            string
	namespace      string
	classification string
}

type matrixOptions struct {
	namespace  string
	repository string
	gate       string
	output     string
}

// These are kubectl connection/authentication flags. They are extracted from
// the plugin's arguments and placed before the delegated kubectl verb so an
// explicitly selected kubeconfig/context is used for every request.
var passthroughGlobalFlags = map[string]bool{
	"--cache-dir":             true,
	"--certificate-authority": true,
	"--cluster":               true,
	"--context":               true,
	"--kubeconfig":            true,
	"--request-timeout":       true,
	"--server":                true,
	"--tls-server-name":       true,
	"--user":                  true,
}

func parseInvocation(args []string) (invocation, error) {
	globals, commandArgs, err := splitGlobalArgs(args)
	if err != nil {
		return invocation{}, &usageError{message: err.Error()}
	}
	if len(commandArgs) == 0 {
		return invocation{}, &usageError{message: "a subcommand is required"}
	}
	if commandArgs[0] != "run" && commandArgs[0] != "logs" && commandArgs[0] != "cancel" && commandArgs[0] != "review" && commandArgs[0] != "matrix" {
		return invocation{}, &usageError{message: fmt.Sprintf("unknown subcommand %q", commandArgs[0])}
	}
	return invocation{verb: commandArgs[0], globalArgs: globals, args: commandArgs[1:]}, nil
}

func splitGlobalArgs(args []string) ([]string, []string, error) {
	globals := make([]string, 0, len(args))
	commandArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inlineValue, hasInlineValue := strings.Cut(arg, "=")
		needsValue, isGlobal := passthroughGlobalFlags[name]
		if !isGlobal {
			commandArgs = append(commandArgs, arg)
			continue
		}
		if hasInlineValue {
			if needsValue && inlineValue == "" {
				return nil, nil, fmt.Errorf("global flag %s requires a value", name)
			}
			globals = append(globals, arg)
			continue
		}
		globals = append(globals, arg)
		if !needsValue {
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return nil, nil, fmt.Errorf("global flag %s requires a value", arg)
		}
		globals = append(globals, args[i+1])
		i++
	}
	return globals, commandArgs, nil
}

func parseRunOptions(args []string) (runOptions, error) {
	options := runOptions{namespace: defaultNamespace}
	namespaceSet := false
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--filename":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return runOptions{}, usageErr(err.Error())
			}
			if options.file != "" {
				return runOptions{}, usageErr("run accepts exactly one --filename/-f")
			}
			options.file, i = value, next
		case strings.HasPrefix(arg, "--filename="):
			if options.file != "" {
				return runOptions{}, usageErr("run accepts exactly one --filename/-f")
			}
			options.file = strings.TrimPrefix(arg, "--filename=")
		case strings.HasPrefix(arg, "-f="):
			if options.file != "" {
				return runOptions{}, usageErr("run accepts exactly one --filename/-f")
			}
			options.file = strings.TrimPrefix(arg, "-f=")
		case arg == "-n" || arg == "--namespace":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return runOptions{}, usageErr(err.Error())
			}
			if namespaceSet {
				return runOptions{}, usageErr("run accepts exactly one namespace")
			}
			options.namespace, i = value, next
			namespaceSet = true
		case strings.HasPrefix(arg, "--namespace="):
			if namespaceSet {
				return runOptions{}, usageErr("run accepts exactly one namespace")
			}
			options.namespace = strings.TrimPrefix(arg, "--namespace=")
			namespaceSet = true
		case strings.HasPrefix(arg, "-n="):
			if namespaceSet {
				return runOptions{}, usageErr("run accepts exactly one namespace")
			}
			options.namespace = strings.TrimPrefix(arg, "-n=")
			namespaceSet = true
		default:
			if strings.HasPrefix(arg, "-") {
				return runOptions{}, usageErr(fmt.Sprintf("unknown run option %q", arg))
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 0 {
		return runOptions{}, usageErr("run does not accept positional arguments; use -f FILE")
	}
	if options.file == "" {
		return runOptions{}, usageErr("run requires -f FILE")
	}
	if err := validateNamespace(options.namespace); err != nil {
		return runOptions{}, usageErr(err.Error())
	}
	return options, nil
}

func parseLogsOptions(args []string) (logsOptions, error) {
	options := logsOptions{namespace: defaultNamespace, container: "agent"}
	namespaceSet := false
	containerSet := false
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--follow":
			options.follow = true
		case arg == "--container":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return logsOptions{}, usageErr(err.Error())
			}
			if containerSet {
				return logsOptions{}, usageErr("logs accepts exactly one --container")
			}
			options.container, i = value, next
			containerSet = true
		case strings.HasPrefix(arg, "--container="):
			if containerSet {
				return logsOptions{}, usageErr("logs accepts exactly one --container")
			}
			options.container = strings.TrimPrefix(arg, "--container=")
			containerSet = true
		case arg == "-n" || arg == "--namespace":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return logsOptions{}, usageErr(err.Error())
			}
			if namespaceSet {
				return logsOptions{}, usageErr("logs accepts exactly one namespace")
			}
			options.namespace, i = value, next
			namespaceSet = true
		case strings.HasPrefix(arg, "--namespace="):
			if namespaceSet {
				return logsOptions{}, usageErr("logs accepts exactly one namespace")
			}
			options.namespace = strings.TrimPrefix(arg, "--namespace=")
			namespaceSet = true
		case strings.HasPrefix(arg, "-n="):
			if namespaceSet {
				return logsOptions{}, usageErr("logs accepts exactly one namespace")
			}
			options.namespace = strings.TrimPrefix(arg, "-n=")
			namespaceSet = true
		default:
			if strings.HasPrefix(arg, "-") {
				return logsOptions{}, usageErr(fmt.Sprintf("unknown logs option %q", arg))
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 1 {
		return logsOptions{}, usageErr("logs requires exactly one RUN name")
	}
	if err := validateName(positionals[0], "run"); err != nil {
		return logsOptions{}, usageErr(err.Error())
	}
	if err := validateNamespace(options.namespace); err != nil {
		return logsOptions{}, usageErr(err.Error())
	}
	if options.container != "agent" && options.container != "broker" && options.container != "verify" {
		return logsOptions{}, usageErr("--container must be one of agent, broker, or verify")
	}
	options.run = positionals[0]
	return options, nil
}

func parseCancelOptions(args []string) (cancelOptions, error) {
	options := cancelOptions{namespace: defaultNamespace}
	namespaceSet := false
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-n" || arg == "--namespace":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return cancelOptions{}, usageErr(err.Error())
			}
			if namespaceSet {
				return cancelOptions{}, usageErr("cancel accepts exactly one namespace")
			}
			options.namespace, i = value, next
			namespaceSet = true
		case strings.HasPrefix(arg, "--namespace="):
			if namespaceSet {
				return cancelOptions{}, usageErr("cancel accepts exactly one namespace")
			}
			options.namespace = strings.TrimPrefix(arg, "--namespace=")
			namespaceSet = true
		case strings.HasPrefix(arg, "-n="):
			if namespaceSet {
				return cancelOptions{}, usageErr("cancel accepts exactly one namespace")
			}
			options.namespace = strings.TrimPrefix(arg, "-n=")
			namespaceSet = true
		default:
			if strings.HasPrefix(arg, "-") {
				return cancelOptions{}, usageErr(fmt.Sprintf("unknown cancel option %q", arg))
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 1 {
		return cancelOptions{}, usageErr("cancel requires exactly one RUN name")
	}
	if err := validateName(positionals[0], "run"); err != nil {
		return cancelOptions{}, usageErr(err.Error())
	}
	if err := validateNamespace(options.namespace); err != nil {
		return cancelOptions{}, usageErr(err.Error())
	}
	options.run = positionals[0]
	return options, nil
}

func parseReviewOptions(args []string) (reviewOptions, error) {
	options := reviewOptions{namespace: defaultNamespace}
	namespaceSet := false
	classificationSet := false
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--diff-good":
			if classificationSet {
				return reviewOptions{}, usageErr("review accepts exactly one of --diff-good or --diff-bad")
			}
			options.classification, classificationSet = "diff-good", true
		case arg == "--diff-bad":
			if classificationSet {
				return reviewOptions{}, usageErr("review accepts exactly one of --diff-good or --diff-bad")
			}
			options.classification, classificationSet = "diff-bad", true
		case arg == "-n" || arg == "--namespace":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return reviewOptions{}, usageErr(err.Error())
			}
			if namespaceSet {
				return reviewOptions{}, usageErr("review accepts exactly one namespace")
			}
			options.namespace, i = value, next
			namespaceSet = true
		case strings.HasPrefix(arg, "--namespace="):
			if namespaceSet {
				return reviewOptions{}, usageErr("review accepts exactly one namespace")
			}
			options.namespace, namespaceSet = strings.TrimPrefix(arg, "--namespace="), true
		case strings.HasPrefix(arg, "-n="):
			if namespaceSet {
				return reviewOptions{}, usageErr("review accepts exactly one namespace")
			}
			options.namespace, namespaceSet = strings.TrimPrefix(arg, "-n="), true
		default:
			if strings.HasPrefix(arg, "-") {
				return reviewOptions{}, usageErr(fmt.Sprintf("unknown review option %q", arg))
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 1 {
		return reviewOptions{}, usageErr("review requires exactly one RUN name")
	}
	if !classificationSet {
		return reviewOptions{}, usageErr("review requires exactly one of --diff-good or --diff-bad")
	}
	if err := validateName(positionals[0], "run"); err != nil {
		return reviewOptions{}, usageErr(err.Error())
	}
	if err := validateNamespace(options.namespace); err != nil {
		return reviewOptions{}, usageErr(err.Error())
	}
	options.run = positionals[0]
	return options, nil
}

func parseMatrixOptions(args []string) (matrixOptions, error) {
	options := matrixOptions{namespace: defaultNamespace, output: "table"}
	namespaceSet, repositorySet, gateSet, outputSet := false, false, false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-n" || arg == "--namespace":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return matrixOptions{}, usageErr(err.Error())
			}
			if namespaceSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one namespace")
			}
			options.namespace, i, namespaceSet = value, next, true
		case strings.HasPrefix(arg, "--namespace="):
			if namespaceSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one namespace")
			}
			options.namespace, namespaceSet = strings.TrimPrefix(arg, "--namespace="), true
		case strings.HasPrefix(arg, "-n="):
			if namespaceSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one namespace")
			}
			options.namespace, namespaceSet = strings.TrimPrefix(arg, "-n="), true
		case arg == "--repo" || arg == "--repository":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return matrixOptions{}, usageErr(err.Error())
			}
			if repositorySet {
				return matrixOptions{}, usageErr("matrix accepts exactly one repository filter")
			}
			options.repository, i, repositorySet = value, next, true
		case strings.HasPrefix(arg, "--repo="):
			if repositorySet {
				return matrixOptions{}, usageErr("matrix accepts exactly one repository filter")
			}
			options.repository, repositorySet = strings.TrimPrefix(arg, "--repo="), true
		case arg == "--gate":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return matrixOptions{}, usageErr(err.Error())
			}
			if gateSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one Gate filter")
			}
			options.gate, i, gateSet = value, next, true
		case strings.HasPrefix(arg, "--gate="):
			if gateSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one Gate filter")
			}
			options.gate, gateSet = strings.TrimPrefix(arg, "--gate="), true
		case arg == "--output":
			value, next, err := requiredOptionValue(args, i, arg)
			if err != nil {
				return matrixOptions{}, usageErr(err.Error())
			}
			if outputSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one output format")
			}
			options.output, i, outputSet = value, next, true
		case strings.HasPrefix(arg, "--output="):
			if outputSet {
				return matrixOptions{}, usageErr("matrix accepts exactly one output format")
			}
			options.output, outputSet = strings.TrimPrefix(arg, "--output="), true
		default:
			return matrixOptions{}, usageErr(fmt.Sprintf("unknown matrix option %q", arg))
		}
	}
	if err := validateNamespace(options.namespace); err != nil {
		return matrixOptions{}, usageErr(err.Error())
	}
	if options.repository != "" && !strings.HasPrefix(options.repository, "github.com/") {
		return matrixOptions{}, usageErr("matrix repository filter must start with github.com/")
	}
	if options.gate != "" && len(validation.IsDNS1123Subdomain(options.gate)) != 0 {
		return matrixOptions{}, usageErr("matrix Gate filter must be a DNS name")
	}
	if options.output != "table" && options.output != "json" {
		return matrixOptions{}, usageErr("matrix output must be table or json")
	}
	return options, nil
}

func requiredOptionValue(args []string, index int, flag string) (string, int, error) {
	if index+1 >= len(args) || args[index+1] == "" || args[index+1] == "--" || strings.HasPrefix(args[index+1], "-") {
		return "", index, fmt.Errorf("option %s requires a value", flag)
	}
	return args[index+1], index + 1, nil
}

func validateName(value, kind string) error {
	if value == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if problems := validation.IsDNS1123Subdomain(value); len(problems) > 0 {
		return fmt.Errorf("invalid %s name %q: %s", kind, value, strings.Join(problems, "; "))
	}
	return nil
}

func validateNamespace(value string) error {
	if value == "" {
		return fmt.Errorf("namespace is required")
	}
	if problems := validation.IsDNS1123Label(value); len(problems) > 0 {
		return fmt.Errorf("invalid namespace %q: %s", value, strings.Join(problems, "; "))
	}
	return nil
}

func usageErr(message string) error { return &usageError{message: message} }

func usageText() string {
	return "usage:\n  kubectl agw run -f FILE [-n NAMESPACE]\n  kubectl agw logs [-f] RUN [-n NAMESPACE] [--container agent|broker|verify]\n  kubectl agw cancel RUN [-n NAMESPACE]\n  kubectl agw review RUN --diff-good|--diff-bad [-n NAMESPACE]\n  kubectl agw matrix [-n NAMESPACE] [--repo REPO] [--gate GATE] [--output table|json]"
}
