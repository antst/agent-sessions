//go:build !darwin && !linux

package procinfo

import "errors"

// Read cannot establish a process identity on unsupported hosts.
func Read(_ int) Info {
	return Info{Status: Unknown}
}

// Args reports that argv inspection is unavailable on unsupported hosts.
func Args(_ int) ([]string, error) {
	return nil, errors.New("process argv inspection is unsupported on this platform")
}

// Environment reports that process environment inspection is unavailable.
func Environment(_ int) ([]string, error) {
	return nil, errors.New("process environment inspection is unsupported on this platform")
}
