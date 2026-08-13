package federator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// AgentOptions configures one host agent and its local Claude registry.
type AgentOptions struct {
	Hub                  string
	HostID               string
	HostName             string
	ClaudeConfigDir      string
	RuntimeDir           string
	Executable           string
	ScanInterval         time.Duration
	HeartbeatInterval    time.Duration
	HeartbeatTimeout     time.Duration
	EnableRemoteLanes    bool
	CodexLaneExecutable  string
	ClaudeLaneExecutable string
	Logger               *log.Logger
}

type agent struct {
	options     AgentOptions
	logger      *log.Logger
	registryDir string
	controlPath string

	mu           sync.RWMutex
	local        map[string]localPeer
	remote       map[string]Peer
	remoteHosts  map[string]Host
	network      *wireConn
	localChanged chan struct{}

	laneMu       sync.Mutex
	pendingLanes map[string]*pendingLane
	laneRuns     map[string]*laneRun

	shadowMu sync.Mutex
	shadows  map[string]*shadowHandle
}

type shadowHandle struct {
	peer         Peer
	pid          int
	socket       string
	registryPath string
	process      *os.Process
	done         chan struct{}
}

// RunAgent connects one local Claude registry to a federation hub until ctx is canceled.
//
//nolint:gocyclo // Startup validation remains linear so each operator error is explicit.
func RunAgent(ctx context.Context, options AgentOptions) error {
	if options.Hub == "" {
		return errors.New("agent hub address is required")
	}
	options.HostID = cleanID(options.HostID)
	if options.HostID == "" {
		return errors.New("agent host id is required")
	}
	if options.HostName == "" {
		options.HostName = options.HostID
	}
	if options.ClaudeConfigDir == "" {
		options.ClaudeConfigDir = DefaultClaudeConfigDir()
	}
	if options.RuntimeDir == "" {
		options.RuntimeDir = DefaultRuntimeDir()
	}
	if options.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		options.Executable = executable
	}
	if options.ScanInterval <= 0 {
		options.ScanInterval = time.Second
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = 5 * time.Second
	}
	if options.HeartbeatTimeout <= 0 {
		options.HeartbeatTimeout = 20 * time.Second
	}
	if err := configureLaneExecutables(&options); err != nil {
		return err
	}
	if err := ensureDir(options.RuntimeDir); err != nil {
		return err
	}
	instanceLock, err := acquireAgentInstanceLock(options.RuntimeDir)
	if err != nil {
		return err
	}
	defer releaseAgentInstanceLock(instanceLock)
	registryDir := filepath.Join(options.ClaudeConfigDir, "sessions")
	if err := ensureDir(registryDir); err != nil {
		return err
	}
	registryLock, err := acquireAgentRegistryLock(options.ClaudeConfigDir)
	if err != nil {
		return err
	}
	defer releaseAgentInstanceLock(registryLock)
	agent := &agent{
		options: options, logger: defaultLogger(options.Logger), registryDir: registryDir,
		controlPath: filepath.Join(options.RuntimeDir, "agent.sock"),
		local:       map[string]localPeer{}, remote: map[string]Peer{}, remoteHosts: map[string]Host{}, shadows: map[string]*shadowHandle{},
		pendingLanes: map[string]*pendingLane{}, laneRuns: map[string]*laneRun{},
		localChanged: make(chan struct{}, 1),
	}
	return agent.run(ctx)
}

func configureLaneExecutables(options *AgentOptions) error {
	if !options.EnableRemoteLanes {
		options.CodexLaneExecutable = ""
		options.ClaudeLaneExecutable = ""
		return nil
	}
	codexConfigured := options.CodexLaneExecutable
	claudeConfigured := options.ClaudeLaneExecutable
	options.CodexLaneExecutable = resolveLaneExecutable(codexConfigured, "codex-peer-lane")
	options.ClaudeLaneExecutable = resolveLaneExecutable(claudeConfigured, "claude-peer-lane")
	if codexConfigured != "" && options.CodexLaneExecutable == "" {
		return fmt.Errorf("configured codex lane launcher %q is not executable", codexConfigured)
	}
	if claudeConfigured != "" && options.ClaudeLaneExecutable == "" {
		return fmt.Errorf("configured Claude lane launcher %q is not executable", claudeConfigured)
	}
	return nil
}

func (a *agent) run(ctx context.Context) error {
	listener, err := a.startControlListener()
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(a.controlPath)
		a.setNetwork(nil)
		a.clearRemotePeers()
	}()
	go a.controlLoop(ctx, listener)
	if err := a.refreshLocal(); err != nil {
		a.logger.Printf("local discovery failed: %v", err)
	}
	go a.localDiscoveryLoop(ctx)
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		err = a.runHubSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		a.logger.Printf("hub session ended: %v", err)
		a.setNetwork(nil)
		a.clearRemotePeers()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
}

func (a *agent) runHubSession(ctx context.Context) error {
	conn, err := net.DialTimeout("tcp", a.options.Hub, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	wire := newWireConn(conn)
	if err := wire.Send(Message{
		Type: "hello", Version: ProtocolVersion, HostID: a.options.HostID, HostName: a.options.HostName,
		Capabilities: a.laneCapabilities(),
	}); err != nil {
		return err
	}
	a.setNetwork(wire)
	previous := a.localSnapshot()
	if err := wire.Send(Message{Type: "snapshot", Peers: previous}); err != nil {
		return err
	}
	a.logger.Printf("connected to hub %s as %s", a.options.Hub, a.options.HostID)
	readErr := make(chan error, 1)
	var lastHubActivity atomic.Int64
	lastHubActivity.Store(time.Now().UnixNano())
	go func() {
		readErr <- scanMessages(conn, func(message Message) error {
			lastHubActivity.Store(time.Now().UnixNano())
			return a.handleHubMessage(message)
		})
	}()
	scanTicker := time.NewTicker(a.options.ScanInterval)
	pingTicker := time.NewTicker(a.options.HeartbeatInterval)
	defer scanTicker.Stop()
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case <-a.localChanged:
			current := a.localSnapshot()
			if !reflect.DeepEqual(previous, current) {
				if err := wire.Send(Message{Type: "snapshot", Peers: current}); err != nil {
					return err
				}
				previous = current
			}
		case <-scanTicker.C:
			a.reconcileShadows()
		case <-pingTicker.C:
			lastActivity := time.Unix(0, lastHubActivity.Load())
			if time.Since(lastActivity) > a.options.HeartbeatTimeout {
				return errors.New("hub heartbeat timed out")
			}
			if err := wire.Send(Message{Type: "ping"}); err != nil {
				return err
			}
		}
	}
}

func (a *agent) handleHubMessage(message Message) error {
	switch message.Type {
	case "hello_ok", "pong":
		return nil
	case "roster":
		remote := map[string]Peer{}
		for _, peer := range message.Peers {
			if peer.HostID != a.options.HostID {
				remote[peer.ID] = peer
			}
		}
		a.mu.Lock()
		a.remote = remote
		a.remoteHosts = make(map[string]Host, len(message.Hosts))
		for _, host := range message.Hosts {
			if host.ID != a.options.HostID {
				a.remoteHosts[host.ID] = host
			}
		}
		a.mu.Unlock()
		a.reconcileShadows()
		return nil
	case "deliver":
		if err := a.deliverLocal(message); err != nil {
			a.logger.Printf("delivery %s -> %s failed: %v", message.SourceID, message.TargetID, err)
		}
		return nil
	case "delivery_error":
		a.logger.Printf("delivery %s -> %s failed: %s", message.SourceID, message.TargetID, message.Error)
		return nil
	case "lane_exec":
		a.startRemoteLane(message)
		return nil
	case "lane_cancel":
		a.cancelRemoteLane(message.RequestID)
		return nil
	case "lane_stdout", "lane_stderr", "lane_exit", "lane_error":
		a.deliverLaneResponse(message)
		return nil
	case "error":
		return errors.New(message.Error)
	default:
		return fmt.Errorf("unsupported hub frame %q", message.Type)
	}
}

func (a *agent) startControlListener() (net.Listener, error) {
	_ = os.Remove(a.controlPath)
	listener, err := net.Listen("unix", a.controlPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(a.controlPath, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func (a *agent) controlLoop(ctx context.Context, listener net.Listener) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go a.handleControl(conn)
	}
}

func (a *agent) handleControl(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxWireBytes)
	response := Message{Type: "error", Error: "invalid agent control request"}
	if scanner.Scan() {
		var message Message
		if json.Unmarshal(scanner.Bytes(), &message) == nil {
			if message.Type == "lane_exec" {
				_ = conn.SetDeadline(time.Time{})
				a.handleLaneControl(conn, message)
				return
			}
			switch message.Type {
			case "shadow_deliver":
				if err := a.forwardShadowFrame(message.TargetID, message.Frame); err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "accepted"}
				}
			case "status":
				body, err := json.Marshal(a.status())
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "status", Frame: body}
				}
			case "hosts":
				if hosts, ok := a.remoteHostSnapshot(); ok {
					response = Message{Type: "hosts", Hosts: hosts}
				} else {
					response.Error = "hub is disconnected"
				}
			}
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = newWireConn(conn).Send(response)
}

func (a *agent) status() AgentStatus {
	a.mu.RLock()
	status := AgentStatus{
		RuntimeVersion:  RuntimeVersion,
		ProtocolVersion: ProtocolVersion,
		HostID:          a.options.HostID,
		HostName:        a.options.HostName,
		Hub:             a.options.Hub,
		Connected:       a.network != nil,
		LocalPeers:      len(a.local),
		RemotePeers:     len(a.remote),
		RemoteHosts:     len(a.remoteHosts),
		Capabilities:    a.laneCapabilities(),
		RegistryDir:     a.registryDir,
		RuntimeDir:      a.options.RuntimeDir,
	}
	a.mu.RUnlock()
	a.shadowMu.Lock()
	status.Shadows = len(a.shadows)
	a.shadowMu.Unlock()
	return status
}

func (a *agent) forwardShadowFrame(targetID string, frame json.RawMessage) error {
	if err := a.refreshLocal(); err != nil {
		return err
	}
	a.mu.RLock()
	sourceID, err := sourcePeerID(frame, a.local)
	wire := a.network
	_, targetExists := a.remote[targetID]
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	if wire == nil {
		return errors.New("hub is disconnected")
	}
	if !targetExists {
		return errors.New("remote target is no longer live")
	}
	return wire.Send(Message{Type: "deliver", SourceID: sourceID, TargetID: targetID, Frame: frame})
}

func (a *agent) deliverLocal(message Message) error {
	if err := a.refreshLocal(); err != nil {
		return err
	}
	a.mu.RLock()
	target, targetExists := a.local[message.TargetID]
	source, sourceExists := a.remote[message.SourceID]
	a.mu.RUnlock()
	if !targetExists {
		return fmt.Errorf("local target %s is no longer live", message.TargetID)
	}
	if !sourceExists {
		return fmt.Errorf("remote source %s is not in the roster", message.SourceID)
	}
	a.shadowMu.Lock()
	shadow := a.shadows[message.SourceID]
	a.shadowMu.Unlock()
	if shadow == nil || !processLive(shadow.pid) {
		a.reconcileShadows()
		a.shadowMu.Lock()
		shadow = a.shadows[message.SourceID]
		a.shadowMu.Unlock()
	}
	if shadow == nil {
		return fmt.Errorf("remote source %s has no local shadow", message.SourceID)
	}
	rewritten, err := rewriteInboundFrame(message.Frame, target, source, shadow.socket)
	if err != nil {
		return err
	}
	return sendUnixFrame(target.Socket, rewritten, 5*time.Second)
}

func (a *agent) refreshLocal() error {
	local, err := discoverLocalPeers(a.registryDir, a.options.HostID, a.options.HostName)
	if err != nil {
		return err
	}
	a.mu.Lock()
	changed := !reflect.DeepEqual(a.local, local)
	a.local = local
	a.mu.Unlock()
	if changed {
		select {
		case a.localChanged <- struct{}{}:
		default:
		}
	}
	return nil
}

func (a *agent) localDiscoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(a.options.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.refreshLocal(); err != nil {
				a.logger.Printf("local discovery failed: %v", err)
			}
		}
	}
}

func (a *agent) localSnapshot() []Peer {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return peersFromLocal(a.local)
}

func (a *agent) setNetwork(wire *wireConn) {
	a.mu.Lock()
	a.network = wire
	a.mu.Unlock()
	if wire == nil {
		a.failPendingLanes("hub is disconnected")
		a.cancelAllRemoteLanes()
	}
}

func (a *agent) clearRemotePeers() {
	a.mu.Lock()
	a.remote = map[string]Peer{}
	a.remoteHosts = map[string]Host{}
	a.mu.Unlock()
	a.reconcileShadows()
}

func (a *agent) reconcileShadows() {
	a.mu.RLock()
	remote := make(map[string]Peer, len(a.remote))
	for id, peer := range a.remote {
		remote[id] = peer
	}
	a.mu.RUnlock()
	a.shadowMu.Lock()
	defer a.shadowMu.Unlock()
	for id, shadow := range a.shadows {
		peer, exists := remote[id]
		if !exists || !processLive(shadow.pid) {
			a.stopShadowLocked(id, shadow)
			continue
		}
		if !reflect.DeepEqual(shadow.peer, peer) {
			path, err := writeShadowRecord(a.registryDir, shadow.pid, shadow.socket, peer)
			if err != nil {
				a.logger.Printf("update shadow %s failed: %v", id, err)
				continue
			}
			shadow.peer = peer
			shadow.registryPath = path
		}
	}
	ids := make([]string, 0, len(remote))
	for id := range remote {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, exists := a.shadows[id]; exists {
			continue
		}
		shadow, err := a.startShadowLocked(remote[id])
		if err != nil {
			a.logger.Printf("start shadow %s failed: %v", id, err)
			continue
		}
		a.shadows[id] = shadow
	}
}

func (a *agent) startShadowLocked(peer Peer) (*shadowHandle, error) {
	shadowID := defaultString(peer.InstanceID, peer.ID)
	socket := filepath.Join(a.options.RuntimeDir, "shadows", sessionKey(shadowID)+".sock")
	if err := ensureDir(filepath.Dir(socket)); err != nil {
		return nil, err
	}
	// #nosec G204 -- the executable is the current installed binary and every argument is explicit.
	cmd := exec.Command(a.options.Executable,
		"shadow", "--listen", socket, "--control", a.controlPath,
		"--target", peer.ID, "--owner-pid", strconv.Itoa(os.Getpid()),
		"--registry-dir", a.registryDir,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	handle := &shadowHandle{
		peer: peer, pid: cmd.Process.Pid, socket: socket, process: cmd.Process, done: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(handle.done)
	}()
	if !waitFor(func() bool { return probeUnix(socket, 100*time.Millisecond) }, 3*time.Second) {
		stopProcess(cmd.Process, time.Second)
		return nil, errors.New("shadow socket did not become ready")
	}
	path, err := writeShadowRecord(a.registryDir, handle.pid, socket, peer)
	if err != nil {
		stopProcess(cmd.Process, time.Second)
		return nil, err
	}
	handle.registryPath = path
	a.logger.Printf("published %s as %s (pid %d)", peer.ID, peer.DisplayName, handle.pid)
	return handle, nil
}

func (a *agent) stopShadowLocked(id string, shadow *shadowHandle) {
	delete(a.shadows, id)
	if shadow.registryPath != "" {
		_ = os.Remove(shadow.registryPath)
	}
	if shadow.process != nil {
		_ = shadow.process.Signal(os.Interrupt)
		select {
		case <-shadow.done:
		case <-time.After(time.Second):
			_ = shadow.process.Kill()
			<-shadow.done
		}
	}
	_ = os.Remove(shadow.socket)
	a.logger.Printf("removed shadow %s", id)
}

func sendUnixFrame(socket string, frame json.RawMessage, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	_, err = conn.Write(append(append([]byte(nil), frame...), '\n'))
	return err
}

// DefaultClaudeConfigDir returns CLAUDE_CONFIG_DIR or the standard per-user location.
func DefaultClaudeConfigDir() string {
	if value := os.Getenv("CLAUDE_CONFIG_DIR"); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// DefaultRuntimeDir returns the per-user ephemeral directory used by an agent and its shadows.
func DefaultRuntimeDir() string {
	if value := os.Getenv("XDG_RUNTIME_DIR"); value != "" {
		return filepath.Join(value, "peer-federator")
	}
	return filepath.Join(os.TempDir(), "peer-federator-"+strconv.Itoa(os.Getuid()))
}
