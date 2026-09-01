//go:build !linux && !darwin

package localtransport

import (
	"errors"
	"net"
)

// CapturePeerIdentity fails closed on unsupported hosts.
func CapturePeerIdentity(_ *net.UnixConn) (PeerIdentity, error) {
	return PeerIdentity{}, errors.New("kernel Unix peer identity is unsupported on this platform")
}
