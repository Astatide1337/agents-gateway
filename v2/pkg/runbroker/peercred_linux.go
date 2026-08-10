//go:build linux

package runbroker

import (
	"context"
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func capturePeerCredentials(connection net.Conn) (PeerCredentials, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return PeerCredentials{}, ErrPeerCredentials
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return PeerCredentials{}, fmtPeerError(err)
	}
	var credentials *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return PeerCredentials{}, fmtPeerError(err)
	}
	if controlErr != nil || credentials == nil {
		if controlErr != nil {
			return PeerCredentials{}, fmtPeerError(controlErr)
		}
		return PeerCredentials{}, ErrPeerCredentials
	}
	return PeerCredentials{PID: credentials.Pid, UID: credentials.Uid, GID: credentials.Gid}, nil
}

func withPeerCredentials(ctx context.Context, credentials PeerCredentials) context.Context {
	return context.WithValue(ctx, contextPeerCredentialsKey{}, credentials)
}

func withPeerError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, contextPeerErrorKey{}, err)
}

func fmtPeerError(err error) error {
	if err == nil {
		return ErrPeerCredentials
	}
	return errors.Join(ErrPeerCredentials, err)
}
