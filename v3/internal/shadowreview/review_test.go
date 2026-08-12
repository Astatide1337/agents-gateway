package shadowreview

import (
	"bytes"
	"strings"
	"testing"
)

func validReview() Review {
	return Review{
		SchemaVersion: SchemaVersion, ArtifactType: ArtifactType, Provenance: Provenance,
		RunUID: "11111111-1111-4111-8111-111111111111", Namespace: "agw-runs", RunName: "jobmark-fix-427",
		Repository: "github.com/Astatide1337/jobmark", GateRef: "go-default", GateName: "go-default", GateUID: "gate-uid", GateGeneration: 7,
		GateMode: "shadow", MachineVerdict: "Accepted", SpecDigest: "sha256:" + strings.Repeat("a", 64),
		BaseSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), ReportDigest: "sha256:" + strings.Repeat("d", 64),
		DiffClassification: DiffGood, Reviewer: Reviewer{Username: "sohim", UID: "user-1", Groups: []string{"dev", "system:authenticated"}},
	}
}

func TestCanonicalReviewRoundTripAndSortsGroups(t *testing.T) {
	review := validReview()
	body, err := CanonicalBytes(review)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Reviewer.Groups; len(got) != 2 || got[0] != "dev" || got[1] != "system:authenticated" {
		t.Fatalf("groups = %#v", got)
	}
	reencoded, err := CanonicalBytes(parsed)
	if err != nil || !bytes.Equal(body, reencoded) {
		t.Fatalf("canonical bytes changed: err=%v body=%q reencoded=%q", err, body, reencoded)
	}
}

func TestParseRejectsMutationAndUnknownFields(t *testing.T) {
	body, err := CanonicalBytes(validReview())
	if err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), body...)
	mutated[len(mutated)-2] = ' '
	if _, err := ParseCanonicalBytes(mutated); err == nil {
		t.Fatal("mutated review was accepted")
	}
	unknown := append([]byte(`{"extra":true,`), body[1:]...)
	if _, err := ParseCanonicalBytes(unknown); err == nil {
		t.Fatal("review with unknown field was accepted")
	}
}

func TestConfigMapNameIsDeterministicAndBounded(t *testing.T) {
	first, err := ConfigMapName(validReview().RunUID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ConfigMapName(validReview().RunUID)
	if err != nil || first != second {
		t.Fatalf("names differ: %q %q err=%v", first, second, err)
	}
	if _, err := ConfigMapName("bad uid"); err == nil {
		t.Fatal("unsafe UID accepted")
	}
}

func TestDeriveMatrixMakesFalseAcceptsExplicit(t *testing.T) {
	good := validReview()
	bad := good
	bad.MachineVerdict = "Accepted"
	bad.DiffClassification = DiffBad
	rejectedGood := good
	rejectedGood.MachineVerdict = "Rejected"
	rejectedGood.DiffClassification = DiffGood
	rejectedBad := good
	rejectedBad.MachineVerdict = "Rejected"
	rejectedBad.DiffClassification = DiffBad
	rows := DeriveMatrix([]Review{good, bad, rejectedGood, rejectedBad})
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	row := rows[0]
	if row.AcceptedGood != 1 || row.AcceptedBad != 1 || row.RejectedGood != 1 || row.RejectedBad != 1 || row.FalseAccepts() != 1 {
		t.Fatalf("matrix row = %#v", row)
	}
}
