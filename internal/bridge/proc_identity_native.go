package bridge

import "github.com/antst/agent-sessions/internal/procinfo"

func probeProcessIdentity(pid int) processIdentityProbe {
	info := procinfo.Read(pid)
	switch info.Status {
	case procinfo.Absent:
		return processIdentityProbe{status: processIdentityProbeAbsent}
	case procinfo.Known:
		return processIdentityProbe{status: processIdentityProbeKnown, state: info.State, start: info.Start}
	case procinfo.Unknown:
		return processIdentityProbe{status: processIdentityProbeUnknown}
	}
	return processIdentityProbe{status: processIdentityProbeUnknown}
}

func parentProcessIDNative(pid int) (int, bool) {
	if pid <= 1 {
		return 0, false
	}
	info := procinfo.Read(pid)
	return info.Parent, info.Status == procinfo.Known && info.Parent > 0
}

func readProcessArgs(pid int) ([]string, error) {
	return procinfo.Args(pid)
}
