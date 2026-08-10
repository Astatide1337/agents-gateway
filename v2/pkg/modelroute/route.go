// Package modelroute selects an explicitly configured model entitlement while
// preserving provider ownership and cooldown boundaries.
package modelroute

import (
	"errors"
	"sort"
	"time"
)

type Mode string

const (
	ModeSubscription Mode = "subscription"
	ModeAPI          Mode = "api"
)

type OwnerType string

const (
	OwnerUser         OwnerType = "user"
	OwnerOrganization OwnerType = "organization"
)

type Entitlement struct {
	ID            string
	Provider      string
	Mode          Mode
	OwnerType     OwnerType
	OwnerID       string
	Models        []string
	Priority      int
	Enabled       bool
	CooldownUntil time.Time
	CredentialRef string
}

type Entry struct {
	Provider string
	Mode     Mode
	Model    string
}

type Route struct {
	Entries     []Entry
	OnExhausted string // "wait" or "fail"
}

type Request struct {
	UserID         string
	OrganizationID string
	Model          string
	Now            time.Time
}

type Selection struct {
	Entitlement Entitlement
	EntryIndex  int
}

type CapacityError struct{ RetryAt time.Time }

func (e *CapacityError) Error() string { return "no eligible model entitlement is currently available" }

var ErrNoEntitlement = errors.New("no eligible model entitlement")

func Select(route Route, entitlements []Entitlement, request Request) (Selection, error) {
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	var earliest time.Time
	for entryIndex, entry := range route.Entries {
		candidates := make([]Entitlement, 0)
		for _, entitlement := range entitlements {
			if !entitlement.Enabled || entitlement.Provider != entry.Provider || entitlement.Mode != entry.Mode {
				continue
			}
			if entry.Model != "" && entry.Model != request.Model {
				continue
			}
			if !supports(entitlement.Models, request.Model) {
				continue
			}
			if !ownedBy(entitlement, request) {
				continue
			}
			if entitlement.CooldownUntil.After(request.Now) {
				if earliest.IsZero() || entitlement.CooldownUntil.Before(earliest) {
					earliest = entitlement.CooldownUntil
				}
				continue
			}
			candidates = append(candidates, entitlement)
		}
		if len(candidates) == 0 {
			continue
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].Priority == candidates[j].Priority {
				return candidates[i].ID < candidates[j].ID
			}
			return candidates[i].Priority > candidates[j].Priority
		})
		return Selection{Entitlement: candidates[0], EntryIndex: entryIndex}, nil
	}
	if route.OnExhausted == "wait" && !earliest.IsZero() {
		return Selection{}, &CapacityError{RetryAt: earliest}
	}
	return Selection{}, ErrNoEntitlement
}

func ownedBy(entitlement Entitlement, request Request) bool {
	switch entitlement.OwnerType {
	case OwnerUser:
		return entitlement.Mode == ModeSubscription && entitlement.OwnerID == request.UserID
	case OwnerOrganization:
		return entitlement.Mode == ModeAPI && entitlement.OwnerID == request.OrganizationID
	default:
		return false
	}
}

func supports(models []string, requested string) bool {
	for _, model := range models {
		if model == "*" || model == requested {
			return true
		}
	}
	return false
}
