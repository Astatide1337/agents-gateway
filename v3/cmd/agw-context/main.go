// agw-context materializes and verifies the immutable ContextPack before the
// network airlock and before the agent starts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
)

func main() {
	if err := run(context.Background(), os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "agw-context: materialization failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	config, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	_, err = contextmaterializer.Run(ctx, config)
	return err
}

func configFromEnv(getenv func(string) string) (contextmaterializer.Config, error) {
	if getenv == nil {
		return contextmaterializer.Config{}, errors.New("environment reader is required")
	}
	config := contextmaterializer.Config{
		InputJSON:          getenv("AGW_CONTEXT_INPUT_JSON"),
		ExpectedRunUID:     getenv("AGW_RUN_UID"),
		ExpectedSpecDigest: getenv("AGW_CONTEXT_EXPECTED_SPEC_DIGEST"),
		ExpectedBaseSHA:    getenv("AGW_CONTEXT_EXPECTED_BASE_SHA"),
		BaseDir:            getenv("AGW_CONTEXT_BASE_DIR"),
		SkillsDir:          getenv("AGW_CONTEXT_SKILLS_DIR"),
		OutputDir:          getenv("AGW_CONTEXT_OUTPUT_DIR"),
	}
	if config.BaseDir == "" {
		config.BaseDir = contextmaterializer.DefaultBaseDir
	}
	if config.SkillsDir == "" {
		config.SkillsDir = contextmaterializer.DefaultSkillsDir
	}
	if config.OutputDir == "" {
		config.OutputDir = contextmaterializer.DefaultOutputDir
	}
	for _, value := range []string{config.BaseDir, config.SkillsDir, config.OutputDir} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return contextmaterializer.Config{}, errors.New("context directory paths must be absolute and canonical")
		}
	}
	adapterConfig, configured, err := contextmaterializer.LocalSymbolAdapterConfigFromEnv(getenv)
	if err != nil {
		return contextmaterializer.Config{}, err
	}
	if configured {
		provider, err := contextmaterializer.NewLocalSymbolProvider(adapterConfig)
		if err != nil {
			return contextmaterializer.Config{}, err
		}
		config.SymbolProvider = provider
	}
	return config, nil
}
