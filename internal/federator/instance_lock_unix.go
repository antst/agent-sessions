//go:build linux || darwin

package federator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireAgentInstanceLock(runtimeDir string) (*os.File, error) {
	return acquireAgentFileLock(filepath.Join(runtimeDir, "agent.lock"), "runtime directory "+runtimeDir)
}

func acquireAgentRegistryLock(configDir string) (*os.File, error) {
	return acquireAgentFileLock(
		filepath.Join(configDir, ".peer-federator-agent.lock"),
		"claude registry "+configDir,
	)
}

func acquireAgentFileLock(path, owner string) (*os.File, error) {
	// #nosec G304 -- path is a fixed lock name under a configured runtime or registry directory.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("another peer-federator agent already owns %s", owner)
		}
		return nil, err
	}
	return file, nil
}

func releaseAgentInstanceLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
