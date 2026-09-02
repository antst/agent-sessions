package bridge

import "github.com/antst/agent-sessions/internal/sessiontools"

// NormalizePeerName applies the public peer-address normalization used by the
// central daemon's live roster.
func NormalizePeerName(value string) string { return sessiontools.NormalizePeerName(value) }

// WrapPeerMessage delegates the product-owned carrier envelope to the shared
// session-tools implementation.
func WrapPeerMessage(product, from, sessionID, name, mode, messageID, sentAt, message string) string {
	wrapped, err := sessiontools.WrapPeerMessage(product, from, sessionID, name, mode, messageID, sentAt, message)
	if err != nil {
		panic(err)
	}
	return wrapped
}
