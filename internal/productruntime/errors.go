// Package productruntime defines product-neutral native driver contracts and
// the explicit runtime registry. It owns no daemon lifecycle state.
package productruntime

import "errors"

var (
	ErrUnavailable         = errors.New("product runtime unavailable")
	ErrIncompatible        = errors.New("product runtime incompatible")
	ErrUnauthorized        = errors.New("product runtime unauthorized")
	ErrStale               = errors.New("product runtime stale")
	ErrAmbiguousSession    = errors.New("product runtime session ambiguous")
	ErrUnsupportedPolicy   = errors.New("product runtime permission policy unsupported")
	ErrUnsupportedRename   = errors.New("product runtime rename unsupported")
	ErrUnsupportedSteer    = errors.New("product runtime steer unsupported")
	ErrUnsupportedRecovery = errors.New("product runtime recovery unsupported")
	ErrNativeRejected      = errors.New("product native operation rejected")
	ErrProtocol            = errors.New("product native protocol error")
	ErrTimedOut            = errors.New("product native operation timed out")
	ErrCleanupDebt         = errors.New("product native cleanup debt")
)
