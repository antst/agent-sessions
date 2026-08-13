//go:build !linux && !darwin

package bridge

import (
	"errors"
	"net"
)

func unixPeerPID(_ net.Conn) (int, error) {
	return 0, errors.New("unix socket peer identity is unsupported on this platform")
}
