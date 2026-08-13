package bridge

import (
	"errors"
	"strings"
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
func observeProcessIdentity(pid int, expected string) processIdentityObservation {
	return classifyProcessIdentityProbe(probeProcessIdentity(pid), expected)
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
	observation := observeProcessIdentity(pid, expected)
	if observation.Status == processIdentityMatches && observation.ProcStart != expected {
		return processIdentityObservation{Status: processIdentityUnknown}
	}
	return observation
}

// cleanupProcessIdentityStatus preserves tokenless legacy records while their
// PID may still exist, but permits cleanup once absence is proven. It never
// turns a live tokenless PID into authorization or a signal target.
func cleanupProcessIdentityStatus(pid int, expected string) processIdentityObservation {
	if pid <= 1 {
		return processIdentityObservation{Status: processIdentityStale}
	}
	if expected != "" {
		return exactProcessIdentityStatus(pid, expected)
	}
	observation := observeProcessIdentity(pid, "")
	if observation.Status == processIdentityStale {
		return observation
	}
	return processIdentityObservation{Status: processIdentityUnknown}
}

func captureProcessStart(pid int) (string, error) {
	observation := observeProcessIdentity(pid, "")
	if observation.Status != processIdentityMatches || observation.ProcStart == "" {
		return "", errors.New("cannot capture a stable process identity")
	}
	return observation.ProcStart, nil
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
