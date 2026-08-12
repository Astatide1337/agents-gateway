package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/phase0/agentgateway-guard/recording"
)

func main() {
	listen := envOr("AGW_RECORDING_LISTEN", "127.0.0.1:9090")
	canary := envOr("AGW_RECORDING_CANARY", "phase0-only-canary")
	fixture, err := recording.New(canary)
	if err != nil {
		log.Fatal("configure recording server: ", err)
	}
	server := &http.Server{Addr: listen, Handler: fixture.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("phase0 recording upstream listening on %s", listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("recording server stopped: ", err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
