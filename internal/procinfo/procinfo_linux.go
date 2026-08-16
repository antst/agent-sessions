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
		environment = append(environment, string(part))
	}
	return environment, nil
}
