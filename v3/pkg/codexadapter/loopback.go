package codexadapter

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// These routes are the stable in-pod surface exposed by agw-broker. The
// adapter never dials a non-loopback address; the broker owns provider
// credentials and any external network access.
const (
	ResponsesPath      = "/v1"
	MCPPath            = "/mcp"
	ArtifactPath       = "/v1/artifacts/output"
	ArtifactCreatePath = "/v1/artifacts/create"
)

func validateLoopbackBaseURL(raw string) (string, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return "", errors.New("must be a non-empty loopback URL of at most 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be an HTTP loopback URL without credentials, query, or fragment")
	}
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		return "", errors.New("broker URL path must be empty or /")
	}
	host := strings.ToLower(u.Hostname())
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("host must be localhost or a loopback IP")
		}
	}
	if port := u.Port(); port != "" {
		parsed, portErr := strconv.Atoi(port)
		if portErr != nil || parsed < 1 || parsed > 65535 {
			return "", errors.New("port must be between 1 and 65535")
		}
	}
	return strings.TrimRight(raw, "/"), nil
}

func parseBooleanEnv(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, errors.New("must be a boolean")
	}
}
