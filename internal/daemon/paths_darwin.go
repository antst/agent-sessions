//go:build darwin

package daemon

import (
	"fmt"
	"path/filepath"
)

func platformRuntimeRoot(uid int) (string, error) {
	return filepath.Clean(fmt.Sprintf("/tmp/agent-sessions-%d", uid)), nil
}
