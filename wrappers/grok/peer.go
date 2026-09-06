package grok

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

type peerSession struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	Activity  string `json:"activity"`
	Resident  *bool  `json:"resident"`
	Yolo      *bool  `json:"yolo"`
}

var errNoLeader = errors.New("no_leader")
var errRosterActorGone = errors.New("Grok roster actor is not live")

type PeerBackend struct {
	mu       sync.Mutex
	peer     *sessionkit.Peer
	observer *acpClient
	process  *nativeProcess
	leader   *nativeProcess
	identity sessionkit.PeerIdentity
	failed   chan error
}

func newPeerBackend(ctx context.Context, identity sessionkit.PeerIdentity, leaderPath, cwd string, leader *nativeProcess) (*PeerBackend, error) {
	id := identity.SessionID
	if identity.Groups == nil {
		identity.Groups = []string{}
	}
	cmd := command("grok", "--no-auto-update", "--permission-mode", "default", "--leader-socket", leaderPath, "agent", "--leader", "stdio")
	cmd.Dir, cmd.Env, cmd.Stderr, cmd.SysProcAttr = cwd, nativeEnvironment(), os.Stderr, &syscall.SysProcAttr{Setpgid: true}
	input, _ := cmd.StdinPipe()
	output, _ := cmd.StdoutPipe()
	process, err := startNative(cmd)
	if err != nil {
		return nil, err
	}
	changed := make(chan struct{}, 1)
	observer := newACPClient(input, output, func(frame acpFrame) {
		if frame.Method == "_x.ai/sessions/changed" {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	})
	stop := func(err error) (*PeerBackend, error) {
		observer.close()
		stopPeerProcess(process)
		return nil, errors.Join(err, closeNative("leader", leader))
	}
	if err = initializeACP(ctx, observer); err != nil {
		return stop(err)
	}
	b := &PeerBackend{observer: observer, process: process, leader: leader, failed: make(chan error, 1)}
	wanted := identity.Name
	var row peerSession
	for {
		row, err = roster(ctx, observer, id)
		if err == nil {
			break
		}
		if !errors.Is(err, errNoLeader) {
			return stop(err)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return stop(ctx.Err())
		}
	}
	if wanted != "" && wanted != id && wanted != row.Title {
		var renamed struct {
			Success bool `json:"success"`
		}
		if err = observer.request(ctx, "_x.ai/session/rename", map[string]string{"sessionId": id, "title": wanted}, &renamed); err != nil || !renamed.Success {
			return stop(errors.New("Grok peer title is unavailable"))
		}
		if row, err = roster(ctx, observer, id); err != nil || row.Title != wanted {
			return stop(errors.New("Grok peer title was not confirmed"))
		}
	}
	b.identity = sessionkit.PeerIdentity{Product: "grok", SessionID: id, Name: first(row.Title, wanted, id), Groups: identity.Groups, Info: map[string]any{"cwd": row.Cwd}}
	peer, err := sessionkit.ConnectPeer(b.identity, b.deliver)
	if err != nil {
		return stop(err)
	}
	b.peer = peer
	select {
	case <-peer.Ready():
	case <-peer.Closed():
		err = peer.Err()
		if err == nil {
			err = errors.New("Agentbus peer closed before ready")
		}
		return stop(err)
	case <-ctx.Done():
		peer.Shutdown()
		return stop(ctx.Err())
	}
	go b.watchRoster(ctx, observer, changed)
	return b, nil
}

func RunInteractive(ctx context.Context, plan host.ExecPlan) error {
	if environmentValue(plan.Env, host.SessionIDEnv) == "" {
		path, err := exec.LookPath(plan.Path)
		if err != nil {
			return err
		}
		return syscall.Exec(path, append([]string{path}, plan.Args...), plan.Env)
	}
	identity, socket, err := peerIdentity(plan.Env)
	if err != nil {
		return err
	}
	key := host.LaunchTokenDigest(identity.SessionID)
	endpoint, err := host.ListenPrivate(socket, key)
	if err != nil {
		return err
	}
	cwd, err := interactiveCwd(plan.Args)
	if err != nil {
		_ = endpoint.Close()
		return err
	}
	leaderPath := leaderSocket(socket, key)
	defer os.Remove(leaderPath)
	defer os.Remove(strings.TrimSuffix(leaderPath, ".sock") + ".lock")
	leader, err := startLeader(socket, key, cwd, interactivePermission(plan.Args), setEnvironment(nativeEnvironment(), mcp.LaneSocketEnv, endpoint.Path))
	if err != nil {
		_ = endpoint.Close()
		return err
	}
	plan.Env = setEnvironment(plan.Env, mcp.LaneSocketEnv, endpoint.Path)
	plan.Args = insertBeforeSeparator(plan.Args, "--leader-socket", leaderPath)
	child := command(plan.Path, plan.Args...)
	child.Env, child.Stdin, child.Stdout, child.Stderr = plan.Env, os.Stdin, os.Stdout, os.Stderr
	if err = child.Start(); err != nil {
		_ = endpoint.Close()
		return errors.Join(err, closeNative("leader", leader))
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	childDone := make(chan error, 1)
	go func() {
		childDone <- child.Wait()
		close(childDone)
		cancel()
	}()
	backend, err := newPeerBackend(childCtx, identity, leaderPath, cwd, leader)
	if err != nil {
		select {
		case childErr := <-childDone:
			_ = endpoint.Close()
			return childErr
		default:
		}
		_ = endpoint.Close()
		if ctx.Err() != nil {
			return finishInteractiveChild(child, childDone, interactiveSignal(ctx))
		}
		_ = finishInteractiveChild(child, childDone, syscall.SIGTERM)
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- mcp.ServeLane(childCtx, endpoint, backend) }()
	peer, observer := backend.peer, backend.observer
	var childErr, dependencyErr error
	var stop os.Signal
	select {
	case childErr = <-childDone:
	case <-ctx.Done():
		stop = interactiveSignal(ctx)
	case <-peer.Closed():
		dependencyErr, stop = peer.Err(), syscall.SIGTERM
		if dependencyErr == nil {
			dependencyErr = errors.New("Agentbus peer closed")
		}
	case <-observer.done:
		dependencyErr, stop = fmt.Errorf("Grok observer closed: %w", observer.err), syscall.SIGTERM
	case dependencyErr = <-backend.failed:
		stop = syscall.SIGTERM
	}
	if stop != nil {
		childErr = finishInteractiveChild(child, childDone, stop)
	}
	backend.Shutdown()
	_ = endpoint.Close()
	<-serveDone
	if dependencyErr != nil {
		return dependencyErr
	}
	return childErr
}

func finishInteractiveChild(child *exec.Cmd, done <-chan error, stop os.Signal) error {
	if stop != nil {
		_ = child.Process.Signal(stop)
	}
	return <-done
}

func interactiveSignal(ctx context.Context) os.Signal {
	var caught interface{ CaughtSignal() os.Signal }
	if errors.As(context.Cause(ctx), &caught) {
		return caught.CaughtSignal()
	}
	return syscall.SIGTERM
}

func peerIdentity(environment []string) (sessionkit.PeerIdentity, string, error) {
	id, name := environmentValue(environment, host.SessionIDEnv), environmentValue(environment, host.NameEnv)
	if id == "" {
		return sessionkit.PeerIdentity{}, "", errors.New("Grok peer identity is unavailable; start Grok with grok-peer")
	}
	groups := []string{}
	if raw := environmentValue(environment, host.GroupsEnv); raw != "" && json.Unmarshal([]byte(raw), &groups) != nil {
		return sessionkit.PeerIdentity{}, "", errors.New("AGENTBUS_GROUPS must be a JSON array")
	}
	socket := first(environmentValue(environment, host.SocketEnv), sessionkit.Socket())
	return sessionkit.PeerIdentity{Product: "grok", SessionID: id, Name: first(name, id), Groups: groups}, socket, nil
}

func environmentValue(environment []string, name string) string {
	for _, value := range environment {
		if key, body, found := strings.Cut(value, "="); found && key == name {
			return body
		}
	}
	return ""
}

func setEnvironment(environment []string, name, value string) []string {
	result := slices.DeleteFunc(slices.Clone(environment), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return key == name
	})
	return append(result, name+"="+value)
}

func (b *PeerBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	peer := b.peer
	b.mu.Unlock()
	if peer == nil {
		return nil, errors.New("not connected")
	}
	return peer.Call(ctx, method, params)
}

func (b *PeerBackend) Caller() *sessionkit.Caller {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.peer == nil {
		return nil
	}
	return b.peer.Caller
}

func (b *PeerBackend) Prepare(ctx context.Context, _ json.RawMessage) error {
	return b.prepare(ctx)
}

func (b *PeerBackend) prepare(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, old, observer, peer := b.identity.SessionID, b.identity, b.observer, b.peer
	rows, err := rosterRows(ctx, observer)
	if err != nil {
		return err
	}
	row, err := observedRoster(rows, id)
	if err != nil {
		return err
	}
	name := first(row.Title, row.SessionID)
	info := map[string]any{"cwd": row.Cwd}
	if row.SessionID == id && name == old.Name && row.Cwd == old.Info["cwd"] {
		return nil
	}
	next := sessionkit.PeerIdentity{Product: "grok", SessionID: row.SessionID, Name: name, Groups: old.Groups, Info: info}
	if row.SessionID != id {
		err = peer.Replace(ctx, next)
	} else {
		err = peer.Rehello(name, info)
	}
	if err != nil {
		return err
	}
	b.identity = next
	return nil
}

func (b *PeerBackend) watchRoster(ctx context.Context, observer *acpClient, changed <-chan struct{}) {
	for {
		select {
		case <-changed:
			if err := b.prepare(ctx); err != nil {
				select {
				case b.failed <- err:
				default:
				}
				return
			}
		case <-observer.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (b *PeerBackend) deliver(ctx context.Context, identity sessionkit.PeerIdentity, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	b.mu.Lock()
	observer := b.observer
	b.mu.Unlock()
	row, err := roster(ctx, observer, identity.SessionID)
	if err != nil {
		if errors.Is(err, errNoLeader) {
			return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "no_leader"}, nil
		}
		return sessionkit.DeliveryReceipt{}, err
	}
	message, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	if err = observer.interject(ctx, identity.SessionID, request.MessageID, message); err != nil {
		return sessionkit.DeliveryReceipt{}, fmt.Errorf("Grok interject: %w", err)
	}
	if row.Activity == "working" {
		return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
	}
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func roster(ctx context.Context, observer *acpClient, id string) (peerSession, error) {
	sessions, err := rosterRows(ctx, observer)
	if err != nil {
		return peerSession{}, err
	}
	return exactRoster(sessions, id)
}

func rosterRows(ctx context.Context, observer *acpClient) ([]peerSession, error) {
	if observer == nil {
		return nil, errors.New("Grok observer is unavailable")
	}
	var reply struct {
		Result struct {
			Sessions []peerSession `json:"sessions"`
		} `json:"result"`
	}
	if err := observer.request(ctx, "_x.ai/sessions/list", map[string]any{}, &reply); err != nil {
		return nil, err
	}
	return reply.Result.Sessions, nil
}

func exactRoster(sessions []peerSession, id string) (peerSession, error) {
	var found peerSession
	matches := 0
	for _, session := range sessions {
		if session.SessionID == id {
			matches++
			found = session
		}
	}
	if matches == 0 {
		return peerSession{}, errNoLeader
	}
	if matches != 1 {
		return peerSession{}, fmt.Errorf("Grok roster returned %d exact rows for %s", matches, id)
	}
	if found.Resident == nil {
		return peerSession{}, errors.New("exact Grok session roster row has no resident state")
	}
	if !*found.Resident || slices.Contains([]string{"completed", "dormant", "dead"}, found.Activity) {
		return peerSession{}, fmt.Errorf("%w: %s", errRosterActorGone, id)
	}
	if found.Yolo == nil {
		return peerSession{}, errors.New("live Grok roster row has no yolo state")
	}
	if !slices.Contains([]string{"working", "needs_input", "idle"}, found.Activity) {
		return peerSession{}, fmt.Errorf("live Grok roster row has unsupported activity %q", found.Activity)
	}
	return found, nil
}

func observedRoster(sessions []peerSession, id string) (peerSession, error) {
	current, currentErr := exactRoster(sessions, id)
	if currentErr == nil {
		return current, nil
	}
	if !errors.Is(currentErr, errNoLeader) && !errors.Is(currentErr, errRosterActorGone) {
		return peerSession{}, currentErr
	}
	var next []peerSession
	seen := map[string]bool{id: true}
	for _, session := range sessions {
		if session.SessionID == "" || seen[session.SessionID] {
			continue
		}
		seen[session.SessionID] = true
		if candidate, err := exactRoster(sessions, session.SessionID); err == nil {
			next = append(next, candidate)
		}
	}
	if len(next) != 1 {
		return peerSession{}, currentErr
	}
	return next[0], nil
}

func (b *PeerBackend) Shutdown() {
	b.mu.Lock()
	peer, observer, process, leader := b.peer, b.observer, b.process, b.leader
	b.peer, b.observer, b.process, b.leader = nil, nil, nil, nil
	b.mu.Unlock()
	if peer != nil {
		peer.Shutdown()
	}
	if observer != nil {
		observer.close()
	}
	if process != nil {
		stopPeerProcess(process)
	}
	_ = closeNative("leader", leader)
}

func stopPeerProcess(process *nativeProcess) {
	_ = syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
	<-process.done
}

func InteractivePlan(arguments, environment []string) (host.ExecPlan, error) {
	if grokPassthrough(arguments) {
		return host.ExecPlan{Path: "grok", Args: slices.Clone(arguments), Env: slices.Clone(environment)}, nil
	}
	if argument := grokHeadless(arguments); argument != "" {
		return host.ExecPlan{}, fmt.Errorf("%s is headless; use an Agentbus Grok lane", argument)
	}
	id, err := interactiveIdentity(arguments)
	if err != nil {
		return host.ExecPlan{}, err
	}
	if id == "" {
		id, err = newSessionID()
		if err != nil {
			return host.ExecPlan{}, err
		}
		arguments = append([]string{"--session-id", id}, arguments...)
	}
	index := slices.Index(arguments, "--")
	if index < 0 {
		index = len(arguments)
	}
	arguments = slices.Insert(arguments, index, "--leader")
	if !slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		environment = append(environment, host.SocketEnv+"="+sessionkit.Socket())
	}
	return host.InteractivePlan("grok", arguments, environment, host.PeerIdentity{SessionID: id, Name: id}, grokOptionTakesValue)
}

func interactiveCwd(arguments []string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if value := nativeOption(arguments, "--cwd"); value != "" {
		cwd, err = filepath.Abs(value)
	}
	return cwd, err
}

func interactivePermission(arguments []string) string {
	if nativeOption(arguments, "--always-approve") != "" {
		return "bypassPermissions"
	}
	return first(nativeOption(arguments, "--permission-mode"), "default")
}

func insertBeforeSeparator(arguments []string, values ...string) []string {
	result := slices.Clone(arguments)
	index := slices.Index(result, "--")
	if index < 0 {
		index = len(result)
	}
	return slices.Insert(result, index, values...)
}

func nativeOption(arguments []string, target string) string {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		name, value, attached := strings.Cut(argument, "=")
		if name == target {
			if target == "--always-approve" {
				return "true"
			}
			if attached {
				return value
			}
			if index+1 < len(arguments) {
				return arguments[index+1]
			}
		}
		if !attached && grokOptionTakesValue(argument) {
			index++
		}
	}
	return ""
}

func grokHeadless(arguments []string) string {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		name, _, attached := strings.Cut(argument, "=")
		if slices.Contains([]string{"-p", "--single", "--prompt-file", "--prompt-json", "--output-format", "--json-schema", "--max-turns", "--include-partial-messages"}, name) || strings.HasPrefix(argument, "-p") && !strings.HasPrefix(argument, "--") {
			return name
		}
		if !attached && grokOptionTakesValue(argument) {
			index++
		}
	}
	return ""
}

func interactiveIdentity(arguments []string) (string, error) {
	identity := ""
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		if argument == "--leader" || strings.HasPrefix(argument, "--leader=") || argument == "--no-leader" || strings.HasPrefix(argument, "--no-leader=") || strings.HasPrefix(argument, "--leader-socket") {
			return "", errors.New("grok-peer requires the default leader")
		}
		if argument == "--continue" || strings.HasPrefix(argument, "--continue=") || argument == "-c" || argument == "--fork-session" || strings.HasPrefix(argument, "--fork-session=") {
			return "", fmt.Errorf("%s cannot identify an exact managed Grok session", argument)
		}
		name, value, attached := strings.Cut(argument, "=")
		resume := name == "--resume" || name == "--load" || name == "-r"
		fresh := name == "--session-id" || name == "-s"
		if !attached && strings.HasPrefix(argument, "-r") && argument != "-r" {
			resume, value, attached = true, strings.TrimPrefix(argument, "-r"), true
		}
		if !attached && strings.HasPrefix(argument, "-s") && argument != "-s" {
			fresh, value, attached = true, strings.TrimPrefix(argument, "-s"), true
		}
		if resume || fresh {
			if !attached {
				if index+1 == len(arguments) || strings.HasPrefix(arguments[index+1], "-") {
					return "", errors.New(argument + " requires a value")
				}
				index++
				value = arguments[index]
			}
			if strings.TrimSpace(value) == "" {
				return "", errors.New(argument + " requires a value")
			}
			if identity != "" {
				return "", errors.New("Grok session identity was specified more than once")
			}
			identity = value
			continue
		}
		if grokOptionTakesValue(argument) {
			index++
		}
	}
	return identity, nil
}

var grokCommands = map[string]bool{
	"agent": true, "clone": true, "completions": true, "dashboard": true,
	"doctor": true, "du": true, "disk-usage": true, "export": true,
	"help": true, "inspect": true, "leader": true, "login": true,
	"logout": true, "mcp": true, "memory": true, "models": true,
	"plugin": true, "sessions": true, "setup": true, "trace": true,
	"update": true, "version": true, "v": true, "worktree": true,
	"wrap": true,
}

func grokPassthrough(arguments []string) bool {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			return true
		}
		if !strings.HasPrefix(argument, "-") {
			return grokCommands[argument]
		}
		if grokOptionTakesValue(argument) {
			index++
		}
	}
	return false
}

func grokOptionTakesValue(argument string) bool {
	name, _, attached := strings.Cut(argument, "=")
	if attached {
		return false
	}
	return slices.Contains([]string{"-g", "--group", "--agent", "--agents", "--allow", "--cwd", "--debug-file", "--deny", "--disallowed-tools", "--json-schema", "--leader-socket", "-m", "--model", "--max-turns", "--output-format", "--permission-mode", "--prompt-file", "--prompt-json", "-r", "--resume", "--load", "--reasoning-effort", "--effort", "--rules", "-s", "--session-id", "--sandbox", "--system-prompt-override", "--tools", "--worktree-ref", "--ref"}, name)
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
