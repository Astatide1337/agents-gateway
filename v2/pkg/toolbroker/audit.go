package toolbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	maxAuditIdentity = 512
	maxAuditDigest   = 80
)

var ErrAuditUnavailable = errors.New("tool call audit is unavailable")

// AuditRecord is the broker's privacy-preserving audit contract. Arguments,
// credentials, effect keys, and upstream response bodies are intentionally
// not representable here. The factory adapts this contract to durable store
// events.
type AuditRecord struct {
	OrganizationID string
	ProjectID      string
	UserID         string
	RunID          string
	Server         string
	Tool           string
	Resource       string
	Phase          string
	Outcome        string
	Decision       string
	RequestDigest  string
	EffectDigest   string
}

type AuditSink interface {
	AppendAudit(context.Context, AuditRecord) error
}

func (r AuditRecord) validate() error {
	for _, value := range []string{r.OrganizationID, r.ProjectID, r.UserID, r.RunID, r.Server, r.Tool, r.Phase, r.Outcome, r.Decision} {
		if value == "" || len(value) > maxAuditIdentity || strings.IndexByte(value, 0) >= 0 {
			return errors.New("audit identity is invalid")
		}
	}
	if len(r.Resource) > maxAuditIdentity || strings.IndexByte(r.Resource, 0) >= 0 {
		return errors.New("audit resource is invalid")
	}
	for _, value := range []string{r.RequestDigest, r.EffectDigest} {
		if value != "" && !validAuditDigest(value) {
			return errors.New("audit digest is invalid")
		}
	}
	if r.Phase != "authorization" && r.Phase != "outcome" {
		return errors.New("audit phase is invalid")
	}
	if r.Phase == "outcome" {
		switch r.Outcome {
		case "read", "succeeded", "failed", "unknown", "duplicate":
		default:
			return errors.New("audit outcome is invalid")
		}
	}
	return nil
}

func validAuditDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || len(value) > maxAuditDigest || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func effectDigest(effectKey string) string {
	if effectKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(effectKey))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func identityDigest(server, tool, resource string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{server, tool, resource}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
