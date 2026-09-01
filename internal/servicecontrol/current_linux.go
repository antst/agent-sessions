//go:build linux

package servicecontrol

// Current returns the systemd user-service manager on Linux.
func Current() (Manager, error) { return NewLinux(nil), nil }
