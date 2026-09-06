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
	"syscall"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/wrappers/host"
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

const grokSessionIDEnv = "GROK_SESSION_ID"
const grokLeaderSocketEnv = "GROK_LEADER_SOCKET"

type PeerBackend struct {
	peer       *sessionkit.Peer
	identity   sessionkit.PeerIdentity
	leader     string
	cwd        string
	deliveries chan peerDelivery
	cancel     context.CancelFunc
	done       chan struct{}
}

type peerDelivery struct {
	ctx      context.Context
	identity sessionkit.PeerIdentity
	request  sessionkit.DeliveryRequest
	reply    chan peerDeliveryResult
}

type peerDeliveryResult struct {
	receipt sessionkit.DeliveryReceipt
	err     error
}

func startPeerClient(lifetimeCtx, requestCtx context.Context, leaderPath, cwd string, notify func(acpFrame)) (*acpClient, *nativeProcess, error) {
	cmd := command("grok", "--no-auto-update", "--permission-mode", "default", "--leader-socket", leaderPath, "agent", "--leader", "stdio")
	cmd.Dir, cmd.Env, cmd.Stderr, cmd.SysProcAttr = cwd, nativeEnvironment(), os.Stderr, &syscall.SysProcAttr{Setpgid: true}
	return startObserverClient(lifetimeCtx, requestCtx, cmd, notify)
}

func NewPeerBackend(ctx context.Context, environment []string) (*PeerBackend, error) {
	id := environmentValue(environment, grokSessionIDEnv)
	leader := environmentValue(environment, grokLeaderSocketEnv)
	if id == "" || leader == "" {
		return nil, errors.New("Grok peer identity is unavailable; start Grok with grok-peer")
	}
	groups := []string{}
	if raw := environmentValue(environment, host.GroupsEnv); raw != "" && json.Unmarshal([]byte(raw), &groups) != nil {
		return nil, errors.New("SESSIONBUS_GROUPS must be a JSON array")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	deliveryCtx, cancel := context.WithCancel(ctx)
	b := &PeerBackend{
		identity:   sessionkit.PeerIdentity{Product: "grok", SessionID: id, Name: id, Groups: groups, Info: map[string]any{"cwd": cwd}},
		leader:     leader,
		cwd:        cwd,
		deliveries: make(chan peerDelivery),
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	peer, err := sessionkit.ConnectPeer(b.identity, b.deliver)
	if err != nil {
		cancel()
		return nil, err
	}
	b.peer = peer
	go b.serveDeliveries(deliveryCtx)
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
	cwd, err := interactiveCwd(plan.Args)
	if err != nil {
		return err
	}
	leaderPath := leaderSocket(socket, key)
	defer os.Remove(leaderPath)
	defer os.Remove(strings.TrimSuffix(leaderPath, ".sock") + ".lock")
	leader, err := startLeader(socket, key, cwd, interactivePermission(plan.Args), peerNativeEnvironment(plan.Env))
	if err != nil {
		return err
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	hold, holdProcess, err := startPeerClient(childCtx, childCtx, leaderPath, cwd, nil)
	if err != nil {
		return errors.Join(err, closeNative("leader", leader))
	}
	plan.Args = insertBeforeSeparator(plan.Args, "--leader-socket", leaderPath)
	child := command(plan.Path, plan.Args...)
	child.Env, child.Stdin, child.Stdout, child.Stderr = plan.Env, os.Stdin, os.Stdout, os.Stderr
	if err = child.Start(); err != nil {
		stopPeerClient(hold, holdProcess)
		return errors.Join(err, closeNative("leader", leader))
	}
	childDone := make(chan error, 1)
	go func() {
		childDone <- child.Wait()
		close(childDone)
		cancel()
	}()
	finish := func(childErr error) error {
		stopPeerClient(hold, holdProcess)
		return errors.Join(childErr, closeNative("leader", leader))
	}
	select {
	case childErr := <-childDone:
		return finish(childErr)
	case <-ctx.Done():
		return finish(finishInteractiveChild(child, childDone, interactiveSignal(ctx)))
	case <-hold.done:
		childErr := finishInteractiveChild(child, childDone, syscall.SIGTERM)
		return finish(errors.Join(fmt.Errorf("Grok startup hold closed: %w", hold.err), childErr))
	}
}

func peerNativeEnvironment(environment []string) []string {
	result := nativeEnvironment()
	for _, name := range []string{host.SocketEnv, host.GroupsEnv} {
		if value := environmentValue(environment, name); value != "" {
			result = setEnvironment(result, name, value)
		}
	}
	return result
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
		return sessionkit.PeerIdentity{}, "", errors.New("SESSIONBUS_GROUPS must be a JSON array")
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
	return b.peer.Call(ctx, method, params)
}

func (b *PeerBackend) Caller() *sessionkit.Caller { return b.peer.Caller }

func (b *PeerBackend) Prepare(context.Context, json.RawMessage) error { return b.peer.Err() }

func (b *PeerBackend) deliver(ctx context.Context, identity sessionkit.PeerIdentity, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	delivery := peerDelivery{ctx: ctx, identity: identity, request: request, reply: make(chan peerDeliveryResult, 1)}
	select {
	case b.deliveries <- delivery:
	case <-ctx.Done():
		return sessionkit.DeliveryReceipt{}, ctx.Err()
	case <-b.done:
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "shutting down"}, nil
	}
	select {
	case result := <-delivery.reply:
		return result.receipt, result.err
	case <-ctx.Done():
		return sessionkit.DeliveryReceipt{}, ctx.Err()
	case <-b.done:
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "shutting down"}, nil
	}
}

func (b *PeerBackend) serveDeliveries(ctx context.Context) {
	defer close(b.done)
	var observer *acpClient
	var process *nativeProcess
	defer func() { stopPeerClient(observer, process) }()
	for {
		select {
		case delivery := <-b.deliveries:
			if observer == nil {
				var err error
				observer, process, err = startPeerClient(ctx, delivery.ctx, b.leader, b.cwd, nil)
				if err != nil {
					delivery.reply <- peerDeliveryResult{err: fmt.Errorf("start Grok observer: %w", err)}
					observer, process = nil, nil
					continue
				}
			}
			receipt, err := b.deliverOne(delivery.ctx, observer, delivery)
			if ctx.Err() != nil {
				receipt, err = sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "shutting down"}, nil
			}
			delivery.reply <- peerDeliveryResult{receipt: receipt, err: err}
		case <-ctx.Done():
			for {
				select {
				case delivery := <-b.deliveries:
					delivery.reply <- peerDeliveryResult{receipt: sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "shutting down"}}
				default:
					return
				}
			}
		}
	}
}

func (b *PeerBackend) deliverOne(ctx context.Context, observer *acpClient, delivery peerDelivery) (sessionkit.DeliveryReceipt, error) {
	row, err := roster(ctx, observer, delivery.identity.SessionID)
	if err == nil {
		name := first(row.Title, row.SessionID)
		info := map[string]any{"cwd": row.Cwd}
		err = b.peer.Rehello(ctx, name, info)
	}
	if errors.Is(err, errNoLeader) {
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "no_leader"}, nil
	}
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	message, err := host.RenderNativeMessage(delivery.request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	if err = observer.interject(ctx, delivery.identity.SessionID, delivery.request.MessageID, message); err != nil {
		return sessionkit.DeliveryReceipt{}, fmt.Errorf("Grok interject: %w", err)
	}
	return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
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

func (b *PeerBackend) Shutdown() {
	b.cancel()
	<-b.done
	b.peer.Shutdown()
}

func stopPeerClient(client *acpClient, process *nativeProcess) {
	if client != nil {
		client.close()
	}
	stopAux(process)
}

func InteractivePlan(arguments, environment []string) (host.ExecPlan, error) {
	if grokPassthrough(arguments) {
		return host.ExecPlan{Path: "grok", Args: slices.Clone(arguments), Env: slices.Clone(environment)}, nil
	}
	if argument := grokHeadless(arguments); argument != "" {
		return host.ExecPlan{}, fmt.Errorf("%s is headless; use a Sessionbus Grok lane", argument)
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
	mode := nativeOption(arguments, "--permission-mode")
	if nativeOption(arguments, "--always-approve") != "" || nativeOption(arguments, "--yolo") != "" || mode == "always-approve" {
		return "bypassPermissions"
	}
	return first(mode, "default")
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
			if target == "--always-approve" || target == "--yolo" {
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
