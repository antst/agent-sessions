package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessionkey"
	"github.com/antst/agent-sessions/internal/socketpath"
)

// ErrClaudeSyntheticServiceUnavailable reports a missing vendor-required local service projection.
var ErrClaudeSyntheticServiceUnavailable = errors.New("claude synthetic Agent Sessions service is unavailable")

type claudeDaemonSession struct {
	SessionID        string
	Name             string
	Cwd              string
	Profile          string
	PID              int
	ProcStart        string
	Socket           string
	SyntheticService bool
}

type claudeDaemonClient interface {
	PrepareInteractive(context.Context, daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error)
	ResolveSession(context.Context, string, string) (claudeDaemonSession, bool, error)
	ObserveSession(context.Context, string, int) (claudeDaemonSession, error)
	InspectSession(context.Context, string, string) (claudeDaemonSession, error)
	DeliverFrame(context.Context, string, federation.AgentFrame) error
}

// PrepareInteractive returns the direct Claude vendor handoff for one validated launch intent.
func (adapter *claudeDaemonAdapter) PrepareInteractive(ctx context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	if adapter == nil || adapter.client == nil {
		return daemonpkg.NativeLaunchPlan{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return adapter.client.PrepareInteractive(ctx, request)
}

type claudeDaemonAdapter struct{ client claudeDaemonClient }

func newClaudeDaemonAdapter(client claudeDaemonClient) *claudeDaemonAdapter {
	return &claudeDaemonAdapter{client: client}
}

// NewClaudeDaemonAdapter constructs the in-process native Claude adapter.
func NewClaudeDaemonAdapter() *claudeDaemonAdapter {
	return newClaudeDaemonAdapter(newClaudeNativeCoordinator())
}

// Close removes only this daemon process's synthetic Claude service projection.
func (adapter *claudeDaemonAdapter) Close() {
	if coordinator, ok := adapter.client.(*claudeNativeCoordinator); ok {
		coordinator.close()
	}
}

// ResolveSelection maps an exact UUID or unique Claude name to one native session.
func (adapter *claudeDaemonAdapter) ResolveSelection(ctx context.Context, profile, selector string) (claudeDaemonSession, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(selector) == "" {
		return claudeDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	session, ambiguous, err := adapter.client.ResolveSession(ctx, profile, selector)
	if err != nil {
		return claudeDaemonSession{}, err
	}
	if ambiguous {
		return claudeDaemonSession{}, daemonpkg.ErrAttachmentAmbiguous
	}
	if session.SessionID == "" {
		return claudeDaemonSession{}, daemonpkg.ErrAttachmentNotFound
	}
	return session, nil
}

// ObserveConnector adopts a late-bound Claude name resume from the exact MCP
// connector ancestry after native Claude publishes its session row.
func (adapter *claudeDaemonAdapter) ObserveConnector(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	evidence daemonpkg.ConnectorProcessEvidence,
) (string, map[string]any, error) {
	if adapter == nil || adapter.client == nil || evidence.PID <= 1 {
		return "", nil, daemonpkg.ErrAttachmentSelecting
	}
	session, err := adapter.client.ObserveSession(ctx, claudeRecordProfile(record), evidence.PID)
	if err != nil {
		return "", nil, err
	}
	if session.Cwd != record.Cwd || !matchesOptionalString(record.ProfileIdentity["profile"], session.Profile) {
		return "", nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return session.SessionID, claudeSessionActor(session), nil
}

// Corroborate proves that a prepared Claude attachment selected the expected native session.
func (adapter *claudeDaemonAdapter) Corroborate(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	sessionID := record.SessionID
	if sessionID == "" {
		sessionID = stringValue(evidence["session_id"])
	}
	if sessionID == "" {
		sessionID = stringValue(record.NativeActor["session_id"])
	}
	return adapter.inspectExact(ctx, record, sessionID, evidence)
}

// Reconnect revalidates an already attached Claude session after daemon recovery.
func (adapter *claudeDaemonAdapter) Reconnect(ctx context.Context, record daemonpkg.AttachmentRecord) (map[string]any, error) {
	return adapter.inspectExact(ctx, record, record.SessionID, record.NativeActor)
}

// Deliver forwards one already-admitted frame through the corroborated Claude socket.
func (adapter *claudeDaemonAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	actor, err := adapter.Reconnect(ctx, destination)
	if err != nil {
		return err
	}
	return adapter.client.DeliverFrame(ctx, stringValue(actor["socket"]), frame)
}

//nolint:dupl // Product-specific evidence and readiness rules intentionally remain explicit adapter contracts.
func (adapter *claudeDaemonAdapter) inspectExact(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	sessionID string,
	evidence map[string]any,
) (map[string]any, error) {
	if adapter == nil || adapter.client == nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	profile := claudeRecordProfile(record)
	return inspectDaemonActor(ctx, sessionID, "Claude", true, func(ctx context.Context, id string) (claudeDaemonSession, error) {
		return adapter.client.InspectSession(ctx, profile, id)
	},
		func(session claudeDaemonSession) (map[string]any, error) {
			if !matchesDaemonSession(record, sessionID, session.SessionID, session.Cwd, session.Profile) ||
				!matchesDaemonActorEvidence(record.NativeActor, evidence,
					daemonActorField{key: "pid", value: session.PID},
					daemonActorField{key: "proc_start", value: session.ProcStart},
					daemonActorField{key: "socket", value: session.Socket},
				) {
				return nil, daemonpkg.ErrAttachmentEvidenceChanged
			}
			if !session.SyntheticService {
				return nil, ErrClaudeSyntheticServiceUnavailable
			}
			return map[string]any{
				"session_id": session.SessionID, "pid": session.PID, "proc_start": session.ProcStart,
				"profile": session.Profile, "cwd": session.Cwd, "socket": session.Socket,
				"synthetic_service": true,
			}, nil
		})
}

func claudeRecordProfile(record daemonpkg.AttachmentRecord) string {
	return strings.TrimSpace(stringValue(record.ProfileIdentity["profile"]))
}

func claudeSessionActor(session claudeDaemonSession) map[string]any {
	return map[string]any{
		"session_id": session.SessionID, "pid": session.PID, "proc_start": session.ProcStart,
		"profile": session.Profile, "cwd": session.Cwd, "socket": session.Socket,
		"synthetic_service": session.SyntheticService,
	}
}

type claudeNativeCoordinator struct {
	mu           sync.Mutex
	serviceToken string
	projections  map[string]claudeServiceProjection
}

type claudeServiceProjection struct {
	recordPath string
	keyPath    string
	pid        int
	procStart  string
	keyDevice  string
	keyInode   string
}

type claudeRegistryRecord struct {
	PID                 int    `json:"pid,omitempty"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd,omitempty"`
	Name                string `json:"name"`
	Status              string `json:"status,omitempty"`
	Entrypoint          string `json:"entrypoint,omitempty"`
	PermissionMode      string `json:"permissionMode,omitempty"`
	ProcStart           string `json:"procStart,omitempty"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	Version             string `json:"version,omitempty"`
	PeerProtocol        int    `json:"peerProtocol,omitempty"`
	Kind                string `json:"kind,omitempty"`
	NameSource          string `json:"nameSource,omitempty"`
	AgentService        bool   `json:"agentService,omitempty"`
	UpdatedAt           int64  `json:"updatedAt,omitempty"`
	StatusUpdatedAt     int64  `json:"statusUpdatedAt,omitempty"`
}

func newClaudeNativeCoordinator() *claudeNativeCoordinator {
	return &claudeNativeCoordinator{projections: make(map[string]claudeServiceProjection)}
}

// PrepareInteractive returns a direct Claude launch with a daemon-owned stable
// native messaging socket whenever the session UUID is already known.
func (coordinator *claudeNativeCoordinator) PrepareInteractive(
	ctx context.Context,
	request daemonpkg.AttachmentPrepareRequest,
) (daemonpkg.NativeLaunchPlan, error) {
	profile, err := canonicalClaudeProfile(stringValue(request.ProfileIdentity["profile"]))
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	if err := coordinator.ensureSyntheticService(profile); err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	executable, err := claudeDaemonExecutable()
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	arguments := append([]string(nil), request.Intent.NativeArguments...)
	sessionID := ""
	if exactLaunchThreadIDRE.MatchString(request.Intent.Selector) {
		sessionID = request.Intent.Selector
	}
	environment := claudeDaemonEnvironment(request, profile)
	expected := map[string]any{}
	if sessionID != "" {
		paths, pathErr := daemonpkg.ResolveProductionPaths()
		if pathErr != nil {
			return daemonpkg.NativeLaunchPlan{}, pathErr
		}
		socket := filepath.Join(paths.RuntimeRoot, "claude-"+sessionkey.FromID(sessionID)+".sock")
		if err := socketpath.Validate(socket); err != nil {
			return daemonpkg.NativeLaunchPlan{}, fmt.Errorf("validate Claude messaging socket: %w", err)
		}
		if _, err := os.Lstat(socket); err == nil || !os.IsNotExist(err) {
			return daemonpkg.NativeLaunchPlan{}, errors.New("managed Claude messaging socket already exists")
		}
		arguments = insertClaudeDaemonArguments(arguments, "--messaging-socket-path", socket)
		expected = map[string]any{"session_id": sessionID, "socket": socket}
	}
	select {
	case <-ctx.Done():
		return daemonpkg.NativeLaunchPlan{}, ctx.Err()
	default:
	}
	return daemonpkg.NativeLaunchPlan{
		Executable: executable, Arguments: arguments, Environment: environment,
		SessionID: sessionID, Cwd: request.Cwd, ExpectedNativeActor: expected,
	}, nil
}

// ResolveSession selects one live native Claude registry row.
func (coordinator *claudeNativeCoordinator) ResolveSession(
	ctx context.Context,
	profile, selector string,
) (claudeDaemonSession, bool, error) {
	sessions, err := coordinator.sessions(ctx, profile)
	if err != nil {
		return claudeDaemonSession{}, false, err
	}
	matches := make([]claudeDaemonSession, 0, 1)
	for _, session := range sessions {
		if session.SessionID == selector || session.Name == selector {
			matches = append(matches, session)
		}
	}
	if len(matches) == 0 {
		return claudeDaemonSession{}, false, daemonpkg.ErrAttachmentNotFound
	}
	return matches[0], len(matches) > 1, nil
}

// ObserveSession selects the closest live Claude ancestor of one connector.
func (coordinator *claudeNativeCoordinator) ObserveSession(
	ctx context.Context,
	profile string,
	connectorPID int,
) (claudeDaemonSession, error) {
	sessions, err := coordinator.sessions(ctx, profile)
	if err != nil {
		return claudeDaemonSession{}, err
	}
	byPID := make(map[int]claudeDaemonSession, len(sessions))
	for _, session := range sessions {
		byPID[session.PID] = session
	}
	pid := connectorPID
	for depth := 0; depth < 64 && pid > 1; depth++ {
		if session, ok := byPID[pid]; ok {
			return session, nil
		}
		info := procinfo.Read(pid)
		if info.Status != procinfo.Known || info.Parent <= 1 || info.Parent == pid {
			break
		}
		pid = info.Parent
	}
	return claudeDaemonSession{}, daemonpkg.ErrAttachmentSelecting
}

// InspectSession corroborates one exact live Claude native registry row.
func (coordinator *claudeNativeCoordinator) InspectSession(
	ctx context.Context,
	profile, sessionID string,
) (claudeDaemonSession, error) {
	sessions, err := coordinator.sessions(ctx, profile)
	if err != nil {
		return claudeDaemonSession{}, err
	}
	for _, session := range sessions {
		if session.SessionID == sessionID {
			return session, nil
		}
	}
	return claudeDaemonSession{}, daemonpkg.ErrAttachmentNotFound
}

// DeliverFrame writes one authenticated native Claude cross-session message.
func (coordinator *claudeNativeCoordinator) DeliverFrame(ctx context.Context, socket string, frame federation.AgentFrame) error {
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return err
	}
	configuration, err := daemonpkg.LoadDaemonConfig(paths.ConfigurationFile, paths)
	if err != nil {
		return err
	}
	inner, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	content := claudeCarrierEnvelope(paths.ControlEndpoint, "agent-"+sessionkey.FromID(configuration.HostID),
		"agent-sessions--"+sanitizeName(configuration.HostName), federation.EscapeAgentFrameEnvelopeJSON(inner))
	message, err := json.Marshal(map[string]any{
		"msgV": 1, "msg_id": frame.MessageID, "type": "user", "priority": "next",
		"from":    claudeUDSAddress(paths.ControlEndpoint),
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = connection.Write(append(message, '\n'))
	return err
}

func (coordinator *claudeNativeCoordinator) sessions(ctx context.Context, profile string) ([]claudeDaemonSession, error) {
	canonical, err := canonicalClaudeProfile(profile)
	if err != nil {
		return nil, err
	}
	if err := coordinator.ensureSyntheticService(canonical); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(canonical, "sessions"))
	if err != nil {
		return nil, err
	}
	result := make([]claudeDaemonSession, 0)
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if session, ok := readLiveClaudeRegistrySession(canonical, entry); ok {
			result = append(result, session)
		}
	}
	return result, nil
}

func readLiveClaudeRegistrySession(profile string, entry os.DirEntry) (claudeDaemonSession, bool) {
	pid, ok := claudeRegistryPID(entry)
	if !ok {
		return claudeDaemonSession{}, false
	}
	body, readErr := os.ReadFile(filepath.Join(profile, "sessions", entry.Name())) //nolint:gosec // fixed native registry directory.
	if readErr != nil || len(body) > 1024*1024 {
		return claudeDaemonSession{}, false
	}
	var record claudeRegistryRecord
	if json.Unmarshal(body, &record) != nil || !validClaudeRegistryRecord(record, pid) {
		return claudeDaemonSession{}, false
	}
	return liveClaudeRegistrySession(profile, record, pid)
}

func claudeRegistryPID(entry os.DirEntry) (int, bool) {
	if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
	return pid, err == nil && pid > 1
}

func validClaudeRegistryRecord(record claudeRegistryRecord, pid int) bool {
	return !record.AgentService && record.PID == pid && record.SessionID != "" &&
		record.ProcStart != "" && filepath.IsAbs(record.MessagingSocketPath)
}

func liveClaudeRegistrySession(profile string, record claudeRegistryRecord, pid int) (claudeDaemonSession, bool) {
	identity := procinfo.Read(pid)
	if identity.Status != procinfo.Known || identity.Start != record.ProcStart {
		return claudeDaemonSession{}, false
	}
	info, statErr := os.Lstat(record.MessagingSocketPath)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return claudeDaemonSession{}, false
	}
	return claudeDaemonSession{
		SessionID: record.SessionID, Name: record.Name, Cwd: record.Cwd, Profile: profile,
		PID: pid, ProcStart: record.ProcStart, Socket: record.MessagingSocketPath, SyntheticService: true,
	}, true
}

func (coordinator *claudeNativeCoordinator) ensureSyntheticService(profile string) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if projection, ok := coordinator.projections[profile]; ok {
		if exactClaudeServiceProjection(projection) {
			return nil
		}
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	projection, err := coordinator.createSyntheticService(profile)
	if err != nil {
		return err
	}
	coordinator.projections[profile] = projection
	return nil
}

func (coordinator *claudeNativeCoordinator) createSyntheticService(profile string) (claudeServiceProjection, error) {
	identity := procinfo.Read(os.Getpid())
	if identity.Status != procinfo.Known || identity.Start == "" {
		return claudeServiceProjection{}, errors.New("cannot corroborate daemon process for Claude service projection")
	}
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		return claudeServiceProjection{}, err
	}
	configuration, err := daemonpkg.LoadDaemonConfig(paths.ConfigurationFile, paths)
	if err != nil {
		return claudeServiceProjection{}, err
	}
	registry := filepath.Join(profile, "sessions")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		return claudeServiceProjection{}, err
	}
	if coordinator.serviceToken == "" {
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return claudeServiceProjection{}, err
		}
		coordinator.serviceToken = hex.EncodeToString(token[:])
	}
	keyName, err := federator.ClaudeServiceKeyName(os.Getpid(), paths.ControlEndpoint)
	if err != nil {
		return claudeServiceProjection{}, err
	}
	recordPath := filepath.Join(registry, strconv.Itoa(os.Getpid())+".json")
	keyPath := filepath.Join(registry, keyName)
	now := time.Now().UnixMilli()
	record := claudeRegistryRecord{
		PID: os.Getpid(), SessionID: "agent-" + sessionkey.FromID(configuration.HostID),
		Cwd: paths.StateRoot, Name: "agent-sessions--" + sanitizeName(configuration.HostName), Status: "idle",
		Entrypoint: "agent-sessions", ProcStart: identity.Start, MessagingSocketPath: paths.ControlEndpoint,
		StartedAt: now, Version: "agent-sessions/" + daemonpkg.BuildVersion, PeerProtocol: federation.GroupProtocolVersion,
		Kind: "service", NameSource: "agent", AgentService: true, UpdatedAt: now, StatusUpdatedAt: now,
	}
	if err := writeJSONAtomic(keyPath, map[string]string{"peerToken": coordinator.serviceToken, "procStart": identity.Start}); err != nil {
		return claudeServiceProjection{}, err
	}
	if err := writeJSONAtomic(recordPath, record); err != nil {
		_ = os.Remove(keyPath)
		return claudeServiceProjection{}, err
	}
	keyStat, ok := regularFileStat(keyPath)
	if !ok {
		_ = os.Remove(recordPath)
		_ = os.Remove(keyPath)
		return claudeServiceProjection{}, errors.New("claude service key did not retain an exact regular-file identity")
	}
	device, inode := syscallIdentity(keyStat)
	return claudeServiceProjection{
		recordPath: recordPath, keyPath: keyPath, pid: os.Getpid(), procStart: identity.Start,
		keyDevice: device, keyInode: inode,
	}, nil
}

func regularFileStat(path string) (*syscall.Stat_t, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func syscallIdentity(stat *syscall.Stat_t) (string, string) {
	return fmt.Sprint(stat.Dev), fmt.Sprint(stat.Ino)
}

func exactClaudeServiceProjection(projection claudeServiceProjection) bool {
	recordInfo, recordErr := os.Lstat(projection.recordPath)
	if recordErr != nil || recordInfo.Mode()&os.ModeSymlink != 0 || !recordInfo.Mode().IsRegular() {
		return false
	}
	stat, ok := regularFileStat(projection.keyPath)
	if !ok {
		return false
	}
	device, inode := syscallIdentity(stat)
	return device == projection.keyDevice && inode == projection.keyInode
}

func (coordinator *claudeNativeCoordinator) close() {
	coordinator.mu.Lock()
	projections := coordinator.projections
	coordinator.projections = make(map[string]claudeServiceProjection)
	coordinator.mu.Unlock()
	for _, projection := range projections {
		removeJSONIf(projection.recordPath, func(row map[string]any) bool {
			return intValue(row["pid"]) == projection.pid && stringValue(row["procStart"]) == projection.procStart && boolValue(row["agentService"])
		})
		if stat, ok := regularFileStat(projection.keyPath); ok {
			device, inode := syscallIdentity(stat)
			if device == projection.keyDevice && inode == projection.keyInode {
				_ = os.Remove(projection.keyPath)
			}
		}
	}
}

func canonicalClaudeProfile(configured string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configured = filepath.Join(home, ".claude")
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func claudeDaemonExecutable() (string, error) {
	configured := strings.TrimSpace(os.Getenv("CLAUDE_PEER_CLAUDE_BIN"))
	if configured == "" {
		configured = "claude"
	}
	executable, err := exec.LookPath(configured)
	if err != nil {
		return "", fmt.Errorf("resolve Claude executable %q: %w", configured, err)
	}
	return executable, nil
}

func claudeDaemonEnvironment(request daemonpkg.AttachmentPrepareRequest, profile string) map[string]string {
	environment := map[string]string{}
	if boolValue(request.ProfileIdentity["config_env_set"]) {
		environment["CLAUDE_CONFIG_DIR"] = stringValue(request.ProfileIdentity["config_env_value"])
	} else if profile != "" {
		environment["CLAUDE_CONFIG_DIR"] = profile
	}
	if boolValue(request.ProfileIdentity["secure_env_set"]) {
		environment["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = stringValue(request.ProfileIdentity["secure_config"])
	}
	return environment
}

func insertClaudeDaemonArguments(arguments []string, managed ...string) []string {
	insert := len(arguments)
	for index, argument := range arguments {
		if argument == "--" {
			insert = index
			break
		}
	}
	result := make([]string, 0, len(arguments)+len(managed))
	result = append(result, arguments[:insert]...)
	result = append(result, managed...)
	return append(result, arguments[insert:]...)
}

func claudeUDSAddress(path string) string { return "uds:" + path }

func claudeCarrierEnvelope(socket, sessionID, name string, inner []byte) string {
	clean := strings.NewReplacer("\"", "", "<", "", ">", "", "\n", "", "\r", "")
	return `<cross-session-message from="` + clean.Replace(claudeUDSAddress(socket)) + `" from-session="` +
		clean.Replace(sessionID) + `" from-name="` + clean.Replace(name) + `" from-mode="prompting">` +
		"\n" + string(inner) + "\n</cross-session-message>"
}
