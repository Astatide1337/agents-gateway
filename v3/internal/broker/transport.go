package broker

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	mcpProtocolVersion = "2025-06-18"
	maxSessionIDBytes  = 4096
	maxHTTPHeaderBytes = 32 << 10
	upstreamTimeout    = 2 * time.Minute
)

// DNSResolver is the only name-resolution seam used by the SSRF defense.
// Production uses net.Resolver; tests may inject a deterministic resolver
// from inside this package.
type DNSResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type defaultDNSResolver struct{}

func (defaultDNSResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func newSecureHTTPClient(resolver DNSResolver, testTransport http.RoundTripper, maxMCPResponse int, maxModelResponse int64) (*http.Client, error) {
	if resolver == nil {
		resolver = defaultDNSResolver{}
	}
	if testTransport != nil {
		// This field is package-private and exists only to make protocol
		// handlers testable without binding a real public DNS name. The
		// production integration cannot set it from another package.
		return &http.Client{Transport: testTransport, CheckRedirect: rejectRedirect, Timeout: upstreamTimeout}, nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		return nil, ErrInvalidConfig
	}
	transport := base.Clone()
	transport.Proxy = nil
	transport.DialContext = (&publicDialer{resolver: resolver, dialer: &net.Dialer{Timeout: upstreamTimeout, KeepAlive: 30 * time.Second}}).DialContext
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	// Tool results are bounded before parsing. Do not let net/http silently
	// decompress an attacker-controlled response and turn a small wire body
	// into an unbounded CPU/memory workload; compressed responses fail closed.
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = upstreamTimeout
	transport.MaxResponseHeaderBytes = maxHTTPHeaderBytes
	if maxMCPResponse <= 0 || maxModelResponse <= 0 {
		return nil, ErrInvalidConfig
	}
	return &http.Client{Transport: transport, CheckRedirect: rejectRedirect, Timeout: upstreamTimeout}, nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("redirects are forbidden")
}

type publicDialer struct {
	resolver DNSResolver
	dialer   *net.Dialer
}

func (d *publicDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || d.resolver == nil || d.dialer == nil {
		return nil, ErrUpstreamUnavailable
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return nil, ErrUpstreamUnavailable
	}
	if parsedPort, portErr := strconv.Atoi(port); portErr != nil || parsedPort < 1 || parsedPort > 65535 {
		return nil, ErrUpstreamUnavailable
	}
	ips, err := d.resolver.LookupIPAddr(ctx, strings.Trim(host, "[]"))
	if err != nil || len(ips) == 0 {
		return nil, ErrUpstreamUnavailable
	}
	var selected net.IP
	for _, candidate := range ips {
		ip := candidate.IP
		if !isPublicIP(ip) {
			// A hostname resolving to a mixture of public and private
			// addresses is rejected in full. Otherwise an attacker could
			// win a connection race with the private answer.
			return nil, ErrUpstreamUnavailable
		}
		if selected == nil {
			selected = append(net.IP(nil), ip...)
		}
	}
	if selected == nil {
		return nil, ErrUpstreamUnavailable
	}
	return d.dialer.DialContext(ctx, network, net.JoinHostPort(selected.String(), port))
}

func validatePublicEndpoint(raw, kind string) (string, error) {
	if raw == "" || len(raw) > MaxEndpointBytes || strings.TrimSpace(raw) != raw {
		return "", ErrInvalidConfig
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" {
		return "", ErrInvalidConfig
	}
	if parsed.Path == "" || parsed.Path[0] != '/' || strings.Contains(parsed.Path, "\\") || strings.Contains(parsed.Path, "//") || strings.Contains(parsed.Path, "/../") || strings.HasSuffix(parsed.Path, "/..") {
		return "", ErrInvalidConfig
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return "", ErrInvalidConfig
		}
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return "", ErrInvalidConfig
		}
	} else if !safeHostname(host) {
		return "", ErrInvalidConfig
	}
	if kind == "mcp" && parsed.Path == "/" {
		return "", ErrInvalidConfig
	}
	return parsed.String(), nil
}

func validateResponsesProviderEndpoint(raw, kind string) (string, error) {
	validated, err := validatePublicEndpoint(raw, "responses")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(validated)
	if err != nil {
		return "", ErrInvalidConfig
	}
	want := "/v1/responses"
	if kind == "openrouter-responses" {
		want = "/api/v1/responses"
	}
	if parsed.Path != want {
		return "", ErrInvalidConfig
	}
	return validated, nil
}

func validateAnthropicProviderEndpoint(raw, kind string) (string, error) {
	validated, err := validatePublicEndpoint(raw, "messages")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(validated)
	if err != nil {
		return "", ErrInvalidConfig
	}
	want := "/v1/messages"
	if kind == "openrouter-anthropic-messages" {
		want = "/api/v1/messages"
	}
	if parsed.Path != want {
		return "", ErrInvalidConfig
	}
	return validated, nil
}

func safeHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.EqualFold(host, "localhost") || strings.HasSuffix(host, ".") || strings.ContainsAny(host, " \t\r\n\x00") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else {
		ip = ip.To16()
		if ip == nil {
			return false
		}
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return false
	}
	// IsGlobalUnicast intentionally includes several special-use ranges in
	// Go's net package. Reject those explicitly as well as RFC 6598, which is
	// routable inside some providers but is not a public Internet address.
	for _, block := range specialUseBlocks {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

var specialUseBlocks = mustCIDRs([]string{
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"::/128", "::1/128", "100::/64", "2001:2::/48", "2001:10::/28", "2001:db8::/32", "fc00::/7", "fe80::/10", "fec0::/10",
})

func mustCIDRs(values []string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, block, err := net.ParseCIDR(value)
		if err != nil {
			panic("invalid broker special-use CIDR")
		}
		result = append(result, block)
	}
	return result
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil || limit <= 0 {
		return nil, ErrUpstreamInvalid
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, ErrUpstreamUnavailable
	}
	if int64(len(body)) > limit {
		return nil, ErrUpstreamInvalid
	}
	return body, nil
}

func contentTypeIs(value string, allowed ...string) bool {
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	if media == "" {
		return false
	}
	for _, candidate := range allowed {
		if media == candidate {
			return true
		}
	}
	return false
}
