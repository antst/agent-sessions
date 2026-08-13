//go:build linux

package bridge

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLinuxProcessIdentityObservationMatchesProcStat(t *testing.T) {
	pid := os.Getpid()
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	closingParen := bytes.LastIndexByte(body, ')')
	if closingParen < 0 {
		t.Fatal("current process stat has no command terminator")
	}
	fields := strings.Fields(string(body[closingParen+1:]))
	if len(fields) <= 19 {
		t.Fatalf("current process stat has %d trailing fields", len(fields))
	}
	observation := observeProcessIdentity(pid, fields[19])
	if observation.Status != processIdentityMatches || observation.ProcStart != fields[19] {
		t.Fatalf("process observation = %+v, want start token %q", observation, fields[19])
	}
	if reused := observeProcessIdentity(pid, fields[19]+"-reused"); reused.Status != processIdentityStale {
		t.Fatalf("mismatched process observation = %+v", reused)
	}
}
