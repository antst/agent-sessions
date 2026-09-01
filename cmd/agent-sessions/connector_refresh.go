package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type connectorImageRefresher struct {
	target           string
	launched         os.FileInfo
	launchedIdentity string
	args             []string
	environ          []string
	exec             func(string, []string, []string) error
}

func newConnectorImageRefresher(args, environ []string) (*connectorImageRefresher, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return nil, errors.New("connector executable argument is unavailable")
	}
	target := args[0]
	if !strings.ContainsRune(target, filepath.Separator) {
		resolved, err := exec.LookPath(target)
		if err != nil {
			return nil, err
		}
		target = resolved
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	launched, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	identity, err := connectorBinaryIdentity(absolute)
	if err != nil {
		return nil, err
	}
	return &connectorImageRefresher{
		target: absolute, launched: launched, launchedIdentity: identity,
		args: append([]string(nil), args...), environ: append([]string(nil), environ...),
		exec: syscall.Exec,
	}, nil
}

func (r *connectorImageRefresher) refresh() error {
	if r == nil {
		return nil
	}
	// #nosec G703 -- target is the absolute executable captured from this process's argv.
	current, err := os.Stat(r.target)
	if err != nil {
		return nil //nolint:nilerr // Keep serving from the loaded image during an atomic install window.
	}
	identity := r.launchedIdentity
	if !os.SameFile(r.launched, current) {
		identity, err = connectorBinaryIdentity(r.target)
		if err != nil {
			return nil //nolint:nilerr // Keep serving from the loaded image during an atomic install window.
		}
	}
	args, exact := connectorArgsWithReleaseIdentity(r.args, identity)
	if os.SameFile(r.launched, current) && exact {
		return nil
	}
	args[0] = r.target
	// Exec preserves the vendor-owned stdin/stdout descriptors and PID,
	// replacing only our stale connector image between protocol frames. The
	// argv identity changes with the image so process census never reports the
	// source placeholder or a previous release after the exec succeeds.
	return r.exec(r.target, args, append([]string(nil), r.environ...))
}

func connectorBinaryIdentity(path string) (string, error) {
	// #nosec G304,G703 -- path is the absolute connector executable captured above.
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func connectorArgsWithReleaseIdentity(arguments []string, identity string) ([]string, bool) {
	result := append([]string(nil), arguments...)
	for index := 0; index < len(result); index++ {
		switch {
		case result[index] == "--release-identity":
			if index+1 < len(result) {
				exact := result[index+1] == identity
				result[index+1] = identity
				return result, exact
			}
			result = append(result, identity)
			return result, false
		case strings.HasPrefix(result[index], "--release-identity="):
			exact := strings.TrimPrefix(result[index], "--release-identity=") == identity
			result[index] = "--release-identity=" + identity
			return result, exact
		}
	}
	return append(result, "--release-identity", identity), false
}
