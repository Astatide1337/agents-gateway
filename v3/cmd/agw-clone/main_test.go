package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFromEnvFailsClosed(t *testing.T) {
	values := map[string]string{
		"AGW_REPO": "github.com/Astatide1337/jobmark", "AGW_BASE_SHA": strings.Repeat("a", 40),
		"AGW_WORKSPACE": "/workspace", "AGW_GIT_TOKEN_FILE": "/run/agw/clone/token", "AGW_DEPTH": "1",
	}
	getenv := func(key string) string { return values[key] }
	if cfg, err := configFromEnv(getenv); err != nil || cfg.depth != 1 {
		t.Fatalf("valid config = %#v, %v", cfg, err)
	}
	for key, invalid := range map[string]string{"AGW_REPO": "https://github.com/a/b", "AGW_BASE_SHA": "main", "AGW_DEPTH": "0", "AGW_WORKSPACE": "relative"} {
		original := values[key]
		values[key] = invalid
		if _, err := configFromEnv(getenv); err == nil {
			t.Fatalf("invalid %s was accepted", key)
		}
		values[key] = original
	}
}

func TestReadTokenRejectsLinksAndBounds(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readToken(token)
	if err != nil || string(got) != "secret" {
		t.Fatalf("token=%q err=%v", got, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(token, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(link); err == nil {
		t.Fatal("symlink credential was accepted")
	}
	if err := os.WriteFile(token, []byte(strings.Repeat("x", maxTokenBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(token); err == nil {
		t.Fatal("oversize credential was accepted")
	}
}

func TestSealBaseAndAssignRepoOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership assertion requires namespace/root execution")
	}
	root := t.TempDir()
	file := filepath.Join(root, "run.sh")
	if err := os.WriteFile(file, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := makeTreeReadOnly(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("sealed mode=%v err=%v", info.Mode(), err)
	}
}
