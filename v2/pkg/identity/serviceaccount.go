// Package identity contains workload identity primitives. Human identity is
// verified by OIDC; service credentials are exchanged for short-lived signed
// tokens and are never accepted as API bearer tokens directly.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
)

const credentialPrefix = "agw_sa_"

type ServiceAccount struct {
	ID                string
	OrganizationRoles map[string]authz.Role
	ProjectRoles      map[string]authz.Role
	Disabled          bool
}

type Credential struct{ Secret, Digest string }

func NewCredential() (Credential, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Credential{}, err
	}
	secret := credentialPrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	return Credential{Secret: secret, Digest: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

func VerifyCredential(secret, digest string) bool {
	if !strings.HasPrefix(secret, credentialPrefix) {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(digest)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	actual := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(actual[:], expected) == 1
}

type Issuer struct {
	Issuer, Audience, KeyID string
	private                 ed25519.PrivateKey
	public                  ed25519.PublicKey
	now                     func() time.Time
}

func NewIssuer(issuer, audience, keyID string, private ed25519.PrivateKey) (*Issuer, error) {
	if issuer == "" || audience == "" || keyID == "" {
		return nil, errors.New("issuer, audience, and key ID are required")
	}
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return &Issuer{Issuer: issuer, Audience: audience, KeyID: keyID, private: append(ed25519.PrivateKey(nil), private...), public: append(ed25519.PublicKey(nil), public...), now: func() time.Time { return time.Now().UTC() }}, nil
}

type tokenClaims struct {
	Issuer            string                `json:"iss"`
	Audience          string                `json:"aud"`
	Subject           string                `json:"sub"`
	Type              string                `json:"typ"`
	IssuedAt          int64                 `json:"iat"`
	ExpiresAt         int64                 `json:"exp"`
	JWTID             string                `json:"jti"`
	OrganizationRoles map[string]authz.Role `json:"org_roles,omitempty"`
	ProjectRoles      map[string]authz.Role `json:"project_roles,omitempty"`
}

func (i *Issuer) Issue(account ServiceAccount, ttl time.Duration) (string, error) {
	if account.ID == "" || account.Disabled {
		return "", errors.New("service account is unavailable")
	}
	if ttl <= 0 || ttl > 15*time.Minute {
		return "", errors.New("service token TTL must be between zero and 15 minutes")
	}
	now := i.now()
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", err
	}
	header := map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": i.KeyID}
	claims := tokenClaims{Issuer: i.Issuer, Audience: i.Audience, Subject: account.ID, Type: string(authz.PrincipalServiceAccount), IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix(), JWTID: base64.RawURLEncoding.EncodeToString(jtiBytes), OrganizationRoles: account.OrganizationRoles, ProjectRoles: account.ProjectRoles}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	unsigned := encode(headerJSON) + "." + encode(claimsJSON)
	signature := ed25519.Sign(i.private, []byte(unsigned))
	return unsigned + "." + encode(signature), nil
}

func (i *Issuer) Verify(token string) (authz.Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return authz.Principal{}, errors.New("malformed service token")
	}
	headerBytes, err := decode(parts[0])
	if err != nil {
		return authz.Principal{}, errors.New("malformed token header")
	}
	var header map[string]string
	if json.Unmarshal(headerBytes, &header) != nil || header["alg"] != "EdDSA" || header["typ"] != "JWT" || header["kid"] != i.KeyID {
		return authz.Principal{}, errors.New("invalid token header")
	}
	signature, err := decode(parts[2])
	if err != nil || !ed25519.Verify(i.public, []byte(parts[0]+"."+parts[1]), signature) {
		return authz.Principal{}, errors.New("invalid token signature")
	}
	claimsBytes, err := decode(parts[1])
	if err != nil {
		return authz.Principal{}, errors.New("malformed token claims")
	}
	var claims tokenClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return authz.Principal{}, errors.New("malformed token claims")
	}
	now := i.now().Unix()
	if claims.Issuer != i.Issuer || claims.Audience != i.Audience || claims.Type != string(authz.PrincipalServiceAccount) || claims.Subject == "" || claims.JWTID == "" {
		return authz.Principal{}, errors.New("invalid service token claims")
	}
	if claims.IssuedAt > now+30 || claims.ExpiresAt <= now || claims.ExpiresAt-claims.IssuedAt > int64((15*time.Minute).Seconds()) {
		return authz.Principal{}, errors.New("service token is expired or has invalid lifetime")
	}
	return authz.Principal{ID: claims.Subject, Type: authz.PrincipalServiceAccount, OrgRoles: claims.OrganizationRoles, ProjectRoles: claims.ProjectRoles}, nil
}

func GenerateSigningKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
func encode(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
func decode(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	return decoded, nil
}
