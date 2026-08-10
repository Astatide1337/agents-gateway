package skills

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSkillsGatewayResolveAndMaterializeLive(t *testing.T) {
	if os.Getenv("AGW_SKILLS_LIVE") != "1" {
		t.Skip("set AGW_SKILLS_LIVE=1 to exercise the connected Skills Gateway")
	}
	endpoint := strings.TrimSpace(os.Getenv("AGW_SKILLS_LIVE_URL"))
	token := strings.TrimSpace(os.Getenv("AGW_SKILLS_LIVE_TOKEN"))
	if endpoint == "" || token == "" {
		t.Fatal("AGW_SKILLS_LIVE_URL and AGW_SKILLS_LIVE_TOKEN are required")
	}
	client, err := NewGatewayClient(endpoint, func(context.Context) (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	skillID := "agent-manager/matt-pocock/codebase-design"
	resolved, err := client.Resolve(ctx, skillID)
	if err != nil {
		t.Fatalf("resolve live skill: %v", err)
	}
	if !validGatewayDigest(resolved.ContentDigest) || resolved.SkillID != skillID {
		t.Fatalf("invalid resolved skill metadata: %#v", resolved)
	}
	t.Logf("resolved immutable skill %s at %s", resolved.SkillID, resolved.ContentDigest)
	materializedRoot := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(materializedRoot, func(path string, entry os.DirEntry, _ error) error {
			_ = os.Chmod(path, 0700)
			return nil
		})
	})
	destination := filepath.Join(materializedRoot, "skill")
	materialized, err := client.Materialize(ctx, skillID, resolved.ContentDigest, destination)
	if err != nil {
		t.Fatalf("materialize live skill: %v", err)
	}
	if materialized.ContentDigest != resolved.ContentDigest {
		t.Fatal("materialized content did not match the pinned digest")
	}
	skillDocument, err := os.ReadFile(filepath.Join(destination, "SKILL.md"))
	if err != nil {
		t.Fatalf("read materialized SKILL.md: %v", err)
	}
	t.Logf("materialized SKILL.md proof sha256:%x", sha256.Sum256(skillDocument))
	seen := 0
	err = filepath.WalkDir(destination, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if entry.IsDir() {
			if info.Mode().Perm() != 0555 {
				t.Fatalf("live skill directory %q mode=%o", path, info.Mode().Perm())
			}
			return nil
		}
		seen++
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0444 {
			t.Fatalf("live skill file %q has unsafe mode %v", path, info.Mode())
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), token) {
			t.Fatal("Skills Gateway credential was materialized")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 2 {
		t.Fatalf("live skill materialized only %d files", seen)
	}
	badDestination := filepath.Join(t.TempDir(), "wrong-digest")
	if _, err := client.Materialize(ctx, skillID, "sha256:"+strings.Repeat("0", 64), badDestination); err == nil {
		t.Fatal("live skill accepted a stale digest")
	}
	if _, err := os.Lstat(badDestination); !os.IsNotExist(err) {
		t.Fatal("failed live materialization left a destination behind")
	}
}
