package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTrustedPublicKeyAcceptsBoundedExplicitForms(t *testing.T) {
	directory := t.TempDir()
	key := ed25519.PublicKey(strings.Repeat("k", ed25519.PublicKeySize))
	forms := map[string][]byte{
		"raw":    key,
		"base64": []byte(base64.RawStdEncoding.EncodeToString(key)),
		"hex":    []byte(strings.Repeat("6b", ed25519.PublicKeySize)),
	}
	for name, body := range forms {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name)
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readTrustedPublicKey(path)
			if err != nil || len(got) != ed25519.PublicKeySize {
				t.Fatalf("key=%x err=%v", got, err)
			}
		})
	}
}

func TestReadTrustedPublicKeyRejectsPEMAndOversize(t *testing.T) {
	directory := t.TempDir()
	for name, body := range map[string][]byte{
		"pem":      []byte("-----BEGIN PUBLIC KEY-----"),
		"oversize": []byte(strings.Repeat("x", maxKeyFileBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name)
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readTrustedPublicKey(path); err == nil {
				t.Fatal("invalid trusted key was accepted")
			}
		})
	}
}

func TestParseOptionsRequiresExplicitInputs(t *testing.T) {
	if _, err := parseOptions("verify", nil, os.Stderr); err == nil {
		t.Fatal("missing required options were accepted")
	}
	options, err := parseOptions("verify", []string{
		"--signed-report", "report.json",
		"--gate-public-key", "gate.pub",
		"--bundle", "bundle.json",
		"--key-ref", "awskms://arn:example/key",
		"--verify-key-ref", "/var/run/agw/cosign.pub",
		"--dry-run",
	}, os.Stderr)
	if err != nil || !options.dryRun || options.verifyKeyRef != "/var/run/agw/cosign.pub" {
		t.Fatalf("options=%#v err=%v", options, err)
	}
}
