package main

import (
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

func TestVerifyPhaseAdapterFailsClosedWithoutDriver(t *testing.T) {
	adapter := verifyPhaseAdapter{store: objectstore.Config{Bucket: "agw-artifacts"}}
	if _, err := adapter.Verify(nil, nil, resolved.Snapshot{}); err == nil {
		t.Fatal("unconfigured verification adapter did not fail closed")
	}
}
