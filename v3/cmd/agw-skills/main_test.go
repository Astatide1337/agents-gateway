package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigStrictAndBounded(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	skillsRoot := filepath.Join(root, "skills")
	if err := os.Mkdir(skillsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"AGW_SKILLS_JSON":     fmt.Sprintf(`[{"name":"design","ref":"catalog/design","digest":"sha256:%s"}]`, strings.Repeat("a", 64)),
		"AGW_SKILLS_ENDPOINT": "https://skills.example.test/mcp", "AGW_SKILLS_DIR": skillsRoot, "AGW_SKILLS_TOKEN_FILE": token,
	}
	getenv := func(key string) string { return values[key] }
	refs, _, _, _, err := configFromEnv(getenv)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs=%#v err=%v", refs, err)
	}
	values["AGW_SKILLS_JSON"] = `[{"name":"design","name":"duplicate"}]`
	if _, _, _, _, err := configFromEnv(getenv); err == nil {
		t.Fatal("duplicate JSON key was accepted")
	}
}

func TestCredentialRejectsSymlinkAndWhitespace(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("two words"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredential(token); err == nil {
		t.Fatal("credential with whitespace was accepted")
	}
	if err := os.WriteFile(token, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(token, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredential(link); err == nil {
		t.Fatal("symlink credential was accepted")
	}
}
