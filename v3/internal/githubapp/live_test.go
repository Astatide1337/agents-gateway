package githubapp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGitHubAppInstallationLive is an opt-in, read-only boundary check. It
// validates the real App key, installation, repository scope, and commit
// resolution without printing or persisting the installation token.
func TestGitHubAppInstallationLive(t *testing.T) {
	if os.Getenv("AGW_GITHUB_APP_LIVE") != "1" {
		t.Skip("set AGW_GITHUB_APP_LIVE=1 to exercise the real GitHub App installation")
	}
	appID, err := strconv.ParseInt(os.Getenv("AGW_GITHUB_APP_ID"), 10, 64)
	if err != nil || appID <= 0 {
		t.Fatal("AGW_GITHUB_APP_ID must be a positive integer")
	}
	installationID, err := strconv.ParseInt(os.Getenv("AGW_GITHUB_INSTALLATION_ID"), 10, 64)
	if err != nil || installationID <= 0 {
		t.Fatal("AGW_GITHUB_INSTALLATION_ID must be a positive integer")
	}
	keyPath := os.Getenv("AGW_GITHUB_PRIVATE_KEY_FILE")
	if keyPath == "" {
		t.Fatal("AGW_GITHUB_PRIVATE_KEY_FILE is required")
	}
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key file: %v", err)
	}
	repository := os.Getenv("AGW_GITHUB_REPOSITORY")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 {
		t.Fatal("AGW_GITHUB_REPOSITORY must be owner/name")
	}
	repo := Repository{Owner: parts[0], Name: parts[1]}
	minter, err := New(Config{AppID: appID, InstallationID: installationID, PrivateKeyPEM: privateKey})
	if err != nil {
		t.Fatalf("construct GitHub App minter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := minter.Mint(ctx, repo, PermissionCloneRead)
	if err != nil {
		t.Fatalf("mint repository-scoped installation token: %v", err)
	}
	if token.IsZero() || token.Repository() != repo || token.PermissionProfile() != PermissionCloneRead || !token.ExpiresAt().After(time.Now()) {
		t.Fatal("GitHub App returned an invalid scoped installation token")
	}
	defaultBranch, err := liveDefaultBranch(ctx, token, repo)
	if err != nil {
		t.Fatalf("resolve repository default branch: %v", err)
	}
	commit, err := minter.ResolveCommit(ctx, token, repo, defaultBranch)
	if err != nil {
		t.Fatalf("resolve repository %s commit: %v", defaultBranch, err)
	}
	if len(commit) != 40 {
		t.Fatalf("resolved commit length=%d, want 40", len(commit))
	}
	if _, err := hex.DecodeString(commit); err != nil {
		t.Fatalf("resolved commit is not hexadecimal: %v", err)
	}
	t.Logf("GitHub App installation validated for %s; %s resolved to %s; token expires %s", repo.FullName(), defaultBranch, commit, token.ExpiresAt().UTC().Format(time.RFC3339))
}

func liveDefaultBranch(ctx context.Context, token InstallationToken, repo Repository) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultAPIBaseURL+"/repos/"+url.PathEscape(repo.Owner)+"/"+url.PathEscape(repo.Name), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token.Value())
	request.Header.Set("User-Agent", "agents-gateway-v3-live-test")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil || response == nil || response.Body == nil {
		return "", ErrRequest
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return "", ErrInvalidResponse
	}
	var payload struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || !validReference(payload.DefaultBranch) {
		return "", ErrInvalidResponse
	}
	return payload.DefaultBranch, nil
}
