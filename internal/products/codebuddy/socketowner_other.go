//go:build !linux && !darwin

package codebuddy

import "context"

type unsupportedSocketOwner struct{}

func NewPlatformSocketOwnerVerifier() SocketOwnerVerifier { return unsupportedSocketOwner{} }

func (unsupportedSocketOwner) VerifyOwner(context.Context, string, int) (SocketObservation, error) {
	return SocketObservation{}, ErrSocketOwner
}
