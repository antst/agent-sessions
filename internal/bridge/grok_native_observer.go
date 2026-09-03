package bridge

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// GrokNativeObserver is the legacy ACP observer/interjection transport,
// retained behind a narrow API for the unified daemon. It never loads or owns
// a second native session; it observes the TUI-owned resident actor through
// the launch's private leader.
type GrokNativeObserver struct {
	client    *grokACPClient
	sessionID string
}

// GrokNativeStartupHold is one authenticated native leader client kept open
// while the interactive client passes through its transient startup connects.
type GrokNativeStartupHold struct {
	once   sync.Once
	client *grokACPClient
}

func OpenGrokNativeStartupHold(
	ctx context.Context,
	bin, cwd, leaderSocket string,
	environment []string,
	diagnostics io.Writer,
) (*GrokNativeStartupHold, error) {
	if ctx == nil || !strings.HasPrefix(leaderSocket, "/") {
		return nil, errors.New("invalid Grok ACP startup hold")
	}
	client, err := openGrokAuthenticatedClient(ctx, bin, cwd, leaderSocket, "", environment, diagnostics)
	if err != nil {
		return nil, err
	}
	return &GrokNativeStartupHold{client: client}, nil
}

func (hold *GrokNativeStartupHold) Close() {
	if hold == nil {
		return
	}
	hold.once.Do(func() { hold.client.close() })
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
	client, err := openGrokAuthenticatedClient(ctx, bin, cwd, leaderSocket, sessionID, environment, diagnostics)
	if err != nil {
		return nil, "", err
	}
	fail := func(cause error) (*GrokNativeObserver, string, error) {
		client.close()
		return nil, "", cause
	}
	rosterCtx, cancel := context.WithTimeout(ctx, grokACPStartupTimeout)
	defer cancel()
	roster, err := client.request(rosterCtx, "_x.ai/sessions/list", map[string]any{})
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

func openGrokAuthenticatedClient(
	ctx context.Context,
	bin, cwd, leaderSocket, sessionID string,
	environment []string,
	diagnostics io.Writer,
) (*grokACPClient, error) {
	return openGrokAuthenticatedClientWithArguments(ctx, bin, cwd, leaderSocket, sessionID, environment, diagnostics, nil)
}

func openGrokAuthenticatedClientWithArguments(
	ctx context.Context,
	bin, cwd, leaderSocket, sessionID string,
	environment []string,
	diagnostics io.Writer,
	arguments []string,
) (*grokACPClient, error) {
	argv := []string{"--no-auto-update"}
	argv = append(argv, arguments...)
	argv = append(argv, "--leader-socket", leaderSocket, "agent", "--leader", "stdio")
	return openGrokAuthenticatedClientCommand(ctx, bin, cwd, sessionID, environment, diagnostics, argv)
}

func openGrokAuthenticatedClientCommand(
	ctx context.Context,
	bin, cwd, sessionID string,
	environment []string,
	diagnostics io.Writer,
	argv []string,
) (*grokACPClient, error) {
	command := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // Exact native binary is selected and validated by the daemon preparation.
	command.Dir = cwd
	command.Env = append([]string(nil), environment...)
	if diagnostics == nil {
		diagnostics = os.Stderr
	}
	command.Stderr = diagnostics
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	process, err := startGrokManagedProcess(command, nil)
	if err != nil {
		return nil, err
	}
	client := newGrokACPClient(process, stdin, stdout, sessionID, 1, make(chan grokRosterState, 4))
	fail := func(cause error) (*grokACPClient, error) {
		client.close()
		return nil, cause
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
	return client, nil
}

// GrokNativeSessionTitle asks Grok's own global roster to confirm one exact
// dormant or resident session. The no-leader client owns no product session.
func GrokNativeSessionTitle(
	ctx context.Context,
	bin, cwd string,
	environment []string,
	diagnostics io.Writer,
	sessionID string,
) (string, bool) {
	if ctx == nil || !validSessionID(sessionID) || strings.TrimSpace(cwd) == "" {
		return "", false
	}
	client, err := openGrokAuthenticatedClientCommand(
		ctx, bin, cwd, sessionID, environment, diagnostics,
		[]string{"--no-auto-update", "--yolo", "agent", "--no-leader", "stdio"},
	)
	if err != nil {
		return "", false
	}
	defer client.close()
	deadline, cancel := context.WithTimeout(ctx, grokACPInterjectTimeout)
	defer cancel()
	roster, err := client.request(deadline, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return "", false
	}
	result, _ := roster["result"].(map[string]any)
	rows, _ := result["sessions"].([]any)
	name, matches := grokRosterTitleFromRows(rows, sessionID)
	if matches != 1 {
		return "", false
	}
	if name == "" {
		name = sessionID
	}
	return name, true
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

// Rename writes one manual title through Grok's own session service. The
// caller still confirms the resulting product row through SessionName.
func (observer *GrokNativeObserver) Rename(ctx context.Context, title string) error {
	if observer == nil || observer.client == nil || strings.TrimSpace(title) == "" {
		return errors.New("grok native rename is unavailable")
	}
	deadline, cancel := context.WithTimeout(ctx, grokACPInterjectTimeout)
	defer cancel()
	result, err := observer.client.request(deadline, "_x.ai/session/rename", map[string]any{
		"sessionId": observer.sessionID,
		"title":     title,
	})
	if err != nil {
		return err
	}
	if success, _ := result["success"].(bool); !success {
		return errors.New("grok native rename was not accepted")
	}
	return nil
}

// SessionTitle asks Grok's own roster about one exact UUID. Dormant sessions
// remain valid discovery candidates; the product row, not daemon state,
// confirms their existence.
func (observer *GrokNativeObserver) SessionTitle(ctx context.Context, sessionID string) (string, bool) {
	if observer == nil || observer.client == nil {
		return "", false
	}
	deadline, cancel := context.WithTimeout(ctx, grokACPInterjectTimeout)
	defer cancel()
	roster, err := observer.client.request(deadline, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return "", false
	}
	result, _ := roster["result"].(map[string]any)
	rows, _ := result["sessions"].([]any)
	name, matches := grokRosterTitleFromRows(rows, sessionID)
	if matches != 1 {
		return "", false
	}
	if name == "" {
		name = sessionID
	}
	return name, true
}

// Close releases only the observer process; the TUI and private leader remain
// owned by their attachment lifecycle.
func (observer *GrokNativeObserver) Close() {
	if observer != nil && observer.client != nil {
		observer.client.close()
		observer.client = nil
	}
}
