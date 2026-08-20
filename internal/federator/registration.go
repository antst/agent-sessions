package federator

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

// AgentServiceProjection is the one native Claude service row plus its
// socket-bound authentication sidecar in the shared native profile.
type AgentServiceProjection struct {
	Record    []byte
	PID       int
	ProcStart string
	KeyName   string
	PeerToken string
}

// AgentService projects the host agent into one Claude-native registry.
func AgentService(runtimeDir string) (AgentServiceProjection, error) {
	response, err := requestAgentControl(runtimeDir, Message{Type: "service_record", Version: GroupProtocolVersion})
	if err != nil {
		return AgentServiceProjection{}, err
	}
	if len(response.Data) == 0 || response.ServicePeerToken == "" {
		return AgentServiceProjection{}, errors.New("agent returned no authenticated service record")
	}
	var record registryRecord
	if json.Unmarshal(response.Data, &record) != nil || !record.AgentService || record.PID <= 1 ||
		record.ProcStart == "" || record.MessagingSocketPath == "" {
		return AgentServiceProjection{}, errors.New("agent returned an invalid authenticated service record")
	}
	keyName, err := ClaudeServiceKeyName(record.PID, record.MessagingSocketPath)
	if err != nil {
		return AgentServiceProjection{}, err
	}
	return AgentServiceProjection{
		Record: append([]byte(nil), response.Data...), PID: record.PID, ProcStart: record.ProcStart,
		KeyName: keyName, PeerToken: response.ServicePeerToken,
	}, nil
}

// AgentServiceRecord returns the one Claude-native service record published
// by the host agent in the shared profile.
func AgentServiceRecord(runtimeDir string) ([]byte, error) {
	projection, err := AgentService(runtimeDir)
	if err != nil {
		return nil, err
	}
	return projection.Record, nil
}

// ClaudeServiceKeyName returns Claude's socket-bound native peer-key filename.
func ClaudeServiceKeyName(pid int, socket string) (string, error) {
	if pid <= 1 || !filepath.IsAbs(socket) {
		return "", errors.New("invalid Claude service key identity")
	}
	digest := sha256.Sum256([]byte(filepath.Clean(socket)))
	return strconv.Itoa(pid) + "." + fmt.Sprintf("%x", digest[:]) + ".key", nil
}

// ProcessStart returns the platform process-start identity used by live peer
// registration. It is exposed only for product supershims in this module.
func ProcessStart(pid int) string { return processStart(pid) }

// PeerRegistration binds one live product-owned session to its delivery adapter.
type PeerRegistration struct {
	Version              int                      `json:"version"`
	SessionID            string                   `json:"session_id"`
	Product              string                   `json:"product"`
	Name                 string                   `json:"name"`
	Status               string                   `json:"status,omitempty"`
	PermissionMode       string                   `json:"permission_mode,omitempty"`
	Cwd                  string                   `json:"cwd,omitempty"`
	PID                  int                      `json:"pid"`
	ProcStart            string                   `json:"proc_start"`
	Socket               string                   `json:"socket"`
	LifecyclePID         int                      `json:"lifecycle_pid,omitempty"`
	LifecycleProcStart   string                   `json:"lifecycle_proc_start,omitempty"`
	LifecycleRoot        string                   `json:"lifecycle_root,omitempty"`
	ClaudeConfigRoot     string                   `json:"claude_config_root,omitempty"`
	ClaudeKeyBaseline    []ClaudeKeyBaselineEntry `json:"claude_key_baseline,omitempty"`
	ClaudeKeyBaselineSet bool                     `json:"claude_key_baseline_set,omitempty"`
	ClaudeSocketPath     string                   `json:"claude_socket_path,omitempty"`
	ClaudeSocketPathSet  bool                     `json:"claude_socket_path_set,omitempty"`
	AdapterStrongStart   string                   `json:"adapter_strong_start,omitempty"`
	LifecycleStrongStart string                   `json:"lifecycle_strong_start,omitempty"`
	StartedAt            int64                    `json:"started_at,omitempty"`
}

// ClaudeKeyBaselineEntry fingerprints one PID-prefixed native key artifact
// before a gated Claude adapter can replace it.
type ClaudeKeyBaselineEntry struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

type peerPreparation struct {
	Registration        PeerRegistration    `json:"registration"`
	PriorPreference     *SessionPreferences `json:"prior_preference,omitempty"`
	DesiredPreference   SessionPreferences  `json:"desired_preference,omitempty"`
	RollbackPreferences bool                `json:"rollback_preferences,omitempty"`
	Committed           bool                `json:"committed,omitempty"`
}

// ParentContext is the agent-attested parent layer inherited by a child lane.
// Process identity is lifecycle authority; session identity and groups remain
// durable even when a persistent child outlives that process.
type ParentContext struct {
	HostID           string   `json:"host_id"`
	SessionID        string   `json:"session_id"`
	Product          string   `json:"product"`
	InstanceID       string   `json:"instance_id"`
	Groups           []string `json:"groups"`
	AlwaysApprove    bool     `json:"always_approve"`
	AdapterPID       int      `json:"adapter_pid,omitempty"`
	AdapterProcStart string   `json:"adapter_proc_start,omitempty"`
	AdapterSocket    string   `json:"adapter_socket,omitempty"`
	PID              int      `json:"pid"`
	ProcStart        string   `json:"proc_start"`
	PermissionMode   string   `json:"permission_mode"`
}

// ResolveParentContext returns the exact live local registration and durable
// preferences for a parent session. It never resolves a remote peer.
func ResolveParentContext(runtimeDir, sessionID string) (ParentContext, error) {
	response, err := requestAgentControl(runtimeDir, Message{
		Type: "parent_context", Version: GroupProtocolVersion, SessionID: sessionID,
	})
	if err != nil {
		return ParentContext{}, err
	}
	if response.ParentContext == nil {
		return ParentContext{}, errors.New("agent returned no parent context")
	}
	return *response.ParentContext, nil
}

// RegisterPeer installs or refreshes one live adapter registration.
func RegisterPeer(runtimeDir string, registration PeerRegistration) (Peer, error) {
	response, err := requestAgentControl(runtimeDir, Message{Type: "peer_register", Registration: &registration})
	if err != nil {
		return Peer{}, err
	}
	if len(response.Peers) != 1 {
		return Peer{}, errors.New("agent registration returned no peer")
	}
	return response.Peers[0], nil
}

// PreparePeer gives the host agent ownership of a gated Claude adapter before
// native Claude is allowed to exec or publish shared-registry artifacts.
func PreparePeer(runtimeDir string, registration PeerRegistration) error {
	_, err := requestAgentControl(runtimeDir, Message{Type: "peer_prepare", Registration: &registration})
	return err
}

// PreparePeerLaunch atomically adopts the previewed session preferences and
// gives the host agent durable ownership of the still-gated Claude adapter.
// Cancel/restart reconciliation restores the prior catalog row unless a full
// live registration commits the launch.
func PreparePeerLaunch(
	runtimeDir string,
	registration PeerRegistration,
	request ResolvePreferencesRequest,
	expected SessionPreferences,
) (ResolvedPreferences, error) {
	message := preferenceMessage("peer_prepare_launch", request)
	message.Registration = &registration
	message.Preference = &expected
	response, err := requestAgentControl(runtimeDir, message)
	if err != nil {
		return ResolvedPreferences{}, err
	}
	if response.Type != "peer_prepare_launch" || response.Preference == nil {
		return ResolvedPreferences{}, errors.New("agent returned no prepared peer preferences")
	}
	return ResolvedPreferences{Preference: *response.Preference, EffectiveGroups: response.Groups}, nil
}

// CancelPeerPreparation removes only the exact gated launch preparation.
func CancelPeerPreparation(runtimeDir string, registration PeerRegistration) error {
	_, err := requestAgentControl(runtimeDir, Message{Type: "peer_prepare_cancel", Registration: &registration})
	return err
}

// UpdatePeerStatus refreshes mutable live presentation without changing preferences.
func UpdatePeerStatus(runtimeDir string, registration PeerRegistration) (Peer, error) {
	response, err := requestAgentControl(runtimeDir, Message{Type: "peer_update", Registration: &registration})
	if err != nil {
		return Peer{}, err
	}
	if len(response.Peers) != 1 {
		return Peer{}, errors.New("agent update returned no peer")
	}
	return response.Peers[0], nil
}

// UnregisterPeer removes only the exact live adapter registration.
func UnregisterPeer(runtimeDir string, registration PeerRegistration) error {
	_, err := requestAgentControl(runtimeDir, Message{Type: "peer_unregister", Registration: &registration})
	return err
}

func requestAgentControl(runtimeDir string, request Message) (Message, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir, "agent.sock"), time.Second)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = conn.Close() }()
	// Routing may wait for one remote delivery acknowledgement. Fan-out is
	// concurrent, so one acknowledgement window plus transport margin bounds
	// every control request regardless of recipient count.
	_ = conn.SetDeadline(time.Now().Add(wireWriteTimeout + 5*time.Second))
	if err := newWireConn(conn).Send(request); err != nil {
		return Message{}, err
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return Message{}, err
	}
	if response.Type == "error" {
		return Message{}, errors.New(defaultString(response.Error, "agent control failed"))
	}
	return response, nil
}

func (a *agent) preparationPath(sessionID string) string {
	return filepath.Join(a.preparationDir, sessionKey(sessionID)+".json")
}

//nolint:gocyclo // Loading validates transactional and legacy preparation shapes explicitly.
func (a *agent) loadPeerPreparations() error {
	if err := os.MkdirAll(a.preparationDir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(a.preparationDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(a.preparationDir, entry.Name()))
		if err != nil {
			return err
		}
		var preparation peerPreparation
		if err := json.Unmarshal(body, &preparation); err != nil {
			return errors.New("invalid durable Claude peer preparation")
		}
		if preparation.Registration.SessionID == "" {
			// Development builds briefly persisted the registration directly.
			// Recover that non-transactional shape so an upgrade can still retire
			// its exact gated adapter instead of refusing to start the host agent.
			var legacy PeerRegistration
			if err := json.Unmarshal(body, &legacy); err != nil {
				return errors.New("invalid durable Claude peer preparation")
			}
			preparation.Registration = legacy
		}
		registration := preparation.Registration
		if !validCatalogSessionID(registration.SessionID) || entry.Name() != sessionKey(registration.SessionID)+".json" ||
			registration.AdapterStrongStart == "" || registration.LifecycleStrongStart == "" ||
			!validClaudeKeyBaseline(registration.PID, registration.ClaudeKeyBaselineSet, registration.ClaudeKeyBaseline) ||
			!validLoadedClaudePeerSocket(a.options.RuntimeDir, registration) ||
			(preparation.RollbackPreferences && preparation.DesiredPreference.SessionID != registration.SessionID) {
			return errors.New("invalid durable Claude peer preparation")
		}
		a.preparations[registration.SessionID] = preparation
	}
	return nil
}

func (a *agent) validateClaudePreparation(registration PeerRegistration) error {
	if err := a.validateClaudePreparationIdentity(registration); err != nil {
		return err
	}
	preference, _, ok, err := a.catalog.get(registration.SessionID)
	if err != nil {
		return err
	}
	if !ok || preference.Product != "claude" {
		return errors.New("claude peer preparation has no matching session preferences")
	}
	return nil
}

//nolint:gocyclo // Preparation identity joins path ownership with two strong live process identities.
func (a *agent) validateClaudePreparationIdentity(registration PeerRegistration) error {
	if registration.Version != GroupProtocolVersion || registration.Product != "claude" ||
		!validCatalogSessionID(registration.SessionID) || registration.PID <= 1 || registration.ProcStart == "" ||
		registration.LifecyclePID <= 1 || registration.LifecycleProcStart == "" ||
		registration.LifecycleRoot == "" || registration.ClaudeConfigRoot == "" || registration.Socket != "" {
		return errors.New("invalid Claude peer preparation")
	}
	expectedRoot := ClaudePeerLifecycleRootInState(a.options.StateDir, registration.SessionID)
	if a.options.StateDir == "" {
		expectedRoot = ClaudePeerLifecycleRoot(a.options.HostID, registration.SessionID)
	}
	if filepath.Clean(registration.LifecycleRoot) != filepath.Clean(expectedRoot) ||
		!sameRegistryRoot(registration.ClaudeConfigRoot, a.options.ClaudeConfigDir) {
		return errors.New("claude peer preparation roots are not agent-owned")
	}
	if !registration.ClaudeKeyBaselineSet {
		return errors.New("claude peer preparation key baseline is missing")
	}
	if registration.ClaudeSocketPathSet {
		expectedSocket := ClaudePeerMessagingSocketPath(a.options.RuntimeDir, registration.SessionID)
		if registration.ClaudeSocketPath != expectedSocket || ValidateClaudePeerMessagingSocketPath(expectedSocket) != nil {
			return errors.New("claude peer preparation socket is not agent-owned")
		}
		if _, err := os.Lstat(expectedSocket); err == nil || !os.IsNotExist(err) {
			return errors.New("claude peer preparation socket path already exists")
		}
	} else if registration.ClaudeSocketPath != "" {
		return errors.New("invalid Claude peer preparation socket")
	}
	baseline, err := ClaudePeerKeySidecars(registration.ClaudeConfigRoot, registration.PID)
	if err != nil || !slices.Equal(baseline, registration.ClaudeKeyBaseline) {
		return errors.New("claude peer preparation key baseline changed before persistence")
	}
	adapter := procinfo.Read(registration.PID)
	lifecycle := procinfo.Read(registration.LifecyclePID)
	if adapter.Status != procinfo.Known || adapter.Start != registration.ProcStart || !processLive(registration.PID) ||
		adapter.StrongStart == "" || lifecycle.Status != procinfo.Known || lifecycle.Start != registration.LifecycleProcStart ||
		lifecycle.StrongStart == "" || !processLive(registration.LifecyclePID) {
		return errors.New("claude peer preparation identity is not live")
	}
	return nil
}

func (a *agent) preparePeer(registration PeerRegistration) error {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	if err := a.validateClaudePreparation(registration); err != nil {
		return err
	}
	adapter := procinfo.Read(registration.PID)
	lifecycle := procinfo.Read(registration.LifecyclePID)
	if adapter.Status != procinfo.Known || adapter.Start != registration.ProcStart || adapter.StrongStart == "" ||
		lifecycle.Status != procinfo.Known || lifecycle.Start != registration.LifecycleProcStart || lifecycle.StrongStart == "" {
		return errors.New("claude peer preparation identity changed before persistence")
	}
	registration.AdapterStrongStart = adapter.StrongStart
	registration.LifecycleStrongStart = lifecycle.StrongStart
	a.mu.Lock()
	defer a.mu.Unlock()
	if current, ok := a.preparations[registration.SessionID]; ok &&
		!samePreparedRegistration(current.Registration, registration) {
		return errors.New("claude peer session already has another prepared attachment")
	}
	preparation := peerPreparation{Registration: registration}
	if err := writeJSONAtomic(a.preparationPath(registration.SessionID), preparation); err != nil {
		return err
	}
	a.preparations[registration.SessionID] = preparation
	return nil
}

func (a *agent) preparePeerLaunch(
	registration PeerRegistration,
	update SessionPreferenceUpdate,
	expected SessionPreferences,
) (SessionPreferences, []string, error) {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	if err := a.validateClaudePreparationIdentity(registration); err != nil {
		return SessionPreferences{}, nil, err
	}
	adapter := procinfo.Read(registration.PID)
	lifecycle := procinfo.Read(registration.LifecyclePID)
	if adapter.Status != procinfo.Known || adapter.Start != registration.ProcStart || adapter.StrongStart == "" ||
		lifecycle.Status != procinfo.Known || lifecycle.Start != registration.LifecycleProcStart || lifecycle.StrongStart == "" {
		return SessionPreferences{}, nil, errors.New("claude peer preparation identity changed before persistence")
	}
	registration.AdapterStrongStart = adapter.StrongStart
	registration.LifecycleStrongStart = lifecycle.StrongStart
	persist := func(prior *SessionPreferences, desired SessionPreferences) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if current, ok := a.preparations[registration.SessionID]; ok &&
			!samePreparedRegistration(current.Registration, registration) {
			return errors.New("claude peer session already has another prepared attachment")
		}
		preparation := peerPreparation{
			Registration: registration, PriorPreference: prior, DesiredPreference: desired, RollbackPreferences: true,
		}
		if err := writeJSONAtomic(a.preparationPath(registration.SessionID), preparation); err != nil {
			return err
		}
		a.preparations[registration.SessionID] = preparation
		return nil
	}
	discard := func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		current, ok := a.preparations[registration.SessionID]
		if !ok || !samePreparedRegistration(current.Registration, registration) {
			return nil
		}
		if err := os.Remove(a.preparationPath(registration.SessionID)); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(a.preparations, registration.SessionID)
		return nil
	}
	return a.catalog.prepare(update, expected, persist, discard)
}

func (a *agent) cancelPeerPreparation(registration PeerRegistration) error {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	a.mu.RLock()
	current, ok := a.preparations[registration.SessionID]
	a.mu.RUnlock()
	if !ok {
		return nil
	}
	if !samePreparedRegistrationWeak(current.Registration, registration) {
		return errors.New("claude peer preparation identity changed")
	}
	preparedRegistration := current.Registration
	adapterStatus := exactProcessStatus(exactProcess{
		PID: preparedRegistration.PID, Start: preparedRegistration.ProcStart, StrongStart: preparedRegistration.AdapterStrongStart,
	})
	lifecycleStatus := exactProcessStatus(exactProcess{
		PID: preparedRegistration.LifecyclePID, Start: preparedRegistration.LifecycleProcStart,
		StrongStart: preparedRegistration.LifecycleStrongStart,
	})
	if adapterStatus == procinfo.Unknown || lifecycleStatus != procinfo.Known {
		return errors.New("claude peer preparation identity changed before cancellation")
	}
	if current.RollbackPreferences && !current.Committed {
		if _, err := a.catalog.restorePrepared(current.DesiredPreference, current.PriorPreference); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	latest, ok := a.preparations[registration.SessionID]
	if !ok {
		return nil
	}
	if !samePreparedRegistration(latest.Registration, preparedRegistration) {
		return errors.New("claude peer preparation identity changed during cancellation")
	}
	if err := os.Remove(a.preparationPath(registration.SessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(a.preparations, registration.SessionID)
	return nil
}

func samePreparedRegistration(left, right PeerRegistration) bool {
	return left.PID == right.PID && left.ProcStart == right.ProcStart &&
		left.LifecyclePID == right.LifecyclePID && left.LifecycleProcStart == right.LifecycleProcStart &&
		left.AdapterStrongStart == right.AdapterStrongStart && left.LifecycleStrongStart == right.LifecycleStrongStart &&
		left.ClaudeKeyBaselineSet == right.ClaudeKeyBaselineSet &&
		left.ClaudeSocketPathSet == right.ClaudeSocketPathSet && left.ClaudeSocketPath == right.ClaudeSocketPath &&
		slices.Equal(left.ClaudeKeyBaseline, right.ClaudeKeyBaseline)
}

func samePreparedRegistrationWeak(left, right PeerRegistration) bool {
	return left.PID == right.PID && left.ProcStart == right.ProcStart &&
		left.LifecyclePID == right.LifecyclePID && left.LifecycleProcStart == right.LifecycleProcStart &&
		left.ClaudeKeyBaselineSet == right.ClaudeKeyBaselineSet &&
		left.ClaudeSocketPathSet == right.ClaudeSocketPathSet && left.ClaudeSocketPath == right.ClaudeSocketPath &&
		slices.Equal(left.ClaudeKeyBaseline, right.ClaudeKeyBaseline)
}

// ValidateClaudePeerMessagingSocketPath enforces the sockaddr_un limit before
// a managed Claude adapter is released.
func ValidateClaudePeerMessagingSocketPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("managed Claude messaging socket path is not absolute and clean")
	}
	limit := 107
	if runtime.GOOS == "darwin" {
		limit = 103
	}
	if len([]byte(path)) > limit {
		return fmt.Errorf("managed Claude messaging socket path is %d bytes; platform limit is %d", len([]byte(path)), limit)
	}
	return nil
}

// ClaudePeerKeySidecars snapshots every PID-prefixed key name before a gated
// Claude adapter is released. Cleanup may then distinguish artifacts created
// by that exact launch from stale files left by an earlier use of the PID.
func ClaudePeerKeySidecars(configRoot string, pid int) ([]ClaudeKeyBaselineEntry, error) {
	if pid <= 1 {
		return nil, errors.New("invalid Claude peer key baseline PID")
	}
	entries, err := os.ReadDir(filepath.Join(configRoot, "sessions"))
	if err != nil {
		return nil, err
	}
	prefix := strconv.Itoa(pid) + "."
	result := make([]ClaudeKeyBaselineEntry, 0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".key") {
			fingerprint, fingerprintErr := claudeKeySidecarFingerprint(
				filepath.Join(configRoot, "sessions", entry.Name()),
			)
			if fingerprintErr != nil {
				return nil, fingerprintErr
			}
			result = append(result, ClaudeKeyBaselineEntry{Name: entry.Name(), Fingerprint: fingerprint})
		}
	}
	slices.SortFunc(result, func(left, right ClaudeKeyBaselineEntry) int {
		return strings.Compare(left.Name, right.Name)
	})
	return result, nil
}

func claudeKeySidecarFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("type:%#x", uint32(info.Mode().Type())), nil
	}
	if info.Size() > 4096 {
		return fmt.Sprintf("oversized:%d", info.Size()), nil
	}
	body, err := os.ReadFile(path) //nolint:gosec // bounded PID-prefixed key below the configured native registry.
	if err != nil {
		return "", err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return "", errors.New("native Claude peer-token sidecar changed during snapshot")
	}
	digest := sha256.Sum256(body)
	return "sha256:" + fmt.Sprintf("%x", digest[:]), nil
}

func validClaudeKeyBaseline(pid int, set bool, baseline []ClaudeKeyBaselineEntry) bool {
	if !set {
		return len(baseline) == 0 // legacy preparations did not own token-only cleanup.
	}
	previous := ""
	prefix := strconv.Itoa(pid) + "."
	for _, entry := range baseline {
		if entry.Name <= previous || filepath.Base(entry.Name) != entry.Name ||
			!strings.HasPrefix(entry.Name, prefix) || !strings.HasSuffix(entry.Name, ".key") ||
			!validClaudeKeyFingerprint(entry.Fingerprint) {
			return false
		}
		previous = entry.Name
	}
	return true
}

func validClaudeKeyFingerprint(fingerprint string) bool {
	if digest, ok := strings.CutPrefix(fingerprint, "sha256:"); ok {
		if len(digest) != 64 {
			return false
		}
		for _, character := range digest {
			if character < '0' || character > '9' && (character < 'a' || character > 'f') {
				return false
			}
		}
		return true
	}
	if mode, ok := strings.CutPrefix(fingerprint, "type:"); ok {
		_, err := strconv.ParseUint(mode, 0, 32)
		return err == nil
	}
	if size, ok := strings.CutPrefix(fingerprint, "oversized:"); ok {
		parsed, err := strconv.ParseInt(size, 10, 64)
		return err == nil && parsed > 4096
	}
	return false
}

//nolint:gocyclo // Registration validates adapter, lifecycle, catalog, and replacement identity together.
func (a *agent) registerPeer(registration PeerRegistration, updateOnly bool) (Peer, error) {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	if registration.Version != GroupProtocolVersion || !validCatalogSessionID(registration.SessionID) ||
		!validProduct(registration.Product) || registration.Name == "" || registration.PID <= 1 ||
		registration.ProcStart == "" || registration.Socket == "" {
		return Peer{}, errors.New("invalid peer registration")
	}
	adapterInfo := procinfo.Read(registration.PID)
	if adapterInfo.Status != procinfo.Known || adapterInfo.Start != registration.ProcStart || !processLive(registration.PID) {
		return Peer{}, errors.New("peer registration process identity is not live")
	}
	if !probeUnix(registration.Socket, 250*time.Millisecond) {
		return Peer{}, errors.New("peer registration socket is not reachable")
	}
	lifecyclePID, lifecycleProcStart := registration.LifecyclePID, registration.LifecycleProcStart
	if lifecyclePID == 0 && lifecycleProcStart == "" {
		lifecyclePID, lifecycleProcStart = registration.PID, registration.ProcStart
	}
	lifecycleInfo := procinfo.Read(lifecyclePID)
	if lifecyclePID <= 1 || lifecycleProcStart == "" || lifecycleInfo.Status != procinfo.Known ||
		lifecycleInfo.Start != lifecycleProcStart || !processLive(lifecyclePID) {
		return Peer{}, errors.New("peer lifecycle process identity is not live")
	}
	if registration.LifecycleRoot != "" || registration.ClaudeConfigRoot != "" {
		if registration.Product != "claude" || registration.ClaudeConfigRoot == "" ||
			!sameRegistryRoot(registration.ClaudeConfigRoot, a.options.ClaudeConfigDir) {
			return Peer{}, errors.New("claude peer lifecycle or shared configuration root is not agent-owned")
		}
		if registration.LifecycleRoot != "" {
			expected := ClaudePeerLifecycleRootInState(a.options.StateDir, registration.SessionID)
			if a.options.StateDir == "" {
				expected = ClaudePeerLifecycleRoot(a.options.HostID, registration.SessionID)
			}
			if filepath.Clean(registration.LifecycleRoot) != filepath.Clean(expected) {
				return Peer{}, errors.New("claude peer lifecycle or shared configuration root is not agent-owned")
			}
		}
	}
	preference, groups, ok, err := a.catalog.get(registration.SessionID)
	if err != nil {
		return Peer{}, err
	}
	if !ok || preference.Product != registration.Product {
		return Peer{}, errors.New("peer registration has no matching session preferences")
	}
	if preference.AlwaysApprove != (registration.PermissionMode == "bypassPermissions") {
		return Peer{}, errors.New("peer permission mode does not match durable yolo preference")
	}
	id := a.options.HostID + "/" + registration.SessionID
	a.mu.Lock()
	if _, retiring := a.retirements[id]; retiring {
		a.mu.Unlock()
		return Peer{}, errors.New("session adapter retirement is still pending")
	}
	current, exists := a.local[id]
	prepared, preparedExists := a.preparations[registration.SessionID]
	preparedRegistration := prepared.Registration
	if preparedExists && (preparedRegistration.PID != registration.PID || preparedRegistration.ProcStart != registration.ProcStart ||
		preparedRegistration.LifecyclePID != lifecyclePID || preparedRegistration.LifecycleProcStart != lifecycleProcStart ||
		preparedRegistration.AdapterStrongStart != adapterInfo.StrongStart || preparedRegistration.LifecycleStrongStart != lifecycleInfo.StrongStart ||
		preparedRegistration.ClaudeSocketPathSet && preparedRegistration.ClaudeSocketPath != registration.Socket) {
		a.mu.Unlock()
		return Peer{}, errors.New("claude peer registration does not match its prepared attachment")
	}
	if updateOnly && !exists {
		a.mu.Unlock()
		return Peer{}, errors.New("peer is not registered")
	}
	if exists && (current.PID != registration.PID || current.ProcStart != registration.ProcStart) {
		if processStart(current.PID) == current.ProcStart && processLive(current.PID) {
			a.mu.Unlock()
			return Peer{}, errors.New("session already has a different live adapter")
		}
	}
	startedAt := registration.StartedAt
	if startedAt == 0 {
		startedAt = time.Now().UnixMilli()
	}
	peer := Peer{
		ID: id, HostID: a.options.HostID, HostName: a.options.HostName,
		SessionID: registration.SessionID, GlobalID: globalSessionID(a.options.HostID, registration.SessionID),
		Name: cleanPeerName(registration.Name), DisplayName: qualifiedName(registration.Name, a.options.HostName),
		Status: defaultString(registration.Status, "idle"), Cwd: registration.Cwd,
		Entrypoint: registration.Product, PermissionMode: defaultString(registration.PermissionMode, "default"),
		StartedAt: startedAt, PeerProtocol: GroupProtocolVersion,
		InstanceID: sessionKey(fmt.Sprintf("%s\x00%d\x00%s", a.options.HostID, registration.PID, registration.ProcStart)),
		Groups:     groups, ParentSessionID: preference.ParentSession,
	}
	if preparedExists && !prepared.Committed {
		prepared.Committed = true
		if err := writeJSONAtomic(a.preparationPath(registration.SessionID), prepared); err != nil {
			a.mu.Unlock()
			return Peer{}, err
		}
		a.preparations[registration.SessionID] = prepared
	}
	a.local[id] = localPeer{
		Peer: peer, PID: registration.PID, ProcStart: registration.ProcStart,
		Socket: registration.Socket, GroupProtocol: GroupProtocolVersion,
		LifecyclePID: lifecyclePID, LifecycleProcStart: lifecycleProcStart,
		AdapterStrongStart: adapterInfo.StrongStart, LifecycleStrongStart: lifecycleInfo.StrongStart,
		LifecycleRoot: registration.LifecycleRoot, ClaudeConfigRoot: registration.ClaudeConfigRoot,
		ClaudeKeyBaseline:    append([]ClaudeKeyBaselineEntry(nil), preparedRegistration.ClaudeKeyBaseline...),
		ClaudeKeyBaselineSet: preparedRegistration.ClaudeKeyBaselineSet,
	}
	a.mu.Unlock()
	a.signalLocalChanged()
	return peer, nil
}

func validLoadedClaudePeerSocket(runtimeDir string, registration PeerRegistration) bool {
	if !registration.ClaudeSocketPathSet {
		return registration.ClaudeSocketPath == ""
	}
	if ValidateClaudePeerMessagingSocketPath(registration.ClaudeSocketPath) != nil {
		return false
	}
	expectedName := filepath.Base(ClaudePeerMessagingSocketPath(runtimeDir, registration.SessionID))
	return filepath.Base(registration.ClaudeSocketPath) == expectedName
}

func (a *agent) unregisterPeer(registration PeerRegistration) error {
	if registration.SessionID == "" || registration.PID <= 1 || registration.ProcStart == "" {
		return errors.New("invalid peer unregister request")
	}
	id := a.options.HostID + "/" + registration.SessionID
	a.mu.Lock()
	current, exists := a.local[id]
	removed := false
	if exists && current.PID == registration.PID && current.ProcStart == registration.ProcStart {
		delete(a.local, id)
		removed = true
	}
	a.mu.Unlock()
	if removed {
		a.signalLocalChanged()
	}
	return nil
}

func sameRegistryRoot(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	leftResolved, leftErr := filepath.EvalSymlinks(leftAbsolute)
	rightResolved, rightErr := filepath.EvalSymlinks(rightAbsolute)
	if leftErr == nil && rightErr == nil {
		return filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
	}
	return filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func (a *agent) parentContext(sessionID string) (ParentContext, error) {
	a.reconcileRegisteredPeers()
	a.mu.RLock()
	peer, ok := localPeerBySession(a.local, sessionID)
	a.mu.RUnlock()
	if !ok {
		return ParentContext{}, errors.New("parent session is not a live local peer")
	}
	preference, groups, exists, err := a.catalog.get(sessionID)
	if err != nil {
		return ParentContext{}, err
	}
	if !exists || preference.Product != peer.Entrypoint {
		return ParentContext{}, errors.New("parent session preferences do not match its live registration")
	}
	return ParentContext{
		HostID: a.options.HostID, SessionID: sessionID, Product: preference.Product,
		InstanceID: peer.InstanceID, Groups: groups,
		AlwaysApprove: preference.AlwaysApprove,
		AdapterPID:    peer.PID, AdapterProcStart: peer.ProcStart, AdapterSocket: peer.Socket,
		PID: peer.LifecyclePID, ProcStart: peer.LifecycleProcStart,
		PermissionMode: peer.PermissionMode,
	}, nil
}

//nolint:gocyclo // Reconciliation keeps each identity and retirement transition explicit.
func (a *agent) reconcileRegisteredPeers() {
	logger := defaultLogger(a.logger)
	a.reconcilePeerPreparations(logger)
	a.mu.Lock()
	changed := false
	if a.retirements == nil {
		a.retirements = map[string]localPeer{}
	}
	for id, peer := range a.local {
		adapterStatus := exactProcessStatus(peerAdapterRoot(peer))
		lifecycleStatus := exactProcessStatus(exactProcess{
			PID: peer.LifecyclePID, Start: peer.LifecycleProcStart, StrongStart: peer.LifecycleStrongStart,
		})
		if lifecycleStatus == procinfo.Unknown || adapterStatus == procinfo.Unknown {
			continue
		}
		if lifecycleStatus != procinfo.Known && peer.PID != peer.LifecyclePID {
			a.retirements[id] = peer
		}
		if adapterStatus != procinfo.Known || lifecycleStatus != procinfo.Known || !probeUnix(peer.Socket, 50*time.Millisecond) {
			delete(a.local, id)
			changed = true
		}
	}
	retirements := make(map[string]localPeer, len(a.retirements))
	for id, peer := range a.retirements {
		retirements[id] = peer
	}
	a.mu.Unlock()
	if changed {
		a.signalLocalChanged()
	}
	for id, peer := range retirements {
		if err := retirePeerAdapter(peer); err != nil {
			logger.Printf("clean peer adapter artifacts %s failed: %v", id, err)
			continue
		}
		a.mu.Lock()
		current, exists := a.retirements[id]
		if exists && current.PID == peer.PID && current.ProcStart == peer.ProcStart {
			delete(a.retirements, id)
		}
		a.mu.Unlock()
	}
}

func (a *agent) reconcilePeerPreparations(logger interface{ Printf(string, ...any) }) {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	a.mu.RLock()
	prepared := make(map[string]peerPreparation, len(a.preparations))
	for sessionID, preparation := range a.preparations {
		prepared[sessionID] = preparation
	}
	a.mu.RUnlock()
	for sessionID, preparation := range prepared {
		registration := preparation.Registration
		lifecycle := exactProcessStatus(exactProcess{
			PID: registration.LifecyclePID, Start: registration.LifecycleProcStart,
			StrongStart: registration.LifecycleStrongStart,
		})
		if lifecycle == procinfo.Known || lifecycle == procinfo.Unknown {
			continue
		}
		peer := localPeer{
			Peer: Peer{
				ID: a.options.HostID + "/" + sessionID, HostID: a.options.HostID, HostName: a.options.HostName,
				SessionID: sessionID, Entrypoint: "claude",
			},
			PID: registration.PID, ProcStart: registration.ProcStart, Socket: registration.ClaudeSocketPath,
			LifecyclePID: registration.LifecyclePID, LifecycleProcStart: registration.LifecycleProcStart,
			AdapterStrongStart: registration.AdapterStrongStart, LifecycleStrongStart: registration.LifecycleStrongStart,
			LifecycleRoot: registration.LifecycleRoot, ClaudeConfigRoot: registration.ClaudeConfigRoot,
			ClaudeKeyBaseline:    append([]ClaudeKeyBaselineEntry(nil), registration.ClaudeKeyBaseline...),
			ClaudeKeyBaselineSet: registration.ClaudeKeyBaselineSet,
		}
		if err := retirePeerAdapter(peer); err != nil {
			logger.Printf("clean prepared Claude peer %s failed: %v", sessionID, err)
			continue
		}
		if preparation.RollbackPreferences && !preparation.Committed {
			if _, err := a.catalog.restorePrepared(preparation.DesiredPreference, preparation.PriorPreference); err != nil {
				logger.Printf("restore prepared Claude peer preferences %s failed: %v", sessionID, err)
				continue
			}
		}
		a.mu.Lock()
		current, ok := a.preparations[sessionID]
		if ok && samePreparedRegistration(current.Registration, registration) {
			if err := os.Remove(a.preparationPath(sessionID)); err != nil && !os.IsNotExist(err) {
				a.mu.Unlock()
				logger.Printf("remove prepared Claude peer %s failed: %v", sessionID, err)
				continue
			}
			delete(a.preparations, sessionID)
		}
		a.mu.Unlock()
	}
}

func (a *agent) signalLocalChanged() {
	select {
	case a.localChanged <- struct{}{}:
	default:
	}
}
