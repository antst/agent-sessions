package bridge

import (
	"os"
	"testing"
)

func TestProcessIdentityClassificationKeepsUnknownDistinct(t *testing.T) {
	tests := []struct {
		name     string
		probe    processIdentityProbe
		expected string
		want     processIdentityStatus
	}{
		{name: "unknown", probe: processIdentityProbe{status: processIdentityProbeUnknown}, expected: "recorded", want: processIdentityUnknown},
		{name: "absent", probe: processIdentityProbe{status: processIdentityProbeAbsent}, expected: "recorded", want: processIdentityStale},
		{name: "zombie", probe: processIdentityProbe{status: processIdentityProbeKnown, state: "Z+", start: "recorded"}, expected: "recorded", want: processIdentityStale},
		{name: "missing-start", probe: processIdentityProbe{status: processIdentityProbeKnown, state: "S"}, expected: "recorded", want: processIdentityUnknown},
		{name: "reused", probe: processIdentityProbe{status: processIdentityProbeKnown, state: "S", start: "current"}, expected: "recorded", want: processIdentityStale},
		{name: "match", probe: processIdentityProbe{status: processIdentityProbeKnown, state: "S", start: "recorded"}, expected: "recorded", want: processIdentityMatches},
		{name: "capture", probe: processIdentityProbe{status: processIdentityProbeKnown, state: "S", start: "current"}, want: processIdentityMatches},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyProcessIdentityProbe(test.probe, test.expected); got.Status != test.want {
				t.Fatalf("classification = %+v, want status %v", got, test.want)
			}
		})
	}
}

func TestProcessIdentityAuthorizationRequiresExactKnownToken(t *testing.T) {
	started := readProcStart(os.Getpid())
	if started == "" {
		t.Fatal("current process has no start token")
	}
	if !exactProcessIdentityMatch(os.Getpid(), started) {
		t.Fatal("matching process identity was rejected")
	}
	if exactProcessIdentityMatch(os.Getpid(), started+"-wrong") {
		t.Fatal("mismatched process identity was authorized")
	}
	if exactProcessIdentityMatch(os.Getpid(), "") {
		t.Fatal("empty process identity was authorized")
	}
	if exactProcessIdentityStatus(os.Getpid(), "").Status != processIdentityUnknown {
		t.Fatal("empty expected identity was not classified as unknown")
	}
}

func TestTokenlessLegacyIdentityAllowsOnlyProvenAbsenceCleanup(t *testing.T) {
	if got := cleanupProcessIdentityStatus(os.Getpid(), "").Status; got != processIdentityUnknown {
		t.Fatalf("live tokenless identity = %v, want unknown", got)
	}
	if got := cleanupProcessIdentityStatus(1<<30, "").Status; got != processIdentityStale {
		t.Fatalf("absent tokenless identity = %v, want stale", got)
	}
	if exactProcessIdentityMatch(os.Getpid(), "") {
		t.Fatal("cleanup compatibility authorized a tokenless live process")
	}
}

func TestPIDPathRemovalRequiresDefiniteStaleIdentity(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation processIdentityObservation
		want        bool
	}{
		{name: "unknown", observation: processIdentityObservation{Status: processIdentityUnknown}},
		{name: "matching", observation: processIdentityObservation{Status: processIdentityMatches, ProcStart: "current"}},
		{name: "stale", observation: processIdentityObservation{Status: processIdentityStale}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := processIdentityAllowsPIDPathRemoval(test.observation); got != test.want {
				t.Fatalf("PID path removal = %v, want %v for %+v", got, test.want, test.observation)
			}
		})
	}
}
