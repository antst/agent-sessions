//go:build linux

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
	var credential *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	if credential == nil || credential.Pid <= 1 {
		return 0, errors.New("unix socket peer has no process identity")
	}
	return int(credential.Pid), nil
}
