package temporalconfig

import "testing"

func TestSecureDefaultsAndDevelopmentEscapeHatch(t *testing.T) {
	if _, err := (Config{Address: "temporal:7233"}).ClientOptions(); err == nil {
		t.Fatal("TLS without a server name must fail closed")
	}
	options, err := (Config{Address: "temporal:7233", ServerName: "temporal.internal"}).ClientOptions()
	if err != nil || options.ConnectionOptions.TLS == nil || options.ConnectionOptions.TLS.ServerName != "temporal.internal" {
		t.Fatalf("secure options=%#v err=%v", options, err)
	}
	if _, err := (Config{Address: "temporal:7233", Environment: "production", Insecure: true}).ClientOptions(); err == nil {
		t.Fatal("production plaintext Temporal must fail closed")
	}
	options, err = (Config{Address: "temporal:7233", Environment: "development", Insecure: true}).ClientOptions()
	if err != nil || options.ConnectionOptions.TLS != nil {
		t.Fatalf("development options=%#v err=%v", options, err)
	}
}
