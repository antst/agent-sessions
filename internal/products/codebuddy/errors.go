package codebuddy

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/antst/agent-sessions/internal/productruntime"
)

var (
	ErrInvalidConfiguration = errors.New("codebuddy configuration is invalid")
	ErrInvalidRegistry      = errors.New("codebuddy worker registry is invalid")
	ErrWorkerNotFound       = errors.New("codebuddy interactive worker was not found")
	ErrWorkerAmbiguous      = errors.New("codebuddy interactive worker is ambiguous")
	ErrSocketOwner          = errors.New("codebuddy socket owner could not be corroborated")
	ErrEntrypoint           = errors.New("codebuddy executable identity could not be corroborated")
	ErrOpenAPIDrift         = errors.New("codebuddy openapi contract drifted")
	ErrSecretPersisted      = errors.New("codebuddy lane password was persisted")
)

func nativeStatusError(operation string, status int) error {
	var category error
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		category = productruntime.ErrUnauthorized
	case http.StatusNotFound, http.StatusGone:
		category = productruntime.ErrStale
	case http.StatusConflict:
		category = productruntime.ErrNativeRejected
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		category = productruntime.ErrTimedOut
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		category = productruntime.ErrUnavailable
	default:
		if status >= 500 {
			category = productruntime.ErrUnavailable
		} else {
			category = productruntime.ErrNativeRejected
		}
	}
	return fmt.Errorf("%w: %s returned HTTP %d", category, operation, status)
}
