package launcher

import (
	"fmt"
	"os"
	"strings"
)

// RunLane ensures the shared runtime and replaces the launcher with one of its
// native lane clients.
func RunLane(role string, args []string) error {
	selected, err := EnsureRuntime()
	if err != nil {
		return err
	}
	if role != "lane" && role != "claude-lane" && role != "grok-lane" {
		return fmt.Errorf("unsupported lane role %q", role)
	}
	environment := []string(nil)
	if role == "grok-lane" {
		grok, err := grokExecutable()
		if err != nil {
			return err
		}
		environment = replaceLaneEnvironment(os.Environ(), "GROK_PEER_GROK_BIN", grok)
	}
	return Exec(selected.Path, append([]string{role}, args...), environment)
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
