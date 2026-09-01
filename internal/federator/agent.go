package federator

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
	"github.com/antst/agent-sessions/internal/socketpath"
)

var evaluateQwenLaneReadiness = func(executable string) error {
	profile, err := qwenprofile.Current()
	if err != nil {
		return err
	}
	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	workspace, err = pathidentity.ExistingDirectory(workspace)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	report, err := qwenreadiness.Check(ctx, qwenreadiness.Request{
		Executable: executable, Workspace: workspace, Profile: profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(os.Environ()),
	})
	if err != nil {
		return err
	}
	if !report.Ready {
		issues := make([]string, 0, len(report.Issues))
		for _, issue := range report.Issues {
			issues = append(issues, issue.Code+": "+issue.Message)
		}
		return fmt.Errorf("qwen lane readiness failed: %s", strings.Join(issues, "; "))
	}
	return nil
}

// AgentOptions configures one host agent and its local Claude registry.
type AgentOptions struct {
	Hub                  string
	HostID               string
	HostName             string
	ClaudeConfigDir      string
	RuntimeDir           string
	StateDir             string
	Executable           string
	ScanInterval         time.Duration
	HeartbeatInterval    time.Duration
	HeartbeatTimeout     time.Duration
	EnableRemoteLanes    bool
	CodexLaneExecutable  string
	ClaudeLaneExecutable string
	GrokLaneExecutable   string
	QwenLaneExecutable   string
	QwenExecutable       string
	Logger               *log.Logger
}

type agent struct {
	options       AgentOptions
	logger        *log.Logger
	registryDir   string
	controlPath   string
	catalog       *sessionCatalog
	serviceRecord string
	serviceKey    string
	serviceToken  string
	claudeProfile claudeprofile.Source
	routeRefresh  func() error

	mu             sync.RWMutex
	preparationMu  sync.Mutex
	local          map[string]localPeer
	retirements    map[string]localPeer
	preparations   map[string]peerPreparation
	preparationDir string
	remote         map[string]Peer
	remoteHosts    map[string]Host
	network        *wireConn
	localChanged   chan struct{}

	laneMu            sync.Mutex
	pendingLanes      map[string]*pendingLane
	laneRuns          map[string]*laneRun
	deliveryMu        sync.Mutex
	pendingDeliveries map[string]chan error
	embedded          *embeddedBackend
}

func preferenceUpdateFromMessage(message Message) SessionPreferenceUpdate {
	return SessionPreferenceUpdate{
		SessionID: message.SessionID, Product: message.Product, Kind: message.SessionKind,
		ExplicitGroups: message.Groups, GroupsSpecified: message.GroupsSpecified,
		ParentSession: message.ParentSessionID, ParentHostID: message.ParentHostID,
		ParentGroups: message.ParentGroups, ParentSpecified: message.ParentSpecified,
		InheritParentGroups: message.InheritParentGroups, InheritGroupsSpecified: message.InheritGroupsSpecified,
		AlwaysApprove: message.AlwaysApprove, AlwaysApproveSpecified: message.AlwaysApproveSpecified,
		Qwen: message.QwenSession,
	}
}

func (a *agent) resolvedClaudeProfile() (claudeprofile.Source, error) {
	if a.claudeProfile.ConfigRoot != "" {
		return a.claudeProfile, nil
	}
	return claudeprofile.SharedSource(a.options.ClaudeConfigDir)
}

// RunAgent connects one local Claude registry to a federation hub until ctx is canceled.
//
//nolint:gocyclo // Startup validation remains linear so each operator error is explicit.
func RunAgent(ctx context.Context, options AgentOptions) error {
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
	if options.StateDir == "" {
		options.StateDir = DefaultStateDir(options.HostID)
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
	claudeProfile, err := claudeprofile.SharedSource(options.ClaudeConfigDir)
	if err != nil {
		return fmt.Errorf("resolve agent Claude profile: %w", err)
	}
	if err := ensureDir(options.RuntimeDir); err != nil {
		return err
	}
	if err := ensureDir(options.StateDir); err != nil {
		return err
	}
	catalog, err := openSessionCatalog(filepath.Join(options.StateDir, "sessions.json"), options.HostID)
	if err != nil {
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
	serviceTokenBytes := make([]byte, 16)
	if _, err := rand.Read(serviceTokenBytes); err != nil {
		return fmt.Errorf("create Claude service peer token: %w", err)
	}
	agent := &agent{
		options: options, logger: defaultLogger(options.Logger), registryDir: registryDir,
		claudeProfile: claudeProfile,
		controlPath:   filepath.Join(options.RuntimeDir, "agent.sock"),
		local:         map[string]localPeer{}, retirements: map[string]localPeer{},
		preparations: map[string]peerPreparation{}, preparationDir: filepath.Join(options.StateDir, "claude-peer-preparations"),
		remote: map[string]Peer{}, remoteHosts: map[string]Host{},
		pendingLanes: map[string]*pendingLane{}, laneRuns: map[string]*laneRun{},
		pendingDeliveries: map[string]chan error{},
		localChanged:      make(chan struct{}, 1),
		catalog:           catalog,
		serviceToken:      hex.EncodeToString(serviceTokenBytes),
	}
	if err := agent.loadPeerPreparations(); err != nil {
		return err
	}
	return agent.run(ctx)
}

func configureLaneExecutables(options *AgentOptions) error {
	if !options.EnableRemoteLanes {
		options.CodexLaneExecutable = ""
		options.ClaudeLaneExecutable = ""
		options.GrokLaneExecutable = ""
		options.QwenLaneExecutable = ""
		options.QwenExecutable = ""
		return nil
	}
	codexConfigured := options.CodexLaneExecutable
	claudeConfigured := options.ClaudeLaneExecutable
	grokConfigured := options.GrokLaneExecutable
	qwenConfigured := options.QwenLaneExecutable
	qwenExecutableConfigured := options.QwenExecutable
	if qwenExecutableConfigured == "" {
		qwenExecutableConfigured = strings.TrimSpace(os.Getenv("QWEN_PEER_QWEN_BIN"))
	}
	bindings := []struct {
		configured string
		fallback   string
		label      string
		target     *string
	}{
		{codexConfigured, "codex-peer-lane", "codex", &options.CodexLaneExecutable},
		{claudeConfigured, "claude-peer-lane", "Claude", &options.ClaudeLaneExecutable},
		{grokConfigured, "grok-peer-lane", "Grok", &options.GrokLaneExecutable},
		{qwenConfigured, "qwen-peer-lane", "Qwen", &options.QwenLaneExecutable},
	}
	for _, binding := range bindings {
		*binding.target = resolveLaneExecutable(binding.configured, binding.fallback)
		if binding.configured != "" && *binding.target == "" {
			return fmt.Errorf("configured %s lane launcher %q is not executable", binding.label, binding.configured)
		}
	}
	options.QwenExecutable = resolveLaneExecutable(qwenExecutableConfigured, "qwen")
	return configureQwenLaneReadiness(options, qwenConfigured, qwenExecutableConfigured)
}

func configureQwenLaneReadiness(options *AgentOptions, configuredLauncher, configuredNative string) error {
	if options.QwenLaneExecutable == "" {
		return nil
	}
	if options.QwenExecutable == "" {
		if configuredLauncher != "" || configuredNative != "" {
			return errors.New("configured Qwen lane launcher has no executable native Qwen client")
		}
		options.QwenLaneExecutable = ""
		return nil
	}
	if err := evaluateQwenLaneReadiness(options.QwenExecutable); err != nil {
		if configuredLauncher != "" || configuredNative != "" {
			return fmt.Errorf("configured Qwen lane launcher is not ready: %w", err)
		}
		options.QwenLaneExecutable = ""
		options.QwenExecutable = ""
	}
	return nil
}

func (a *agent) run(ctx context.Context) error {
	if a.embedded != nil {
		return a.runEmbedded(ctx)
	}
	listener, err := a.startControlListener()
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(a.controlPath)
		a.setNetwork(nil)
		a.clearRemotePeers()
		a.removeServiceRecord()
	}()
	if err := a.publishServiceRecord(); err != nil {
		return err
	}
	go a.controlLoop(ctx, listener)
	if err := a.refreshLocal(); err != nil {
		a.logger.Printf("local discovery failed: %v", err)
	}
	go a.localDiscoveryLoop(ctx)
	if a.options.Hub == "" {
		<-ctx.Done()
		return nil
	}
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
			if err := a.refreshLocal(); err != nil {
				a.logger.Printf("local discovery failed: %v", err)
			}
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

//nolint:gocyclo // Hub protocol variants are intentionally dispatched in one audited switch.
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
		return nil
	case "deliver":
		return errors.New("legacy flat delivery is not supported by protocol v3")
	case "group_deliver":
		err := a.deliverGroupedLocal(message)
		if err != nil {
			a.logger.Printf("grouped delivery %s -> %s failed: %v", message.SourceID, message.TargetID, err)
		}
		a.sendDeliveryOutcome(message, err)
		return nil
	case "terminal_notice_deliver":
		err := a.deliverTerminalNoticeLocal(message)
		if err != nil {
			a.logger.Printf("terminal notice %s -> %s failed: %v", message.SourceID, message.TargetID, err)
		}
		a.sendDeliveryOutcome(message, err)
		return nil
	case "delivery_ack", "delivery_error":
		if message.Type == "delivery_error" {
			a.logger.Printf("delivery %s -> %s failed: %s", message.SourceID, message.TargetID, message.Error)
		}
		a.completePendingDelivery(message)
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
	if err := socketpath.Validate(a.controlPath); err != nil {
		return nil, fmt.Errorf("validate agent control socket: %w", err)
	}
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

func (a *agent) publishServiceRecord() error {
	pid := os.Getpid()
	procStart := processStart(pid)
	if procStart == "" {
		return errors.New("cannot corroborate host agent process identity")
	}
	if err := a.removeStaleServiceRecords(pid); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	record := registryRecord{
		PID: pid, SessionID: "agent-" + sessionKey(a.options.HostID),
		Cwd: a.options.StateDir, Name: "agent-sessions--" + cleanPeerName(a.options.HostName),
		Status: "idle", Entrypoint: "agent-sessions", ProcStart: procStart,
		MessagingSocketPath: a.controlPath, StartedAt: now,
		Version: "agent-sessions/" + RuntimeVersion, PeerProtocol: GroupProtocolVersion,
		Kind: "service", NameSource: "agent", AgentService: true,
		UpdatedAt: now, StatusUpdatedAt: now,
	}
	path := filepath.Join(a.registryDir, strconv.Itoa(pid)+".json")
	keyName, err := ClaudeServiceKeyName(pid, a.controlPath)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(a.registryDir, keyName)
	if err := writeJSONAtomic(keyPath, map[string]string{"peerToken": a.serviceToken, "procStart": procStart}); err != nil {
		return err
	}
	// Publish the discoverable row only after its authentication capability is
	// durable. A key without a row is inert; a row without a key is unusable.
	if err := writeJSONAtomic(path, record); err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	a.serviceRecord = path
	a.serviceKey = keyPath
	return nil
}

func (a *agent) removeStaleServiceRecords(currentPID int) error {
	entries, err := os.ReadDir(a.registryDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := a.removeStaleServiceRecord(entry.Name(), currentPID); err != nil {
			return err
		}
	}
	return nil
}

func (a *agent) removeStaleServiceRecord(name string, currentPID int) error {
	pid := parsePID(name)
	if pid <= 1 || pid == currentPID {
		return nil
	}
	path := filepath.Join(a.registryDir, name)
	record, ok, err := readAgentServiceRecord(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if record.PID != pid || record.Entrypoint != "agent-sessions" || record.ProcStart == "" {
		return fmt.Errorf("invalid stale host-agent service record %s", name)
	}
	if processLive(pid) && processStart(pid) == record.ProcStart {
		return fmt.Errorf("another live host-agent service record owns PID %d", pid)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.New("remove stale host-agent service record failed")
	}
	keyName, err := ClaudeServiceKeyName(pid, record.MessagingSocketPath)
	if err != nil {
		return fmt.Errorf("invalid stale host-agent service key identity %s", name)
	}
	if err := os.Remove(filepath.Join(a.registryDir, keyName)); err != nil && !os.IsNotExist(err) {
		return errors.New("remove stale host-agent service key failed")
	}
	return nil
}

func readAgentServiceRecord(path string) (registryRecord, bool, error) {
	body, err := os.ReadFile(path) //nolint:gosec // exact configured registry entry.
	if os.IsNotExist(err) {
		return registryRecord{}, false, nil
	}
	if err != nil {
		return registryRecord{}, false, err
	}
	if !json.Valid(body) {
		return registryRecord{}, false, nil
	}
	var record registryRecord
	_ = json.Unmarshal(body, &record)
	if !record.AgentService {
		return registryRecord{}, false, nil
	}
	return record, true, nil
}

func (a *agent) removeServiceRecord() {
	if a.serviceRecord == "" {
		return
	}
	body, err := os.ReadFile(a.serviceRecord)
	if err != nil {
		return
	}
	var record registryRecord
	if json.Unmarshal(body, &record) == nil && record.AgentService && record.PID == os.Getpid() {
		_ = os.Remove(a.serviceRecord)
		if a.serviceKey != "" {
			_ = os.Remove(a.serviceKey)
		}
	}
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

//nolint:gocyclo // One framed control dispatcher keeps protocol replies uniform.
func (a *agent) handleControl(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxWireBytes)
	response := Message{Type: "error", Error: "invalid agent control request"}
	if scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		authenticatedNativePeer := false
		var auth struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		if json.Unmarshal(line, &auth) == nil && auth.Type == "auth" {
			if subtle.ConstantTimeCompare([]byte(auth.Token), []byte(a.serviceToken)) != 1 {
				return
			}
			if !scanner.Scan() {
				return
			}
			authenticatedNativePeer = true
			line = append([]byte(nil), scanner.Bytes()...)
		}
		var message Message
		if json.Unmarshal(line, &message) == nil {
			if message.Type == "user" {
				if !authenticatedNativePeer {
					return
				}
				if _, err := a.handleNativeCarrierFrame(line); err != nil {
					a.logger.Printf("Claude carrier request failed: %v", err)
				}
				// Claude's native sender writes one frame, half-closes, and treats a
				// clean peer close as its transport acknowledgement. Results travel
				// asynchronously to the registered sender socket.
				for scanner.Scan() {
				}
				return
			}
			if message.Type == "lane_exec" {
				_ = conn.SetDeadline(time.Time{})
				a.handleLaneControl(conn, message)
				return
			}
			switch message.Type {
			case "shadow_deliver":
				response.Error = "legacy shadow delivery is not supported by protocol v3"
			case "status":
				body, err := json.Marshal(a.status())
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "status", Frame: body}
				}
			case "service_record":
				if message.Version != GroupProtocolVersion {
					response.Error = "group protocol is incompatible"
					break
				}
				body, err := os.ReadFile(a.serviceRecord)
				if err != nil {
					response.Error = "host agent service record is unavailable"
				} else {
					response = Message{Type: "service_record", Version: GroupProtocolVersion, Data: body, ServicePeerToken: a.serviceToken}
				}
			case "hosts":
				if hosts, ok := a.remoteHostSnapshot(); ok {
					response = Message{Type: "hosts", Hosts: hosts}
				} else {
					response.Error = "hub is disconnected"
				}
			case "session_preferences", "session_preferences_preview":
				if message.Version != GroupProtocolVersion {
					response.Error = "group protocol is incompatible"
					break
				}
				if err := a.validatePreferenceParentUpdate(message); err != nil {
					response.Error = err.Error()
					break
				}
				update := preferenceUpdateFromMessage(message)
				var preference SessionPreferences
				var groups []string
				var err error
				if message.Type == "session_preferences_preview" {
					preference, groups, err = a.catalog.preview(update)
				} else {
					preference, groups, err = a.catalog.update(update)
				}
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{
						Type: message.Type, Version: GroupProtocolVersion,
						SessionID: message.SessionID, Preference: &preference, Groups: groups,
					}
				}
			case "session_lookup":
				if message.Version != GroupProtocolVersion {
					response.Error = "group protocol is incompatible"
					break
				}
				preference, groups, ok, err := a.catalog.get(message.SessionID)
				switch {
				case err != nil:
					response.Error = err.Error()
				case !ok:
					response.Error = "session is not present in the catalog"
				default:
					name, live := a.sessionProjection(message.SessionID, preference.Product)
					response = Message{
						Type: "session_lookup", Version: GroupProtocolVersion,
						SessionID: message.SessionID, Name: name, Preference: &preference, Groups: groups,
						Peers: live,
					}
				}
			case "session_name_lookup":
				if message.Version != GroupProtocolVersion {
					response.Error = "group protocol is incompatible"
					break
				}
				sessionID, err := a.resolveSessionName(message.Product, message.Name)
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{
						Type: "session_name_lookup", Version: GroupProtocolVersion,
						Product: message.Product, Name: message.Name, SessionID: sessionID,
					}
				}
			case "parent_context":
				if message.Version != GroupProtocolVersion {
					response.Error = "group protocol is incompatible"
					break
				}
				parent, err := a.parentContext(message.SessionID)
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "parent_context", Version: GroupProtocolVersion, ParentContext: &parent}
				}
			case "terminal_notice":
				if message.Version != GroupProtocolVersion {
					response.Error = "group protocol is incompatible"
					break
				}
				result, err := a.routeTerminalNotice(message.SourceSessionID, message.TargetID, message.Frame)
				if err != nil {
					response.Error = err.Error()
				} else {
					body, marshalErr := json.Marshal(result)
					if marshalErr != nil {
						response.Error = marshalErr.Error()
					} else {
						response = Message{Type: "agent_frame_result", Version: GroupProtocolVersion, Frame: body}
					}
				}
			case "peer_register", "peer_update":
				if message.Registration == nil {
					response.Error = "peer registration is required"
					break
				}
				peer, err := a.registerPeer(*message.Registration, message.Type == "peer_update")
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: message.Type, Version: GroupProtocolVersion, Peers: []Peer{peer}}
				}
			case "peer_prepare":
				if message.Registration == nil {
					response.Error = "peer preparation is required"
					break
				}
				if err := a.preparePeer(*message.Registration); err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "peer_prepare", Version: GroupProtocolVersion}
				}
			case "peer_prepare_selection":
				if message.Registration == nil {
					response.Error = "peer preparation is required"
					break
				}
				if err := a.prepareClaudePeerSelection(*message.Registration); err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "peer_prepare_selection", Version: GroupProtocolVersion}
				}
			case "peer_prepare_launch":
				if message.Registration == nil || message.Preference == nil {
					response.Error = "peer preparation and previewed preferences are required"
					break
				}
				if err := a.validatePreferenceParentUpdate(message); err != nil {
					response.Error = err.Error()
					break
				}
				preference, groups, err := a.preparePeerLaunch(
					*message.Registration, preferenceUpdateFromMessage(message), *message.Preference,
				)
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{
						Type: "peer_prepare_launch", Version: GroupProtocolVersion,
						SessionID: message.SessionID, Preference: &preference, Groups: groups,
					}
				}
			case "peer_promote_selection":
				if message.Registration == nil || message.Preference == nil {
					response.Error = "peer preparation and previewed preferences are required"
					break
				}
				if err := a.validatePreferenceParentUpdate(message); err != nil {
					response.Error = err.Error()
					break
				}
				preference, groups, err := a.promoteClaudePeerSelection(
					*message.Registration, preferenceUpdateFromMessage(message), *message.Preference,
				)
				if err != nil {
					response.Error = err.Error()
				} else {
					response = Message{
						Type: "peer_promote_selection", Version: GroupProtocolVersion,
						SessionID: message.SessionID, Preference: &preference, Groups: groups,
					}
				}
			case "peer_prepare_cancel":
				if message.Registration == nil {
					response.Error = "peer preparation is required"
					break
				}
				if err := a.cancelPeerPreparation(*message.Registration); err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "peer_prepare_cancel", Version: GroupProtocolVersion}
				}
			case "peer_unregister":
				if message.Registration == nil {
					response.Error = "peer registration is required"
					break
				}
				if err := a.unregisterPeer(*message.Registration); err != nil {
					response.Error = err.Error()
				} else {
					response = Message{Type: "peer_unregister", Version: GroupProtocolVersion}
				}
			case "agent_frame":
				var frame AgentFrame
				if json.Unmarshal(message.Frame, &frame) != nil {
					response.Error = "invalid agent frame"
					break
				}
				result, err := a.handleAgentFrame(message.SourceSessionID, frame)
				if err != nil {
					response.Error = err.Error()
				} else if body, marshalErr := json.Marshal(result); marshalErr != nil {
					response.Error = marshalErr.Error()
				} else {
					response = Message{Type: "agent_frame_result", Frame: body}
				}
			}
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = newWireConn(conn).Send(response)
}

//nolint:gocyclo // Parent attestation is an ordered fail-closed validation boundary.
func (a *agent) validatePreferenceParentUpdate(message Message) error {
	if !message.ParentSpecified && !message.InheritGroupsSpecified {
		return nil
	}
	parentID := message.ParentSessionID
	if parentID == "" {
		if existing, _, ok, err := a.catalog.get(message.SessionID); err != nil {
			return err
		} else if ok {
			parentID = existing.ParentSession
		}
	}
	if parentID == "" {
		if message.InheritParentGroups {
			return errors.New("cannot inherit groups without a live parent")
		}
		return nil
	}
	if message.ParentHostID != "" && message.ParentHostID != a.options.HostID {
		a.mu.RLock()
		var remoteParent *Peer
		for _, peer := range a.remote {
			if peer.HostID == message.ParentHostID && peer.SessionID == parentID {
				candidate := peer
				remoteParent = &candidate
				break
			}
		}
		a.mu.RUnlock()
		if remoteParent == nil || !reflect.DeepEqual(sortedUnique(message.ParentGroups), sortedUnique(remoteParent.Groups)) {
			return errors.New("remote parent group decision does not match a live federated peer")
		}
		return nil
	}
	if _, err := a.parentContext(parentID); err != nil {
		return fmt.Errorf("parent group decision requires a live registered parent: %w", err)
	}
	return nil
}

func (a *agent) status() AgentStatus {
	claudeProfile, _ := a.resolvedClaudeProfile()
	a.mu.RLock()
	status := AgentStatus{
		RuntimeVersion:       RuntimeVersion,
		ProtocolVersion:      ProtocolVersion,
		HostID:               a.options.HostID,
		HostName:             a.options.HostName,
		Hub:                  a.options.Hub,
		Connected:            a.network != nil,
		LocalPeers:           len(a.local),
		RemotePeers:          len(a.remote),
		RemoteHosts:          len(a.remoteHosts),
		Capabilities:         a.laneCapabilities(),
		RegistryDir:          a.registryDir,
		RuntimeDir:           a.options.RuntimeDir,
		StateDir:             a.options.StateDir,
		ClaudeConfigEnvSet:   claudeProfile.ConfigEnvSet,
		ClaudeConfigEnvValue: claudeProfile.ConfigEnvValue,
		ClaudeSecureConfig:   claudeProfile.SecureConfig,
		ClaudeSecureEnvSet:   claudeProfile.SecureEnvSet,
	}
	a.mu.RUnlock()
	return status
}

func (a *agent) refreshLocal() error {
	if a.embedded != nil {
		return a.refreshEmbeddedLocal()
	}
	a.reconcileRegisteredPeers()
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
		a.failPendingDeliveries("hub is disconnected")
		a.cancelAllRemoteLanes()
	}
}

func (a *agent) sendDeliveryOutcome(request Message, deliveryErr error) {
	a.mu.RLock()
	wire := a.network
	a.mu.RUnlock()
	if wire == nil || request.RequestID == "" {
		return
	}
	result := Message{
		Type: "delivery_ack", RequestID: request.RequestID,
		SourceID: request.SourceID, TargetID: request.TargetID,
	}
	if deliveryErr != nil {
		result.Type, result.Error = "delivery_error", deliveryErr.Error()
	}
	_ = wire.Send(result)
}

func (a *agent) completePendingDelivery(message Message) {
	a.deliveryMu.Lock()
	pending := a.pendingDeliveries[message.RequestID]
	delete(a.pendingDeliveries, message.RequestID)
	a.deliveryMu.Unlock()
	if pending == nil {
		return
	}
	if message.Type == "delivery_error" {
		pending <- errors.New(defaultString(message.Error, "remote delivery failed"))
	} else {
		pending <- nil
	}
}

func (a *agent) failPendingDeliveries(reason string) {
	a.deliveryMu.Lock()
	for id, pending := range a.pendingDeliveries {
		delete(a.pendingDeliveries, id)
		pending <- errors.New(reason)
	}
	a.deliveryMu.Unlock()
}

func (a *agent) clearRemotePeers() {
	a.mu.Lock()
	a.remote = map[string]Peer{}
	a.remoteHosts = map[string]Host{}
	a.mu.Unlock()
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

// DefaultRuntimeDir returns the per-user ephemeral directory used by a host agent.
func DefaultRuntimeDir() string {
	if value := os.Getenv("XDG_RUNTIME_DIR"); value != "" {
		return filepath.Join(value, "agent-sessions-federation")
	}
	return filepath.Join(os.TempDir(), "agent-sessions-federation-"+strconv.Itoa(os.Getuid()))
}
