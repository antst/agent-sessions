package federation

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
)

// ClaudeServiceKeyName derives Claude's socket-bound native service-key name.
// It is a logical connector identity; no legacy routing-agent process owns it.
func ClaudeServiceKeyName(pid int, socket string) (string, error) {
	if pid <= 1 || !filepath.IsAbs(socket) {
		return "", errors.New("invalid Claude service key identity")
	}
	digest := sha256.Sum256([]byte(filepath.Clean(socket)))
	return strconv.Itoa(pid) + "." + fmt.Sprintf("%x", digest[:]) + ".key", nil
}
