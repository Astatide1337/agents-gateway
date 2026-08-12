package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/phase0/agentgateway-guard/guard"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

func main() {
	listen := envOr("AGW_GUARD_LISTEN", ":8081")
	endpoint := envOr("AGW_GUARD_GATEWAY_URL", "http://127.0.0.1:8082/mcp")
	runUID := envOr("AGW_GUARD_RUN_UID", "phase0-run-uid")
	specDigest := envOr("AGW_GUARD_SPEC_DIGEST", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	baseSHA := envOr("AGW_GUARD_BASE_SHA", "phase0-base-sha")

	downstream, err := guard.NewHTTPDownstream(endpoint, nil)
	if err != nil {
		log.Fatal("configure downstream: ", err)
	}
	fixture, err := guard.New(guard.Config{
		RunUID:     runUID,
		SpecDigest: specDigest,
		BaseSHA:    baseSHA,
		Downstream: downstream,
		// The fixture has no object store. Reads can be exercised end to end;
		// writes fail closed with effect_ledger_required until a real ledger is
		// deliberately injected by a later experiment.
		Grants: []toolpolicy.Grant{
			{
				Server:    "recording",
				Tool:      "record",
				Effect:    toolpolicy.EffectRead,
				Approval:  toolpolicy.ApprovalAllow,
				Arguments: []byte(`{"message":"hello","mode":"safe"}`),
			},
			{
				Server:    "recording",
				Tool:      "record",
				Effect:    toolpolicy.EffectWrite,
				Approval:  toolpolicy.ApprovalRequired,
				Arguments: []byte(`{"message":"write","mode":"safe"}`),
			},
		},
	})
	if err != nil {
		log.Fatal("configure guard: ", err)
	}
	server := &http.Server{Addr: listen, Handler: fixture.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("phase0 guard listening on %s", listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("guard stopped: ", err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
