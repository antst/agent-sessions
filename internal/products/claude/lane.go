package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

const ProductID = "claude"

type LaneConfig struct {
	Descriptor productcatalog.Descriptor
	Generation uint64
	Processes  ProcessFactory
	Now        func() time.Time
}

type LaneDriver struct {
	config LaneConfig
	mu     sync.Mutex
	lanes  map[string]*laneSession
}

type laneSession struct {
	mu         sync.Mutex
	ref        productruntime.NativeSessionRef
	permission permissionmode.Mode
	process    streamProcess
	cancel     context.CancelFunc
	active     *laneTurn
	controls   map[string]chan error
	nextID     uint64
	writes     uint64
	replays    uint64
	failed     error
	closed     bool
}

type laneTurn struct {
	ref      productruntime.NativeTurnRef
	done     chan struct{}
	once     sync.Once
	write    uint64
	consumed bool
	terminal productruntime.NativeTerminal
	err      error
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if config.Descriptor.ID != ProductID || strings.TrimSpace(config.Descriptor.NativeExecutable) == "" || config.Generation == 0 || config.Processes == nil {
		return nil, errors.New("Claude lane requires its descriptor, generation, and process factory")
	}
	if len(config.Descriptor.NativeToolGrantArgs) == 0 || len(config.Descriptor.NativeYoloArgs) == 0 {
		return nil, errors.New("Claude lane descriptor is missing its native launch policy")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &LaneDriver{config: config, lanes: make(map[string]*laneSession)}, nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{Steer: true, DurableResume: true, CallerSuppliedSessionID: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if ctx == nil {
		return productruntime.NativeSessionRef{}, errors.New("Claude lane open requires context")
	}
	if err := ctx.Err(); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if request.ProductID != ProductID || strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(request.Cwd) == "" || !request.PermissionMode.Valid() {
		return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
	}
	if request.ResumeNativeID == "" && strings.TrimSpace(request.Name) == "" {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: fresh Claude lane requires a product-native name", productruntime.ErrNativeRejected)
	}
	for _, argument := range request.Arguments {
		if reservedArgument(argument) {
			return productruntime.NativeSessionRef{}, fmt.Errorf("%w: native argument %q is owned by the Claude lane lifecycle", productruntime.ErrUnsupportedPolicy, argument)
		}
	}
	driver.mu.Lock()
	if existing := driver.lanes[request.LaneID]; existing != nil {
		existing.mu.Lock()
		live := !existing.closed && existing.failed == nil && request.ResumeNativeID != "" &&
			existing.ref.NativeSessionID == request.ResumeNativeID && existing.permission == request.PermissionMode
		ref := existing.ref
		existing.mu.Unlock()
		driver.mu.Unlock()
		if live {
			return ref, nil
		}
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: lane %q already has an ephemeral Claude client", productruntime.ErrAmbiguousSession, request.LaneID)
	}
	driver.mu.Unlock()

	nativeID := request.ResumeNativeID
	arguments := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages"}
	if nativeID == "" {
		nativeID = request.LaneID
		arguments = append(arguments, "--session-id", nativeID, "--name", request.Name)
	} else {
		arguments = append(arguments, "--resume", nativeID)
	}
	if request.LaneID != nativeID {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: Claude lane identity must equal its native session", productruntime.ErrProtocol)
	}
	if request.PermissionMode == permissionmode.BypassPermissions {
		arguments = append(arguments, driver.config.Descriptor.NativeYoloArgs...)
	} else {
		arguments = append(arguments, "--permission-mode", "dontAsk")
	}
	arguments = append(arguments, driver.config.Descriptor.NativeToolGrantArgs...)
	arguments = append(arguments, request.Arguments...)
	environment, err := productruntime.ParseNativeEnvironment(request.Environment)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	process, err := driver.config.Processes.StartStream(lifetime, productruntime.NativeCommand{
		Path: driver.config.Descriptor.NativeExecutable,
		Args: arguments,
		Env:  environment,
		Cwd:  request.Cwd,
	})
	if err != nil {
		cancel()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: start Claude stream: %v", productruntime.ErrUnavailable, err)
	}
	ref := productruntime.NativeSessionRef{LaneID: nativeID, NativeSessionID: nativeID, Generation: driver.config.Generation}
	session := &laneSession{
		ref: ref, permission: request.PermissionMode, process: process, cancel: cancel,
		controls: make(map[string]chan error),
	}
	driver.mu.Lock()
	if _, exists := driver.lanes[nativeID]; exists {
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, cleanupOpenFailure(cancel, process, fmt.Errorf("%w: Claude native session %q already has a lane owner", productruntime.ErrAmbiguousSession, nativeID))
	}
	driver.lanes[nativeID] = session
	driver.mu.Unlock()
	go session.readLoop()
	return ref, nil
}

func cleanupOpenFailure(cancel context.CancelFunc, process streamProcess, primary error) error {
	cancel()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cleanupCancel()
	if cleanupErr := process.Cleanup(cleanupCtx); cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("clean failed Claude stream launch: %w", cleanupErr))
	}
	return primary
}

func (driver *LaneDriver) StartTurn(ctx context.Context, ref productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	session, err := driver.session(ref)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if request.PermissionMode != session.permission {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: per-turn policy differs from the Claude launch policy", productruntime.ErrUnsupportedPolicy)
	}
	prompt, err := lanePrompt(request.Prompt)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	session.mu.Lock()
	if session.closed || session.failed != nil || session.active != nil {
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: Claude lane is not idle", productruntime.ErrAmbiguousSession)
	}
	session.nextID++
	turnRef := productruntime.NativeTurnRef{NativeSessionRef: ref, NativeTurnID: strconv.FormatUint(session.nextID, 10)}
	turn := &laneTurn{ref: turnRef, done: make(chan struct{})}
	if err := session.writeUserLocked(ctx, prompt); err != nil {
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, err
	}
	turn.write = session.writes
	session.active = turn
	session.mu.Unlock()
	return turnRef, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, ref productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	if ctx == nil {
		return productruntime.NativeTerminal{}, errors.New("Claude lane wait requires context")
	}
	session, turn, err := driver.turn(ref, false)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	select {
	case <-ctx.Done():
		return productruntime.NativeTerminal{}, ctx.Err()
	case <-turn.done:
	}
	session.mu.Lock()
	if session.active == turn {
		session.active = nil
	}
	session.mu.Unlock()
	return turn.terminal, turn.err
}

func (driver *LaneDriver) Steer(ctx context.Context, ref productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	session, turn, err := driver.turn(ref, true)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if request.PermissionMode != session.permission {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: steer policy differs from the Claude launch policy", productruntime.ErrUnsupportedPolicy)
	}
	prompt, err := lanePrompt(request.Prompt)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	session.mu.Lock()
	if session.active != turn {
		session.mu.Unlock()
		return productruntime.NativeAcceptance{}, productruntime.ErrStale
	}
	if err := session.writeUserLocked(ctx, prompt); err != nil {
		session.mu.Unlock()
		return productruntime.NativeAcceptance{}, err
	}
	session.mu.Unlock()
	return productruntime.NativeAcceptance{
		NativeSessionID: ref.NativeSessionID, NativeMessageID: ref.NativeTurnID, AcceptedAt: driver.config.Now().UTC(),
	}, nil
}

func (driver *LaneDriver) SendMessage(ctx context.Context, ref productruntime.NativeSessionRef, message productruntime.NativeMessage) error {
	session, err := driver.session(ref)
	if err != nil {
		return err
	}
	rendered, err := sessiontools.RenderNativeMessage(message)
	if err != nil {
		return productruntime.ErrProtocol
	}
	prompt, err := lanePrompt(rendered)
	if err != nil {
		return err
	}
	session.mu.Lock()
	if session.closed || session.failed != nil {
		session.mu.Unlock()
		return fmt.Errorf("%w: Claude lane stream is unavailable", productruntime.ErrUnavailable)
	}
	if err := session.writeUserLocked(ctx, prompt); err != nil {
		session.mu.Unlock()
		return err
	}
	session.mu.Unlock()
	return nil
}

func (driver *LaneDriver) Interrupt(ctx context.Context, ref productruntime.NativeTurnRef) error {
	session, _, err := driver.turn(ref, true)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.nextID++
	requestID := "interrupt-" + strconv.FormatUint(session.nextID, 10)
	future := make(chan error, 1)
	session.controls[requestID] = future
	session.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"type": "control_request", "request_id": requestID, "request": map[string]any{"subtype": "interrupt"},
	})
	if err == nil {
		err = session.process.WriteFrame(ctx, body)
	}
	if err != nil {
		session.removeControl(requestID)
		return err
	}
	select {
	case <-ctx.Done():
		session.removeControl(requestID)
		return ctx.Err()
	case err := <-future:
		return err
	}
}

func (driver *LaneDriver) Archive(ctx context.Context, ref productruntime.NativeSessionRef) error {
	session, err := driver.lookupSession(ref)
	if err != nil {
		return err
	}
	session.mu.Lock()
	if session.active != nil {
		session.mu.Unlock()
		return fmt.Errorf("%w: refuse to archive a running Claude turn", productruntime.ErrNativeRejected)
	}
	session.mu.Unlock()
	if err := session.process.Cleanup(ctx); err != nil {
		return fmt.Errorf("archive exact Claude stream: %w", err)
	}
	session.cancel()
	session.mu.Lock()
	session.closed = true
	session.mu.Unlock()
	driver.mu.Lock()
	if driver.lanes[ref.LaneID] == session {
		delete(driver.lanes, ref.LaneID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) session(ref productruntime.NativeSessionRef) (*laneSession, error) {
	session, err := driver.lookupSession(ref)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	closed, failed := session.closed, session.failed
	session.mu.Unlock()
	if closed {
		return nil, productruntime.ErrStale
	}
	if failed != nil {
		return nil, failed
	}
	return session, nil
}

func (driver *LaneDriver) lookupSession(ref productruntime.NativeSessionRef) (*laneSession, error) {
	if ref.Generation != driver.config.Generation || strings.TrimSpace(ref.LaneID) == "" || ref.LaneID != ref.NativeSessionID {
		return nil, productruntime.ErrStale
	}
	driver.mu.Lock()
	session := driver.lanes[ref.LaneID]
	driver.mu.Unlock()
	if session == nil || session.ref != ref {
		return nil, productruntime.ErrStale
	}
	return session, nil
}

func (driver *LaneDriver) turn(ref productruntime.NativeTurnRef, requireRunning bool) (*laneSession, *laneTurn, error) {
	session, err := driver.lookupSession(ref.NativeSessionRef)
	if err != nil {
		return nil, nil, err
	}
	session.mu.Lock()
	turn := session.active
	valid := turn != nil && turn.ref == ref
	done := false
	if valid {
		select {
		case <-turn.done:
			done = true
		default:
		}
	}
	session.mu.Unlock()
	if !valid || requireRunning && done {
		return nil, nil, productruntime.ErrStale
	}
	return session, turn, nil
}

func lanePrompt(prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("%w: Claude lane prompt is empty", productruntime.ErrNativeRejected)
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "<cross-session-message ") {
		prompt = "The following Agent Sessions peer message is the current user turn. " +
			"Act on its enclosed content and preserve its sender metadata.\n\n" + prompt
	}
	return prompt, nil
}

// writeUserLocked writes one product input and advances its FIFO position.
// The caller holds session.mu so stdout replay cannot overtake publication.
func (session *laneSession) writeUserLocked(ctx context.Context, prompt string) error {
	body, err := json.Marshal(map[string]any{
		"type": "user", "message": map[string]any{
			"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}},
		},
	})
	if err != nil {
		return err
	}
	if err := session.process.WriteFrame(ctx, body); err != nil {
		return err
	}
	session.writes++
	return nil
}

func (session *laneSession) readLoop() {
	for {
		body, err := session.process.ReadFrame(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.EOF
			}
			session.fail(err)
			return
		}
		var frame map[string]any
		if json.Unmarshal(body, &frame) != nil {
			session.fail(errors.New("malformed Claude stream frame"))
			return
		}
		typeName, _ := frame["type"].(string)
		switch typeName {
		case "system":
			if subtype, _ := frame["subtype"].(string); subtype == "init" {
				session.checkIdentity(frame)
			}
		case "control_response":
			session.deliverControl(frame)
		case "user":
			if replayed, _ := frame["isReplay"].(bool); replayed {
				session.consumeReplay()
			}
		case "result":
			session.deliverResult(frame)
		}
	}
}

func (session *laneSession) consumeReplay() {
	session.mu.Lock()
	session.replays++
	if session.active != nil && session.replays >= session.active.write {
		session.active.consumed = true
	}
	session.mu.Unlock()
}

func (session *laneSession) checkIdentity(frame map[string]any) {
	nativeID, _ := frame["session_id"].(string)
	if nativeID != "" && nativeID != session.ref.NativeSessionID {
		session.fail(fmt.Errorf("%w: Claude stream changed native session from %q to %q", productruntime.ErrAmbiguousSession, session.ref.NativeSessionID, nativeID))
	}
}

func (session *laneSession) deliverControl(frame map[string]any) {
	response, _ := frame["response"].(map[string]any)
	requestID, _ := response["request_id"].(string)
	subtype, _ := response["subtype"].(string)
	if requestID == "" {
		return
	}
	session.mu.Lock()
	future := session.controls[requestID]
	delete(session.controls, requestID)
	session.mu.Unlock()
	if future == nil {
		return
	}
	if subtype == "success" {
		future <- nil
		return
	}
	detail, _ := response["error"].(string)
	if detail == "" {
		detail = "Claude rejected interrupt control request"
	}
	future <- errors.New(detail)
}

func (session *laneSession) deliverResult(frame map[string]any) {
	session.mu.Lock()
	turn := session.active
	nativeID, _ := frame["session_id"].(string)
	if nativeID != "" && nativeID != session.ref.NativeSessionID {
		session.mu.Unlock()
		session.fail(fmt.Errorf("%w: Claude result changed native session", productruntime.ErrAmbiguousSession))
		return
	}
	if turn == nil || !turn.consumed {
		session.mu.Unlock()
		return
	}
	result, _ := frame["result"].(string)
	subtype, _ := frame["subtype"].(string)
	reason, _ := frame["terminal_reason"].(string)
	isError, _ := frame["is_error"].(bool)
	terminal := productruntime.NativeTerminal{
		Outcome: productruntime.TurnCompleted, Result: result, ResultDigest: sha256.Sum256([]byte(result)), NativeStopReason: reason,
	}
	session.mu.Unlock()
	switch {
	case subtype == "interrupted" || reason == "interrupted" || reason == "aborted_streaming":
		terminal.Outcome, terminal.ExitLike = productruntime.TurnInterrupted, 130
		turn.resolve(terminal, nil)
	case subtype == "success" && !isError:
		turn.resolve(terminal, nil)
	default:
		terminal.Outcome, terminal.ExitLike = productruntime.TurnFailed, 1
		detail, _ := frame["error"].(string)
		if strings.TrimSpace(detail) == "" {
			detail = strings.TrimSpace(result)
		}
		if detail == "" {
			detail = subtype
		}
		turn.resolve(terminal, errors.New(detail))
	}
}

func (session *laneSession) removeControl(requestID string) {
	session.mu.Lock()
	delete(session.controls, requestID)
	session.mu.Unlock()
}

func (session *laneSession) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	session.mu.Lock()
	if session.failed != nil {
		session.mu.Unlock()
		return
	}
	session.failed = err
	turn := session.active
	controls := session.controls
	session.controls = make(map[string]chan error)
	session.mu.Unlock()
	if turn != nil {
		turn.resolve(productruntime.NativeTerminal{Outcome: productruntime.TurnFailed, ExitLike: 1}, err)
	}
	for _, future := range controls {
		future <- err
	}
}

func (turn *laneTurn) resolve(terminal productruntime.NativeTerminal, err error) {
	turn.once.Do(func() {
		turn.terminal, turn.err = terminal, err
		close(turn.done)
	})
}

func reservedArgument(argument string) bool {
	name := argument
	if index := strings.IndexByte(name, '='); index >= 0 {
		name = name[:index]
	}
	switch name {
	case "-p", "--print", "--input-format", "--output-format", "--verbose", "--replay-user-messages", "--session-id", "--resume", "-r", "--name", "-n", "--permission-mode", "--dangerously-skip-permissions", "--yolo":
		return true
	default:
		return argument == "--"
	}
}

var _ productruntime.LaneDriver = (*LaneDriver)(nil)
var _ productruntime.LaneMessageDriver = (*LaneDriver)(nil)
