//go:build darwin

package servicecontrol

// Current returns the launchd user-agent manager on macOS.
func Current() (Manager, error) { return NewDarwin(nil) }
