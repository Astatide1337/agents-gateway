package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	endpoint := strings.TrimSpace(os.Getenv("AGW_HEALTHCHECK_URL"))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8094/health"
	}
	client := http.Client{Timeout: 4 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		fmt.Fprintln(os.Stderr, "healthcheck: unexpected HTTP status", response.StatusCode)
		os.Exit(1)
	}
}
