//go:build !darwin

// Package socketpath centralizes platform Unix-domain socket path budgets.
package socketpath

// Limit is the maximum pathname payload in Linux's 108-byte sun_path field.
// Linux and macOS are the supported runtime platforms.
func Limit() int { return 107 }
