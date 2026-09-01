//go:build darwin && !cgo

package codebuddy

func NewPlatformSocketOwnerVerifier() SocketOwnerVerifier {
	return newLsofSocketOwnerVerifier(CommandRunnerFunc(runBoundedCommand))
}
