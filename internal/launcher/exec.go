package launcher

import (
	"os"
	"syscall"
)

// Exec replaces the launcher with the selected command while preserving the
// launcher's PID for exact owner attestation.
func Exec(path string, args []string, env []string) error {
	if env == nil {
		env = os.Environ()
	}
	return syscall.Exec(path, append([]string{path}, args...), env) //nolint:gosec // explicit installed executable.
}
