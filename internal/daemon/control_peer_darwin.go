//go:build darwin

package daemon

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func controlPeerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *unix.Xucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	if credential == nil {
		return 0, errors.New("control peer credentials are unavailable")
	}
	return credential.Uid, nil
}
