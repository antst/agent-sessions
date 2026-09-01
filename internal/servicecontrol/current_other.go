//go:build !linux && !darwin

package servicecontrol

import "errors"

// Current reports the closed supported-platform boundary.
func Current() (Manager, error) {
	return nil, errors.New("Agent Sessions user service control is supported only on Linux and macOS")
}
