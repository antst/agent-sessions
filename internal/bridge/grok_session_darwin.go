//go:build darwin

package bridge

import (
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func platformGrokProcessSessionMembers(sessionID int) ([]grokSessionMember, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	members := []grokSessionMember{}
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		if pid <= 1 {
			continue
		}
		sid, sidErr := unix.Getsid(pid)
		if sidErr != nil || sid != sessionID {
			continue
		}
		info := procinfo.Read(pid)
		if info.Status == procinfo.Absent {
			continue
		}
		if info.Status != procinfo.Known || info.Start == "" {
			return nil, fmt.Errorf("cannot corroborate process %d in Grok lane session %d", pid, sessionID)
		}
		if info.State == "Z" || info.State == "X" {
			continue
		}
		members = append(members, grokSessionMember{PID: pid, ProcStart: info.Start, StrongStart: info.StrongStart})
	}
	return members, nil
}

func platformGrokTaggedProcessMembers(tokenHash string, roots []grokSessionMember) ([]grokSessionMember, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	candidates := []grokProcessCandidate{}
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		if pid <= 1 {
			continue
		}
		info := procinfo.Read(pid)
		if info.Status == procinfo.Absent {
			continue
		}
		if info.Status != procinfo.Known || info.Start == "" || info.State == "Z" || info.State == "X" {
			continue
		}
		environment, environmentErr := procinfo.Environment(pid)
		candidates = append(candidates, grokProcessCandidate{
			grokSessionMember: grokSessionMember{PID: pid, ProcStart: info.Start, StrongStart: info.StrongStart},
			Parent:            info.Parent,
			Tagged:            environmentErr == nil && grokEnvironmentMatchesTokenHash(environment, tokenHash),
		})
	}
	return grokOwnedProcessMembers(candidates, roots)
}
