// Package daemon owns the single per-user Agent Sessions host runtime.
//
// The package is the composition root for local control, managed attachments,
// delivery, lanes, outbound federation, recovery, diagnostics, and migration.
// Native vendor processes remain external, and the central federation hub is
// implemented by the separate agent-sessions-hub executable.
package daemon
