package modelroute

import (
	"errors"
	"testing"
	"time"
)

func TestOwnerBoundSubscriptionThenOrganizationAPIFallback(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	route := Route{Entries: []Entry{
		{Provider: "openai", Mode: ModeSubscription, Model: "gpt-codex"},
		{Provider: "openai", Mode: ModeAPI, Model: "gpt-codex"},
	}, OnExhausted: "wait"}
	entitlements := []Entitlement{
		{ID: "other-user", Provider: "openai", Mode: ModeSubscription, OwnerType: OwnerUser, OwnerID: "user-2", Models: []string{"gpt-codex"}, Enabled: true, Priority: 99},
		{ID: "mine-cooling", Provider: "openai", Mode: ModeSubscription, OwnerType: OwnerUser, OwnerID: "user-1", Models: []string{"gpt-codex"}, Enabled: true, CooldownUntil: now.Add(time.Hour)},
		{ID: "org-api", Provider: "openai", Mode: ModeAPI, OwnerType: OwnerOrganization, OwnerID: "org-1", Models: []string{"gpt-codex"}, Enabled: true},
	}
	selection, err := Select(route, entitlements, Request{UserID: "user-1", OrganizationID: "org-1", Model: "gpt-codex", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Entitlement.ID != "org-api" {
		t.Fatalf("selected %q", selection.Entitlement.ID)
	}
}

func TestNeverPoolsAnotherUsersSubscription(t *testing.T) {
	route := Route{Entries: []Entry{{Provider: "anthropic", Mode: ModeSubscription, Model: "claude"}}}
	_, err := Select(route, []Entitlement{{
		ID: "not-mine", Provider: "anthropic", Mode: ModeSubscription,
		OwnerType: OwnerUser, OwnerID: "user-2", Models: []string{"claude"}, Enabled: true,
	}}, Request{UserID: "user-1", OrganizationID: "org", Model: "claude"})
	if !errors.Is(err, ErrNoEntitlement) {
		t.Fatalf("expected no entitlement, got %v", err)
	}
}

func TestWaitsForOwnedCooldown(t *testing.T) {
	now := time.Now().UTC()
	retry := now.Add(15 * time.Minute)
	_, err := Select(Route{Entries: []Entry{{Provider: "openai", Mode: ModeSubscription, Model: "codex"}}, OnExhausted: "wait"}, []Entitlement{{
		ID: "mine", Provider: "openai", Mode: ModeSubscription, OwnerType: OwnerUser,
		OwnerID: "user", Models: []string{"codex"}, Enabled: true, CooldownUntil: retry,
	}}, Request{UserID: "user", OrganizationID: "org", Model: "codex", Now: now})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || !capacity.RetryAt.Equal(retry) {
		t.Fatalf("unexpected error %#v", err)
	}
}
