package federator

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
	AttachmentID         string                   `json:"attachment_id,omitempty"`
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

const sessionNameRecordVersion = 1

type sessionNameRecord struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	Product   string `json:"product"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	UpdatedAt int64  `json:"updated_at"`
}

func (a *agent) sessionNameDirectory() string {
	if a.options.StateDir == "" {
		return ""
	}
	return filepath.Join(a.options.StateDir, "session-names")
}

func (a *agent) persistSessionName(peer Peer, kind string) error {
	directory := a.sessionNameDirectory()
	if directory == "" {
		return nil
	}
	path := filepath.Join(directory, sessionKey(peer.SessionID)+".json")
	var current sessionNameRecord
	if body, err := os.ReadFile(path); //nolint:gosec // hashed session ID below the agent-owned state directory.
	err == nil && json.Unmarshal(body, &current) == nil &&
		current.Version == sessionNameRecordVersion && current.SessionID == peer.SessionID &&
		current.Product == peer.Entrypoint && current.Kind == kind && current.Name == peer.Name {
		return nil
	}
	record := sessionNameRecord{
		Version: sessionNameRecordVersion, SessionID: peer.SessionID,
		Product: peer.Entrypoint, Kind: kind, Name: peer.Name, UpdatedAt: time.Now().UnixMilli(),
	}
	return writeJSONAtomic(path, record)
}

//nolint:gocyclo // Live priority, durable validation, and explicit ambiguity errors form one lookup policy.
func (a *agent) resolveSessionName(product, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if !validProduct(product) || requested == "" {
		return "", errors.New("session name lookup requires a product and non-empty name")
	}
	a.mu.RLock()
	liveCandidates := []string{}
	for _, peer := range a.local {
		if peer.Entrypoint == product && strings.EqualFold(peer.Name, requested) {
			liveCandidates = append(liveCandidates, peer.SessionID)
		}
	}
	a.mu.RUnlock()
	live := []string{}
	for _, sessionID := range liveCandidates {
		preference, _, ok, err := a.catalog.get(sessionID)
		if err != nil {
			return "", err
		}
		if ok && preference.Kind == SessionKindInteractive {
			live = append(live, sessionID)
		}
	}
	sort.Strings(live)
	if len(live) == 1 {
		return live[0], nil
	}
	if len(live) > 1 {
		return "", fmt.Errorf("session name %q is ambiguous; use an exact session UUID: %s", requested, strings.Join(live, ", "))
	}
	historical, err := a.historicalSessionNames(product)
	if err != nil {
		return "", err
	}
	matches := []string{}
	for sessionID, name := range historical {
		if strings.EqualFold(name, requested) {
			matches = append(matches, sessionID)
		}
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("session name %q is ambiguous; use an exact session UUID: %s", requested, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("no managed %s session is named %q", product, requested)
}

//nolint:gocyclo // Durable index validation and Claude transcript migration deliberately fail closed per session.
func (a *agent) historicalSessionNames(product string) (map[string]string, error) {
	candidates := map[string]bool{}
	a.catalog.mu.Lock()
	for sessionID, preference := range a.catalog.sessions {
		if preference.Product == product && (preference.Kind == "" || preference.Kind == SessionKindInteractive) {
			candidates[sessionID] = true
		}
	}
	a.catalog.mu.Unlock()
	names := map[string]string{}
	directory := a.sessionNameDirectory()
	if directory != "" {
		entries, err := os.ReadDir(directory)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			body, readErr := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // agent-owned directory entry.
			var record sessionNameRecord
			if readErr != nil || json.Unmarshal(body, &record) != nil || record.Version != sessionNameRecordVersion ||
				!validCatalogSessionID(record.SessionID) || entry.Name() != sessionKey(record.SessionID)+".json" ||
				record.Product != product || record.Kind != SessionKindInteractive || record.Name == "" {
				continue
			}
			_, managed := candidates[record.SessionID]
			if managed {
				names[record.SessionID] = record.Name
			}
		}
	}
	if product != "claude" {
		return names, nil
	}
	transcriptNames, invalid := a.claudeTranscriptSessionNames(candidates)
	for sessionID := range invalid {
		delete(names, sessionID)
	}
	for sessionID, name := range transcriptNames {
		if _, bad := invalid[sessionID]; !bad {
			names[sessionID] = name
		}
	}
	return names, nil
}

func (a *agent) claudeTranscriptSessionNames(
	candidates map[string]bool,
) (map[string]string, map[string]bool) {
	names := map[string]string{}
	invalid := map[string]bool{}
	projects := filepath.Join(a.options.ClaudeConfigDir, "projects")
	projectEntries, err := os.ReadDir(projects)
	if err != nil {
		return names, invalid
	}
	for _, project := range projectEntries {
		if !project.IsDir() {
			continue
		}
		for sessionID := range candidates {
			if _, bad := invalid[sessionID]; bad {
				continue
			}
			path := filepath.Join(projects, project.Name(), sessionID+".jsonl")
			info, statErr := os.Lstat(path)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil || !info.Mode().IsRegular() {
				invalid[sessionID] = true
				continue
			}
			title, titleErr := readClaudeTranscriptTitle(path, sessionID)
			if titleErr != nil {
				invalid[sessionID] = true
				continue
			}
			if title == "" {
				continue
			}
			if previous, exists := names[sessionID]; exists && previous != title {
				delete(names, sessionID)
				invalid[sessionID] = true
				continue
			}
			names[sessionID] = title
		}
	}
	return names, invalid
}

func (a *agent) claudeTranscriptSessionTitle(sessionID string) (string, bool) {
	names, invalid := a.claudeTranscriptSessionNames(map[string]bool{sessionID: true})
	if invalid[sessionID] {
		return "", false
	}
	title, ok := names[sessionID]
	return title, ok && title != ""
}

func readClaudeTranscriptTitle(path, expectedSessionID string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // exact catalog session filename below the configured Claude projects root.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	title := ""
	for scanner.Scan() {
		var event struct {
			Type        string `json:"type"`
			SessionID   string `json:"sessionId"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "custom-title" {
			continue
		}
		if event.SessionID != expectedSessionID {
			return "", errors.New("Claude transcript title identity does not match its catalog session") //nolint:staticcheck // Claude is a product name.
		}
		title = strings.TrimSpace(event.CustomTitle)
		if title == "" || len(title) > 512 {
			return "", errors.New("Claude transcript contains an invalid session title") //nolint:staticcheck // Claude is a product name.
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return title, nil
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
	AgentRuntimeDir  string   `json:"agent_runtime_dir,omitempty"`
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

// PrepareClaudePeerSelection gives the host agent durable cleanup ownership
// before native Claude resolves a named resume target to its actual session ID.
func PrepareClaudePeerSelection(runtimeDir string, registration PeerRegistration) error {
	_, err := requestAgentControl(runtimeDir, Message{Type: "peer_prepare_selection", Registration: &registration})
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

// PromoteClaudePeerSelection atomically binds a gated named-resume attachment
// to the actual session ID published by native Claude and adopts its durable
// preferences. The preparation remains keyed by its launch-scoped attachment
// ID so crash cleanup has no rename window.
func PromoteClaudePeerSelection(
	runtimeDir string,
	registration PeerRegistration,
	request ResolvePreferencesRequest,
	expected SessionPreferences,
) (ResolvedPreferences, error) {
	message := preferenceMessage("peer_promote_selection", request)
	message.Registration = &registration
	message.Preference = &expected
	response, err := requestAgentControl(runtimeDir, message)
	if err != nil {
		return ResolvedPreferences{}, err
	}
	if response.Type != "peer_promote_selection" || response.Preference == nil {
		return ResolvedPreferences{}, errors.New("agent returned no promoted peer preferences")
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

func peerPreparationID(registration PeerRegistration) string {
	if registration.AttachmentID != "" {
		return registration.AttachmentID
	}
	return registration.SessionID
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
		preparationID := peerPreparationID(registration)
		if !validCatalogSessionID(registration.SessionID) || !validCatalogSessionID(preparationID) ||
			entry.Name() != sessionKey(preparationID)+".json" ||
			registration.AdapterStrongStart == "" || registration.LifecycleStrongStart == "" ||
			!validClaudeKeyBaseline(registration.PID, registration.ClaudeKeyBaselineSet, registration.ClaudeKeyBaseline) ||
			!validLoadedClaudePeerSocket(a.options.RuntimeDir, registration) ||
			(preparation.RollbackPreferences && preparation.DesiredPreference.SessionID != registration.SessionID) {
			return errors.New("invalid durable Claude peer preparation")
		}
		a.preparations[preparationID] = preparation
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
	preparationID := peerPreparationID(registration)
	if registration.Version != GroupProtocolVersion || registration.Product != "claude" ||
		!validCatalogSessionID(registration.SessionID) || !validCatalogSessionID(preparationID) ||
		registration.PID <= 1 || registration.ProcStart == "" ||
		registration.LifecyclePID <= 1 || registration.LifecycleProcStart == "" ||
		registration.LifecycleRoot == "" || registration.ClaudeConfigRoot == "" || registration.Socket != "" {
		return errors.New("invalid Claude peer preparation")
	}
	expectedRoot := ClaudePeerLifecycleRootInState(a.options.StateDir, preparationID)
	if a.options.StateDir == "" {
		expectedRoot = ClaudePeerLifecycleRoot(a.options.HostID, preparationID)
	}
	if filepath.Clean(registration.LifecycleRoot) != filepath.Clean(expectedRoot) ||
		!sameRegistryRoot(registration.ClaudeConfigRoot, a.options.ClaudeConfigDir) {
		return errors.New("claude peer preparation roots are not agent-owned")
	}
	if !registration.ClaudeKeyBaselineSet {
		return errors.New("claude peer preparation key baseline is missing")
	}
	if registration.ClaudeSocketPathSet {
		expectedSocket := ClaudePeerMessagingSocketPath(a.options.RuntimeDir, preparationID)
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
	return a.persistUncommittedClaudePreparation(registration)
}

func (a *agent) prepareClaudePeerSelection(registration PeerRegistration) error {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	if registration.AttachmentID == "" || registration.AttachmentID != registration.SessionID {
		return errors.New("named Claude selection requires a provisional attachment ID")
	}
	if err := a.validateClaudePreparationIdentity(registration); err != nil {
		return err
	}
	return a.persistUncommittedClaudePreparation(registration)
}

func (a *agent) persistUncommittedClaudePreparation(registration PeerRegistration) error {
	adapter := procinfo.Read(registration.PID)
	lifecycle := procinfo.Read(registration.LifecyclePID)
	if adapter.Status != procinfo.Known || adapter.Start != registration.ProcStart || adapter.StrongStart == "" ||
		lifecycle.Status != procinfo.Known || lifecycle.Start != registration.LifecycleProcStart || lifecycle.StrongStart == "" {
		return errors.New("claude peer preparation identity changed before persistence")
	}
	registration.AdapterStrongStart = adapter.StrongStart
	registration.LifecycleStrongStart = lifecycle.StrongStart
	preparationID := peerPreparationID(registration)
	a.mu.Lock()
	defer a.mu.Unlock()
	if current, ok := a.preparations[preparationID]; ok &&
		!samePreparedRegistration(current.Registration, registration) {
		return errors.New("claude peer attachment is already prepared")
	}
	preparation := peerPreparation{Registration: registration}
	if err := writeJSONAtomic(a.preparationPath(preparationID), preparation); err != nil {
		return err
	}
	a.preparations[preparationID] = preparation
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
	preparationID := peerPreparationID(registration)
	persist := func(prior *SessionPreferences, desired SessionPreferences) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if current, ok := a.preparations[preparationID]; ok &&
			!samePreparedRegistration(current.Registration, registration) {
			return errors.New("claude peer session already has another prepared attachment")
		}
		preparation := peerPreparation{
			Registration: registration, PriorPreference: prior, DesiredPreference: desired, RollbackPreferences: true,
		}
		if err := writeJSONAtomic(a.preparationPath(preparationID), preparation); err != nil {
			return err
		}
		a.preparations[preparationID] = preparation
		return nil
	}
	discard := func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		current, ok := a.preparations[preparationID]
		if !ok || !samePreparedRegistration(current.Registration, registration) {
			return nil
		}
		if err := os.Remove(a.preparationPath(preparationID)); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(a.preparations, preparationID)
		return nil
	}
	return a.catalog.prepare(update, expected, persist, discard)
}

func (a *agent) promoteClaudePeerSelection(
	registration PeerRegistration,
	update SessionPreferenceUpdate,
	expected SessionPreferences,
) (SessionPreferences, []string, error) {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	promotion, err := a.validateClaudePeerSelectionPromotion(registration, update)
	if err != nil {
		return SessionPreferences{}, nil, err
	}
	return a.catalog.prepare(update, expected, promotion.persist, promotion.discard)
}

type claudePeerSelectionPromotion struct {
	agent         *agent
	preparationID string
	current       peerPreparation
	prepared      PeerRegistration
	promoted      PeerRegistration
}

func (a *agent) validateClaudePeerSelectionPromotion(
	registration PeerRegistration,
	update SessionPreferenceUpdate,
) (claudePeerSelectionPromotion, error) {
	preparationID := peerPreparationID(registration)
	if !validClaudePeerSelectionPromotionRequest(registration, update) {
		return claudePeerSelectionPromotion{}, errors.New("invalid named Claude selection promotion")
	}
	a.mu.RLock()
	current, ok := a.preparations[preparationID]
	a.mu.RUnlock()
	if !validUnpromotedClaudeSelection(current, ok, preparationID, registration) {
		return claudePeerSelectionPromotion{}, errors.New("named Claude selection preparation changed before promotion")
	}
	preparedRegistration := current.Registration
	if preparedRegistration.ClaudeSocketPathSet && registration.Socket != preparedRegistration.ClaudeSocketPath {
		return claudePeerSelectionPromotion{}, errors.New("named Claude selection published an unexpected messaging socket")
	}
	if !livePreparedClaudeSelection(preparedRegistration) {
		return claudePeerSelectionPromotion{}, errors.New("named Claude selection identity changed before promotion")
	}
	promotedRegistration := preparedRegistration
	promotedRegistration.SessionID = registration.SessionID
	return claudePeerSelectionPromotion{
		agent: a, preparationID: preparationID, current: current,
		prepared: preparedRegistration, promoted: promotedRegistration,
	}, nil
}

func validClaudePeerSelectionPromotionRequest(
	registration PeerRegistration,
	update SessionPreferenceUpdate,
) bool {
	return registration.AttachmentID != "" && registration.AttachmentID != registration.SessionID &&
		validClaudeNativeSessionID(registration.SessionID) && update.SessionID == registration.SessionID
}

func validClaudeNativeSessionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func validUnpromotedClaudeSelection(
	current peerPreparation,
	exists bool,
	preparationID string,
	registration PeerRegistration,
) bool {
	return exists && !current.RollbackPreferences && !current.Committed &&
		current.Registration.SessionID == preparationID &&
		samePreparedRegistrationWeak(current.Registration, registration)
}

func livePreparedClaudeSelection(registration PeerRegistration) bool {
	adapter := procinfo.Read(registration.PID)
	lifecycle := procinfo.Read(registration.LifecyclePID)
	return adapter.Status == procinfo.Known && adapter.Start == registration.ProcStart &&
		adapter.StrongStart == registration.AdapterStrongStart &&
		lifecycle.Status == procinfo.Known && lifecycle.Start == registration.LifecycleProcStart &&
		lifecycle.StrongStart == registration.LifecycleStrongStart
}

func (p claudePeerSelectionPromotion) persist(prior *SessionPreferences, desired SessionPreferences) error {
	p.agent.mu.Lock()
	defer p.agent.mu.Unlock()
	latest, exists := p.agent.preparations[p.preparationID]
	if !exists || !samePreparedRegistration(latest.Registration, p.prepared) ||
		latest.RollbackPreferences || latest.Committed {
		return errors.New("named Claude selection preparation changed during promotion")
	}
	promoted := peerPreparation{
		Registration: p.promoted, PriorPreference: prior,
		DesiredPreference: desired, RollbackPreferences: true,
	}
	if err := writeJSONAtomic(p.agent.preparationPath(p.preparationID), promoted); err != nil {
		return err
	}
	p.agent.preparations[p.preparationID] = promoted
	return nil
}

func (p claudePeerSelectionPromotion) discard() error {
	p.agent.mu.Lock()
	defer p.agent.mu.Unlock()
	latest, exists := p.agent.preparations[p.preparationID]
	if !exists || !samePreparedRegistration(latest.Registration, p.promoted) || latest.Committed {
		return nil
	}
	if err := writeJSONAtomic(p.agent.preparationPath(p.preparationID), p.current); err != nil {
		return err
	}
	p.agent.preparations[p.preparationID] = p.current
	return nil
}

func (a *agent) cancelPeerPreparation(registration PeerRegistration) error {
	a.preparationMu.Lock()
	defer a.preparationMu.Unlock()
	preparationID := peerPreparationID(registration)
	a.mu.RLock()
	current, ok := a.preparations[preparationID]
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
	latest, ok := a.preparations[preparationID]
	if !ok {
		return nil
	}
	if !samePreparedRegistration(latest.Registration, preparedRegistration) {
		return errors.New("claude peer preparation identity changed during cancellation")
	}
	if err := os.Remove(a.preparationPath(preparationID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(a.preparations, preparationID)
	return nil
}

func samePreparedRegistration(left, right PeerRegistration) bool {
	return left.SessionID == right.SessionID && peerPreparationID(left) == peerPreparationID(right) &&
		left.PID == right.PID && left.ProcStart == right.ProcStart &&
		left.LifecyclePID == right.LifecyclePID && left.LifecycleProcStart == right.LifecycleProcStart &&
		left.AdapterStrongStart == right.AdapterStrongStart && left.LifecycleStrongStart == right.LifecycleStrongStart &&
		left.ClaudeKeyBaselineSet == right.ClaudeKeyBaselineSet &&
		left.ClaudeSocketPathSet == right.ClaudeSocketPathSet && left.ClaudeSocketPath == right.ClaudeSocketPath &&
		slices.Equal(left.ClaudeKeyBaseline, right.ClaudeKeyBaseline)
}

func samePreparedRegistrationWeak(left, right PeerRegistration) bool {
	return peerPreparationID(left) == peerPreparationID(right) &&
		left.PID == right.PID && left.ProcStart == right.ProcStart &&
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
		registration.ProcStart == "" || registration.Socket == "" ||
		registration.AttachmentID != "" &&
			(registration.Product != "claude" || !validCatalogSessionID(registration.AttachmentID)) {
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
			preparationID := peerPreparationID(registration)
			expected := ClaudePeerLifecycleRootInState(a.options.StateDir, preparationID)
			if a.options.StateDir == "" {
				expected = ClaudePeerLifecycleRoot(a.options.HostID, preparationID)
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
	peerName := registration.Name
	if registration.Product == "claude" {
		if title, ok := a.claudeTranscriptSessionTitle(registration.SessionID); ok {
			peerName = title
		}
	}
	id := a.options.HostID + "/" + registration.SessionID
	preparationID := peerPreparationID(registration)
	a.mu.Lock()
	if _, retiring := a.retirements[id]; retiring {
		a.mu.Unlock()
		return Peer{}, errors.New("session adapter retirement is still pending")
	}
	current, exists := a.local[id]
	prepared, preparedExists := a.preparations[preparationID]
	preparedRegistration := prepared.Registration
	if registration.AttachmentID != "" && !preparedExists {
		a.mu.Unlock()
		return Peer{}, errors.New("named Claude selection has no prepared attachment")
	}
	if preparedExists && (preparedRegistration.SessionID != registration.SessionID ||
		peerPreparationID(preparedRegistration) != preparationID ||
		preparedRegistration.PID != registration.PID || preparedRegistration.ProcStart != registration.ProcStart ||
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
		Name: cleanPeerName(peerName), DisplayName: qualifiedName(peerName, a.options.HostName),
		Status: defaultString(registration.Status, "idle"), Cwd: registration.Cwd,
		Entrypoint: registration.Product, PermissionMode: defaultString(registration.PermissionMode, "default"),
		StartedAt: startedAt, PeerProtocol: GroupProtocolVersion,
		InstanceID: sessionKey(fmt.Sprintf("%s\x00%d\x00%s", a.options.HostID, registration.PID, registration.ProcStart)),
		Groups:     groups, ParentSessionID: preference.ParentSession,
	}
	preferenceKind := preference.Kind
	if preferenceKind == "" {
		preferenceKind = SessionKindInteractive
	}
	if preparedExists && !prepared.Committed {
		prepared.Committed = true
		if err := writeJSONAtomic(a.preparationPath(preparationID), prepared); err != nil {
			a.mu.Unlock()
			return Peer{}, err
		}
		a.preparations[preparationID] = prepared
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
	if err := a.persistSessionName(peer, preferenceKind); err != nil {
		defaultLogger(a.logger).Printf("persist peer session name %s: %v", registration.SessionID, err)
	}
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
	expectedName := filepath.Base(ClaudePeerMessagingSocketPath(runtimeDir, peerPreparationID(registration)))
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
	resolvedSessionID := sessionID
	peer, ok := localPeerBySession(a.local, resolvedSessionID)
	if !ok {
		if prepared, exists := a.preparations[sessionID]; exists && prepared.Committed &&
			prepared.Registration.SessionID != sessionID {
			resolvedSessionID = prepared.Registration.SessionID
			peer, ok = localPeerBySession(a.local, resolvedSessionID)
		}
	}
	a.mu.RUnlock()
	if !ok {
		return ParentContext{}, errors.New("parent session is not a live local peer")
	}
	preference, groups, exists, err := a.catalog.get(resolvedSessionID)
	if err != nil {
		return ParentContext{}, err
	}
	if !exists || preference.Product != peer.Entrypoint {
		return ParentContext{}, errors.New("parent session preferences do not match its live registration")
	}
	return ParentContext{
		HostID: a.options.HostID, SessionID: resolvedSessionID, Product: preference.Product,
		InstanceID: peer.InstanceID, Groups: groups,
		AlwaysApprove: preference.AlwaysApprove, AgentRuntimeDir: absolutePathOrOriginal(a.options.RuntimeDir),
		AdapterPID: peer.PID, AdapterProcStart: peer.ProcStart, AdapterSocket: peer.Socket,
		PID: peer.LifecyclePID, ProcStart: peer.LifecycleProcStart,
		PermissionMode: peer.PermissionMode,
	}, nil
}

func absolutePathOrOriginal(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
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
	for preparationID, preparation := range prepared {
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
				ID: a.options.HostID + "/" + registration.SessionID, HostID: a.options.HostID, HostName: a.options.HostName,
				SessionID: registration.SessionID, Entrypoint: "claude",
			},
			PID: registration.PID, ProcStart: registration.ProcStart, Socket: registration.ClaudeSocketPath,
			LifecyclePID: registration.LifecyclePID, LifecycleProcStart: registration.LifecycleProcStart,
			AdapterStrongStart: registration.AdapterStrongStart, LifecycleStrongStart: registration.LifecycleStrongStart,
			LifecycleRoot: registration.LifecycleRoot, ClaudeConfigRoot: registration.ClaudeConfigRoot,
			ClaudeKeyBaseline:    append([]ClaudeKeyBaselineEntry(nil), registration.ClaudeKeyBaseline...),
			ClaudeKeyBaselineSet: registration.ClaudeKeyBaselineSet,
			ClaudeSessionUnresolved: registration.AttachmentID != "" &&
				registration.AttachmentID == registration.SessionID && !preparation.RollbackPreferences,
		}
		if err := retirePeerAdapter(peer); err != nil {
			logger.Printf("clean prepared Claude peer %s failed: %v", registration.SessionID, err)
			continue
		}
		if preparation.RollbackPreferences && !preparation.Committed {
			if _, err := a.catalog.restorePrepared(preparation.DesiredPreference, preparation.PriorPreference); err != nil {
				logger.Printf("restore prepared Claude peer preferences %s failed: %v", registration.SessionID, err)
				continue
			}
		}
		a.mu.Lock()
		current, ok := a.preparations[preparationID]
		if ok && samePreparedRegistration(current.Registration, registration) {
			if err := os.Remove(a.preparationPath(preparationID)); err != nil && !os.IsNotExist(err) {
				a.mu.Unlock()
				logger.Printf("remove prepared Claude peer %s failed: %v", registration.SessionID, err)
				continue
			}
			delete(a.preparations, preparationID)
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
