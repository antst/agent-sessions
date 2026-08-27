//go:build darwin

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
	peerPID := 0
	var credential *unix.Xucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		peerPID, controlErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if controlErr == nil {
			credential, controlErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		}
	}); err != nil {
		return controlPeerEvidence{}, err
	}
	if controlErr != nil {
		return controlPeerEvidence{}, controlErr
	}
	if peerPID <= 1 || credential == nil {
		return controlPeerEvidence{}, errors.New("local control peer has no kernel process identity")
	}
	info := procinfo.Read(peerPID)
	if info.Status != procinfo.Known || info.Start == "" || info.StrongStart == "" {
		return controlPeerEvidence{}, fmt.Errorf("local control peer process %d cannot be corroborated", peerPID)
	}
	return controlPeerEvidence{
		UID: int(credential.Uid), PID: peerPID, ProcStart: info.Start, StrongStart: info.StrongStart,
	}, nil
}
