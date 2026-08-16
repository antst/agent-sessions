//go:build linux

package bridge

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func platformGrokProcessSessionMembers(sessionID int) ([]grokSessionMember, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	members := []grokSessionMember{}
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 1 {
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
		members = append(members, grokSessionMember{PID: pid, ProcStart: info.Start})
	}
	return members, nil
}

func platformGrokTaggedProcessMembers(tokenHash string) ([]grokSessionMember, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	members := []grokSessionMember{}
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 1 {
			continue
		}
		environment, environmentErr := procinfo.Environment(pid)
		if environmentErr != nil || !grokEnvironmentMatchesTokenHash(environment, tokenHash) {
			continue
		}
		info := procinfo.Read(pid)
		if info.Status == procinfo.Absent {
			continue
		}
		if info.Status != procinfo.Known || info.Start == "" {
			return nil, fmt.Errorf("cannot corroborate tagged Grok lane process %d", pid)
		}
		if info.State == "Z" || info.State == "X" {
			continue
		}
		members = append(members, grokSessionMember{PID: pid, ProcStart: info.Start})
	}
	return members, nil
}
