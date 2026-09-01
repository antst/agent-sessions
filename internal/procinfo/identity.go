package procinfo

import (
	"errors"
	"fmt"
	"strings"
)

// Identity is the exact process token persisted for later ownership checks.
// Start preserves the established cross-product token while StrongStart adds
// the finest kernel identity available on the host.
type Identity struct {
	PID         int
	Start       string
	StrongStart string
}

// IdentityStatus classifies an exact process identity observation.
type IdentityStatus uint8

const (
	// IdentityUnknown means the observer cannot authorize or prove absence.
	IdentityUnknown IdentityStatus = iota
	// IdentityMatches means the expected live identity was corroborated.
	IdentityMatches
	// IdentityStale means absence, terminal state, or PID reuse was proven.
	IdentityStale
)

// IdentityObservation contains the classification and currently observed token.
type IdentityObservation struct {
	Status  IdentityStatus
	Current Identity
}

// CaptureIdentity captures one live nonterminal process identity.
func CaptureIdentity(pid int) (Identity, error) {
	info := Read(pid)
	if info.Status != Known || terminalState(info.State) || info.Start == "" || info.StrongStart == "" {
		return Identity{}, fmt.Errorf("cannot capture exact process identity for pid %d", pid)
	}
	return Identity{PID: pid, Start: info.Start, StrongStart: info.StrongStart}, nil
}

// ObserveIdentity re-reads and classifies one persisted process identity.
func ObserveIdentity(expected Identity) IdentityObservation {
	if expected.PID <= 1 || expected.Start == "" {
		return IdentityObservation{Status: IdentityUnknown}
	}
	return ClassifyIdentity(expected, Read(expected.PID))
}

// ClassifyIdentity classifies one host snapshot without collapsing an
// unreadable identity into either authorization or proof of death. A legacy
// token without StrongStart remains start-compatible; new captures always
// contain and require StrongStart.
func ClassifyIdentity(expected Identity, current Info) IdentityObservation {
	observed := Identity{PID: expected.PID, Start: current.Start, StrongStart: current.StrongStart}
	switch {
	case expected.PID <= 1 || expected.Start == "":
		return IdentityObservation{Status: IdentityUnknown, Current: observed}
	case current.Status == Absent || terminalState(current.State):
		return IdentityObservation{Status: IdentityStale, Current: observed}
	case current.Status != Known || current.Start == "":
		return IdentityObservation{Status: IdentityUnknown, Current: observed}
	case current.Start != expected.Start:
		return IdentityObservation{Status: IdentityStale, Current: observed}
	case expected.StrongStart != "" && current.StrongStart == "":
		return IdentityObservation{Status: IdentityUnknown, Current: observed}
	case expected.StrongStart != "" && current.StrongStart != expected.StrongStart:
		return IdentityObservation{Status: IdentityStale, Current: observed}
	default:
		return IdentityObservation{Status: IdentityMatches, Current: observed}
	}
}

// DescendsFrom reports whether child is the exact ancestor itself or has that
// exact ancestor within maxDepth parent links. Unknown observations fail
// closed with an error; proven reuse or absence returns false.
func DescendsFrom(child, ancestor Identity, maxDepth int) (bool, error) {
	if maxDepth < 0 {
		return false, errors.New("process ancestry depth must be non-negative")
	}
	current := child
	for depth := 0; depth <= maxDepth; depth++ {
		info := Read(current.PID)
		observation := ClassifyIdentity(current, info)
		switch observation.Status {
		case IdentityUnknown:
			return false, fmt.Errorf("cannot corroborate process %d while walking ancestry", current.PID)
		case IdentityStale:
			return false, nil
		case IdentityMatches:
		}
		if current.PID == ancestor.PID {
			ancestorObservation := ClassifyIdentity(ancestor, info)
			switch ancestorObservation.Status {
			case IdentityMatches:
				return true, nil
			case IdentityStale:
				return false, nil
			case IdentityUnknown:
				return false, fmt.Errorf("cannot corroborate ancestor process %d", ancestor.PID)
			}
		}
		if info.Parent <= 1 {
			return false, nil
		}
		parent, err := CaptureIdentity(info.Parent)
		if err != nil {
			return false, fmt.Errorf("capture parent process %d: %w", info.Parent, err)
		}
		current = parent
	}
	return false, nil
}

func terminalState(state string) bool {
	return strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X")
}
