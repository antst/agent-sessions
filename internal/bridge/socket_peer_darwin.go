//go:build darwin

package bridge

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func unixPeerPID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("connection is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	peerPID := 0
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		peerPID, controlErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	if peerPID <= 1 {
		return 0, errors.New("unix socket peer has no process identity")
	}
	return peerPID, nil
}
