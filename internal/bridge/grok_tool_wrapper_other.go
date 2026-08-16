//go:build !linux && !darwin

package bridge

import "errors"

const grokToolWrapperModeEnv = "AGENT_SESSIONS_GROK_TOOL_WRAPPER"
const grokToolWrapperPathEnv = "AGENT_SESSIONS_GROK_WRAPPER_PATH"

type grokToolRegistryGuard struct{}

func runGrokToolWrapper(_ []string) int { return 126 }
func isGrokToolWrapperInvocation() bool { return false }

func (m *grokLaneManager) prepareToolRegistry() error {
	return errors.New("grok lane tool registry is unsupported on this platform")
}

func grokLaneWorkerEnvironment(environment []string, _ string, _ grokLaneState, _, _ string) []string {
	return environment
}

func grokLaneWorkerRoot(state grokLaneState) grokSessionMember {
	return grokSessionMember{PID: state.WorkerPID, ProcStart: state.WorkerProcStart, StrongStart: state.WorkerStrongStart}
}

func grokLaneCleanupRoots(state grokLaneState, _ bool) (*grokToolRegistryGuard, []grokSessionMember, error) {
	return &grokToolRegistryGuard{}, []grokSessionMember{grokLaneWorkerRoot(state)}, nil
}

func (guard *grokToolRegistryGuard) close()                 {}
func (guard *grokToolRegistryGuard) removeArtifacts() error { return nil }
