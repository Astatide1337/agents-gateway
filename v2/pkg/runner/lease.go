package runner

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrLeaseRunMismatch = errors.New("lease run id mismatch")
	ErrLeaseIDMismatch  = errors.New("lease id mismatch")
	ErrLeaseTokenStale  = errors.New("lease fencing token is stale")
	ErrLeaseExpired     = errors.New("lease is expired")
	ErrLeaseOwner       = errors.New("lease owner mismatch")
)

type Lease struct {
	RunID        string    `json:"run_id"`
	LeaseID      string    `json:"lease_id"`
	Owner        string    `json:"owner"`
	FencingToken uint64    `json:"fencing_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ValidateLease proves that a command belongs to the current lease. The
// fencing token is monotonically assigned by the control plane and prevents a
// delayed runner from mutating a reassigned run.
func ValidateLease(current, presented Lease, now time.Time) error {
	if current.RunID == "" || presented.RunID != current.RunID {
		return ErrLeaseRunMismatch
	}
	if current.LeaseID == "" || presented.LeaseID != current.LeaseID {
		return ErrLeaseIDMismatch
	}
	if current.Owner == "" || presented.Owner != current.Owner {
		return ErrLeaseOwner
	}
	if current.FencingToken == 0 || presented.FencingToken != current.FencingToken {
		return fmt.Errorf("%w: expected %d, got %d", ErrLeaseTokenStale, current.FencingToken, presented.FencingToken)
	}
	if !now.Before(current.ExpiresAt) || !now.Before(presented.ExpiresAt) {
		return ErrLeaseExpired
	}
	return nil
}
