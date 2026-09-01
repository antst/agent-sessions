package bridge

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

// GrokNativeObserver is the legacy ACP observer/interjection transport,
// retained behind a narrow API for the unified daemon. It never loads or owns
// a second native session; it observes the TUI-owned resident actor through
// the launch's private leader.
type GrokNativeObserver struct {
	client    *grokACPClient
	sessionID string
}

// OpenGrokNativeObserver connects to one private leader, performs the exact
// cached-token ACP handshake, and proves the resident session in the roster.
func OpenGrokNativeObserver(
	ctx context.Context,
	bin, cwd, leaderSocket, sessionID string,
	environment []string,
	diagnostics io.Writer,
) (*GrokNativeObserver, error) {
	observer, _, err := openGrokNativeObserver(ctx, bin, cwd, leaderSocket, sessionID, false, environment, diagnostics)
	return observer, err
}

// OpenGrokNativeSelectionObserver asks the product which resident session a
// native --resume selector opened. The provisional ID only scopes the launch;
// the returned UUID comes from Grok's live session roster.
func OpenGrokNativeSelectionObserver(
	ctx context.Context,
	bin, cwd, leaderSocket, provisionalID string,
	environment []string,
	diagnostics io.Writer,
) (*GrokNativeObserver, string, error) {
	return openGrokNativeObserver(ctx, bin, cwd, leaderSocket, provisionalID, true, environment, diagnostics)
}

func openGrokNativeObserver(
	ctx context.Context,
	bin, cwd, leaderSocket, sessionID string,
	selectResident bool,
	environment []string,
	diagnostics io.Writer,
) (*GrokNativeObserver, string, error) {
	if ctx == nil || !validSessionID(sessionID) || !strings.HasPrefix(leaderSocket, "/") {
		return nil, "", errors.New("invalid Grok ACP observer identity")
	}
	command := exec.CommandContext(ctx, bin, //nolint:gosec // Exact native binary is selected and validated by the daemon preparation.
		"--no-auto-update", "--permission-mode", "default",
		"--leader-socket", leaderSocket, "agent", "--leader", "stdio",
	)
	command.Dir = cwd
	command.Env = append([]string(nil), environment...)
	if diagnostics == nil {
		diagnostics = os.Stderr
	}
	command.Stderr = diagnostics
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	process, err := startGrokManagedProcess(command, nil)
	if err != nil {
		return nil, "", err
	}
	client := newGrokACPClient(process, stdin, stdout, sessionID, 1, make(chan grokRosterState, 4))
	fail := func(cause error) (*GrokNativeObserver, string, error) {
		client.close()
		return nil, "", cause
	}
	handshake, cancel := context.WithTimeout(ctx, grokACPStartupTimeout)
	defer cancel()
	initialized, err := client.request(handshake, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return fail(err)
	}
	if !grokCachedTokenAdvertised(initialized) {
		return fail(errors.New("grok ACP cached_token authentication is unavailable"))
	}
	if _, err := client.request(handshake, "authenticate", map[string]any{
		"methodId": "cached_token", "_meta": map[string]any{"headless": true},
	}); err != nil {
		return fail(err)
	}
	roster, err := client.request(handshake, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return fail(err)
	}
	selectedID := sessionID
	if selectResident {
		selectedID, _, err = grokSelectedResidentSession(roster)
	} else {
		_, err = grokRosterStateFromResponse(roster, sessionID)
	}
	if err != nil {
		return fail(err)
	}
	return &GrokNativeObserver{client: client, sessionID: selectedID}, selectedID, nil
}

// Interject delivers exactly one immutable message to the resident actor.
func (observer *GrokNativeObserver) Interject(ctx context.Context, messageID, text string) error {
	if observer == nil || observer.client == nil {
		return errors.New("grok ACP observer is unavailable")
	}
	deadline, cancel := context.WithTimeout(ctx, grokACPInterjectTimeout)
	defer cancel()
	return observer.client.requestInterjection(deadline, observer.sessionID, messageID, text)
}

// SessionName returns the current product-native title for the exact resident
// session already corroborated by this observer.
func (observer *GrokNativeObserver) SessionName(ctx context.Context) (string, error) {
	if observer == nil || observer.client == nil {
		return "", errors.New("grok ACP observer is unavailable")
	}
	deadline, cancel := context.WithTimeout(ctx, grokACPInterjectTimeout)
	defer cancel()
	roster, err := observer.client.request(deadline, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return "", err
	}
	state, err := grokRosterStateFromResponse(roster, observer.sessionID)
	if err != nil {
		return "", err
	}
	return state.name, nil
}

// Close releases only the observer process; the TUI and private leader remain
// owned by their attachment lifecycle.
func (observer *GrokNativeObserver) Close() {
	if observer != nil && observer.client != nil {
		observer.client.close()
		observer.client = nil
	}
}
