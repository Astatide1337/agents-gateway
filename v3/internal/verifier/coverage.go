package verifier

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

const goCoverageProfileMaxBytes = 64 << 20

type goCoverageStats struct {
	covered int64
	total   int64
}

// computeCoverageDelta is deliberately an adapter, not a parser for Gate
// command output. The Go tool owns the coverage profile and the verifier reads
// only that profile after the tool exits. Repository stdout/stderr, including
// strings that look like the former AGW_COVERAGE_DELTA_V1 marker, never enters
// this calculation.
func computeCoverageDelta(ctx context.Context, cfg Config) (string, error) {
	if cfg.Adapter != v1alpha1.GateAdapterGo {
		return "", ErrCoverage
	}
	coverageRoot, err := os.MkdirTemp(cfg.Workspace, ".agw-coverage-")
	if err != nil {
		return "", ErrCoverage
	}
	defer os.RemoveAll(coverageRoot)
	if err := os.Chmod(coverageRoot, 0700); err != nil {
		return "", ErrCoverage
	}
	base, err := runGoCoverage(ctx, cfg, cfg.BasePath, coverageRoot, "base")
	if err != nil {
		return "", err
	}
	patched, err := runGoCoverage(ctx, cfg, cfg.RepoPath, coverageRoot, "patched")
	if err != nil {
		return "", err
	}
	baseStats, err := parseGoCoverageProfile(base, cfg.BasePath)
	if err != nil {
		return "", fmt.Errorf("%w: base profile: %v", ErrCoverage, err)
	}
	patchedStats, err := parseGoCoverageProfile(patched, cfg.RepoPath)
	if err != nil {
		return "", fmt.Errorf("%w: patched profile: %v", ErrCoverage, err)
	}
	if baseStats.total == 0 || patchedStats.total == 0 {
		return "", ErrCoverage
	}

	baseRatio := new(big.Rat).SetFrac(big.NewInt(baseStats.covered), big.NewInt(baseStats.total))
	patchedRatio := new(big.Rat).SetFrac(big.NewInt(patchedStats.covered), big.NewInt(patchedStats.total))
	delta := new(big.Rat).Sub(patchedRatio, baseRatio)
	delta.Mul(delta, big.NewRat(100, 1))
	return normalizeDecimal(delta.FloatString(6)), nil
}

func runGoCoverage(ctx context.Context, cfg Config, cwd, coverageRoot, label string) ([]byte, error) {
	if ctx == nil || cwd == "" || label == "" {
		return nil, ErrCoverage
	}
	if coverageRoot == "" || !pathWithin(cfg.Workspace, coverageRoot) {
		return nil, ErrCoverage
	}
	profile := filepath.Join(coverageRoot, label+".profile")
	command := v1alpha1.VerifyCommand{Argv: []string{
		"go", "test", "-count=1", "-covermode=atomic", "-coverprofile=" + profile, "./...",
	}}
	result := executeCommandWithEnv(ctx, cfg.MaxOutputBytes, cwd, 0, command, map[string]string{
		"GOCACHE":     filepath.Join(coverageRoot, "cache"),
		"GOMODCACHE":  filepath.Join(coverageRoot, "modcache"),
		"GOTOOLCHAIN": "local",
	})
	if result.Operational || result.Observation.ExitCode == nil || *result.Observation.ExitCode != 0 {
		return nil, fmt.Errorf("%w: verifier-owned go test did not pass", ErrCoverage)
	}
	body, err := readBoundedRegular(profile, goCoverageProfileMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: coverage profile unavailable: %v", ErrCoverage, err)
	}
	return body, nil
}

func parseGoCoverageProfile(body []byte, root string) (goCoverageStats, error) {
	var stats goCoverageStats
	if len(body) == 0 || root == "" {
		return stats, ErrCoverage
	}
	modulePath, err := goModulePath(root)
	if err != nil {
		return stats, ErrCoverage
	}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	mode := ""
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if mode == "" {
			if !strings.HasPrefix(line, "mode: ") {
				return stats, ErrCoverage
			}
			mode = strings.TrimSpace(strings.TrimPrefix(line, "mode: "))
			if mode != "set" && mode != "count" && mode != "atomic" {
				return stats, ErrCoverage
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return stats, ErrCoverage
		}
		if err := validateGoProfileLocation(root, modulePath, fields[0]); err != nil {
			return stats, err
		}
		statements, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || statements <= 0 {
			return stats, ErrCoverage
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || count < 0 {
			return stats, ErrCoverage
		}
		if stats.total > (1<<62)-statements {
			return stats, ErrCoverage
		}
		stats.total += statements
		if count > 0 {
			stats.covered += statements
		}
	}
	if err := scanner.Err(); err != nil || lineNumber < 2 || stats.total == 0 {
		return goCoverageStats{}, ErrCoverage
	}
	return stats, nil
}

func goModulePath(root string) (string, error) {
	body, err := readBoundedRegular(filepath.Join(root, "go.mod"), 1<<20)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" && fields[1] != "" && !strings.ContainsAny(fields[1], "\\\x00\r\n\t ") {
			return fields[1], nil
		}
	}
	return "", ErrCoverage
}

func validateGoProfileLocation(root, modulePath, location string) error {
	colon := strings.LastIndexByte(location, ':')
	if colon <= 0 || colon == len(location)-1 {
		return ErrCoverage
	}
	profilePath := location[:colon]
	for _, position := range strings.Split(location[colon+1:], ",") {
		parts := strings.Split(position, ".")
		if len(parts) != 2 {
			return ErrCoverage
		}
		for _, value := range parts {
			parsed, err := strconv.ParseInt(value, 10, 32)
			if err != nil || parsed <= 0 {
				return ErrCoverage
			}
		}
	}
	path := filepath.FromSlash(profilePath)
	if !filepath.IsAbs(path) {
		if profilePath != modulePath && !strings.HasPrefix(profilePath, modulePath+"/") {
			return ErrCoverage
		}
		relative := strings.TrimPrefix(profilePath, modulePath)
		relative = strings.TrimPrefix(relative, "/")
		path = filepath.Join(root, filepath.FromSlash(relative))
	}
	path = filepath.Clean(path)
	if !pathWithin(root, path) {
		return ErrCoverage
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrCoverage
	}
	return nil
}
