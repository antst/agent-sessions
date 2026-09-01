package launcher

import "testing"

func TestGrokLaneLifecycleCommandsDoNotRequireGrokExecutable(t *testing.T) {
	t.Parallel()

	for _, command := range []string{"wait", "status", "interrupt", "archive", "list"} {
		if grokLaneNeedsExecutable([]string{command, "lane-a"}) {
			t.Fatalf("%s unexpectedly requires Grok executable resolution", command)
		}
	}
	for _, args := range [][]string{nil, {"--help"}, {"start", "--help"}} {
		if grokLaneNeedsExecutable(args) {
			t.Fatalf("help argv %#v unexpectedly requires Grok executable resolution", args)
		}
	}
	for _, command := range []string{"run", "start", "resume", "doctor"} {
		if !grokLaneNeedsExecutable([]string{command}) {
			t.Fatalf("%s must resolve the validated Grok executable", command)
		}
	}
}
