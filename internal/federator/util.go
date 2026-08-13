package federator

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/antst/agent-sessions/internal/fileutil"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

// RuntimeVersion is the build version published in federated registry rows.
// The command sets it from link-time version metadata before starting a daemon.
var RuntimeVersion = "dev"

func defaultLogger(logger *log.Logger) *log.Logger {
	if logger != nil {
		return logger
	}
	return log.New(os.Stderr, "peer-federator: ", log.LstdFlags|log.Lmicroseconds)
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func cleanID(value string) string {
	var output strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			output.WriteRune(r)
		}
		if output.Len() >= 48 {
			break
		}
	}
	return strings.Trim(output.String(), "_-")
}

func sessionKey(value string) string {
	return sessionkey.FromID(value)
}

func globalSessionID(hostID, sessionID string) string {
	// Claude/Codex derive the short confirmation ref from the leading session
	// ID characters. Lead with a digest of the complete remote identity so two
	// same-named sessions on one host never inherit the same host-only prefix.
	identity := hostID + "\x00" + sessionID
	value := sessionKey(identity) + "_" + cleanID(hostID) + "_" + cleanID(sessionID)
	if len(value) <= 100 {
		return value
	}
	return value[:100]
}

func qualifiedName(name, host string) string {
	name = cleanPeerName(defaultString(strings.TrimSpace(name), "peer"))
	host = cleanPeerName(defaultString(strings.TrimSpace(host), "host"))
	return name + "--" + host
}

func cleanPeerName(value string) string {
	var output strings.Builder
	separator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '.' || r == '_' || r == '-' {
			output.WriteRune(r)
			separator = false
		} else if !separator {
			output.WriteByte('-')
			separator = true
		}
		if output.Len() >= 72 {
			break
		}
	}
	return defaultString(strings.Trim(output.String(), "._-"), "peer")
}

func processLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func processStart(pid int) string {
	info := procinfo.Read(pid)
	if info.Status != procinfo.Known {
		return ""
	}
	return info.Start
}

func parentProcessID(pid int) (int, bool) {
	if pid <= 1 {
		return 0, false
	}
	info := procinfo.Read(pid)
	return info.Parent, info.Status == procinfo.Known && info.Parent > 0
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	// #nosec G302 -- directories need owner execute permission and are not group/world accessible.
	return os.Chmod(path, 0700)
}

func writeJSONAtomic(path string, value any) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return fileutil.WriteJSONAtomic(path, value)
}

func parsePID(name string) int {
	if filepath.Ext(name) != ".json" {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return pid
}

func waitFor(predicate func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return predicate()
}
