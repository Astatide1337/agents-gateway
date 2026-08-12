// agw-skills materializes digest-pinned skills before the network airlock.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/pkg/skills"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const maxCredentialBytes = 4096

func main() {
	if err := run(context.Background(), os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "agw-skills: materialization failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	refs, endpoint, root, tokenPath, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	var credential []byte
	var provider skills.CredentialProvider
	if tokenPath != "" {
		credential, err = readCredential(tokenPath)
		if err != nil {
			return err
		}
		defer clearBytes(credential)
		provider = func(context.Context) (string, error) { return string(credential), nil }
	}
	client, err := skills.NewGatewayClient(endpoint, provider)
	if err != nil {
		return errors.New("skills gateway configuration is invalid")
	}
	client.HTTPClient = &httpClientNoRedirect
	for _, ref := range refs {
		if _, err := client.Materialize(ctx, ref.Ref, ref.Digest, filepath.Join(root, ref.Name)); err != nil {
			return errors.New("a skill could not be materialized and verified")
		}
	}
	if err := os.Chmod(root, 0555); err != nil {
		return errors.New("skills root could not be sealed")
	}
	return nil
}

var httpClientNoRedirect = http.Client{
	Timeout:       2 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func configFromEnv(getenv func(string) string) ([]v1alpha1.SkillRef, string, string, string, error) {
	if getenv == nil {
		return nil, "", "", "", errors.New("environment reader is required")
	}
	raw := []byte(getenv("AGW_SKILLS_JSON"))
	if len(raw) == 0 || len(raw) > 128<<10 {
		return nil, "", "", "", errors.New("skills contract is missing or oversized")
	}
	normalized, err := strictjson.Normalize(raw)
	if err != nil {
		return nil, "", "", "", errors.New("skills contract is not strict JSON")
	}
	var refs []v1alpha1.SkillRef
	decoder := json.NewDecoder(strings.NewReader(string(normalized)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&refs); err != nil || len(refs) == 0 || len(refs) > 32 {
		return nil, "", "", "", errors.New("skills contract is invalid")
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if !safeName(ref.Name) || ref.Ref == "" || len(ref.Ref) > 4096 || !validDigest(ref.Digest) {
			return nil, "", "", "", errors.New("skill reference is invalid")
		}
		if _, exists := seen[ref.Name]; exists {
			return nil, "", "", "", errors.New("skill names must be unique")
		}
		seen[ref.Name] = struct{}{}
	}
	endpoint, root, tokenPath := getenv("AGW_SKILLS_ENDPOINT"), getenv("AGW_SKILLS_DIR"), getenv("AGW_SKILLS_TOKEN_FILE")
	if endpoint == "" || !filepath.IsAbs(root) || root == "/" || (tokenPath != "" && !filepath.IsAbs(tokenPath)) {
		return nil, "", "", "", errors.New("skills endpoint, root, or credential path is invalid")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", "", "", errors.New("skills root is not a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		return nil, "", "", "", errors.New("skills root must be empty")
	}
	return refs, endpoint, root, tokenPath, nil
}

func readCredential(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxCredentialBytes {
		return nil, errors.New("skills credential file is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("skills credential cannot be opened")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil || len(body) > maxCredentialBytes {
		return nil, errors.New("skills credential cannot be read safely")
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 || strings.ContainsAny(string(body), "\x00\r\n\t ") {
		return nil, errors.New("skills credential is malformed")
	}
	return body, nil
}

func safeName(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
			return false
		}
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[7:] {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
