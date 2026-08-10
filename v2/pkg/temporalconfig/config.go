// Package temporalconfig centralizes secure Temporal client configuration for
// the API and worker processes.
package temporalconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.temporal.io/sdk/client"
)

type Config struct {
	Address        string
	Namespace      string
	Environment    string
	ServerName     string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	// Insecure must be an explicit deployment decision and is intended only
	// for a private, single-host network namespace. It is never inferred from
	// a missing certificate configuration.
	Insecure bool
}

func (c Config) ClientOptions() (client.Options, error) {
	if strings.TrimSpace(c.Address) == "" {
		return client.Options{}, errors.New("Temporal address is required")
	}
	namespace := strings.TrimSpace(c.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	options := client.Options{HostPort: c.Address, Namespace: namespace}
	if c.Insecure {
		if !strings.EqualFold(strings.TrimSpace(c.Environment), "development") {
			return client.Options{}, errors.New("plaintext Temporal is allowed only when AGW_ENVIRONMENT=development")
		}
		return options, nil
	}
	if strings.TrimSpace(c.ServerName) == "" {
		return client.Options{}, errors.New("Temporal TLS server name is required")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return client.Options{}, fmt.Errorf("read Temporal CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return client.Options{}, errors.New("Temporal CA contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	if (c.ClientCertFile == "") != (c.ClientKeyFile == "") {
		return client.Options{}, errors.New("Temporal client certificate and key must be configured together")
	}
	if c.ClientCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
		if err != nil {
			return client.Options{}, fmt.Errorf("load Temporal client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	options.ConnectionOptions.TLS = tlsConfig
	return options, nil
}
