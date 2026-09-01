//go:build !linux && !darwin

package localtransport

import (
	"errors"
	"net"
)

func closeOnExec(*net.UnixConn) error {
	return errors.New("close-on-exec Unix transport is unsupported")
}
