package runbroker

import "errors"

var (
	ErrInvalidInput       = errors.New("runbroker: invalid input")
	ErrMalformedSessionID = errors.New("runbroker: malformed session id")
	ErrMalformedToken     = errors.New("runbroker: malformed bearer token")
	ErrUnauthorized       = errors.New("runbroker: unauthorized")
	ErrExpired            = errors.New("runbroker: session expired")
	ErrRevoked            = errors.New("runbroker: session revoked")
	ErrWrongPolicy        = errors.New("runbroker: wrong policy digest")
	ErrWrongModel         = errors.New("runbroker: model is not allowed")
	ErrCrossRun           = errors.New("runbroker: cross-run access denied")
	ErrWrongBinding       = errors.New("runbroker: session binding mismatch")
	ErrAlreadyRunning     = errors.New("runbroker: socket is already in use")
	ErrNotUnixSocket      = errors.New("runbroker: socket path is not a Unix socket")
	ErrSocketUnsafe       = errors.New("runbroker: socket directory is unsafe")
	ErrPeerCredentials    = errors.New("runbroker: peer credentials unavailable")
	ErrClosed             = errors.New("runbroker: server is closed")
	ErrAlreadyServing     = errors.New("runbroker: server is already serving")
)
