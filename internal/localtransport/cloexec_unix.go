//go:build linux || darwin

package localtransport

import (
	"net"

	"golang.org/x/sys/unix"
)

func closeOnExec(connection *net.UnixConn) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return err
	}
	return raw.Control(func(fd uintptr) { unix.CloseOnExec(int(fd)) })
}
