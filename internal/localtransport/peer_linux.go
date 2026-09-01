//go:build linux

package localtransport

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// CapturePeerIdentity obtains the PID and effective UID supplied by Linux for
// this exact accepted AF_UNIX stream.
func CapturePeerIdentity(connection *net.UnixConn) (PeerIdentity, error) {
	if connection == nil {
		return PeerIdentity{}, errors.New("connection is nil")
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return PeerIdentity{}, err
	}
	var credential *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return PeerIdentity{}, err
	}
	if controlErr != nil {
		return PeerIdentity{}, controlErr
	}
	if credential == nil {
		return PeerIdentity{}, errors.New("kernel returned no peer credentials")
	}
	peer := PeerIdentity{PID: int(credential.Pid), UID: int(credential.Uid)}
	if !peer.Valid() {
		return PeerIdentity{}, errors.New("kernel returned unusable peer credentials")
	}
	return peer, nil
}
