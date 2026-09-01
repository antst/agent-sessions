package launcher

import (
	"reflect"
	"testing"
)

func TestLaneHelpDoesNotActivateRuntime(t *testing.T) {
	previousEnsure, previousDiscover, previousExec := ensureLaneRuntime, discoverLaneRuntime, execLaneRuntime
	t.Cleanup(func() {
		ensureLaneRuntime, discoverLaneRuntime, execLaneRuntime = previousEnsure, previousDiscover, previousExec
	})
	ensureCalls, discoverCalls := 0, 0
	ensureLaneRuntime = func() (Runtime, error) {
		ensureCalls++
		return Runtime{Path: "/must-not-activate"}, nil
	}
	discoverLaneRuntime = func() (Runtime, error) {
		discoverCalls++
		return Runtime{Path: "/packaged/agent-session-runtime"}, nil
	}
	var gotPath string
	var gotArgs []string
	execLaneRuntime = func(path string, args []string, _ []string) error {
		gotPath = path
		gotArgs = append([]string(nil), args...)
		return nil
	}

	for _, role := range []string{"lane", "claude-lane", "grok-lane", "qwen-lane"} {
		gotPath, gotArgs = "", nil
		if err := RunLane(role, []string{"--help"}); err != nil {
			t.Fatalf("%s help: %v", role, err)
		}
		if gotPath != "/packaged/agent-session-runtime" || !reflect.DeepEqual(gotArgs, []string{role, "--help"}) {
			t.Fatalf("%s help dispatch = %q %#v", role, gotPath, gotArgs)
		}
	}
	if ensureCalls != 0 || discoverCalls != 4 {
		t.Fatalf("help runtime selection: ensure=%d discover=%d", ensureCalls, discoverCalls)
	}
}

func TestLaneCommandsStillActivateRuntime(t *testing.T) {
	previousEnsure, previousDiscover, previousExec := ensureLaneRuntime, discoverLaneRuntime, execLaneRuntime
	t.Cleanup(func() {
		ensureLaneRuntime, discoverLaneRuntime, execLaneRuntime = previousEnsure, previousDiscover, previousExec
	})
	ensureCalls, discoverCalls := 0, 0
	ensureLaneRuntime = func() (Runtime, error) {
		ensureCalls++
		return Runtime{Path: "/packaged/agent-session-runtime"}, nil
	}
	discoverLaneRuntime = func() (Runtime, error) {
		discoverCalls++
		return Runtime{}, nil
	}
	execLaneRuntime = func(string, []string, []string) error { return nil }

	if err := RunLane("qwen-lane", []string{"list", "--all"}); err != nil {
		t.Fatal(err)
	}
	if ensureCalls != 1 || discoverCalls != 0 {
		t.Fatalf("command runtime selection: ensure=%d discover=%d", ensureCalls, discoverCalls)
	}
}
