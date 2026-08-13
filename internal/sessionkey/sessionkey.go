// Package sessionkey defines the stable, filename-safe key shared by local
// lifecycle state and federated session projections.
package sessionkey

import (
	"crypto/sha256"
	"encoding/hex"
)

// FromID returns the established 20-character key for one durable identity.
func FromID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:20]
}
