// Package brokerdispatch wraps workflow runner activities with the lifecycle
// of one private, short-lived broker session per idempotent task.
package brokerdispatch

import "errors"

var (
	ErrInvalidConfig    = errors.New("brokerdispatch: invalid configuration")
	ErrClosed           = errors.New("brokerdispatch: dispatcher is closed")
	ErrIdentityMismatch = errors.New("brokerdispatch: idempotency identity mismatch")
	ErrSessionClosed    = errors.New("brokerdispatch: broker session is closed")
	ErrUnsafePath       = errors.New("brokerdispatch: unsafe private path")
)
