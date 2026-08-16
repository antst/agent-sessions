//go:build darwin

package procinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const zombieState = 5

// Read uses Darwin's kernel process table. The formatted start token is the
// LC_ALL=C TZ=UTC `ps -o lstart=` value defined by Claude's shared registry.
func Read(pid int) Info {
	if pid <= 0 {
		return Info{Status: Absent}
	}
	if err := unix.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Info{Status: Absent}
		}
		return Info{Status: Unknown}
	}
	value, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || value == nil {
		if killErr := unix.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return Info{Status: Absent}
		}
		return Info{Status: Unknown}
	}
	state := "L"
	if value.Proc.P_stat == zombieState {
		state = "Z"
	}
	started := time.Unix(value.Proc.P_starttime.Sec, int64(value.Proc.P_starttime.Usec)*int64(time.Microsecond)).UTC()
	return Info{
		Status:      Known,
		State:       state,
		Start:       started.Format("Mon Jan _2 15:04:05 2006"),
		StrongStart: fmt.Sprintf("%d:%06d", value.Proc.P_starttime.Sec, value.Proc.P_starttime.Usec),
		Parent:      int(value.Eproc.Ppid),
	}
}

// Args uses KERN_PROCARGS2 so prompts that resemble flags remain distinct
// from the actual process argument vector.
func Args(pid int) ([]string, error) {
	_, args, err := processArgsAndEnvironment(pid)
	return args, err
}

// Environment uses the environment tail in Darwin's KERN_PROCARGS2 payload.
func Environment(pid int) ([]string, error) {
	environment, _, err := processArgsAndEnvironment(pid)
	return environment, err
}

func processArgsAndEnvironment(pid int) ([]string, []string, error) {
	body, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, err
	}
	if len(body) < 4 {
		return nil, nil, errors.New("kern.procargs2 response is truncated")
	}
	argc := int(binary.LittleEndian.Uint32(body[:4]))
	if argc <= 0 || argc > 65536 {
		return nil, nil, errors.New("kern.procargs2 returned an invalid argc")
	}
	rest := body[4:]
	executableEnd := bytes.IndexByte(rest, 0)
	if executableEnd < 0 {
		return nil, nil, errors.New("kern.procargs2 omitted executable terminator")
	}
	rest = bytes.TrimLeft(rest[executableEnd+1:], "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		args = append(args, string(rest[:end]))
		if end == len(rest) {
			rest = nil
		} else {
			rest = rest[end+1:]
		}
	}
	if len(args) != argc {
		return nil, nil, errors.New("kern.procargs2 omitted argv entries")
	}
	environment := make([]string, 0)
	rest = bytes.TrimLeft(rest, "\x00")
	for len(rest) > 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		entry := string(rest[:end])
		if strings.Contains(entry, "=") {
			environment = append(environment, entry)
		}
		if end == len(rest) {
			break
		}
		rest = bytes.TrimLeft(rest[end+1:], "\x00")
	}
	return environment, args, nil
}
