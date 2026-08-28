//go:build linux

package procinfo

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Read parses /proc once so process start and ancestry use the same snapshot.
func Read(pid int) Info {
	if pid <= 0 {
		return Info{Status: Absent}
	}
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return Info{Status: Absent}
	}
	if err != nil {
		return Info{Status: Unknown}
	}
	closingParen := bytes.LastIndexByte(body, ')')
	if closingParen < 0 {
		return Info{Status: Unknown}
	}
	fields := strings.Fields(string(body[closingParen+1:]))
	if len(fields) <= 19 {
		return Info{Status: Unknown}
	}
	parent, _ := strconv.Atoi(fields[1])
	return Info{Status: Known, State: fields[0], Start: fields[19], StrongStart: fields[19], Parent: parent}
}

// Args reads the process argument vector without losing argument boundaries.
func Args(pid int) ([]string, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(bytes.TrimRight(body, "\x00"), []byte{0})
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		args = append(args, string(part))
	}
	return args, nil
}

// Environment reads the process environment without losing entry boundaries.
func Environment(pid int) ([]string, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(bytes.TrimRight(body, "\x00"), []byte{0})
	environment := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		environment = append(environment, string(part))
	}
	return observableEnvironment(pid, environment)
}

// List returns one best-effort coherent identity for every observable process.
// A disappearing process is omitted; an unreadable live identity fails closed.
func List() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	result := make([]Process, 0, len(entries))
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 1 {
			continue
		}
		info := Read(pid)
		process, present, classifyErr := reconcileListedProcess(pid, info, Read)
		if classifyErr != nil {
			return nil, classifyErr
		}
		if present {
			result = append(result, process)
		}
	}
	return result, nil
}

// reconcileListedProcess closes the unavoidable /proc readdir/read race. A
// process that disappears after directory enumeration is not live evidence;
// a process that remains unidentifiable after one immediate re-observation
// still fails closed for identity-sensitive callers.
func reconcileListedProcess(pid int, info Info, observe func(int) Info) (Process, bool, error) {
	if info.Status == Unknown {
		info = observe(pid)
	}
	switch info.Status {
	case Absent:
		return Process{}, false, nil
	case Known:
		if info.State == "Z" || info.State == "X" {
			return Process{}, false, nil
		}
		return Process{PID: pid, Info: info}, true, nil
	case Unknown:
		return Process{}, false, fmt.Errorf("cannot identify live process %d", pid)
	default:
		return Process{}, false, fmt.Errorf("process %d has invalid identity status %d", pid, info.Status)
	}
}
