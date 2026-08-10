package main

import "testing"

func TestResolveCoolifyPreviewDatabaseURL(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		input    string
		expected string
	}{
		{
			name:     "production fallback",
			service:  "postgres",
			input:    "postgres://agw:secret@postgres:5432/agents_gateway?sslmode=disable",
			expected: "postgres://agw:secret@postgres:5432/agents_gateway?sslmode=disable",
		},
		{
			name:     "preview service suffix",
			service:  "postgres-pr-20",
			input:    "postgres://agw:secret@postgres:5432/agents_gateway?sslmode=disable",
			expected: "postgres://agw:secret@postgres-pr-20:5432/agents_gateway?sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SERVICE_NAME_POSTGRES", tt.service)
			got, err := resolveCoolifyPreviewDatabaseURL(tt.input)
			if err != nil {
				t.Fatalf("resolveCoolifyPreviewDatabaseURL() error = %v", err)
			}
			if got != tt.expected {
				t.Fatalf("resolveCoolifyPreviewDatabaseURL() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestResolveCoolifyPreviewDatabaseURLRejectsMissingHost(t *testing.T) {
	t.Setenv("SERVICE_NAME_POSTGRES", "postgres-pr-20")
	if _, err := resolveCoolifyPreviewDatabaseURL("postgres:///agents_gateway"); err == nil {
		t.Fatal("resolveCoolifyPreviewDatabaseURL() error = nil, want missing-host error")
	}
}
