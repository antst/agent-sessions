//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func observeControlPeer(connection *net.UnixConn) (controlPeerEvidence, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return controlPeerEvidence{}, err
	}
	var credential *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return controlPeerEvidence{}, err
	}
	if controlErr != nil {
		return controlPeerEvidence{}, controlErr
	}
	if credential == nil || credential.Pid <= 1 {
		return controlPeerEvidence{}, errors.New("local control peer has no kernel process identity")
	}
	info := procinfo.Read(int(credential.Pid))
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		return controlPeerEvidence{}, fmt.Errorf("local control peer process %d cannot be corroborated", credential.Pid)
	}
	return controlPeerEvidence{
		UID: int(credential.Uid), PID: int(credential.Pid), ProcStart: info.Start, StrongStart: info.StrongStart,
	}, nil
}
