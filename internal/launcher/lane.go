package launcher

import "fmt"

// RunLane ensures the shared runtime and replaces the launcher with one of its
// native lane clients.
func RunLane(role string, args []string) error {
	selected, err := EnsureRuntime()
	if err != nil {
		return err
	}
	if role != "lane" && role != "claude-lane" {
		return fmt.Errorf("unsupported lane role %q", role)
	}
	return Exec(selected.Path, append([]string{role}, args...), nil)
}
