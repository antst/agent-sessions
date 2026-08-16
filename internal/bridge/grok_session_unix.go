//go:build linux || darwin

package bridge

import (
	"errors"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
)

type grokSessionMember struct {
	PID       int
	ProcStart string
}

const grokTaggedQuiescencePasses = 3

func grokProcessSessionID(pid int) (int, error) {
	return unix.Getsid(pid)
}

// grokProcessSessionMembers is implemented by the host-specific process-table
// reader. Membership is corroborated with getsid; a PID from another session
// is never inferred from ancestry or a numeric process-group collision.
func grokProcessSessionMembers(sessionID int) ([]grokSessionMember, error) {
	members, err := platformGrokProcessSessionMembers(sessionID)
	if err != nil {
		return nil, err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return members, nil
}

func grokEnvironmentMatchesTokenHash(environment []string, tokenHash string) bool {
	if len(tokenHash) != 64 {
		return false
	}
	prefix := grokLaunchTokenEnv + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) && grokTokenHash(strings.TrimPrefix(entry, prefix)) == tokenHash {
			return true
		}
	}
	return false
}

func grokTaggedProcessMembers(tokenHash string) ([]grokSessionMember, error) {
	if len(tokenHash) != 64 {
		return nil, nil
	}
	members, err := platformGrokTaggedProcessMembers(tokenHash)
	if err != nil {
		return nil, err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return members, nil
}

func grokTaggedProcessStatus(member grokSessionMember, tokenHash string) processIdentityStatus {
	identity := exactProcessIdentityStatus(member.PID, member.ProcStart)
	if identity.Status != processIdentityMatches {
		return identity.Status
	}
	environment, err := procinfo.Environment(member.PID)
	if err != nil {
		if procinfo.Read(member.PID).Status == procinfo.Absent {
			return processIdentityStale
		}
		return processIdentityUnknown
	}
	if !grokEnvironmentMatchesTokenHash(environment, tokenHash) {
		return processIdentityStale
	}
	return processIdentityMatches
}

func grokSessionMemberStatus(member grokSessionMember, sessionID int) processIdentityStatus {
	identity := exactProcessIdentityStatus(member.PID, member.ProcStart)
	if identity.Status != processIdentityMatches {
		return identity.Status
	}
	sid, err := unix.Getsid(member.PID)
	if errors.Is(err, syscall.ESRCH) {
		return processIdentityStale
	}
	if err != nil {
		return processIdentityUnknown
	}
	if sid != sessionID {
		return processIdentityStale
	}
	return processIdentityMatches
}

//nolint:gocyclo // Session cleanup repeatedly corroborates identity while escalating bounded signals.
func stopGrokProcessSession(sessionID int, expectedLeaderStart string, excludedPID int) error {
	if sessionID <= 1 {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	signal := syscall.SIGTERM
	for {
		members, err := grokProcessSessionMembers(sessionID)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.PID == sessionID && (expectedLeaderStart == "" || member.ProcStart != expectedLeaderStart) {
				return errors.New("refuse reused Grok lane process-session identity")
			}
		}
		remaining := 0
		for _, member := range members {
			if member.PID == excludedPID {
				continue
			}
			switch grokSessionMemberStatus(member, sessionID) {
			case processIdentityMatches:
				remaining++
				_ = syscall.Kill(member.PID, signal)
			case processIdentityUnknown:
				return errors.New("cannot corroborate Grok lane process-session member")
			case processIdentityStale:
			}
		}
		if remaining == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if signal == syscall.SIGKILL {
				return errors.New("timed out stopping Grok lane process session")
			}
			signal = syscall.SIGKILL
			deadline = time.Now().Add(2 * time.Second)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func grokProcessSessionHasMembers(sessionID, excludedPID int) bool {
	if sessionID <= 1 {
		return false
	}
	members, err := grokProcessSessionMembers(sessionID)
	if err != nil {
		return true
	}
	for _, member := range members {
		if member.PID == excludedPID {
			continue
		}
		switch grokSessionMemberStatus(member, sessionID) {
		case processIdentityMatches, processIdentityUnknown:
			return true
		case processIdentityStale:
		}
	}
	return false
}

// stopGrokTaggedProcesses terminates every launch descendant that retained the
// per-launch capability in its environment. Grok deliberately starts MCP and
// tool commands in independent sessions, so a worker-session sweep alone is
// insufficient after a manager or worker crash.
func stopGrokTaggedProcesses(tokenHash string, excludedPID int) error {
	if len(tokenHash) != 64 {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	signal := syscall.SIGTERM
	emptyPasses := 0
	for {
		members, err := grokTaggedProcessMembers(tokenHash)
		if err != nil {
			return err
		}
		remaining := 0
		for _, member := range members {
			if member.PID == excludedPID {
				continue
			}
			switch grokTaggedProcessStatus(member, tokenHash) {
			case processIdentityMatches:
				remaining++
				_ = syscall.Kill(member.PID, signal)
			case processIdentityUnknown:
				return errors.New("cannot corroborate tagged Grok lane process")
			case processIdentityStale:
			}
		}
		if remaining == 0 {
			emptyPasses++
			if emptyPasses >= grokTaggedQuiescencePasses {
				return nil
			}
			time.Sleep(25 * time.Millisecond)
			continue
		}
		emptyPasses = 0
		if time.Now().After(deadline) {
			if signal == syscall.SIGKILL {
				return errors.New("timed out stopping tagged Grok lane processes")
			}
			signal = syscall.SIGKILL
			deadline = time.Now().Add(2 * time.Second)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func grokTaggedProcessesRemain(tokenHash string, excludedPID int) bool {
	if len(tokenHash) != 64 {
		return false
	}
	for pass := 0; pass < grokTaggedQuiescencePasses; pass++ {
		members, err := grokTaggedProcessMembers(tokenHash)
		if err != nil {
			return true
		}
		for _, member := range members {
			if member.PID == excludedPID {
				continue
			}
			switch grokTaggedProcessStatus(member, tokenHash) {
			case processIdentityMatches, processIdentityUnknown:
				return true
			case processIdentityStale:
			}
		}
		if pass+1 < grokTaggedQuiescencePasses {
			time.Sleep(25 * time.Millisecond)
		}
	}
	return false
}
