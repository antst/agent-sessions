//go:build !linux && !darwin

package bridge

import "errors"

func grokProcessSessionID(_ int) (int, error) {
	return 0, errors.New("Grok lane process sessions are unsupported on this platform")
}

type grokSessionMember struct {
	PID       int
	ProcStart string
}

func grokProcessSessionMembers(_ int) ([]grokSessionMember, error) {
	return nil, errors.New("Grok lane process-session inspection is unsupported")
}

func stopGrokProcessSession(sessionID int, _ string, _ int) error {
	if sessionID <= 1 {
		return nil
	}
	return errors.New("Grok lane process-session cleanup is unsupported")
}

func grokProcessSessionHasMembers(sessionID, _ int) bool { return sessionID > 1 }

func stopGrokTaggedProcesses(tokenHash string, _ int, _ ...grokSessionMember) error {
	if tokenHash == "" {
		return nil
	}
	return errors.New("Grok lane tagged-process cleanup is unsupported")
}

func grokTaggedProcessesRemain(tokenHash string, _ int, _ ...grokSessionMember) bool {
	return tokenHash != ""
}
