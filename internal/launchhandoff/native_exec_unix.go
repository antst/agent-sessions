//go:build linux || darwin

package launchhandoff

import "golang.org/x/sys/unix"

// nativeExec is the sole production image-replacement path. The handoff
// socket has already been explicitly closed and marked CLOEXEC before entry.
func nativeExec(path string, args, environment []string, cwd string) error {
	if err := unix.Chdir(cwd); err != nil {
		return err
	}
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, path)
	argv = append(argv, args...)
	return unix.Exec(path, argv, environment)
}
