package launcher

import (
	"errors"
	"reflect"
	"testing"
)

func TestLaneHelpDoesNotActivateRuntime(t *testing.T) {
	previousRequire, previousDiscover, previousExec := requireLaneDaemon, discoverLaneRuntime, execLaneRuntime
	t.Cleanup(func() {
		requireLaneDaemon, discoverLaneRuntime, execLaneRuntime = previousRequire, previousDiscover, previousExec
	})
	requireCalls, discoverCalls := 0, 0
	requireLaneDaemon = func() error {
		requireCalls++
		return nil
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
	if requireCalls != 0 || discoverCalls != 4 {
		t.Fatalf("help runtime selection: require=%d discover=%d", requireCalls, discoverCalls)
	}
}

func TestLaneCommandsRequireDaemonWithoutActivatingRuntime(t *testing.T) {
	previousRequire, previousDiscover, previousExec := requireLaneDaemon, discoverLaneRuntime, execLaneRuntime
	t.Cleanup(func() {
		requireLaneDaemon, discoverLaneRuntime, execLaneRuntime = previousRequire, previousDiscover, previousExec
	})
	requireCalls, discoverCalls := 0, 0
	requireLaneDaemon = func() error {
		requireCalls++
		return nil
	}
	discoverLaneRuntime = func() (Runtime, error) {
		discoverCalls++
		return Runtime{Path: "/packaged/agent-session-runtime"}, nil
	}
	execLaneRuntime = func(string, []string, []string) error { return nil }

	if err := RunLane("qwen-lane", []string{"list", "--all"}); err != nil {
		t.Fatal(err)
	}
	if requireCalls != 1 || discoverCalls != 1 {
		t.Fatalf("command runtime selection: require=%d discover=%d", requireCalls, discoverCalls)
	}
}

func TestLaneUnavailableDoesNotDiscoverOrExecRuntime(t *testing.T) {
	previousRequire, previousDiscover, previousExec := requireLaneDaemon, discoverLaneRuntime, execLaneRuntime
	t.Cleanup(func() {
		requireLaneDaemon, discoverLaneRuntime, execLaneRuntime = previousRequire, previousDiscover, previousExec
	})
	want := errors.New("daemon unavailable")
	requireLaneDaemon = func() error { return want }
	discoverLaneRuntime = func() (Runtime, error) {
		t.Fatal("unavailable lane discovered a runtime")
		return Runtime{}, nil
	}
	execLaneRuntime = func(string, []string, []string) error {
		t.Fatal("unavailable lane executed a runtime")
		return nil
	}
	if err := RunLane("qwen-lane", []string{"list", "--all"}); !errors.Is(err, want) {
		t.Fatalf("RunLane error = %v, want %v", err, want)
	}
}
