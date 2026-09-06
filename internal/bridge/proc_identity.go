package bridge

import (
	"errors"
	"strings"

	"github.com/antst/sessionbus/internal/procinfo"
)

type processIdentityStatus uint8

const (
	processIdentityUnknown processIdentityStatus = iota
	processIdentityMatches
	processIdentityStale
)

type processIdentityObservation struct {
	Status    processIdentityStatus
	ProcStart string
}

type processIdentityProbeStatus uint8

const (
	processIdentityProbeUnknown processIdentityProbeStatus = iota
	processIdentityProbeKnown
	processIdentityProbeAbsent
)

type processIdentityProbe struct {
	status processIdentityProbeStatus
	state  string
	start  string
}

// observeProcessIdentity never collapses an observation failure into either
// authorization or proof of death. Callers must explicitly choose whether
// unknown means fail closed, retry, or preserve existing state.
func observeProcessIdentity(pid int) processIdentityObservation {
	return classifyProcessIdentityProbe(probeProcessIdentity(pid), "")
}

// exactProcessIdentityMatch authorizes a persisted process identity only when
// the observer corroborates the exact non-empty start token for a valid owner.
func exactProcessIdentityMatch(pid int, expected string) bool {
	return exactProcessIdentityStatus(pid, expected).Status == processIdentityMatches
}

func exactProcessIdentityStatus(pid int, expected string) processIdentityObservation {
	if pid <= 1 || expected == "" {
		return processIdentityObservation{Status: processIdentityUnknown}
	}
	shared := procinfo.ObserveIdentity(procinfo.Identity{PID: pid, Start: expected})
	switch shared.Status {
	case procinfo.IdentityMatches:
		if shared.Current.Start != expected {
			return processIdentityObservation{Status: processIdentityUnknown}
		}
		return processIdentityObservation{Status: processIdentityMatches, ProcStart: shared.Current.Start}
	case procinfo.IdentityStale:
		return processIdentityObservation{Status: processIdentityStale, ProcStart: shared.Current.Start}
	case procinfo.IdentityUnknown:
		return processIdentityObservation{Status: processIdentityUnknown}
	}
	return processIdentityObservation{Status: processIdentityUnknown}
}

// cleanupProcessIdentityStatus permits exact-token reconciliation and also
// recognizes a definitely absent process. Absence may authorize removal of
// exact Agent Sessions-owned files, but never authorizes signaling a process.
func cleanupProcessIdentityStatus(pid int, expected string) processIdentityObservation {
	if pid <= 1 {
		return processIdentityObservation{Status: processIdentityStale}
	}
	if expected != "" {
		return exactProcessIdentityStatus(pid, expected)
	}
	observation := observeProcessIdentity(pid)
	if observation.Status == processIdentityStale {
		return observation
	}
	return processIdentityObservation{Status: processIdentityUnknown}
}

func captureProcessStart(pid int) (string, error) {
	identity, err := procinfo.CaptureIdentity(pid)
	if err != nil || identity.Start == "" {
		return "", errors.New("cannot capture a stable process identity")
	}
	return identity.Start, nil
}

func processIdentityAllowsPIDPathRemoval(observation processIdentityObservation) bool {
	return observation.Status == processIdentityStale
}

// processIdentityMayBeLive is for preservation and waiting, never
// authorization. Unknown observations deliberately keep existing state.
func processIdentityMayBeLive(pid int, expected string) bool {
	return pid > 1 && cleanupProcessIdentityStatus(pid, expected).Status != processIdentityStale
}

func processIdentityStoppedOrUnset(pid int, expected string) bool {
	return cleanupProcessIdentityStatus(pid, expected).Status == processIdentityStale
}

func classifyProcessIdentityProbe(probe processIdentityProbe, expected string) processIdentityObservation {
	switch probe.status {
	case processIdentityProbeAbsent:
		return processIdentityObservation{Status: processIdentityStale}
	case processIdentityProbeKnown:
		if strings.HasPrefix(probe.state, "Z") || strings.HasPrefix(probe.state, "X") {
			return processIdentityObservation{Status: processIdentityStale, ProcStart: probe.start}
		}
		if probe.start == "" {
			return processIdentityObservation{Status: processIdentityUnknown}
		}
		if expected != "" && probe.start != expected {
			return processIdentityObservation{Status: processIdentityStale, ProcStart: probe.start}
		}
		return processIdentityObservation{Status: processIdentityMatches, ProcStart: probe.start}
	case processIdentityProbeUnknown:
		return processIdentityObservation{Status: processIdentityUnknown}
	}
	return processIdentityObservation{Status: processIdentityUnknown}
}
