package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
)

func TestParseIDMapRequiresSubordinateRange(t *testing.T) {
	valid, err := parseIDMap("         0     100000      65536\n")
	if err != nil {
		t.Fatalf("valid user namespace map rejected: %v", err)
	}
	if valid.Inside != 0 || valid.Outside != 100000 || valid.Length != 65536 {
		t.Fatalf("unexpected parsed map: %#v", valid)
	}

	for _, raw := range []string{
		"         0          0 4294967295\n",
		"         1     100000      65536\n",
		"         0     100000      1024\n",
		"not a map\n",
		"",
	} {
		if _, err := parseIDMap(raw); err == nil {
			t.Fatalf("unsafe user namespace map accepted: %q", raw)
		}
	}
}

func TestVolumeFilesystemFamilyFailsClosedForMissingHostPath(t *testing.T) {
	if _, err := volumeFilesystemFamily(t.TempDir() + "/missing"); err == nil {
		t.Fatal("missing hostPath filesystem was accepted")
	}
}

func TestHostProcIsCheckedAsProcfsAndNeverAsAnIdmapVolume(t *testing.T) {
	if got, err := hostProcFilesystem("/proc"); err != nil || got != "procfs" {
		t.Fatalf("host /proc was not recognized as procfs: %q %v", got, err)
	}
	if _, err := volumeFilesystemFamily("/proc"); err == nil {
		t.Fatal("procfs was incorrectly accepted as an idmap volume filesystem family")
	}
}

func TestCollectFingerprintsSeparatesProcfsFromVolumeFilesystemChecks(t *testing.T) {
	t.Setenv("NODE_NAME", "phase0-test-node")
	cfg := config{
		HostProc:       "/proc",
		HostKubelet:    t.TempDir(),
		RuncPath:       "/usr/bin/ls",
		ContainerdPath: "/usr/bin/ls",
		K3sPath:        "/usr/bin/ls",
	}
	if _, err := collectFingerprints(t.Context(), cfg); err != nil {
		t.Fatalf("collectFingerprints applied the volume filesystem check to procfs: %v", err)
	}
}

func TestAllChecksPassedFailsClosedForEmptyOrMissingChecks(t *testing.T) {
	for _, checks := range [][]check{
		nil,
		{{Name: "uid", Status: "passed"}, {Name: "network", Status: ""}},
		{{Name: "uid", Status: "passed"}, {Name: "network", Status: "failed"}},
		{{Name: "", Status: "passed"}},
	} {
		if allChecksPassed(checks) {
			t.Fatalf("incomplete checks were accepted: %#v", checks)
		}
	}
	if !allChecksPassed([]check{{Name: "uid", Status: "passed"}, {Name: "network", Status: "passed"}}) {
		t.Fatal("complete passing checks were rejected")
	}
}

func TestRequiredChecksMustBeCompleteBeforeAttestationCanPass(t *testing.T) {
	complete := make([]check, 0, len(requiredAttestorChecks))
	for _, name := range requiredAttestorChecks {
		complete = append(complete, check{Name: name, Status: "passed"})
	}
	if !allRequiredChecksPassed(complete) {
		t.Fatal("complete required check set was rejected")
	}

	for i := range complete {
		missing := append([]check(nil), complete[:i]...)
		missing = append(missing, complete[i+1:]...)
		if allRequiredChecksPassed(missing) {
			t.Fatalf("missing check %q was accepted", complete[i].Name)
		}
	}

	withUnknown := append([]check(nil), complete...)
	withUnknown[len(withUnknown)-1] = check{Name: "unexpected", Status: "passed"}
	if allRequiredChecksPassed(withUnknown) {
		t.Fatal("unknown check substituted for a required check")
	}
}

func TestResultJSONAlwaysSatisfiesExistingContractAndNeverInventsPass(t *testing.T) {
	failed, err := resultJSON(newReport("failed", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := preflight.ParseResult(failed)
	if err != nil {
		t.Fatalf("failed result did not satisfy the admission parser: %v", err)
	}
	if parsed.Passed || parsed.NodeFingerprint != "unavailable" || parsed.RuntimeFingerprint != "unavailable" {
		t.Fatalf("failed result was not fail-closed: %#v", parsed)
	}

	incomplete := newReport("passed", "sha256:node", "sha256:runtime")
	incomplete.Passed = true
	incomplete.Checks = []check{{Name: "every-check", Status: "passed"}}
	raw, err := resultJSON(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = preflight.ParseResult(raw)
	if err != nil || parsed.Passed {
		t.Fatalf("incomplete passing report invented a passing contract: %v %#v", err, parsed)
	}

	passing := newReport("passed", "sha256:node", "sha256:runtime")
	passing.Passed = true
	for _, name := range requiredAttestorChecks {
		passing.Checks = append(passing.Checks, check{Name: name, Status: "passed"})
	}
	raw, err = resultJSON(passing)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = preflight.ParseResult(raw)
	if err != nil || !parsed.Passed {
		t.Fatalf("complete passing result was not accepted by the admission parser: %v %#v", err, parsed)
	}
}

func TestWriteAtomicIsBoundedAndUsesTargetDirectory(t *testing.T) {
	path := t.TempDir() + "/agent.json"
	if err := writeAtomic(path, []byte(`{"schema_version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"schema_version":1}` {
		t.Fatalf("unexpected atomic file contents: %q", raw)
	}
	if err := writeAtomic(path, []byte(strings.Repeat("x", maxAgentResultSize+1)), 0644); err == nil {
		t.Fatal("oversized evidence was written")
	}
}

func TestBoundDetailRemovesControlCharacters(t *testing.T) {
	got := boundDetail("a\n\tb\x00c")
	if strings.ContainsAny(got, "\r\n\t") || strings.ContainsRune(got, 0) {
		t.Fatalf("control characters survived detail bounding: %q", got)
	}
}
