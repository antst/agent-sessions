//go:build !linux && !darwin

package bridge

import "errors"

type qwenToolRegistryGuard struct{}

func (m *qwenLaneManager) prepareToolRegistry() error         { return nil }
func (m *qwenLaneManager) prepareSharedToolRootLedger() error { return nil }
func qwenLaneWorkerToolEnvironment(environment []string, _ qwenLaneState) []string {
	return environment
}
func isQwenToolWrapperInvocation() bool { return false }
func runQwenToolWrapper([]string) int   { return 126 }
func lockQwenToolRegistry(qwenLaneState) (*qwenToolRegistryGuard, error) {
	return nil, errors.New("qwen tool-root ownership is unsupported on this platform")
}
func (guard *qwenToolRegistryGuard) close()                 {}
func (guard *qwenToolRegistryGuard) removeArtifacts() error { return nil }
