//go:build darwin

// Package socketpath centralizes platform Unix-domain socket path budgets.
package socketpath

// Limit is the maximum pathname payload in Darwin's 104-byte sun_path field.
func Limit() int { return 103 }
