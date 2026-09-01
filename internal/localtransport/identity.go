// Package localtransport owns platform-neutral local transport records. The
// platform socket mechanics land separately; importing this package never
// opens a socket or performs product-runtime dispatch.
package localtransport

// PeerIdentity is kernel-provided identity captured at local socket accept
// time. PID and UID must both be corroborated by the caller before they
// authorize a managed attachment.
type PeerIdentity struct {
	PID int `json:"pid"`
	UID int `json:"uid"`
}

// Valid reports whether both kernel identity fields are usable.
func (p PeerIdentity) Valid() bool {
	return p.PID > 1 && p.UID >= 0
}
