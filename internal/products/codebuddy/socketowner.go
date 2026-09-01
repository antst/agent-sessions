package codebuddy

import "context"

type SocketObservation struct {
	PID      int
	Endpoint string
	Identity string
}

type SocketOwnerVerifier interface {
	VerifyOwner(context.Context, string, int) (SocketObservation, error)
}

type SocketOwnerVerifierFunc func(context.Context, string, int) (SocketObservation, error)

func (function SocketOwnerVerifierFunc) VerifyOwner(ctx context.Context, endpoint string, pid int) (SocketObservation, error) {
	return function(ctx, endpoint, pid)
}
