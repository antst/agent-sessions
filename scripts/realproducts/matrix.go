package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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

const matrixMaxFrameBytes = 1024 * 1024

var (
	errMatrixFrameTooLarge  = errors.New("matrix live frame exceeds 1 MiB")
	errMatrixFrameTruncated = errors.New("matrix live frame is not newline terminated")
)

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

type matrixFramedConn struct {
	net.Conn
	reader *bufio.Reader
	frame  []byte
}

func newMatrixFramedConn(connection net.Conn) *matrixFramedConn {
	return &matrixFramedConn{Conn: connection, reader: bufio.NewReaderSize(connection, matrixMaxFrameBytes+1)}
}

func (connection *matrixFramedConn) Read(body []byte) (int, error) {
	if len(body) == 0 {
		return 0, nil
	}
	if len(connection.frame) == 0 {
		frame, err := connection.reader.ReadSlice('\n')
		if len(frame) > matrixMaxFrameBytes || errors.Is(err, bufio.ErrBufferFull) {
			return 0, errMatrixFrameTooLarge
		}
		if err != nil {
			if len(frame) != 0 {
				return 0, fmt.Errorf("%w: %v", errMatrixFrameTruncated, err)
			}
			return 0, err
		}
		connection.frame = frame
	}
	written := copy(body, connection.frame)
	connection.frame = connection.frame[written:]
	return written, nil
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
			nativeSubmitDelay:     codexTUISubmitDelay,
			nativeQuitDocumentURL: "https://github.com/openai/codex/blob/main/codex-rs/tui/src/slash_command.rs",
			nativeTrustPrompt:     "Do you trust the contents of this directory?",
			nativeReadyMarker:     "› Ask Codex to do anything",
		},
		{
			id: "claude", nativeExecutable: "claude", peerExecutable: "claude-peer",
			displayArguments: []string{"--ax-screen-reader"}, nativeQuitCommand: "/exit",
			nativeQuitDocumentURL: "https://code.claude.com/docs/en/commands",
			nativeTrustPrompt:     "Permission Required: Accessing workspace:",
			nativeReadyMarker:     "\n$\n",
		},
		{
			id: "grok", nativeExecutable: "grok", peerExecutable: "grok-peer",
			displayArguments: []string{"--no-alt-screen", "--minimal"}, nativeQuitCommand: "/quit",
			nativeQuitDocumentURL: "https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/04-slash-commands.md",
			nativeReadyMarker:     "\n❯\nGrok ",
		},
		{
			id: "qwen", nativeExecutable: "qwen", peerExecutable: "qwen-peer",
			displayArguments: []string{"--screen-reader"}, nativeQuitCommand: "/quit",
			nativeQuitDocumentURL: "https://github.com/QwenLM/qwen-code/blob/main/docs/users/features/commands.md",
			nativeReadyMarker:     "Auto mode   Type your message or @path/to/file",
			nativeBusyMarker:      "esc to cancel",
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

	if launchErr != nil {
		for _, cell := range []string{"06-resume-by-name", "07-terminal-quit", "07b-native-quit-resume"} {
			runner.record(product.id, cell, matrixSkip, "named identity session was unavailable", nil)
		}
		return
	}
	resumed := runner.runResumeCell(ctx, product, identity, identityName, identityGroup)
	if resumed == nil {
		runner.record(product.id, "07-terminal-quit", matrixSkip, "resume cell did not produce a live TUI", nil)
		runner.record(product.id, "07b-native-quit-resume", matrixSkip, "resume cell did not produce a live TUI", nil)
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
		marker := matrixReceiptToken(runner.runID, "03")
		response, wirePath, sendErr := runner.sendV1(ctx, product.id, "direct", group, map[string]any{
			"target": name, "message": marker,
		})
		evidence["wire_evidence"] = wirePath
		evidence["response"] = response
		if sendErr == nil {
			sendErr = requireAcceptedSessions(response, []string{tui.sessionID})
		}
		capturePath, receiptErr := runner.captureReceipt(ctx, product.id, "03-direct-send", tui, marker, "target", "", "")
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
		marker := matrixReceiptToken(runner.runID, "04")
		response, wirePath, sendErr := runner.sendV1(ctx, product.id, "multicast", group, map[string]any{
			"targets": []string{first.name, second.name}, "message": marker,
		})
		evidence["wire_evidence"] = wirePath
		evidence["response"] = response
		if sendErr == nil {
			sendErr = requireAcceptedSessions(response, []string{first.sessionID, second.sessionID})
		}
		firstCapture, firstReceipt := runner.captureReceipt(ctx, product.id, "04-targets-multicast", first, marker, "target-a", "", "")
		secondCapture, secondReceipt := runner.captureReceipt(ctx, product.id, "04-targets-multicast", second, marker, "target-b", "", "")
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
		marker := matrixReceiptToken(runner.runID, "05")
		response, wirePath, sendErr := runner.sendV1(ctx, product.id, "group", group, map[string]any{
			"group": group, "message": marker,
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
			firstCapture, firstReceipt := runner.captureReceipt(ctx, product.id, "05-group-send", first, marker, "target-a", "", "")
			secondCapture, secondReceipt := runner.captureReceipt(ctx, product.id, "05-group-send", second, marker, "target-b", "", "")
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

func (runner *matrixRunner) runResumeCell(
	ctx context.Context,
	product matrixProduct,
	identity *matrixTUI,
	name, group string,
) *matrixTUI {
	evidence := map[string]any{"original_native_session_id": identity.sessionID}
	persistencePath, persistenceErr := runner.persistNativeTurn(ctx, product, identity)
	evidence["persistence_pane"] = persistencePath
	if persistenceErr != nil {
		persistenceErr = errors.Join(persistenceErr, runner.endTUI(ctx, identity))
		runner.record(product.id, "06-resume-by-name", matrixFail, persistenceErr.Error(), evidence)
		return nil
	}
	if err := runner.endTUI(ctx, identity); err != nil {
		runner.record(product.id, "06-resume-by-name", matrixFail, err.Error(), evidence)
		return nil
	}
	resumed, launchEvidence, err := runner.launchResume(ctx, product, name, group, "resume-1", identity.sessionID)
	evidence["resume_launch"] = launchEvidence
	if err != nil {
		if resumed != nil {
			err = errors.Join(err, runner.endTUI(ctx, resumed))
		}
		runner.record(product.id, "06-resume-by-name", matrixFail, err.Error(), evidence)
		return nil
	}
	runner.record(product.id, "06-resume-by-name", matrixPass, "resume by exact unique name retained the product-native id without a fork", evidence)
	return resumed
}

func (runner *matrixRunner) persistNativeTurn(ctx context.Context, product matrixProduct, tui *matrixTUI) (string, error) {
	if _, err := runner.captureReceipt(ctx, product.id, "06-resume-by-name", tui, product.nativeReadyMarker, "ready-before-persistence", "", product.nativeBusyMarker); err != nil {
		return "", err
	}
	prompt, expected := matrixPersistencePrompt(runner.runID)
	if err := runner.sendTUIInput(ctx, tui, prompt); err != nil {
		return "", err
	}
	return runner.captureReceipt(ctx, product.id, "06-resume-by-name", tui, expected, "persistence", product.nativeReadyMarker, product.nativeBusyMarker)
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
	return tui, command, nil
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
	product, cell, group string,
	params map[string]any,
) (response livepresence.Frame, evidencePath string, resultErr error) {
	uuid, err := matrixUUID()
	if err != nil {
		return livepresence.Frame{}, "", err
	}
	relative := filepath.Join(product, cell+"-wire.json")
	evidencePath = filepath.Join(runner.config.evidenceDir, relative)
	evidence := matrixWireEvidence{Frames: []matrixWireFrame{}}
	var connection net.Conn
	var stopCancel func() bool
	defer func() {
		if stopCancel != nil {
			stopCancel()
		}
		var closeErr error
		if connection != nil {
			closeErr = connection.Close()
			if errors.Is(closeErr, net.ErrClosed) {
				closeErr = nil
			}
		}
		terminalErr := errors.Join(resultErr, closeErr)
		if terminalErr != nil {
			evidence.Error = terminalErr.Error()
		}
		resultErr = errors.Join(terminalErr, runner.writeJSON(relative, evidence))
	}()

	commandCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	connection, err = (&net.Dialer{}).DialContext(commandCtx, "unix", runner.config.presenceSocket)
	if err != nil {
		return livepresence.Frame{}, evidencePath, fmt.Errorf("connect raw v1 client: %w", err)
	}
	if deadline, ok := commandCtx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return livepresence.Frame{}, evidencePath, fmt.Errorf("set raw v1 deadline: %w", err)
		}
	}
	stopCancel = context.AfterFunc(commandCtx, func() { _ = connection.Close() })
	live := livepresence.NewConnection(newMatrixFramedConn(connection))
	report := map[string]any{"protocol": livepresence.ProtocolVersion, "uuid": uuid, "name": "matrix-" + runner.runID + "-source-" + product + "-" + cell,
		"groups": []string{group}, "product": "claude", "info": map[string]string{}}
	hello, err := matrixLiveCall(live, "hello", "session.hello", report, uuid, &evidence)
	if err != nil {
		return livepresence.Frame{}, evidencePath, fmt.Errorf("session.hello: %w", err)
	}
	var acknowledged map[string]json.RawMessage
	if hello.Error != nil || livepresence.DecodeStrict(hello.Result, &acknowledged) != nil || acknowledged == nil || len(acknowledged) != 0 {
		return livepresence.Frame{}, evidencePath, errors.New("session.hello was not acknowledged with an empty object")
	}
	response, err = matrixLiveCall(live, "send", "message.send", params, uuid, &evidence)
	if err != nil {
		return livepresence.Frame{}, evidencePath, fmt.Errorf("message.send: %w", err)
	}
	return response, evidencePath, nil
}

func matrixLiveCall(
	connection *livepresence.Connection,
	id, method string,
	params any,
	sourceUUID string,
	evidence *matrixWireEvidence,
) (livepresence.Frame, error) {
	wireID, _ := json.Marshal(id)
	body, err := json.Marshal(params)
	if err != nil {
		return livepresence.Frame{}, err
	}
	request := livepresence.Frame{JSONRPC: "2.0", ID: wireID, Method: method, Params: body}
	evidence.Frames = append(evidence.Frames, matrixWireFrame{Direction: "send", Frame: request})
	if err := connection.Write(request); err != nil {
		return livepresence.Frame{}, err
	}
	for {
		var frame livepresence.Frame
		if err := connection.Decode(&frame); err != nil {
			return livepresence.Frame{}, err
		}
		evidence.Frames = append(evidence.Frames, matrixWireFrame{Direction: "receive", Frame: frame})
		if !livepresence.ValidFrame(frame) {
			return livepresence.Frame{}, errors.New("daemon returned an invalid JSON-RPC frame")
		}
		if frame.Method == "" {
			if !bytes.Equal(frame.ID, wireID) {
				return livepresence.Frame{}, fmt.Errorf("daemon returned response id %s while awaiting %s", frame.ID, wireID)
			}
			return frame, nil
		}
		if frame.Method != "message.deliver" || !livepresence.ValidRequest(frame) {
			return livepresence.Frame{}, fmt.Errorf("daemon interleaved unexpected method %q", frame.Method)
		}
		if _, err := livepresence.DecodeDeliver(frame.Params); err != nil {
			return livepresence.Frame{}, fmt.Errorf("daemon interleaved invalid message.deliver: %w", err)
		}
		busy := livepresence.Failure(frame.ID, livepresence.Busy, "Session busy", map[string]any{"uuid": sourceUUID})
		evidence.Frames = append(evidence.Frames, matrixWireFrame{Direction: "send", Frame: busy})
		if err := connection.Write(busy); err != nil {
			return livepresence.Frame{}, err
		}
	}
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
	product, cell string,
	tui *matrixTUI,
	marker, label string,
	afterMarker, busyMarker string,
) (string, error) {
	relative := filepath.Join(product, cell+"-"+label+".pane.txt")
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
			return path, fmt.Errorf("product TUI did not reach expected marker state %s", marker)
		case <-ticker.C:
		}
	}
}

func paneHasMarkerThenReady(capture, marker, readyMarker, busyMarker string) bool {
	markerAt := strings.Index(capture, marker)
	return markerAt >= 0 &&
		(readyMarker == "" || strings.Contains(capture[markerAt+len(marker):], readyMarker)) &&
		(busyMarker == "" || !strings.Contains(capture, busyMarker))
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
