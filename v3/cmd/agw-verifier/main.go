// agw-verifier runs the independent Gate commands after the verify Sandbox's
// network lockdown initContainer has completed. Stdout is reserved for one
// AGW_VERIFY_EVIDENCE_V1 frame; diagnostics are intentionally terse and go to
// stderr only.
package main

import (
	"os"

	"github.com/Astatide1337/agents-gateway/v3/internal/verifier"
)

func main() {
	os.Exit(verifier.Main())
}
