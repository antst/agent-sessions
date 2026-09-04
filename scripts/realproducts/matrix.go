package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/pathidentity"
)

const (
	matrixPass = "PASS"
	matrixFail = "FAIL"
	matrixSkip = "SKIP"
)

const codexTUISubmitDelay = 150 * time.Millisecond

const matrixGrokPermissionRule = "MCPTool(agent_sessions__*)"

type matrixOptions struct {
	repositoryRoot string
	cwd            string
	evidenceDir    string
	agentSessions  string
	tmux           string
	presenceSocket string
	timeout        time.Duration
}

type matrixProduct struct {
	id                    string
	nativeExecutable      string
	peerExecutable        string
	displayArguments      []string
	nativeQuitCommand     string
	nativeQuitDocumentURL string
	nativeTrustPrompt     string
	nativeReadyMarker     string
	nativeBusyMarker      string
	nativeSubmitDelay     time.Duration
	nativeEnvelopeFormat  string
	nativeQuotaMarker     string
	nativeLaneProduct     string
}

type matrixRoster struct {
	Schema string `json:"schema"`
	Host   struct {
		ID string `json:"id"`
	} `json:"host"`
	Local []matrixRosterEntry `json:"local"`
}

type matrixRosterEntry struct {
	Kind            string   `json:"kind"`
	Scope           string   `json:"scope"`
	ID              string   `json:"id"`
	NativeSessionID string   `json:"native_session_id"`
	Name            string   `json:"name"`
	Product         string   `json:"product"`
	Live            bool     `json:"live"`
	Groups          []string `json:"groups"`
}

type matrixTUI struct {
	tmuxName  string
	pane      string
	product   string
	name      string
	sessionID string
	paneCWD   string

	submitDelay time.Duration
}

type matrixDelivery struct {
	Target     string `json:"target"`
	SessionID  string `json:"session_id"`
	DeliveryID string `json:"delivery_id"`
	Status     string `json:"status"`
}

type matrixSendResult struct {
	MessageID  string           `json:"message_id"`
	Deliveries []matrixDelivery `json:"deliveries"`
}

type matrixWireFrame struct {
	Direction string             `json:"direction"`
	Frame     livepresence.Frame `json:"frame"`
}

type matrixWireEvidence struct {
	Frames []matrixWireFrame `json:"frames"`
	Error  string            `json:"error,omitempty"`
}

type matrixConnector struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *json.Decoder
	stderr  bytes.Buffer
	frames  []matrixWireFrame
	serial  int
}

type matrixRunner struct {
	config     matrixOptions
	output     io.Writer
	runID      string
	active     map[string]*matrixTUI
	failures   int
	cellCount  int
	tmuxSerial int
}

func standingMatrixRequested(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if argument == "--standing-matrix" || strings.HasPrefix(argument, "--standing-matrix=") {
			return true
		}
	}
	return false
}

func runStandingMatrix(ctx context.Context, args []string, output io.Writer) error {
	config, err := parseMatrixOptions(args)
	if err != nil {
		return err
	}
	runID, err := matrixRunID()
	if err != nil {
		return err
	}
	runner := &matrixRunner{
		config: config, output: output, runID: runID,
		active: map[string]*matrixTUI{},
	}
	defer runner.bestEffortCleanup()

	if _, err := fmt.Fprintf(output, "EVIDENCE %s\n", config.evidenceDir); err != nil {
		return err
	}
	if err := runner.writeMetadata(); err != nil {
		return err
	}
	for _, product := range matrixProductInventory() {
		if ctx.Err() != nil {
			break
		}
		resolved, reason := resolveMatrixProduct(product)
		if reason != "" {
			runner.record(product.id, "product", matrixSkip, reason, map[string]any{
				"native_candidate": product.nativeExecutable,
				"peer_candidate":   product.peerExecutable,
			})
			continue
		}
		runner.runProduct(ctx, resolved)
	}
	if ctx.Err() == nil {
		runner.runInvalidCwdCell(ctx)
	}
	cleanupEvidence := map[string]any{}
	cleanupErr := runner.cleanupAll(context.Background(), cleanupEvidence)
	if cleanupErr != nil {
		runner.record("matrix", "cleanup", matrixFail, cleanupErr.Error(), cleanupEvidence)
	} else {
		runner.record("matrix", "cleanup", matrixPass, "all test-owned tmux sessions and live matrix rows are gone", cleanupEvidence)
	}
	if runner.failures != 0 {
		return fmt.Errorf("standing matrix had %d failed cell(s) out of %d; evidence: %s", runner.failures, runner.cellCount, config.evidenceDir)
	}
	return nil
}

//nolint:gocyclo // Matrix preflight reports each independently actionable missing prerequisite.
func parseMatrixOptions(args []string) (matrixOptions, error) {
	set := flag.NewFlagSet("test-real-products --standing-matrix", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	standing := set.Bool("standing-matrix", false, "run the standing live-product regression matrix")
	repositoryRoot := set.String("repository-root", ".", "repository root")
	approvedCWD := set.String("cwd", "", "required existing directory already approved in each installed product; prefer a dedicated empty directory")
	evidenceDir := set.String("evidence-dir", "", "matrix evidence directory (created when absent)")
	temporaryRoot := set.String("temporary-root", shortTemporaryRoot(), "existing absolute temporary parent")
	timeout := set.Duration("matrix-timeout", 75*time.Second, "per-operation live-product timeout")
	if err := set.Parse(args); err != nil {
		return matrixOptions{}, err
	}
	if !*standing || set.NArg() != 0 {
		return matrixOptions{}, errors.New("standing matrix received invalid arguments")
	}
	root, err := existingDirectory(*repositoryRoot)
	if err != nil {
		return matrixOptions{}, fmt.Errorf("repository root: %w", err)
	}
	if strings.TrimSpace(*approvedCWD) == "" {
		return matrixOptions{}, errors.New("--cwd is required for the standing matrix")
	}
	cwd, err := existingDirectory(*approvedCWD)
	if err != nil {
		return matrixOptions{}, fmt.Errorf("approved product cwd: %w", err)
	}
	temporary, err := existingDirectory(*temporaryRoot)
	if err != nil {
		return matrixOptions{}, fmt.Errorf("temporary root: %w", err)
	}
	if matrixPathsOverlap(cwd, temporary) {
		return matrixOptions{}, errors.New("approved product cwd and temporary root must be disjoint")
	}
	futureEvidence := strings.TrimSpace(*evidenceDir)
	if futureEvidence != "" {
		futureEvidence, err = filepath.Abs(futureEvidence)
		if err == nil {
			futureEvidence, err = pathidentity.FuturePath(futureEvidence)
		}
		if err != nil {
			return matrixOptions{}, fmt.Errorf("matrix evidence directory: %w", err)
		}
	}
	if futureEvidence != "" && matrixPathsOverlap(cwd, futureEvidence) {
		return matrixOptions{}, errors.New("approved product cwd and evidence directory must be disjoint")
	}
	if *timeout < 5*time.Second {
		return matrixOptions{}, errors.New("--matrix-timeout must be at least 5s")
	}
	if err := requireMatrixGitCWD(cwd); err != nil {
		return matrixOptions{}, err
	}
	lookup := func(name string) (string, error) {
		path, lookupErr := exec.LookPath(name)
		if lookupErr != nil {
			return "", fmt.Errorf("standing matrix requires %s: %w", name, lookupErr)
		}
		return filepath.Abs(path)
	}
	agentSessions, err := lookup("agent-sessions")
	if err != nil {
		return matrixOptions{}, err
	}
	tmux, err := lookup("tmux")
	if err != nil {
		return matrixOptions{}, err
	}
	socket, err := matrixPresenceSocket()
	if err != nil {
		return matrixOptions{}, err
	}
	if err := rejectExistingMatrixTMUX(tmux); err != nil {
		return matrixOptions{}, err
	}
	if err := requireMatrixGrokPermission(cwd); err != nil {
		return matrixOptions{}, err
	}
	evidence, err := prepareMatrixEvidenceDir(futureEvidence, temporary)
	if err != nil {
		return matrixOptions{}, err
	}
	return matrixOptions{
		repositoryRoot: root, cwd: cwd, evidenceDir: evidence,
		agentSessions: agentSessions, tmux: tmux,
		presenceSocket: socket, timeout: *timeout,
	}, nil
}

// requireMatrixGitCWD catches a checkout whose repository metadata resolves
// the approved directory to a different worktree. Non-Git directories remain
// valid and are the preferred isolated matrix workspace.
func requireMatrixGitCWD(cwd string) error {
	_, metadataErr := os.Lstat(filepath.Join(cwd, ".git"))
	if errors.Is(metadataErr, os.ErrNotExist) {
		return nil
	}
	if metadataErr != nil {
		return fmt.Errorf("inspect approved product cwd Git metadata: %w", metadataErr)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("approved product cwd contains .git but git is unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, git, "-C", cwd, "rev-parse", "--is-inside-work-tree", "--show-toplevel").CombinedOutput() //nolint:gosec // read-only Git metadata check for the caller-selected cwd.
	if err != nil {
		return fmt.Errorf("approved product cwd has invalid Git worktree metadata: %w: %s", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 {
		return fmt.Errorf("approved product cwd Git metadata returned an unexpected worktree projection: %q", strings.TrimSpace(string(output)))
	}
	topLevel, err := existingDirectory(lines[1])
	if err != nil {
		return fmt.Errorf("approved product cwd Git top-level: %w", err)
	}
	if lines[0] != "true" || topLevel != cwd {
		return fmt.Errorf("approved product cwd Git metadata resolves to another worktree: cwd=%s inside_work_tree=%s top_level=%s", cwd, lines[0], topLevel)
	}
	return nil
}

func requireMatrixGrokPermission(cwd string) error {
	if _, err := exec.LookPath("grok"); err != nil {
		return nil
	}
	if _, err := exec.LookPath("grok-peer"); err != nil {
		return nil
	}
	grokHome := os.Getenv("GROK_HOME")
	if grokHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve Grok configuration home: %w", err)
		}
		grokHome = filepath.Join(home, ".grok")
	}
	paths := []string{filepath.Join(grokHome, "config.toml"), filepath.Join(cwd, ".grok", "config.toml")}
	for _, path := range paths {
		body, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return fmt.Errorf("read Grok permission configuration %s: %w", path, readErr)
		}
		if matrixGrokConfigAllowsAgentSessions(body) {
			return nil
		}
	}
	return fmt.Errorf("Grok standing matrix requires this exact one-line permission in %s or trusted %s:\n[permission]\nallow = [\"%s\"]", paths[0], paths[1], matrixGrokPermissionRule)
}

func matrixGrokConfigAllowsAgentSessions(body []byte) bool {
	inPermission := false
	for _, rawLine := range strings.Split(string(body), "\n") {
		line, _, _ := strings.Cut(rawLine, "#")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inPermission = line == "[permission]"
			continue
		}
		if compact := strings.ReplaceAll(strings.ReplaceAll(line, " ", ""), "\t", ""); inPermission && compact == `allow=["`+matrixGrokPermissionRule+`"]` {
			return true
		}
	}
	return false
}

func rejectExistingMatrixTMUX(tmux string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, tmux, "list-sessions", "-F", "#{session_name}").CombinedOutput() //nolint:gosec // resolved tmux and fixed read-only argv.
	if err != nil {
		message := strings.TrimSpace(string(output))
		if !strings.Contains(message, "\n") && ((strings.HasPrefix(message, "no server running on /") && len(message) > len("no server running on /")) ||
			(strings.HasPrefix(message, "error connecting to /") && strings.HasSuffix(message, " (No such file or directory)"))) {
			return nil
		}
		return fmt.Errorf("list tmux sessions before standing matrix: %w: %s", err, message)
	}
	var commands []string
	for _, name := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if strings.HasPrefix(name, "matrix-") {
			commands = append(commands, "tmux kill-session -t '="+strings.ReplaceAll(name, "'", "'\"'\"'")+"'")
		}
	}
	if len(commands) != 0 {
		sort.Strings(commands)
		return errors.New("standing matrix found existing matrix-* tmux sessions; run these exact commands first:\n" + strings.Join(commands, "\n"))
	}
	return nil
}

func matrixPathsOverlap(left, right string) bool {
	return matrixPathWithin(left, right) || matrixPathWithin(right, left)
}

func matrixPathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func prepareMatrixEvidenceDir(raw, temporaryRoot string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		path, err := os.MkdirTemp(temporaryRoot, "agent-sessions-matrix-evidence.")
		if err != nil {
			return "", fmt.Errorf("create matrix evidence directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // Evidence directories require owner traversal.
			return "", fmt.Errorf("secure matrix evidence directory: %w", err)
		}
		return path, nil
	}
	if !filepath.IsAbs(raw) {
		absolute, err := filepath.Abs(raw)
		if err != nil {
			return "", fmt.Errorf("matrix evidence directory: %w", err)
		}
		raw = absolute
	}
	if err := os.MkdirAll(raw, 0o700); err != nil {
		return "", fmt.Errorf("create matrix evidence directory: %w", err)
	}
	return existingDirectory(raw)
}

func matrixPresenceSocket() (string, error) {
	path := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRESENCE_SOCKET"))
	if path == "" {
		if root := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_STATE_ROOT")); root != "" {
			path = filepath.Join(root, "run", "presence.sock")
		}
	}
	if path == "" {
		if state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); state != "" {
			path = filepath.Join(state, "agent-sessions", "run", "presence.sock")
		}
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve presence socket: %w", err)
		}
		path = filepath.Join(home, ".local", "state", "agent-sessions", "run", "presence.sock")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("agent Sessions presence socket is not absolute")
	}
	info, err := os.Lstat(path) //nolint:gosec // Closed protocol resolution order; the socket must already exist.
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return "", fmt.Errorf("agent Sessions presence socket is unavailable at %s", path)
	}
	return path, nil
}

func matrixProductInventory() []matrixProduct {
	return []matrixProduct{
		{
			id: "codex", nativeExecutable: "codex", peerExecutable: "codex-peer",
			displayArguments: []string{"--no-alt-screen"}, nativeQuitCommand: "/quit",
			nativeLaneProduct:     "codex",
			nativeSubmitDelay:     codexTUISubmitDelay,
			nativeQuitDocumentURL: "https://github.com/openai/codex/blob/main/codex-rs/tui/src/slash_command.rs",
			nativeTrustPrompt:     "Do you trust the contents of this directory?",
			nativeReadyMarker:     "› Ask Codex to do anything",
			nativeBusyMarker:      "Working (",
			nativeEnvelopeFormat:  `<cross-session-message from="%s"`,
		},
		{
			id: "claude", nativeExecutable: "claude", peerExecutable: "claude-peer",
			displayArguments: []string{"--ax-screen-reader"}, nativeQuitCommand: "/exit",
			nativeQuitDocumentURL: "https://code.claude.com/docs/en/commands",
			nativeTrustPrompt:     "Permission Required: Accessing workspace:",
			nativeReadyMarker:     "\n$\n",
			nativeEnvelopeFormat:  "Message from @%s:",
			nativeLaneProduct:     "codex",
		},
		{
			id: "grok", nativeExecutable: "grok", peerExecutable: "grok-peer",
			displayArguments: []string{"--no-alt-screen", "--minimal"}, nativeQuitCommand: "/quit",
			nativeLaneProduct:     "codex",
			nativeQuitDocumentURL: "https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/04-slash-commands.md",
			nativeReadyMarker:     "\n❯\nGrok ",
			nativeBusyMarker:      "Thinking…",
			nativeEnvelopeFormat:  `<cross-session-message from="%s"`,
		},
		{
			id: "qwen", nativeExecutable: "qwen", peerExecutable: "qwen-peer",
			displayArguments: []string{"--screen-reader"}, nativeQuitCommand: "/quit",
			nativeQuitDocumentURL: "https://github.com/QwenLM/qwen-code/blob/main/docs/users/features/commands.md",
			nativeReadyMarker:     "Auto mode   Type your message or @path/to/file",
			nativeBusyMarker:      "esc to cancel",
			nativeEnvelopeFormat:  `<cross-session-message from="%s"`,
			nativeQuotaMarker:     "insufficient_quota: 429",
		},
	}
}

func resolveMatrixProduct(product matrixProduct) (matrixProduct, string) {
	native, nativeErr := exec.LookPath(product.nativeExecutable)
	peer, peerErr := exec.LookPath(product.peerExecutable)
	var missing []string
	if nativeErr != nil {
		missing = append(missing, product.nativeExecutable+" native executable is absent")
	}
	if peerErr != nil {
		missing = append(missing, product.peerExecutable+" launcher is absent")
	}
	if len(missing) != 0 {
		return product, strings.Join(missing, "; ")
	}
	product.nativeExecutable, _ = filepath.Abs(native)
	// Preserve the peer alias path: all installed aliases may point at the same
	// multicall binary, whose argv[0] selects the product launcher.
	product.peerExecutable, _ = filepath.Abs(peer)
	return product, ""
}

func matrixRunID() (string, error) {
	body := make([]byte, 6)
	if _, err := rand.Read(body); err != nil {
		return "", fmt.Errorf("mint matrix run id: %w", err)
	}
	return fmt.Sprintf("%d-%s", time.Now().UTC().Unix(), hex.EncodeToString(body)), nil
}

func (runner *matrixRunner) writeMetadata() error {
	return runner.writeJSON("metadata.json", map[string]any{
		"run_id":          runner.runID,
		"repository_root": runner.config.repositoryRoot,
		"product_cwd":     runner.config.cwd,
		"presence_socket": runner.config.presenceSocket,
		"started_at":      time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (runner *matrixRunner) record(product, cell, status, detail string, evidence map[string]any) {
	runner.cellCount++
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidence["product"] = product
	evidence["cell"] = cell
	evidence["status"] = status
	evidence["detail"] = detail
	evidence["recorded_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	relative := filepath.Join(product, cell+".json")
	path := filepath.Join(runner.config.evidenceDir, relative)
	if err := runner.writeJSON(relative, evidence); err != nil {
		status, detail = matrixFail, detail+"; write evidence: "+err.Error()
	}
	if status == matrixFail {
		runner.failures++
	}
	if _, err := fmt.Fprintf(runner.output, "%s %s/%s evidence=%s detail=%q\n", status, product, cell, path, detail); err != nil {
		runner.failures++
	}
}

func (runner *matrixRunner) writeJSON(relative string, value any) error {
	path := filepath.Join(runner.config.evidenceDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

func (runner *matrixRunner) writeText(relative, body string) (string, error) {
	path := filepath.Join(runner.config.evidenceDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, err
	}
	if body == "" {
		body = "<empty capture>\n"
	}
	return path, os.WriteFile(path, []byte(body), 0o600)
}

func (runner *matrixRunner) runProduct(ctx context.Context, product matrixProduct) {
	prefix := "matrix-" + runner.runID + "-" + product.id
	identityName := prefix + "-identity"
	identityGroup := prefix + "-identity"
	identity, launchEvidence, launchErr := runner.launchNamed(ctx, product, identityName, identityGroup, "identity")
	if launchErr != nil {
		if identity != nil {
			launchErr = errors.Join(launchErr, runner.endTUI(ctx, identity))
		}
		runner.record(product.id, "01-named-start", matrixFail, launchErr.Error(), launchEvidence)
	} else {
		runner.record(product.id, "01-named-start", matrixPass, "exact named row and explicit group are live", launchEvidence)
	}

	runner.runNoGroupCell(ctx, product)
	runner.runDirectCell(ctx, product, prefix)
	runner.runMulticastCell(ctx, product, prefix)
	runner.runGroupCell(ctx, product, prefix)
	if product.nativeLaneProduct != "" && launchErr == nil {
		runner.runLaneCell(ctx, product, identity, prefix, identityGroup)
	}

	if launchErr != nil {
		for _, cell := range []string{"06-resume-by-name", "07-terminal-quit", "07b-native-quit-resume"} {
			runner.record(product.id, cell, matrixSkip, "named identity session was unavailable", nil)
		}
		return
	}
	resumed, blockedReason := runner.runResumeCell(ctx, product, identity, identityName, identityGroup)
	if resumed == nil {
		if blockedReason == "" {
			blockedReason = "resume cell did not produce a live TUI"
		}
		runner.record(product.id, "07-terminal-quit", matrixSkip, blockedReason, nil)
		runner.record(product.id, "07b-native-quit-resume", matrixSkip, blockedReason, nil)
		return
	}
	if err := runner.endTUI(ctx, resumed); err != nil {
		runner.record(product.id, "07-terminal-quit", matrixFail, err.Error(), map[string]any{"native_session_id": resumed.sessionID})
	} else {
		runner.record(product.id, "07-terminal-quit", matrixPass, "test-owned terminal exit removed the exact row", map[string]any{"native_session_id": resumed.sessionID})
	}
	runner.runNativeQuitResumeCell(ctx, product, identityName, identityGroup, identity.sessionID)
}

func (runner *matrixRunner) runNoGroupCell(ctx context.Context, product matrixProduct) {
	before, rawBefore, err := runner.roster(ctx)
	evidence := map[string]any{"roster_before": json.RawMessage(rawBefore)}
	if err != nil {
		runner.record(product.id, "02-private-anchor-only", matrixFail, err.Error(), evidence)
		return
	}
	baseline := rosterIDSet(before.Local)
	tui, command, err := runner.startTUI(ctx, product, "", "", "no-group", false)
	evidence["command"] = command
	recordTUILaunchEvidence(evidence, tui)
	if err != nil {
		err = runner.diagnoseAttachFailure(product, "no-group", tui, evidence, err)
		runner.record(product.id, "02-private-anchor-only", matrixFail, err.Error(), evidence)
		return
	}
	row, after, rawAfter, err := runner.waitForNewProductRow(ctx, product.id, baseline)
	evidence["roster_after"] = json.RawMessage(rawAfter)
	if err == nil {
		tui.sessionID, tui.name = row.NativeSessionID, row.Name
		evidence["native_session_id"] = row.NativeSessionID
		err = requireOnlyPrivateAnchor(after, row)
	} else {
		err = runner.diagnoseAttachFailure(product, "no-group", tui, evidence, err)
	}
	cleanupErr := runner.endTUI(ctx, tui)
	err = errors.Join(err, cleanupErr)
	if err != nil {
		runner.record(product.id, "02-private-anchor-only", matrixFail, err.Error(), evidence)
		return
	}
	runner.record(product.id, "02-private-anchor-only", matrixPass, "fresh launch without -g has only its private anchor group", evidence)
}

func (runner *matrixRunner) runDirectCell(ctx context.Context, product matrixProduct, prefix string) {
	name, group := prefix+"-direct", prefix+"-direct"
	tui, evidence, err := runner.launchNamed(ctx, product, name, group, "direct")
	if err == nil {
		token := matrixReceiptToken(runner.runID, "03")
		source := matrixEnvelopeSource(token, product.id)
		response, wirePath, sendErr := runner.sendV1(ctx, product.id, "direct", group, source, map[string]any{
			"target": name, "message": matrixDeliveryMessage(token),
		})
		evidence["wire_evidence"] = wirePath
		evidence["response"] = response
		if sendErr == nil {
			sendErr = requireAcceptedSessions(response, []string{tui.sessionID})
		}
		capturePath, receiptErr := runner.captureReceipt(ctx, product, "03-direct-send", tui, matrixEnvelopeMarker(product, source), "target", "", "")
		evidence["pane_evidence"] = capturePath
		err = errors.Join(sendErr, receiptErr)
	}
	if tui != nil {
		err = errors.Join(err, runner.endTUI(ctx, tui))
	}
	if err != nil {
		runner.record(product.id, "03-direct-send", matrixFail, err.Error(), evidence)
		return
	}
	runner.record(product.id, "03-direct-send", matrixPass, "raw v1 direct send by name was accepted and rendered by the product TUI", evidence)
}

func (runner *matrixRunner) runMulticastCell(ctx context.Context, product matrixProduct, prefix string) {
	group := prefix + "-multicast"
	first, firstEvidence, firstErr := runner.launchNamed(ctx, product, prefix+"-multi-a", group, "multi-a")
	second, secondEvidence, secondErr := runner.launchNamed(ctx, product, prefix+"-multi-b", group, "multi-b")
	evidence := map[string]any{"first_launch": firstEvidence, "second_launch": secondEvidence}
	err := errors.Join(firstErr, secondErr)
	if err == nil {
		token := matrixReceiptToken(runner.runID, "04")
		source := matrixEnvelopeSource(token, product.id)
		response, wirePath, sendErr := runner.sendV1(ctx, product.id, "multicast", group, source, map[string]any{
			"targets": []string{first.name, second.name}, "message": matrixDeliveryMessage(token),
		})
		evidence["wire_evidence"] = wirePath
		evidence["response"] = response
		if sendErr == nil {
			sendErr = requireAcceptedSessions(response, []string{first.sessionID, second.sessionID})
		}
		envelope := matrixEnvelopeMarker(product, source)
		firstCapture, firstReceipt := runner.captureReceipt(ctx, product, "04-targets-multicast", first, envelope, "target-a", "", "")
		secondCapture, secondReceipt := runner.captureReceipt(ctx, product, "04-targets-multicast", second, envelope, "target-b", "", "")
		evidence["pane_evidence"] = []string{firstCapture, secondCapture}
		err = errors.Join(sendErr, firstReceipt, secondReceipt)
	}
	if first != nil {
		err = errors.Join(err, runner.endTUI(ctx, first))
	}
	if second != nil {
		err = errors.Join(err, runner.endTUI(ctx, second))
	}
	if err != nil {
		runner.record(product.id, "04-targets-multicast", matrixFail, err.Error(), evidence)
		return
	}
	runner.record(product.id, "04-targets-multicast", matrixPass, "explicit targets multicast reached exactly two product TUIs", evidence)
}

func (runner *matrixRunner) runGroupCell(ctx context.Context, product matrixProduct, prefix string) {
	group := prefix + "-group"
	first, firstEvidence, firstErr := runner.launchNamed(ctx, product, prefix+"-group-a", group, "group-a")
	second, secondEvidence, secondErr := runner.launchNamed(ctx, product, prefix+"-group-b", group, "group-b")
	evidence := map[string]any{"first_launch": firstEvidence, "second_launch": secondEvidence}
	err := errors.Join(firstErr, secondErr)
	if err == nil {
		token := matrixReceiptToken(runner.runID, "05")
		source := matrixEnvelopeSource(token, product.id)
		response, wirePath, sendErr := runner.sendV1(ctx, product.id, "group", group, source, map[string]any{
			"group": group, "message": matrixDeliveryMessage(token),
		})
		evidence["wire_evidence"] = wirePath
		evidence["response"] = response
		if sendErr != nil {
			err = sendErr
		} else if response.Error != nil {
			err = fmt.Errorf("group message.send failed: %s (%d)", response.Error.Message, response.Error.Code)
		} else if acceptErr := requireAcceptedSessions(response, []string{first.sessionID, second.sessionID}); acceptErr != nil {
			err = acceptErr
		} else {
			envelope := matrixEnvelopeMarker(product, source)
			firstCapture, firstReceipt := runner.captureReceipt(ctx, product, "05-group-send", first, envelope, "target-a", "", "")
			secondCapture, secondReceipt := runner.captureReceipt(ctx, product, "05-group-send", second, envelope, "target-b", "", "")
			evidence["pane_evidence"] = []string{firstCapture, secondCapture}
			err = errors.Join(firstReceipt, secondReceipt)
		}
	}
	if first != nil {
		err = errors.Join(err, runner.endTUI(ctx, first))
	}
	if second != nil {
		err = errors.Join(err, runner.endTUI(ctx, second))
	}
	if err != nil {
		runner.record(product.id, "05-group-send", matrixFail, err.Error(), evidence)
		return
	}
	runner.record(product.id, "05-group-send", matrixPass, "group selector reached exactly the two group member TUIs", evidence)
}

func (runner *matrixRunner) runLaneCell(ctx context.Context, product matrixProduct, parent *matrixTUI, prefix, group string) {
	evidence := map[string]any{"parent_native_session_id": parent.sessionID}
	laneName := prefix + "-lane"
	prompt, expected := matrixLanePrompt(runner.runID, laneName, group)
	err := runner.sendTUIInput(ctx, parent, prompt)
	if err == nil {
		var pane string
		pane, err = runner.captureReceipt(ctx, product, "08-mcp-lane", parent, expected, "lifecycle", product.nativeReadyMarker, product.nativeBusyMarker)
		evidence["pane_evidence"] = pane
	}
	cleanupCtx, stopCleanup := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer stopCleanup()
	connector, connectorErr := runner.openMatrixConnector(cleanupCtx)
	if connectorErr != nil {
		err = errors.Join(err, connectorErr)
		runner.record(product.id, "08-mcp-lane", matrixFail, err.Error(), evidence)
		return
	}
	rows, listErr := connector.listLanes(parent.sessionID, true)
	evidence["lane_list_after_model"] = rows
	if listErr == nil {
		row, matchErr := namedLane(rows, laneName)
		if matchErr != nil || row == nil || row["product"] != product.nativeLaneProduct || row["owner_session_id"] != parent.sessionID || row["state"] != "archived" || row["outcome"] != "completed" {
			listErr = fmt.Errorf("completed archived lane %s was not proven: %+v", laneName, row)
		}
	}
	err = errors.Join(err, listErr, connector.recoverLane(parent.sessionID, laneName))
	finalRows, finalErr := connector.listLanes(parent.sessionID, false)
	evidence["lane_list_after_cleanup"] = finalRows
	if row, matchErr := namedLane(finalRows, laneName); finalErr == nil && (matchErr != nil || row != nil) {
		finalErr = errors.Join(matchErr, fmt.Errorf("lane %s survived cleanup: %+v", laneName, row))
	}
	err = errors.Join(err, finalErr, connector.Close())
	evidence["connector_frames"], evidence["connector_stderr"] = connector.frames, connector.stderr.String()
	if err != nil {
		runner.record(product.id, "08-mcp-lane", matrixFail, err.Error(), evidence)
		return
	}
	runner.record(product.id, "08-mcp-lane", matrixPass, "real product MCP tool completed and archived one Codex lane", evidence)
}
func (runner *matrixRunner) runInvalidCwdCell(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	connector, err := runner.openMatrixConnector(cleanupCtx)
	evidence := map[string]any{}
	if err == nil {
		evidence["invalid_cwd"], err = runner.runInvalidCwdConnectorCase(cleanupCtx, connector, "matrix-"+runner.runID+"-invalid-cwd", "matrix-"+runner.runID+"-invalid-cwd")
		err = errors.Join(err, connector.Close())
		evidence["connector_frames"], evidence["connector_stderr"] = connector.frames, connector.stderr.String()
	}
	if err != nil {
		runner.record("matrix", "08-invalid-cwd", matrixFail, err.Error(), evidence)
	} else {
		runner.record("matrix", "08-invalid-cwd", matrixPass, "connector preserved exact invalid-cwd RPC structure", evidence)
	}
}
func matrixLanePrompt(runID, laneName, group string) (string, string) {
	expected := matrixReceiptToken(runID, "08")
	left, right := expected[:len(expected)/2], expected[len(expected)/2:]
	return fmt.Sprintf("Use only the agent_sessions lane tool, never a shell. Start a codex lane with arguments [\"--name\",%q,\"--group\",%q] and input %q. Wait for that lane, archive it, then output only the exact result returned by wait.",
		laneName, group, "Without tools, concatenate "+left+" and "+right+". Reply with exactly the result."), expected
}
func (runner *matrixRunner) openMatrixConnector(ctx context.Context) (*matrixConnector, error) {
	command := exec.CommandContext(ctx, runner.config.agentSessions, "connector", "codex") //nolint:gosec // exact preflighted binary.
	command.Dir, command.Env = runner.config.cwd, os.Environ()
	connector := &matrixConnector{command: command}
	var err error
	if connector.input, err = command.StdinPipe(); err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = connector.input.Close()
		return nil, err
	}
	connector.output = json.NewDecoder(output)
	command.Stderr = &connector.stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	return connector, nil
}
func (connector *matrixConnector) Close() error {
	return errors.Join(connector.input.Close(), connector.command.Wait())
}
func (connector *matrixConnector) call(source string, arguments map[string]any) (json.RawMessage, *livepresence.RPCError, error) {
	connector.serial++
	id := json.RawMessage(fmt.Sprintf(`"matrix-lane-%d"`, connector.serial))
	params, _ := json.Marshal(map[string]any{"name": "lane", "arguments": arguments, "_meta": map[string]any{"threadId": source}})
	request := livepresence.Frame{JSONRPC: "2.0", ID: id, Method: "tools/call", Params: params}
	connector.frames = append(connector.frames, matrixWireFrame{Direction: "send", Frame: request})
	if err := json.NewEncoder(connector.input).Encode(request); err != nil {
		return nil, nil, err
	}
	var response livepresence.Frame
	if err := connector.output.Decode(&response); err != nil {
		return nil, nil, err
	}
	connector.frames = append(connector.frames, matrixWireFrame{Direction: "receive", Frame: response})
	if !livepresence.ValidFrame(response) || response.Method != "" || !bytes.Equal(response.ID, id) {
		return nil, nil, fmt.Errorf("invalid connector response for id %s: %+v", id, response)
	}
	return response.Result, response.Error, nil
}
func (connector *matrixConnector) lane(source, command string, arguments []string, input string) (map[string]any, error) {
	params := map[string]any{"product": "codex", "command": command, "arguments": arguments}
	if input != "" {
		params["input"] = input
	}
	result, rpcErr, err := connector.call(source, params)
	if err != nil || rpcErr != nil {
		return nil, errors.Join(err, rpcErr)
	}
	var envelope struct {
		Structured map[string]any `json:"structuredContent"`
	}
	if json.Unmarshal(result, &envelope) != nil || envelope.Structured == nil {
		return nil, errors.New("connector returned an invalid structured lane result")
	}
	return envelope.Structured, nil
}
func (connector *matrixConnector) listLanes(source string, all bool) ([]map[string]any, error) {
	arguments := []string{"--mine"}
	if all {
		arguments = append(arguments, "--all")
	}
	result, err := connector.lane(source, "list", arguments, "")
	body, _ := json.Marshal(result["lanes"])
	var rows []map[string]any
	if err != nil || json.Unmarshal(body, &rows) != nil || rows == nil {
		return nil, errors.Join(err, errors.New("lane.list omitted lanes"))
	}
	return rows, nil
}
func namedLane(rows []map[string]any, name string) (map[string]any, error) {
	var match map[string]any
	for _, row := range rows {
		if row["name"] == name {
			if match != nil {
				return nil, fmt.Errorf("lane name %s is ambiguous", name)
			}
			match = row
		}
	}
	return match, nil
}
func (connector *matrixConnector) recoverLane(source, name string) error {
	rows, err := connector.listLanes(source, true)
	row, matchErr := namedLane(rows, name)
	err = errors.Join(err, matchErr)
	if err != nil || row == nil {
		return err
	}
	wait := []string{"wait", name, "--timeout", "30"}
	commands, known := map[string][][]string{
		"running":  {{"interrupt", name}, wait, {"archive", name}},
		"retiring": {wait, {"archive", name}}, "preparing": {wait, {"archive", name}}, "interrupting": {wait, {"archive", name}},
		"idle": {{"archive", name}}, "terminal": {{"archive", name}}, "archived": {},
	}[fmt.Sprint(row["state"])]
	if !known {
		return fmt.Errorf("lane %s has unrecoverable state %s", name, row["state"])
	}
	for _, command := range commands {
		if _, err := connector.lane(source, command[0], command[1:], ""); err != nil {
			return err
		}
	}
	return nil
}
func (runner *matrixRunner) runInvalidCwdConnectorCase(ctx context.Context, connector *matrixConnector, group, name string) (map[string]any, error) {
	evidence := map[string]any{}
	uuid, err := matrixUUID()
	if err != nil {
		return evidence, err
	}
	missing := filepath.Join(runner.config.evidenceDir, "missing-"+uuid)
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		return evidence, errors.New("invalid-cwd fixture unexpectedly exists")
	}
	parentCtx, cancel := context.WithCancel(ctx)
	wire := matrixWireEvidence{}
	report := livepresence.Report{UUID: uuid, Name: name, Groups: []string{group}, Product: "claude", Info: map[string]string{"cwd": missing}}
	live := livepresence.StartClient(parentCtx, runner.config.presenceSocket, report, nil, func(direction string, frame livepresence.Frame) {
		wire.Frames = append(wire.Frames, matrixWireFrame{Direction: direction, Frame: frame})
	})
	defer func() { cancel(); <-live.Done(); evidence["wire"] = wire }()
	select {
	case <-live.Ready():
	case <-live.Done():
		return evidence, errors.New("invalid-cwd parent stopped before hello")
	case <-ctx.Done():
		return evidence, ctx.Err()
	}
	_, rpcErr, err := connector.call(uuid, map[string]any{
		"product": "codex", "command": "start", "arguments": []string{"--name", name}, "input": "must not run",
	})
	evidence["rpc_error"] = rpcErr
	if err != nil {
		return evidence, err
	}
	if rpcErr == nil || rpcErr.Code != livepresence.InvalidParams || rpcErr.Message != "Invalid params" || string(rpcErr.Data) != `{"method":"lane.start"}` {
		return evidence, fmt.Errorf("invalid cwd error = %+v", rpcErr)
	}
	rows, err := connector.listLanes(uuid, true)
	evidence["lane_list"] = rows
	row, matchErr := namedLane(rows, name)
	err = errors.Join(err, matchErr)
	if err == nil && row != nil {
		err = errors.Join(fmt.Errorf("invalid cwd created lane %s", name), connector.recoverLane(uuid, name))
	}
	return evidence, err
}

func (runner *matrixRunner) runResumeCell(
	ctx context.Context,
	product matrixProduct,
	identity *matrixTUI,
	name, group string,
) (*matrixTUI, string) {
	evidence := map[string]any{"original_native_session_id": identity.sessionID}
	persistencePath, persistenceErr := runner.persistNativeTurn(ctx, product, identity)
	evidence["persistence_pane"] = persistencePath
	if persistenceErr != nil {
		blockedReason := ""
		if strings.HasPrefix(persistenceErr.Error(), "OWNER-ENVIRONMENT quota:") {
			evidence["classification"] = "OWNER-ENVIRONMENT"
			blockedReason = persistenceErr.Error()
		}
		persistenceErr = errors.Join(persistenceErr, runner.endTUI(ctx, identity))
		runner.record(product.id, "06-resume-by-name", matrixFail, persistenceErr.Error(), evidence)
		return nil, blockedReason
	}
	if err := runner.endTUI(ctx, identity); err != nil {
		runner.record(product.id, "06-resume-by-name", matrixFail, err.Error(), evidence)
		return nil, ""
	}
	resumed, launchEvidence, err := runner.launchResume(ctx, product, name, group, "resume-1", identity.sessionID)
	evidence["resume_launch"] = launchEvidence
	if err != nil {
		if resumed != nil {
			err = errors.Join(err, runner.endTUI(ctx, resumed))
		}
		runner.record(product.id, "06-resume-by-name", matrixFail, err.Error(), evidence)
		return nil, ""
	}
	runner.record(product.id, "06-resume-by-name", matrixPass, "resume by exact unique name retained the product-native id without a fork", evidence)
	return resumed, ""
}

func (runner *matrixRunner) persistNativeTurn(ctx context.Context, product matrixProduct, tui *matrixTUI) (string, error) {
	if _, err := runner.captureReceipt(ctx, product, "06-resume-by-name", tui, product.nativeReadyMarker, "ready-before-persistence", "", product.nativeBusyMarker); err != nil {
		return "", err
	}
	prompt, expected := matrixPersistencePrompt(runner.runID)
	if err := runner.sendTUIInput(ctx, tui, prompt); err != nil {
		return "", err
	}
	return runner.captureReceipt(ctx, product, "06-resume-by-name", tui, expected, "persistence", product.nativeReadyMarker, product.nativeBusyMarker)
}

func (runner *matrixRunner) runNativeQuitResumeCell(
	ctx context.Context,
	product matrixProduct,
	name, group, expectedID string,
) {
	evidence := map[string]any{
		"native_session_id": expectedID,
		"quit_command":      product.nativeQuitCommand,
		"documentation":     product.nativeQuitDocumentURL,
	}
	if product.nativeQuitCommand == "" || product.nativeQuitDocumentURL == "" {
		runner.record(product.id, "07b-native-quit-resume", matrixSkip, "no first-party documented native quit command", evidence)
		return
	}
	beforeQuit, beforeEvidence, err := runner.launchResume(ctx, product, name, group, "native-quit-before", expectedID)
	evidence["before_quit_resume"] = beforeEvidence
	if err == nil {
		err = runner.sendTUIInput(ctx, beforeQuit, product.nativeQuitCommand)
	}
	if err == nil {
		err = runner.waitForRowGone(ctx, expectedID)
	}
	if err == nil && runner.tmuxExists(beforeQuit.tmuxName) {
		err = errors.New("documented native quit left its test-owned tmux session alive")
	}
	if err != nil {
		if beforeQuit != nil {
			err = errors.Join(err, runner.endTUI(ctx, beforeQuit))
		}
		runner.record(product.id, "07b-native-quit-resume", matrixFail, err.Error(), evidence)
		return
	}
	delete(runner.active, beforeQuit.tmuxName)
	var resumed *matrixTUI
	var afterEvidence map[string]any
	resumed, afterEvidence, err = runner.launchResume(ctx, product, name, group, "native-quit-after", expectedID)
	evidence["after_quit_resume"] = afterEvidence
	if err != nil {
		if resumed != nil {
			err = errors.Join(err, runner.endTUI(ctx, resumed))
		}
		runner.record(product.id, "07b-native-quit-resume", matrixFail, err.Error(), evidence)
		return
	}
	evidence["resumed_native_session_id"] = resumed.sessionID
	runner.record(product.id, "07b-native-quit-resume", matrixPass, "documented native quit removed the row and name resume retained the same native id", evidence)
}

func (runner *matrixRunner) launchNamed(
	ctx context.Context,
	product matrixProduct,
	name, group, label string,
) (*matrixTUI, map[string]any, error) {
	evidence := map[string]any{}
	before, rawBefore, err := runner.roster(ctx)
	evidence["roster_before"] = json.RawMessage(rawBefore)
	if err != nil {
		return nil, evidence, err
	}
	if rows := namedRows(before, product.id, name); len(rows) != 0 {
		return nil, evidence, fmt.Errorf("matrix name %s was already live", name)
	}
	tui, command, err := runner.startTUI(ctx, product, name, group, label, false)
	evidence["command"] = command
	recordTUILaunchEvidence(evidence, tui)
	if err != nil {
		err = runner.diagnoseAttachFailure(product, label, tui, evidence, err)
		return tui, evidence, err
	}
	row, roster, rawAfter, err := runner.waitForNamedRow(ctx, product.id, name)
	evidence["roster_after"] = json.RawMessage(rawAfter)
	if err != nil {
		err = runner.diagnoseAttachFailure(product, label, tui, evidence, err)
		return tui, evidence, err
	}
	tui.name, tui.sessionID = row.Name, row.NativeSessionID
	evidence["native_session_id"] = row.NativeSessionID
	if err := requireNamedGroups(roster, row, group); err != nil {
		return tui, evidence, err
	}
	return tui, evidence, nil
}

func (runner *matrixRunner) launchResume(
	ctx context.Context,
	product matrixProduct,
	name, group, label, expectedID string,
) (*matrixTUI, map[string]any, error) {
	evidence := map[string]any{"expected_native_session_id": expectedID}
	before, rawBefore, err := runner.roster(ctx)
	evidence["roster_before"] = json.RawMessage(rawBefore)
	if err != nil {
		return nil, evidence, err
	}
	if rows := namedRows(before, product.id, name); len(rows) != 0 {
		return nil, evidence, fmt.Errorf("resume target %s was still live", name)
	}
	tui, command, err := runner.startTUI(ctx, product, name, group, label, true)
	evidence["command"] = command
	recordTUILaunchEvidence(evidence, tui)
	if err != nil {
		err = runner.diagnoseAttachFailure(product, label, tui, evidence, err)
		return tui, evidence, err
	}
	row, roster, rawAfter, err := runner.waitForNamedRow(ctx, product.id, name)
	evidence["roster_after"] = json.RawMessage(rawAfter)
	if err != nil {
		err = runner.diagnoseAttachFailure(product, label, tui, evidence, err)
		return tui, evidence, err
	}
	tui.name, tui.sessionID = row.Name, row.NativeSessionID
	if row.NativeSessionID != expectedID {
		return tui, evidence, fmt.Errorf("resume forked native id %s, want %s", row.NativeSessionID, expectedID)
	}
	if len(namedRows(roster, product.id, name)) != 1 {
		return tui, evidence, errors.New("resume created more than one exact-name row")
	}
	if err := requireNamedGroups(roster, row, group); err != nil {
		return tui, evidence, err
	}
	return tui, evidence, nil
}

func (runner *matrixRunner) startTUI(
	ctx context.Context,
	product matrixProduct,
	name, group, label string,
	resume bool,
) (*matrixTUI, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	runner.tmuxSerial++
	tmuxName := fmt.Sprintf("matrix-%s-%s-%02d", runner.runID, product.id, runner.tmuxSerial)
	if runner.tmuxExists(tmuxName) {
		return nil, nil, fmt.Errorf("test-owned tmux name already exists: %s", tmuxName)
	}
	tui := &matrixTUI{tmuxName: tmuxName, pane: tmuxName + ":0.0", product: product.id, name: name, submitDelay: product.nativeSubmitDelay}
	runner.active[tmuxName] = tui
	peerArgs := matrixPeerArguments(product, name, group, resume)
	command := make([]string, 0, 8+len(peerArgs))
	command = append(command, runner.config.tmux, "new-session", "-d", "-s", tmuxName, "-c", runner.config.cwd, product.peerExecutable)
	command = append(command, peerArgs...)
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command[0], command[1:]...) //nolint:gosec // closed tmux argv and resolved product alias.
	var diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &diagnostics, &diagnostics
	if err := cmd.Run(); err != nil {
		return tui, command, fmt.Errorf("start detached %s TUI: %w: %s", product.id, err, strings.TrimSpace(diagnostics.String()))
	}
	paneCWD, err := runner.paneWorkingDirectory(ctx, tui)
	if err != nil {
		cause := fmt.Errorf("observe detached %s TUI cwd: %w", product.id, err)
		return tui, command, errors.Join(cause, runner.endTUI(context.Background(), tui))
	}
	tui.paneCWD = paneCWD
	if paneCWD != runner.config.cwd {
		cause := fmt.Errorf("detached %s TUI started in cwd %s, want approved cwd %s", product.id, paneCWD, runner.config.cwd)
		return tui, command, errors.Join(cause, runner.endTUI(context.Background(), tui))
	}
	return tui, command, nil
}

func recordTUILaunchEvidence(evidence map[string]any, tui *matrixTUI) {
	if tui != nil && tui.paneCWD != "" {
		evidence["pane_cwd"] = tui.paneCWD
	}
}

func (runner *matrixRunner) paneWorkingDirectory(ctx context.Context, tui *matrixTUI) (string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(queryCtx, runner.config.tmux, "display-message", "-p", "-t", tui.pane, "#{pane_current_path}").CombinedOutput() //nolint:gosec // exact generated pane and fixed read-only format.
	if err != nil {
		return "", fmt.Errorf("query exact pane %s: %w: %s", tui.pane, err, strings.TrimSpace(string(output)))
	}
	path, err := existingDirectory(strings.TrimSuffix(string(output), "\n"))
	if err != nil {
		return "", fmt.Errorf("validate exact pane %s cwd: %w", tui.pane, err)
	}
	return path, nil
}

func matrixPeerArguments(product matrixProduct, name, group string, resume bool) []string {
	arguments := make([]string, 0, 10)
	if resume {
		arguments = append(arguments, "--resume", name)
	} else if name != "" {
		arguments = append(arguments, "-n", name)
	}
	if group != "" {
		arguments = append(arguments, "-g", group)
	}
	arguments = append(arguments, "--no-inherit-groups")
	return append(arguments, product.displayArguments...)
}

func (runner *matrixRunner) endTUI(ctx context.Context, tui *matrixTUI) error {
	if tui == nil {
		return nil
	}
	if runner.tmuxExists(tui.tmuxName) {
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(commandCtx, runner.config.tmux, "kill-session", "-t", "="+tui.tmuxName) //nolint:gosec // exact test-owned tmux name.
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("terminate test-owned tmux %s: %w: %s", tui.tmuxName, err, strings.TrimSpace(string(output)))
		}
	}
	delete(runner.active, tui.tmuxName)
	if tui.sessionID == "" {
		return nil
	}
	return runner.waitForRowGone(ctx, tui.sessionID)
}

func (runner *matrixRunner) sendTUIInput(ctx context.Context, tui *matrixTUI, input string) error {
	if tui == nil || !runner.tmuxExists(tui.tmuxName) {
		return errors.New("native input target TUI is unavailable")
	}
	for index, arguments := range [][]string{
		{"send-keys", "-t", tui.pane, "-l", "--", input},
		{"send-keys", "-t", tui.pane, "Enter"},
	} {
		if index == 1 && tui.submitDelay > 0 {
			// https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/tui/src/bottom_pane/paste_burst.rs#L337-L353 suppresses Enter for 120ms.
			// The 150ms gap leaves a 30ms pty-read margin; a stalled TUI still fails loudly.
			timer := time.NewTimer(tui.submitDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		output, err := exec.CommandContext(commandCtx, runner.config.tmux, arguments...).CombinedOutput() //nolint:gosec // exact test-owned pane and documented literal.
		if commandErr := commandCtx.Err(); commandErr != nil {
			err = commandErr
		}
		cancel()
		if err != nil {
			return fmt.Errorf("send native TUI input %q: %w: %s", input, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (runner *matrixRunner) tmuxExists(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, runner.config.tmux, "has-session", "-t", "="+name).Run() == nil //nolint:gosec // exact generated name.
}

func (runner *matrixRunner) bestEffortCleanup() {
	for name, tui := range runner.active {
		if runner.tmuxExists(name) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = exec.CommandContext(ctx, runner.config.tmux, "kill-session", "-t", "="+name).Run() //nolint:gosec // exact generated name.
			cancel()
		}
		delete(runner.active, tui.tmuxName)
	}
}

func (runner *matrixRunner) cleanupAll(ctx context.Context, evidence map[string]any) error {
	names := make([]string, 0, len(runner.active))
	var cleanupErrors []error
	for name := range runner.active {
		names = append(names, name)
	}
	sort.Strings(names)
	evidence["test_owned_tmux_sessions"] = names
	for _, name := range names {
		if !runner.tmuxExists(name) {
			delete(runner.active, name)
			continue
		}
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		output, err := exec.CommandContext(commandCtx, runner.config.tmux, "kill-session", "-t", "="+name).CombinedOutput() //nolint:gosec // exact generated name.
		cancel()
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("kill tmux %s: %w: %s", name, err, strings.TrimSpace(string(output))))
			continue
		}
		delete(runner.active, name)
	}
	roster, raw, rosterErr := runner.waitRoster(ctx, func(current matrixRoster) bool {
		prefix := "matrix-" + runner.runID + "-"
		for _, row := range current.Local {
			if strings.HasPrefix(row.Name, prefix) {
				return false
			}
		}
		return true
	})
	evidence["roster_after_cleanup"] = json.RawMessage(raw)
	if rosterErr != nil {
		cleanupErrors = append(cleanupErrors, rosterErr)
	} else {
		var survivors []string
		for _, row := range roster.Local {
			if strings.HasPrefix(row.Name, "matrix-"+runner.runID+"-") {
				survivors = append(survivors, row.ID)
			}
		}
		if len(survivors) != 0 {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("live matrix rows survived: %v", survivors))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (runner *matrixRunner) roster(ctx context.Context) (matrixRoster, []byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, runner.config.agentSessions, "roster", "--json").CombinedOutput() //nolint:gosec // resolved installed CLI, closed argv.
	if err != nil {
		return matrixRoster{}, output, fmt.Errorf("read live roster: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var roster matrixRoster
	if err := json.Unmarshal(output, &roster); err != nil || roster.Schema != "agent-sessions.roster.v1" || roster.Host.ID == "" {
		return matrixRoster{}, output, errors.New("live daemon returned an incompatible roster")
	}
	return roster, output, nil
}

func (runner *matrixRunner) waitRoster(
	ctx context.Context,
	predicate func(matrixRoster) bool,
) (matrixRoster, []byte, error) {
	deadline, cancel := context.WithTimeout(ctx, runner.config.timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last matrixRoster
	var raw []byte
	var lastErr error
	for {
		current, currentRaw, err := runner.roster(deadline)
		if err == nil {
			last, raw = current, currentRaw
			if predicate(current) {
				return current, currentRaw, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-deadline.Done():
			if lastErr != nil {
				return last, raw, fmt.Errorf("wait for live roster: %w", lastErr)
			}
			return last, raw, errors.New("timed out waiting for expected live roster state")
		case <-ticker.C:
		}
	}
}

func (runner *matrixRunner) waitForNamedRow(
	ctx context.Context,
	product, name string,
) (matrixRosterEntry, matrixRoster, []byte, error) {
	var selected matrixRosterEntry
	roster, raw, err := runner.waitRoster(ctx, func(current matrixRoster) bool {
		rows := namedRows(current, product, name)
		if len(rows) != 1 {
			return false
		}
		selected = rows[0]
		return selected.NativeSessionID != ""
	})
	return selected, roster, raw, err
}

func (runner *matrixRunner) waitForNewProductRow(
	ctx context.Context,
	product string,
	baseline map[string]bool,
) (matrixRosterEntry, matrixRoster, []byte, error) {
	var selected matrixRosterEntry
	roster, raw, err := runner.waitRoster(ctx, func(current matrixRoster) bool {
		var rows []matrixRosterEntry
		for _, row := range current.Local {
			if row.Kind == "peer" && row.Scope == "local" && row.Live && row.Product == product && !baseline[row.ID] {
				rows = append(rows, row)
			}
		}
		if len(rows) != 1 || rows[0].NativeSessionID == "" {
			return false
		}
		selected = rows[0]
		return true
	})
	return selected, roster, raw, err
}

func (runner *matrixRunner) waitForRowGone(ctx context.Context, sessionID string) error {
	_, _, err := runner.waitRoster(ctx, func(current matrixRoster) bool {
		for _, row := range current.Local {
			if row.ID == sessionID || row.NativeSessionID == sessionID {
				return false
			}
		}
		return true
	})
	return err
}

func namedRows(roster matrixRoster, product, name string) []matrixRosterEntry {
	var rows []matrixRosterEntry
	for _, row := range roster.Local {
		if row.Kind == "peer" && row.Scope == "local" && row.Live && row.Product == product && row.Name == name {
			rows = append(rows, row)
		}
	}
	return rows
}

func rosterIDSet(rows []matrixRosterEntry) map[string]bool {
	result := make(map[string]bool, len(rows))
	for _, row := range rows {
		result[row.ID] = true
	}
	return result
}

func requireNamedGroups(roster matrixRoster, row matrixRosterEntry, explicit string) error {
	want := []string{explicit, "session:" + roster.Host.ID + "/" + row.ID}
	if !equalOrderedStrings(row.Groups, want) {
		return fmt.Errorf("named row groups = %v, want %v", row.Groups, want)
	}
	return nil
}

func requireOnlyPrivateAnchor(roster matrixRoster, row matrixRosterEntry) error {
	want := []string{"session:" + roster.Host.ID + "/" + row.ID}
	if !equalOrderedStrings(row.Groups, want) {
		return fmt.Errorf("no-group row groups = %v, want only %v", row.Groups, want)
	}
	return nil
}

func equalOrderedStrings(left, right []string) bool {
	return slices.Equal(left, right)
}

func (runner *matrixRunner) sendV1(
	ctx context.Context,
	product, cell, group, source string,
	params map[string]any,
) (response livepresence.Frame, evidencePath string, resultErr error) {
	uuid, err := matrixUUID()
	if err != nil {
		return livepresence.Frame{}, "", err
	}
	relative := filepath.Join(product, cell+"-wire.json")
	evidencePath = filepath.Join(runner.config.evidenceDir, relative)
	evidence := matrixWireEvidence{Frames: []matrixWireFrame{}}
	commandCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	var live *livepresence.Client
	defer func() {
		cancel()
		if live != nil {
			<-live.Done()
		}
		if resultErr != nil {
			evidence.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, runner.writeJSON(relative, evidence))
	}()
	report := livepresence.Report{UUID: uuid, Name: source, Groups: []string{group}, Product: "claude", Info: map[string]string{}}
	live = livepresence.StartClient(commandCtx, runner.config.presenceSocket, report,
		func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
			if method != "message.deliver" {
				return nil, livepresence.NewError(livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": method})
			}
			if _, err := livepresence.DecodeDeliver(params); err != nil {
				return nil, err
			}
			return nil, livepresence.NewError(livepresence.Busy, "Session busy", map[string]any{"uuid": uuid})
		}, func(direction string, frame livepresence.Frame) {
			evidence.Frames = append(evidence.Frames, matrixWireFrame{Direction: direction, Frame: frame})
		})
	select {
	case <-live.Ready():
	case <-live.Done():
		return response, evidencePath, errors.New("raw v1 client stopped before session.hello acknowledgement")
	case <-commandCtx.Done():
		return response, evidencePath, commandCtx.Err()
	}
	result, err := live.Call(commandCtx, "send", "message.send", params)
	if err != nil {
		return response, evidencePath, err
	}
	return livepresence.Success(nil, result), evidencePath, nil
}

func matrixUUID() (string, error) {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(body)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func matrixReceiptToken(runID, cell string) string {
	return "MX" + runID[len(runID)-8:] + cell
}

func matrixEnvelopeSource(token, product string) string {
	return token + "-" + product
}

func matrixDeliveryMessage(token string) string {
	return "Matrix delivery " + token + "."
}

func matrixEnvelopeMarker(product matrixProduct, source string) string {
	return fmt.Sprintf(product.nativeEnvelopeFormat, source)
}

func matrixPersistencePrompt(runID string) (string, string) {
	expected := matrixReceiptToken(runID, "06")
	left, right := expected[:len(expected)/2], expected[len(expected)/2:]
	return fmt.Sprintf("Without tools, concatenate %s and %s. Output only the result.", left, right), expected
}

func requireAcceptedSessions(response livepresence.Frame, expected []string) error {
	if response.Error != nil {
		return fmt.Errorf("message.send failed: %s (%d)", response.Error.Message, response.Error.Code)
	}
	var result matrixSendResult
	if json.Unmarshal(response.Result, &result) != nil || result.MessageID == "" {
		return errors.New("message.send returned an invalid result")
	}
	got := make([]string, 0, len(result.Deliveries))
	for _, delivery := range result.Deliveries {
		if delivery.Target == "" || delivery.SessionID == "" || delivery.DeliveryID == "" || delivery.Status != "accepted" {
			return fmt.Errorf("message.send returned invalid delivery %+v", delivery)
		}
		got = append(got, delivery.SessionID)
	}
	want := append([]string(nil), expected...)
	sort.Strings(got)
	sort.Strings(want)
	if !equalOrderedStrings(got, want) {
		return fmt.Errorf("accepted native session ids = %v, want exactly %v", got, want)
	}
	return nil
}

func (runner *matrixRunner) captureReceipt(
	ctx context.Context,
	product matrixProduct,
	cell string,
	tui *matrixTUI,
	marker, label string,
	afterMarker, busyMarker string,
) (string, error) {
	relative := filepath.Join(product.id, cell+"-"+label+".pane.txt")
	deadline, cancel := context.WithTimeout(ctx, runner.config.timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		capture, err := runner.capturePane(deadline, tui)
		if err == nil {
			last = capture
			if paneHasMarkerThenReady(capture, marker, afterMarker, busyMarker) {
				path, writeErr := runner.writeText(relative, capture)
				return path, writeErr
			}
		}
		select {
		case <-deadline.Done():
			path, writeErr := runner.writeText(relative, last)
			if writeErr != nil {
				return path, writeErr
			}
			if detail := matrixOwnerEnvironmentDetail(product, last); detail != "" {
				return path, errors.New(detail)
			}
			return path, fmt.Errorf("product TUI did not reach expected marker state %s", marker)
		case <-ticker.C:
		}
	}
}

func matrixOwnerEnvironmentDetail(product matrixProduct, capture string) string {
	const startText, endText = "Quota exhausted:", "(Press Ctrl+Y to retry)"
	start := strings.Index(capture, startText)
	if start < 0 {
		return ""
	}
	verbatim := capture[start:]
	end := strings.Index(verbatim, endText)
	if end < 0 {
		return ""
	}
	verbatim = verbatim[:end+len(endText)]
	if product.nativeQuotaMarker == "" || !strings.Contains(verbatim, product.nativeQuotaMarker) {
		return ""
	}
	return "OWNER-ENVIRONMENT quota: " + strings.TrimSpace(verbatim)
}

func paneHasMarkerThenReady(capture, marker, readyMarker, busyMarker string) bool {
	markerAt := strings.Index(capture, marker)
	if markerAt < 0 {
		return false
	}
	after := capture[markerAt+len(marker):]
	return (readyMarker == "" || strings.Contains(after, readyMarker)) &&
		(busyMarker == "" || !strings.Contains(after, busyMarker))
}

func (runner *matrixRunner) capturePane(ctx context.Context, tui *matrixTUI) (string, error) {
	if tui == nil {
		return "", errors.New("capture target is nil")
	}
	output, err := exec.CommandContext(ctx, runner.config.tmux, "capture-pane", "-p", "-J", "-S", "-", "-t", tui.pane).CombinedOutput() //nolint:gosec // exact generated pane.
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (runner *matrixRunner) diagnoseAttachFailure(product matrixProduct, label string, tui *matrixTUI, evidence map[string]any, attachErr error) error {
	if tui == nil {
		return attachErr
	}
	captureCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	capture, err := runner.capturePane(captureCtx, tui)
	if err != nil {
		return errors.Join(attachErr, fmt.Errorf("capture unattached %s TUI: %w", product.id, err))
	}
	relative := filepath.Join(product.id, "attach-"+label+".pane.txt")
	path, err := runner.writeText(relative, capture)
	if err != nil {
		return errors.Join(attachErr, fmt.Errorf("preserve unattached %s pane: %w", product.id, err))
	}
	evidence["pane_evidence"] = path
	detail := fmt.Errorf("%w; %s TUI did not attach from approved cwd %s; pane evidence: %s", attachErr, product.id, runner.config.cwd, path)
	if product.nativeTrustPrompt != "" && strings.Contains(capture, product.nativeTrustPrompt) {
		detail = fmt.Errorf("%w; trust %s once via %s's own prompt and rerun", detail, runner.config.cwd, product.id)
	}
	return detail
}
