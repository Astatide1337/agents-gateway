//go:build !linux

package runbroker

import (
	"context"
	"net"
)

func capturePeerCredentials(net.Conn) (PeerCredentials, error) {
	return PeerCredentials{}, ErrPeerCredentials
}

func withPeerCredentials(ctx context.Context, credentials PeerCredentials) context.Context {
	return context.WithValue(ctx, contextPeerCredentialsKey{}, credentials)
}

func withPeerError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, contextPeerErrorKey{}, err)
}
