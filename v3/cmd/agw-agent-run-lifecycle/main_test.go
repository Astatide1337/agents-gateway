package main

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
)

func TestRunHasOnlyBoundedArgoModes(t *testing.T) {
	for _, mode := range []string{"prepare", "stage", "wait", "handoff", "cleanup"} {
		t.Run(mode, func(t *testing.T) {
			// Missing configuration is expected locally; the important contract
			// is that the mode is dispatched rather than silently accepted as a
			// fixture or fail-closed placeholder.
			err := run(context.Background(), []string{mode}, io.Discard)
			if errors.Is(err, errUsage) {
				t.Fatalf("mode %q was not recognized: %v", mode, err)
			}
		})
	}
}

func TestRuntimeConfigIsLeastPrivilegeForStageAndHandoff(t *testing.T) {
	for _, name := range []string{
		"AGW_SYSTEM_NAMESPACE", "AGW_GITHUB_APP_SECRET", "AGW_ARTIFACT_STS_ROLE_ARN",
		"AGW_ARTIFACT_CREDENTIAL_TTL", "AGW_CLONE_IMAGE", "AGW_SKILLS_IMAGE",
		"AGW_CONTEXT_IMAGE", "AGW_LOCKDOWN_IMAGE", "AGW_BROKER_IMAGE",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("AGW_RUN_GENERATION", "1")
	t.Setenv("AGW_NAMESPACE", "agw-runs")
	t.Setenv("AGW_RUN_NAME", "sample-run")
	t.Setenv("AGW_RUN_UID", "run-uid-1")
	t.Setenv("AGW_SPEC_DIGEST", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("AGW_BASE_SHA", "0123456789012345678901234567890123456789")
	t.Setenv("AGW_WORKFLOW_TEMPLATE", "agw-agent-run-lifecycle")
	t.Setenv("AGW_WORKFLOW_TEMPLATE_UID", "template-uid-1")
	t.Setenv("AGW_WORKFLOW_TEMPLATE_DIGEST", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	t.Setenv("AGW_OBJECT_STORE_BUCKET", "agw-artifacts")
	t.Setenv("AGW_OBJECT_STORE_PREFIX", "agents-gateway/v3")
	t.Setenv("AGW_OBJECT_STORE_REGION", "us-east-1")
	t.Setenv("AGW_OBJECT_STORE_PATH_STYLE", "false")
	t.Setenv("AGW_OBJECT_STORE_MAX_BYTES", "67108864")

	if _, err := loadRuntimeConfig(); err != nil {
		t.Fatalf("stage/handoff runtime config requires prepare-only settings: %v", err)
	}
}

func TestLoadObjectStoreConfigRejectsLimitAboveGeneralCeiling(t *testing.T) {
	t.Setenv("AGW_OBJECT_STORE_BUCKET", "agw-artifacts")
	t.Setenv("AGW_OBJECT_STORE_PREFIX", "agents-gateway/v3")
	t.Setenv("AGW_OBJECT_STORE_REGION", "us-east-1")
	value := strconv.FormatInt(objectstore.GeneralMaxObjectBytes+1, 10)
	t.Setenv("AGW_OBJECT_STORE_MAX_BYTES", value)

	_, err := loadObjectStoreConfig()
	if err == nil || err.Error() != "AGW_OBJECT_STORE_MAX_BYTES is invalid" {
		t.Fatalf("loadObjectStoreConfig error=%v, want bounded invalid-size error", err)
	}
	if strings.Contains(err.Error(), value) {
		t.Fatalf("loadObjectStoreConfig error exposed configured size: %v", err)
	}
}

func TestLoadObjectStoreConfigAcceptsGeneralLimitAtConstructionBoundary(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AGW_OBJECT_STORE_BUCKET", "agw-artifacts")
	t.Setenv("AGW_OBJECT_STORE_PREFIX", "agents-gateway/v3")
	t.Setenv("AGW_OBJECT_STORE_REGION", "us-east-1")
	t.Setenv("AGW_OBJECT_STORE_MAX_BYTES", strconv.FormatInt(objectstore.GeneralMaxObjectBytes, 10))

	config, err := loadObjectStoreConfig()
	if err != nil {
		t.Fatalf("loadObjectStoreConfig() rejected the shared boundary: %v", err)
	}
	if config.MaxObjectBytes != objectstore.GeneralMaxObjectBytes {
		t.Fatalf("parsed object limit=%d, want %d", config.MaxObjectBytes, objectstore.GeneralMaxObjectBytes)
	}
	if _, err := newObjectStore(context.Background(), config); err != nil {
		t.Fatalf("object-store construction rejected a parser-accepted boundary: %v", err)
	}
}

func TestWaitRequiresResolvedPerRunTimeout(t *testing.T) {
	err := runWait(context.Background(), []string{
		"--name", "sandbox", "--namespace", "agw-runs", "--uid", "sandbox-uid", "--condition", "Ready",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "positive per-run --timeout") {
		t.Fatalf("runWait without resolved timeout=%v, want bounded-timeout error", err)
	}
}

func TestRunRejectsProduceAndUnknownModes(t *testing.T) {
	for _, args := range [][]string{{}, {"produce"}, {"fixture"}, {"supervise"}, {"handoff", "extra"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			if err := run(context.Background(), args, io.Discard); !errors.Is(err, errUsage) {
				t.Fatalf("run(%v)=%v, want usage error", args, err)
			}
		})
	}
}

func TestRunRequiresContextAndStderr(t *testing.T) {
	if err := run(nil, []string{"stage"}, io.Discard); !errors.Is(err, errUsage) {
		t.Fatalf("nil context error=%v", err)
	}
	if err := run(context.Background(), []string{"stage"}, nil); !errors.Is(err, errUsage) {
		t.Fatalf("nil stderr error=%v", err)
	}
}
