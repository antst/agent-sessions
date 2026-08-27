package launcher

import (
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federator"
)

var (
	requireLaneDaemon   = requireUserDaemon
	discoverLaneRuntime = discoverRuntime
	execLaneRuntime     = Exec
)

// RunLane requires the existing user daemon and replaces the launcher with a
// transitional native lane client. It never manages daemon lifetime.
func RunLane(role string, args []string) error {
	if _, ok := launcherProductByLaneRole(role); !ok {
		return fmt.Errorf("unsupported lane role %q", role)
	}
	if laneHelpRequested(args) {
		// Help is a read-only parser surface. It must remain available while a
		// daemon is explicitly stopped and therefore never queries or activates
		// service lifetime.
	} else if err := requireLaneDaemon(); err != nil {
		return err
	}
	selected, err := discoverLaneRuntime()
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
