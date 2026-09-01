//go:build darwin

package localtransport

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// CapturePeerIdentity combines Darwin's exact local peer PID with the peer
// effective UID from getpeereid for this accepted stream.
func CapturePeerIdentity(connection *net.UnixConn) (PeerIdentity, error) {
	if connection == nil {
		return PeerIdentity{}, errors.New("connection is nil")
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return PeerIdentity{}, err
	}
	peerPID := 0
	var credential *unix.Xucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		peerPID, controlErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if controlErr != nil {
			return
		}
		credential, controlErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return PeerIdentity{}, err
	}
	if controlErr != nil {
		return PeerIdentity{}, controlErr
	}
	if credential == nil {
		return PeerIdentity{}, errors.New("kernel returned no peer credentials")
	}
	peer := PeerIdentity{PID: peerPID, UID: int(credential.Uid)}
	if !peer.Valid() {
		return PeerIdentity{}, errors.New("kernel returned unusable peer credentials")
	}
	return peer, nil
}
