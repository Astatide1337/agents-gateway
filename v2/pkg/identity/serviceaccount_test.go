package identity

import (
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
)

func TestCredentialAndShortLivedExchange(t *testing.T) {
	credential, err := NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyCredential(credential.Secret, credential.Digest) || VerifyCredential(credential.Secret+"x", credential.Digest) {
		t.Fatal("credential verification boundary failed")
	}
	_, private, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer("https://gateway.example", "agents-gateway", "key-1", private)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	issuer.now = func() time.Time { return now }
	token, err := issuer.Issue(ServiceAccount{ID: "ci", ProjectRoles: map[string]authz.Role{"org/project": authz.RoleProjectEditor}}, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := issuer.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.ID != "ci" || principal.ProjectRoles["org/project"] != authz.RoleProjectEditor {
		t.Fatalf("principal=%#v", principal)
	}
	issuer.now = func() time.Time { return now.Add(11 * time.Minute) }
	if _, err := issuer.Verify(token); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestRejectsTamperingAndLongTTL(t *testing.T) {
	_, private, _ := GenerateSigningKey()
	issuer, _ := NewIssuer("issuer", "audience", "key", private)
	if _, err := issuer.Issue(ServiceAccount{ID: "ci"}, time.Hour); err == nil {
		t.Fatal("long TTL accepted")
	}
	token, err := issuer.Issue(ServiceAccount{ID: "ci"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if _, err := issuer.Verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("tampered token accepted")
	}
}
