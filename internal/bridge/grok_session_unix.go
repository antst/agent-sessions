//go:build linux || darwin

package bridge

import (
	"errors"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
)

type grokSessionMember struct {
	PID         int
	ProcStart   string
	StrongStart string
}

type grokProcessCandidate struct {
	grokSessionMember
	Parent int
	Tagged bool
}

const (
	grokTaggedQuiescencePasses = 3
	grokProcessSurveyInterval  = 100 * time.Millisecond
)

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

func grokTaggedProcessMembers(tokenHash string, roots ...grokSessionMember) ([]grokSessionMember, error) {
	if len(tokenHash) != 64 {
		return nil, nil
	}
	members, err := platformGrokTaggedProcessMembers(tokenHash, roots)
	if err != nil {
		return nil, err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return members, nil
}

// grokOwnedProcessMembers expands directly tagged processes with the exact
// descendants of durable ownership roots. Darwin intentionally omits the
// environment from KERN_PROCARGS2 for restricted system binaries, so a token
// alone cannot identify commands such as /bin/sleep even though they inherited
// it. Parentage is authoritative only while an exact root/previously discovered
// member is still live; callers retain the returned PID/start identities across
// subsequent surveys and signal escalation.
//
//nolint:gocyclo // Ownership expansion distinguishes stale, unknown, tagged, and transitive exact identities.
func grokOwnedProcessMembers(candidates []grokProcessCandidate, roots []grokSessionMember) ([]grokSessionMember, error) {
	byPID := make(map[int]grokProcessCandidate, len(candidates))
	owned := make(map[int]bool, len(candidates))
	for _, candidate := range candidates {
		byPID[candidate.PID] = candidate
		if candidate.Tagged {
			owned[candidate.PID] = true
		}
	}
	for _, root := range roots {
		if root.PID <= 1 || root.ProcStart == "" {
			continue
		}
		candidate, ok := byPID[root.PID]
		if ok && grokProcessIdentityEqual(candidate.grokSessionMember, root) {
			owned[root.PID] = true
			continue
		}
		switch grokProcessIdentityStatus(root) {
		case processIdentityUnknown:
			return nil, errors.New("cannot corroborate Grok lane ownership root")
		case processIdentityMatches:
			return nil, errors.New("grok lane ownership root is missing from process snapshot")
		case processIdentityStale:
		}
	}
	for changed := true; changed; {
		changed = false
		for _, candidate := range candidates {
			if !owned[candidate.PID] && owned[candidate.Parent] {
				owned[candidate.PID] = true
				changed = true
			}
		}
	}
	members := make([]grokSessionMember, 0, len(owned))
	for pid := range owned {
		members = append(members, byPID[pid].grokSessionMember)
	}
	return members, nil
}

func grokProcessIdentityEqual(observed, expected grokSessionMember) bool {
	if observed.PID != expected.PID || observed.ProcStart != expected.ProcStart {
		return false
	}
	return expected.StrongStart == "" || observed.StrongStart == expected.StrongStart
}

// grokProcessIdentityStatus is the destructive-cleanup identity check. New
// Grok lane records carry StrongStart so Darwin PID reuse within procStart's
// one-second display granularity cannot authorize a signal.
func grokProcessIdentityStatus(member grokSessionMember) processIdentityStatus {
	if member.PID <= 1 || member.ProcStart == "" {
		return processIdentityUnknown
	}
	info := procinfo.Read(member.PID)
	switch info.Status {
	case procinfo.Absent:
		return processIdentityStale
	case procinfo.Known:
		if info.State == "Z" || info.State == "X" {
			return processIdentityStale
		}
		if info.Start == "" || info.Start != member.ProcStart {
			return processIdentityStale
		}
		if member.StrongStart != "" && info.StrongStart != member.StrongStart {
			return processIdentityStale
		}
		if member.StrongStart != "" && info.StrongStart == "" {
			return processIdentityUnknown
		}
		return processIdentityMatches
	case procinfo.Unknown:
		return processIdentityUnknown
	}
	return processIdentityUnknown
}

func grokSessionMemberStatus(member grokSessionMember, sessionID int) processIdentityStatus {
	identity := grokProcessIdentityStatus(member)
	if identity != processIdentityMatches {
		return identity
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

func stopGrokProcessSession(sessionID int, expectedLeaderStart string, excludedPID int) error {
	return stopGrokProcessSessionStrong(sessionID, expectedLeaderStart, "", excludedPID)
}

//nolint:gocyclo // Session cleanup repeatedly corroborates identity while escalating bounded signals.
func stopGrokProcessSessionStrong(sessionID int, expectedLeaderStart, expectedLeaderStrongStart string, excludedPID int) error {
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
			if member.PID == sessionID && (expectedLeaderStart == "" || member.ProcStart != expectedLeaderStart ||
				(expectedLeaderStrongStart != "" && member.StrongStart != expectedLeaderStrongStart)) {
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

func stopStaleGrokLaneWorker(member grokSessionMember) {
	if grokProcessIdentityStatus(member) != processIdentityMatches {
		return
	}
	_ = syscall.Kill(-member.PID, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if grokProcessIdentityStatus(member) == processIdentityStale {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if grokProcessIdentityStatus(member) == processIdentityMatches {
		_ = syscall.Kill(-member.PID, syscall.SIGKILL)
	}
}

func grokProcessSessionHasMembers(sessionID, excludedPID int) bool {
	if sessionID <= 1 {
		return false
	}
	// The caller is authoritative evidence for its own kernel session and does
	// not depend on a potentially incomplete platform process-table snapshot.
	if os.Getpid() != excludedPID {
		if currentSessionID, err := grokProcessSessionID(os.Getpid()); err == nil && currentSessionID == sessionID {
			return true
		}
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

// stopGrokTaggedProcesses terminates every process attributable through the
// per-launch capability or the exact live ancestry of a durable ownership
// root. Grok deliberately starts MCP and tool commands in independent
// sessions, so a worker-session sweep alone is insufficient after a manager
// or worker crash.
//
//nolint:gocyclo // Cleanup repeatedly expands and corroborates exact ownership while escalating bounded signals.
func stopGrokTaggedProcesses(tokenHash string, excludedPID int, roots ...grokSessionMember) error {
	if len(tokenHash) != 64 {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	signal := syscall.SIGTERM
	emptyPasses := 0
	known := map[int]grokSessionMember{}
	for _, root := range roots {
		if root.PID > 1 && root.ProcStart != "" {
			known[root.PID] = root
		}
	}
	for {
		roots := make([]grokSessionMember, 0, len(known))
		for _, member := range known {
			roots = append(roots, member)
		}
		members, err := grokTaggedProcessMembers(tokenHash, roots...)
		if err != nil {
			return err
		}
		for _, member := range members {
			known[member.PID] = member
		}
		remaining := 0
		for pid, member := range known {
			if member.PID == excludedPID {
				continue
			}
			switch grokProcessIdentityStatus(member) {
			case processIdentityMatches:
				remaining++
				_ = syscall.Kill(member.PID, signal)
			case processIdentityUnknown:
				return errors.New("cannot corroborate tagged Grok lane process")
			case processIdentityStale:
				delete(known, pid)
			}
		}
		if remaining == 0 {
			emptyPasses++
			if emptyPasses >= grokTaggedQuiescencePasses {
				return nil
			}
			time.Sleep(grokProcessSurveyInterval)
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
		time.Sleep(grokProcessSurveyInterval)
	}
}

func grokTaggedProcessesRemain(tokenHash string, excludedPID int, roots ...grokSessionMember) bool {
	if len(tokenHash) != 64 {
		return false
	}
	for pass := 0; pass < grokTaggedQuiescencePasses; pass++ {
		members, err := grokTaggedProcessMembers(tokenHash, roots...)
		if err != nil {
			return true
		}
		for _, member := range members {
			if member.PID == excludedPID {
				continue
			}
			switch grokProcessIdentityStatus(member) {
			case processIdentityMatches, processIdentityUnknown:
				return true
			case processIdentityStale:
			}
		}
		if pass+1 < grokTaggedQuiescencePasses {
			time.Sleep(grokProcessSurveyInterval)
		}
	}
	return false
}
