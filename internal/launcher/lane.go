package launcher

import (
	"fmt"
	"os"
	"strings"

	"github.com/antst/agent-sessions/internal/federator"
)

// RunLane ensures the shared runtime and replaces the launcher with one of its
// native lane clients.
func RunLane(role string, args []string) error {
	selected, err := EnsureRuntime()
	if err != nil {
		return err
	}
	if _, ok := launcherProductByLaneRole(role); !ok {
		return fmt.Errorf("unsupported lane role %q", role)
	}
	environment := replaceLaneEnvironment(os.Environ(), agentRuntimeDirEnv, agentRuntimeDir())
	if role == "grok-lane" && grokLaneNeedsExecutable(args) {
		grok, err := grokExecutable()
		if err != nil {
			return err
		}
		environment = replaceLaneEnvironment(environment, "GROK_PEER_GROK_BIN", grok)
		environment = replaceLaneEnvironment(environment, "GROK_PEER_NATIVE_RUNTIME", selected.Path)
	}
	return Exec(selected.Path, append([]string{role}, args...), environment)
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

func replaceLaneEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			updated = append(updated, entry)
		}
	}
	return append(updated, prefix+value)
}
