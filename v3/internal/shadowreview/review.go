// Package shadowreview defines the small, append-only human-review contract
// used by shadow mode. It deliberately does not sign a human claim. The
// reviewer identity is the Kubernetes API server's authenticated
// SelfSubjectReview result, and the provenance field says exactly that.
package shadowreview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	SchemaVersion       = "agents.astatide.com/shadow-review/v1alpha1"
	ArtifactType        = "shadow-review"
	ConfigMapDataKey    = "review.json"
	ConfigMapNamePrefix = "agw-shadow-review-"
	Provenance          = "kubernetes-authenticated-subject"

	MaxReviewBytes   = 64 << 10
	MaxUsernameBytes = 256
	MaxGroupBytes    = 256
	MaxGroups        = 64
)

var (
	baseSHAPattern    = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
	repositoryPattern = regexp.MustCompile(`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	uidPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
)

var (
	ErrInvalidReview = errors.New("invalid shadow review")
	ErrNonCanonical  = errors.New("shadow review is not canonical JSON")
)

// Classification is the owner's assessment of the actual diff, independent
// of the machine Gate verdict.
type Classification string

const (
	DiffGood Classification = "diff-good"
	DiffBad  Classification = "diff-bad"
)

// Reviewer is copied from the Kubernetes SelfSubjectReview response. It is
// an authenticated API subject at record time, not proof of a human identity
// and not a cryptographic signature.
type Reviewer struct {
	Username string   `json:"username"`
	UID      string   `json:"uid,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}

// Review is the canonical, immutable owner assertion for one terminal shadow
// run. Every field needed to recompute the confusion-matrix cell is inside
// the immutable payload; ConfigMap labels and annotations are discovery hints
// only and must never be treated as evidence.
type Review struct {
	SchemaVersion string `json:"schemaVersion"`
	ArtifactType  string `json:"artifactType"`

	RunUID     string `json:"runUID"`
	Namespace  string `json:"namespace"`
	RunName    string `json:"runName"`
	Repository string `json:"repository"`

	GateRef        string `json:"gateRef"`
	GateName       string `json:"gateName"`
	GateUID        string `json:"gateUID"`
	GateGeneration int64  `json:"gateGeneration"`
	GateMode       string `json:"gateMode"`
	MachineVerdict string `json:"machineVerdict"`

	SpecDigest   string `json:"specDigest"`
	BaseSHA      string `json:"baseSHA"`
	PatchDigest  string `json:"patchDigest"`
	ReportDigest string `json:"reportDigest"`

	DiffClassification Classification `json:"diffClassification"`
	Reviewer           Reviewer       `json:"reviewer"`
	Provenance         string         `json:"provenance"`
}

// CanonicalBytes validates and serializes a review deterministically. The
// review is intentionally unsigned: adding an operator signature here would
// falsely turn an operator-authenticated write into a human attestation.
func CanonicalBytes(input Review) ([]byte, error) {
	normalized, err := normalize(input)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal shadow review: %w", err)
	}
	if len(encoded) > MaxReviewBytes {
		return nil, fmt.Errorf("%w: review exceeds %d bytes", ErrInvalidReview, MaxReviewBytes)
	}
	return encoded, nil
}

// ParseCanonicalBytes rejects unknown fields, trailing JSON, and alternate
// encodings. A stored review is accepted only when its bytes are exactly the
// canonical bytes that validate its content.
func ParseCanonicalBytes(encoded []byte) (Review, error) {
	var zero Review
	if len(encoded) == 0 || len(encoded) > MaxReviewBytes {
		return zero, fmt.Errorf("%w: review size is out of bounds", ErrInvalidReview)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var review Review
	if err := decoder.Decode(&review); err != nil {
		return zero, fmt.Errorf("%w: decode: %v", ErrInvalidReview, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return zero, fmt.Errorf("%w: trailing JSON", ErrNonCanonical)
	}
	canonicalBytes, err := CanonicalBytes(review)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonicalBytes, encoded) {
		return zero, ErrNonCanonical
	}
	return review, nil
}

// Digest returns the content digest of canonical review bytes.
func Digest(review Review) (string, error) {
	body, err := CanonicalBytes(review)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// ConfigMapName is deterministic for the run UID and therefore gives one
// immutable review slot per run. A second classification cannot silently
// replace the first one; callers must compare bytes and report a conflict.
func ConfigMapName(runUID string) (string, error) {
	if !safeUID(runUID) {
		return "", fmt.Errorf("%w: invalid run UID", ErrInvalidReview)
	}
	sum := sha256.Sum256([]byte(runUID))
	name := ConfigMapNamePrefix + hex.EncodeToString(sum[:20])
	if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
		return "", fmt.Errorf("%w: invalid ConfigMap name: %s", ErrInvalidReview, strings.Join(problems, "; "))
	}
	return name, nil
}

// MatrixRow is one per repository and immutable Gate revision. Gate
// generation is included so a policy update never mixes old and new runs.
type MatrixRow struct {
	Repository     string `json:"repository"`
	GateRef        string `json:"gateRef"`
	GateName       string `json:"gateName"`
	GateUID        string `json:"gateUID"`
	GateGeneration int64  `json:"gateGeneration"`
	AcceptedGood   int    `json:"acceptedGood"`
	AcceptedBad    int    `json:"acceptedBad"`
	RejectedGood   int    `json:"rejectedGood"`
	RejectedBad    int    `json:"rejectedBad"`
}

func (r MatrixRow) Total() int {
	return r.AcceptedGood + r.AcceptedBad + r.RejectedGood + r.RejectedBad
}

func (r MatrixRow) FalseAccepts() int { return r.AcceptedBad }

// DeriveMatrix deterministically maps validated reviews to the four cells in
// the §10 confusion matrix. Validation against live AgentRun objects belongs
// to the CLI, because only it has the Kubernetes read context.
func DeriveMatrix(reviews []Review) []MatrixRow {
	rows := make(map[string]MatrixRow)
	for _, review := range reviews {
		key := review.Repository + "\x00" + review.GateRef + "\x00" + review.GateName + "\x00" + review.GateUID + "\x00" + fmt.Sprint(review.GateGeneration)
		row := rows[key]
		row.Repository = review.Repository
		row.GateRef = review.GateRef
		row.GateName = review.GateName
		row.GateUID = review.GateUID
		row.GateGeneration = review.GateGeneration
		switch {
		case review.MachineVerdict == "Accepted" && review.DiffClassification == DiffGood:
			row.AcceptedGood++
		case review.MachineVerdict == "Accepted" && review.DiffClassification == DiffBad:
			row.AcceptedBad++
		case review.MachineVerdict == "Rejected" && review.DiffClassification == DiffGood:
			row.RejectedGood++
		case review.MachineVerdict == "Rejected" && review.DiffClassification == DiffBad:
			row.RejectedBad++
		}
		rows[key] = row
	}
	output := make([]MatrixRow, 0, len(rows))
	for _, row := range rows {
		output = append(output, row)
	}
	sort.Slice(output, func(i, j int) bool {
		left := output[i]
		right := output[j]
		for _, pair := range [][2]string{{left.Repository, right.Repository}, {left.GateRef, right.GateRef}, {left.GateUID, right.GateUID}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return left.GateGeneration < right.GateGeneration
	})
	return output
}

func normalize(input Review) (Review, error) {
	if input.SchemaVersion != SchemaVersion || input.ArtifactType != ArtifactType || input.Provenance != Provenance {
		return Review{}, fmt.Errorf("%w: unsupported schema, artifact type, or provenance", ErrInvalidReview)
	}
	if !safeUID(input.RunUID) || !validNamespace(input.Namespace) || !validName(input.RunName) ||
		!repositoryPattern.MatchString(input.Repository) || strings.Contains(input.Repository, "..") {
		return Review{}, fmt.Errorf("%w: run or repository identity is invalid", ErrInvalidReview)
	}
	if !validName(input.GateRef) || !validName(input.GateName) || !safeUID(input.GateUID) || input.GateGeneration <= 0 {
		return Review{}, fmt.Errorf("%w: Gate identity is invalid", ErrInvalidReview)
	}
	if input.GateMode != "shadow" || (input.MachineVerdict != "Accepted" && input.MachineVerdict != "Rejected") {
		return Review{}, fmt.Errorf("%w: only terminal shadow verdicts can be reviewed", ErrInvalidReview)
	}
	if !canonical.ValidDigest(input.SpecDigest) || !canonical.ValidDigest(input.PatchDigest) || !canonical.ValidDigest(input.ReportDigest) || !baseSHAPattern.MatchString(input.BaseSHA) {
		return Review{}, fmt.Errorf("%w: evidence identity is invalid", ErrInvalidReview)
	}
	if input.DiffClassification != DiffGood && input.DiffClassification != DiffBad {
		return Review{}, fmt.Errorf("%w: classification must be diff-good or diff-bad", ErrInvalidReview)
	}
	if err := normalizeReviewer(&input.Reviewer); err != nil {
		return Review{}, err
	}
	return input, nil
}

func normalizeReviewer(reviewer *Reviewer) error {
	if reviewer == nil || reviewer.Username == "" || len(reviewer.Username) > MaxUsernameBytes || !validText(reviewer.Username) {
		return fmt.Errorf("%w: reviewer username is invalid", ErrInvalidReview)
	}
	if len(reviewer.UID) > MaxUsernameBytes || (reviewer.UID != "" && !validText(reviewer.UID)) {
		return fmt.Errorf("%w: reviewer UID is invalid", ErrInvalidReview)
	}
	if len(reviewer.Groups) > MaxGroups {
		return fmt.Errorf("%w: reviewer group count is out of bounds", ErrInvalidReview)
	}
	reviewer.Groups = append([]string(nil), reviewer.Groups...)
	sort.Strings(reviewer.Groups)
	for index, group := range reviewer.Groups {
		if len(group) == 0 || len(group) > MaxGroupBytes || !validText(group) || (index > 0 && reviewer.Groups[index-1] == group) {
			return fmt.Errorf("%w: reviewer groups are invalid", ErrInvalidReview)
		}
	}
	return nil
}

func validText(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func safeUID(value string) bool { return uidPattern.MatchString(value) }

func validName(value string) bool {
	return value != "" && len(value) <= 253 && len(validation.IsDNS1123Subdomain(value)) == 0
}

func validNamespace(value string) bool {
	return value != "" && len(value) <= 63 && len(validation.IsDNS1123Label(value)) == 0
}

// CreationTime is kept separate from the canonical payload because the
// Kubernetes API server supplies it and makes it immutable. It is not copied
// into a client-supplied claim whose clock could be misleading.
func CreationTime(meta metav1.ObjectMeta) string {
	return meta.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}
