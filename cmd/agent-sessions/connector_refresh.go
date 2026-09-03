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
	"sync"
	"syscall"
)

type connectorImageRefresher struct {
	mu               sync.Mutex
	target           string
	launchedIdentity string
	daemonIdentity   string
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
	identity, err := connectorBinaryIdentity(absolute)
	if err != nil {
		return nil, err
	}
	return &connectorImageRefresher{
		target: absolute, launchedIdentity: identity,
		args: append([]string(nil), args...), environ: append([]string(nil), environ...),
		exec: syscall.Exec,
	}, nil
}

func (r *connectorImageRefresher) observeDaemon(identity string) {
	if r == nil || !validConnectorReleaseIdentity(identity) {
		return
	}
	r.mu.Lock()
	r.daemonIdentity = identity
	r.mu.Unlock()
}

func (r *connectorImageRefresher) refresh() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.daemonIdentity == "" || r.daemonIdentity == r.launchedIdentity {
		return nil
	}
	identity, err := connectorBinaryIdentity(r.target)
	if err != nil || identity != r.daemonIdentity {
		return nil //nolint:nilerr // A staged or rolled-back file is not the ready daemon's release.
	}
	args, _ := connectorArgsWithReleaseIdentity(r.args, r.daemonIdentity)
	args[0] = r.target
	// Exec preserves the vendor-owned stdin/stdout descriptors and PID,
	// replacing only an image attested by the ready daemon. The MCP response
	// carrying that identity has already reached the vendor before this runs.
	if err := r.exec(r.target, args, append([]string(nil), r.environ...)); err != nil {
		return err
	}
	r.launchedIdentity = r.daemonIdentity
	r.args = args
	return nil
}

func validConnectorReleaseIdentity(identity string) bool {
	decoded, err := hex.DecodeString(identity)
	return err == nil && len(decoded) == sha256.Size
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
