package launcher

import (
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federator"
)

var (
	ensureLaneRuntime   = EnsureRuntime
	discoverLaneRuntime = discoverRuntime
	execLaneRuntime     = Exec
)

// RunLane ensures the shared runtime and replaces the launcher with one of its
// native lane clients.
func RunLane(role string, args []string) error {
	if _, ok := launcherProductByLaneRole(role); !ok {
		return fmt.Errorf("unsupported lane role %q", role)
	}
	resolve := ensureLaneRuntime
	if laneHelpRequested(args) {
		// Help is a read-only parser surface. It must remain available while a
		// different installed plugin version is live and therefore cannot run
		// activation, supervisor, or App Server bootstrap first.
		resolve = discoverLaneRuntime
	}
	selected, err := resolve()
	if err != nil {
		return err
	}
	environment := envutil.Set(os.Environ(), agentRuntimeDirEnv, agentRuntimeDir())
	if role == "grok-lane" && grokLaneNeedsExecutable(args) {
		grok, err := grokExecutable()
		if err != nil {
			return err
		}
		environment = envutil.Set(environment, "GROK_PEER_GROK_BIN", grok)
		environment = envutil.Set(environment, "GROK_PEER_NATIVE_RUNTIME", selected.Path)
	}
	if role == "qwen-lane" && qwenLaneNeedsExecutable(args) {
		qwen, err := qwenExecutable()
		if err != nil {
			return err
		}
		environment = envutil.Set(environment, "QWEN_PEER_QWEN_BIN", qwen)
	}
	return execLaneRuntime(selected.Path, append([]string{role}, args...), environment)
}

func laneHelpRequested(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, argument := range args {
		if argument == "-h" || argument == "--help" {
			return true
		}
	}
	return false
}

func launcherProductByLaneRole(role string) (launcherProduct, bool) {
	for _, descriptor := range federator.ProductDescriptors() {
		if descriptor.LaneRuntimeRole == role {
			return launcherProduct{descriptor: descriptor}, true
		}
	}
	return launcherProduct{}, false
}

func grokLaneNeedsExecutable(args []string) bool {
	for _, argument := range args {
		if argument == "-h" || argument == "--help" {
			return false
		}
	}
	return len(args) > 0 && (args[0] == "run" || args[0] == "start" || args[0] == "resume" || args[0] == "doctor")
}

func qwenLaneNeedsExecutable(args []string) bool {
	for _, argument := range args {
		if argument == "-h" || argument == "--help" {
			return false
		}
	}
	return len(args) > 0 && (args[0] == "run" || args[0] == "start" || args[0] == "resume" || args[0] == "doctor")
}
