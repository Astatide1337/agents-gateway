package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
)

func TestConfigFromEnvUsesBoundedDefaults(t *testing.T) {
	values := map[string]string{
		"AGW_CONTEXT_INPUT_JSON":           "{}",
		"AGW_RUN_UID":                      "run-uid",
		"AGW_CONTEXT_EXPECTED_SPEC_DIGEST": "sha256:" + strings.Repeat("a", 64),
		"AGW_CONTEXT_EXPECTED_BASE_SHA":    strings.Repeat("b", 40),
	}
	config, err := configFromEnv(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseDir == "" || config.SkillsDir == "" || config.OutputDir == "" {
		t.Fatalf("defaults were not applied: %#v", config)
	}
}

func TestConfigFromEnvRejectsNonCanonicalPath(t *testing.T) {
	values := map[string]string{
		"AGW_CONTEXT_INPUT_JSON":           "{}",
		"AGW_RUN_UID":                      "run-uid",
		"AGW_CONTEXT_EXPECTED_SPEC_DIGEST": "sha256:" + strings.Repeat("a", 64),
		"AGW_CONTEXT_EXPECTED_BASE_SHA":    strings.Repeat("b", 40),
		"AGW_CONTEXT_BASE_DIR":             "/workspace/../base",
		"AGW_CONTEXT_SKILLS_DIR":           "/opt/agw/skills",
		"AGW_CONTEXT_OUTPUT_DIR":           "/opt/agw/context",
	}
	if _, err := configFromEnv(func(name string) string { return values[name] }); err == nil {
		t.Fatal("non-canonical context path was accepted")
	}
}

func TestConfigFromEnvWiresExplicitLocalSymbolAdapter(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"AGW_CONTEXT_INPUT_JSON":                      "{}",
		"AGW_RUN_UID":                                 "run-uid",
		"AGW_CONTEXT_EXPECTED_SPEC_DIGEST":            "sha256:" + strings.Repeat("a", 64),
		"AGW_CONTEXT_EXPECTED_BASE_SHA":               strings.Repeat("b", 40),
		"AGW_CONTEXT_SYMBOL_ADAPTER":                  executable,
		"AGW_CONTEXT_SYMBOL_ADAPTER_SHA256":           digestExecutableForTest(t, executable),
		"AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON":        `["-test.run=TestLSPHelperProcessGood"]`,
		"AGW_CONTEXT_SYMBOL_ADAPTER_MAX_ITEMS":        "16",
		"AGW_CONTEXT_SYMBOL_ADAPTER_MAX_MESSAGES":     "32",
		"AGW_CONTEXT_SYMBOL_ADAPTER_MAX_OUTPUT_BYTES": "65536",
	}
	config, err := configFromEnv(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.SymbolProvider == nil {
		t.Fatal("explicit local symbol adapter was not wired into context config")
	}
}

func TestConfigFromEnvFailsClosedForUnavailableExplicitSymbolAdapter(t *testing.T) {
	values := map[string]string{
		"AGW_CONTEXT_INPUT_JSON":            "{}",
		"AGW_RUN_UID":                       "run-uid",
		"AGW_CONTEXT_EXPECTED_SPEC_DIGEST":  "sha256:" + strings.Repeat("a", 64),
		"AGW_CONTEXT_EXPECTED_BASE_SHA":     strings.Repeat("b", 40),
		"AGW_CONTEXT_SYMBOL_ADAPTER":        "/does/not/exist/agw-lsp",
		"AGW_CONTEXT_SYMBOL_ADAPTER_SHA256": "sha256:" + strings.Repeat("0", 64),
	}
	_, err := configFromEnv(func(name string) string { return values[name] })
	if err == nil || !errors.Is(err, contextmaterializer.ErrProducerUnavailable) {
		t.Fatalf("unavailable adapter error=%v, want producer unavailable", err)
	}
}

func digestExecutableForTest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
