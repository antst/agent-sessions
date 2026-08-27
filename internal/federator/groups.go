package federator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/antst/agent-sessions/internal/federation"
)

const (
	groupCatalogVersion = 2
	// Generated private anchors include a 48-byte host id and a 128-byte
	// session id. Keep one shared bound large enough to reopen every catalog
	// entry the same implementation can create.
	maxGroupBytes      = 192
	maxSessionIDBytes  = 128
	privateGroupPrefix = "session:"
)

const (
	// SessionKindInteractive remains a compatibility projection of the shared contract.
	SessionKindInteractive = federation.SessionKindInteractive
	// SessionKindLane remains a compatibility projection of the shared contract.
	SessionKindLane = federation.SessionKindLane
)

// SessionPreferences remains a compatibility alias during package convergence.
type SessionPreferences = federation.SessionPreferences

// QwenSessionMetadata remains a compatibility alias during package convergence.
type QwenSessionMetadata = federation.QwenSessionMetadata

// SessionPreferenceUpdate remains a compatibility alias during package convergence.
type SessionPreferenceUpdate = federation.SessionPreferenceUpdate

type sessionCatalogFile struct {
	Version  int                           `json:"version"`
	Sessions map[string]SessionPreferences `json:"sessions"`
}

type sessionCatalog struct {
	mu       sync.Mutex
	path     string
	hostID   string
	sessions map[string]SessionPreferences
	now      func() int64
	revision func() (string, error)
}

//nolint:gocyclo // Catalog reopening validates every durable invariant before use.
func openSessionCatalog(path, hostID string) (*sessionCatalog, error) {
	hostID = cleanID(hostID)
	if hostID == "" {
		return nil, errors.New("session catalog requires a host id")
	}
	catalog := &sessionCatalog{
		path: path, hostID: hostID, sessions: map[string]SessionPreferences{},
		now:      func() int64 { return time.Now().UnixMilli() },
		revision: newSessionPreferenceRevision,
	}
	body, err := os.ReadFile(path) //nolint:gosec // path is the configured bridge-owned catalog.
	if os.IsNotExist(err) {
		return catalog, nil
	}
	if err != nil {
		return nil, err
	}
	var file sessionCatalogFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("decode session catalog: %w", err)
	}
	if file.Version != groupCatalogVersion || file.Sessions == nil {
		return nil, fmt.Errorf("unsupported session catalog version %d", file.Version)
	}
	for key, preference := range file.Sessions {
		if key != preference.SessionID || !validCatalogSessionID(key) {
			return nil, errors.New("session catalog contains an invalid session id")
		}
		if !validProduct(preference.Product) {
			return nil, fmt.Errorf("session %s has an invalid product", key)
		}
		if preference.Kind != "" && !validSessionKind(preference.Kind) {
			return nil, fmt.Errorf("session %s has an invalid kind", key)
		}
		groups, normalizeErr := normalizeExplicitGroups(preference.ExplicitGroups)
		if normalizeErr != nil {
			return nil, fmt.Errorf("session %s: %w", key, normalizeErr)
		}
		if preference.ParentSession != "" && !validCatalogSessionID(preference.ParentSession) {
			return nil, fmt.Errorf("session %s has an invalid parent", key)
		}
		if preference.ParentHostID != "" && cleanID(preference.ParentHostID) != preference.ParentHostID {
			return nil, fmt.Errorf("session %s has an invalid parent host", key)
		}
		preference.ExplicitGroups = groups
		preference.InheritedGroups, normalizeErr = normalizeEffectiveGroups(preference.InheritedGroups)
		if normalizeErr != nil {
			return nil, fmt.Errorf("session %s: %w", key, normalizeErr)
		}
		if preference.InheritParentGroups && preference.ParentSession == "" {
			return nil, fmt.Errorf("session %s inherits groups without a parent", key)
		}
		if err := validateCatalogQwenMetadata(preference.Product, preference.Qwen); err != nil {
			return nil, fmt.Errorf("session %s: %w", key, err)
		}
		catalog.sessions[key] = preference
	}
	return catalog, nil
}

func (c *sessionCatalog) update(update SessionPreferenceUpdate) (SessionPreferences, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateLocked(update, true)
}

// preview applies every validation and inheritance rule without writing the
// catalog. Launchers use the result to construct native argv before a gated
// adapter exists; the agent re-evaluates the same update when it durably owns
// that adapter.
func (c *sessionCatalog) preview(update SessionPreferenceUpdate) (SessionPreferences, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateLocked(update, false)
}

//nolint:gocyclo // One transaction applies presence-sensitive resume and parent rules atomically.
func (c *sessionCatalog) updateLocked(update SessionPreferenceUpdate, persist bool) (SessionPreferences, []string, error) {
	if !validCatalogSessionID(update.SessionID) {
		return SessionPreferences{}, nil, errors.New("invalid session id")
	}
	preference, exists := c.sessions[update.SessionID]
	if !exists {
		preference.SessionID = update.SessionID
		if !validProduct(update.Product) {
			return SessionPreferences{}, nil, errors.New("new session requires a valid product")
		}
		preference.Product = update.Product
	} else if update.Product != "" && update.Product != preference.Product {
		return SessionPreferences{}, nil, errors.New("session product cannot change")
	}
	if update.Qwen != nil {
		if preference.Product != "qwen" {
			return SessionPreferences{}, nil, errors.New("qwen session metadata requires product qwen")
		}
		if err := validateCatalogQwenMetadata("qwen", update.Qwen); err != nil {
			return SessionPreferences{}, nil, err
		}
		if preference.Qwen != nil && !sameQwenResumeIdentity(*preference.Qwen, *update.Qwen) {
			return SessionPreferences{}, nil, errors.New("qwen resume profile or working directory cannot change")
		}
		qwen := *update.Qwen
		preference.Qwen = &qwen
	}
	switch {
	case !exists:
		if update.Kind == "" {
			update.Kind = SessionKindInteractive
		}
		if !validSessionKind(update.Kind) {
			return SessionPreferences{}, nil, errors.New("new session requires a valid kind")
		}
		preference.Kind = update.Kind
	case preference.Kind == "" && validSessionKind(update.Kind):
		preference.Kind = update.Kind
	case update.Kind != "" && update.Kind != preference.Kind:
		return SessionPreferences{}, nil, errors.New("session kind cannot change")
	}
	if update.GroupsSpecified {
		groups, err := normalizeExplicitGroups(update.ExplicitGroups)
		if err != nil {
			return SessionPreferences{}, nil, err
		}
		preference.ExplicitGroups = groups
	}
	requestedParentHost := update.ParentHostID
	if update.ParentSpecified && update.ParentSession != "" && requestedParentHost == "" {
		requestedParentHost = c.hostID
	}
	parentChanged := update.ParentSpecified && (update.ParentSession != preference.ParentSession ||
		requestedParentHost != preference.ParentHostID)
	if update.ParentSpecified {
		if update.ParentSession != "" && !validCatalogSessionID(update.ParentSession) {
			return SessionPreferences{}, nil, errors.New("invalid parent session id")
		}
		if update.ParentSession == update.SessionID {
			return SessionPreferences{}, nil, errors.New("session cannot be its own parent")
		}
		preference.ParentSession = update.ParentSession
		preference.ParentHostID = requestedParentHost
	}
	if parentChanged && !update.InheritGroupsSpecified {
		preference.InheritParentGroups = false
	}
	if update.InheritGroupsSpecified {
		preference.InheritParentGroups = update.InheritParentGroups
	}
	if parentChanged || update.InheritGroupsSpecified {
		switch {
		case preference.ParentSession == "":
			if preference.InheritParentGroups {
				return SessionPreferences{}, nil, errors.New("cannot inherit groups without a parent session")
			}
			preference.InheritedGroups = nil
		case preference.ParentHostID != "" && preference.ParentHostID != c.hostID:
			parentGroups, normalizeErr := normalizeEffectiveGroups(update.ParentGroups)
			if normalizeErr != nil {
				return SessionPreferences{}, nil, normalizeErr
			}
			anchor := privateGroupPrefix + preference.ParentHostID + "/" + preference.ParentSession
			if !containsStringValue(parentGroups, anchor) {
				return SessionPreferences{}, nil, errors.New("remote parent groups omit its private anchor")
			}
			preference.InheritedGroups = []string{anchor}
			if preference.InheritParentGroups {
				preference.InheritedGroups = parentGroups
			}
		default:
			parent, parentExists := c.sessions[preference.ParentSession]
			if !parentExists {
				return SessionPreferences{}, nil, errors.New("parent session is not registered")
			}
			preference.ParentHostID = c.hostID
			preference.InheritedGroups = []string{c.privateGroup(parent.SessionID)}
			if preference.InheritParentGroups {
				preference.InheritedGroups = c.effectiveGroups(parent)
			}
		}
	}
	if update.AlwaysApproveSpecified {
		preference.AlwaysApprove = update.AlwaysApprove
	}
	revision, err := c.revision()
	if err != nil {
		return SessionPreferences{}, nil, fmt.Errorf("create session preference revision: %w", err)
	}
	preference.Revision = revision
	now := c.now()
	if now <= preference.UpdatedAt {
		now = preference.UpdatedAt + 1
	}
	preference.UpdatedAt = now
	candidate := clonePreferences(c.sessions)
	candidate[update.SessionID] = preference
	groups := c.effectiveGroups(preference)
	if !persist {
		return preference, groups, nil
	}
	if err := c.write(candidate); err != nil {
		return SessionPreferences{}, nil, err
	}
	c.sessions = candidate
	return preference, groups, nil
}

// prepare writes a rollback journal before committing the previewed catalog
// decision. A crash can therefore restore an ordinary session's prior catalog
// state instead of leaving a failed peer opt-in adopted or modified.
func (c *sessionCatalog) prepare(
	update SessionPreferenceUpdate,
	expected SessionPreferences,
	persistJournal func(prior *SessionPreferences, desired SessionPreferences) error,
	discardJournal func() error,
) (SessionPreferences, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	desired, groups, err := c.updateLocked(update, false)
	if err != nil {
		return SessionPreferences{}, nil, err
	}
	if !samePreferenceDecision(desired, expected) {
		return SessionPreferences{}, nil, errors.New("session preferences changed before peer preparation")
	}
	var prior *SessionPreferences
	if current, ok := c.sessions[update.SessionID]; ok {
		priorPreference := clonePreference(current)
		prior = &priorPreference
	}
	if err := persistJournal(prior, desired); err != nil {
		return SessionPreferences{}, nil, err
	}
	candidate := clonePreferences(c.sessions)
	candidate[update.SessionID] = desired
	if err := c.write(candidate); err != nil {
		return SessionPreferences{}, nil, errors.Join(err, discardJournal())
	}
	c.sessions = candidate
	return desired, groups, nil
}

// restorePrepared rolls back only the exact decision owned by a failed gated
// launch. A later explicit catalog change is never overwritten.
func (c *sessionCatalog) restorePrepared(desired SessionPreferences, prior *SessionPreferences) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.sessions[desired.SessionID]
	if !ok || !samePreferenceRevision(current, desired) {
		return false, nil
	}
	candidate := clonePreferences(c.sessions)
	if prior == nil {
		delete(candidate, desired.SessionID)
	} else {
		candidate[desired.SessionID] = clonePreference(*prior)
	}
	if err := c.write(candidate); err != nil {
		return false, err
	}
	c.sessions = candidate
	return true, nil
}

func validSessionKind(kind string) bool {
	return kind == SessionKindInteractive || kind == SessionKindLane
}

func (c *sessionCatalog) get(sessionID string) (SessionPreferences, []string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	preference, ok := c.sessions[sessionID]
	if !ok {
		return SessionPreferences{}, nil, false, nil
	}
	return preference, c.effectiveGroups(preference), true, nil
}

func (c *sessionCatalog) write(sessions map[string]SessionPreferences) error {
	return writeJSONAtomic(c.path, sessionCatalogFile{Version: groupCatalogVersion, Sessions: sessions})
}

func (c *sessionCatalog) effectiveGroups(preference SessionPreferences) []string {
	groups := append([]string(nil), preference.ExplicitGroups...)
	groups = append(groups, preference.InheritedGroups...)
	groups = append(groups, c.privateGroup(preference.SessionID))
	return sortedUnique(groups)
}

func (c *sessionCatalog) privateGroup(sessionID string) string {
	return privateGroupPrefix + c.hostID + "/" + sessionID
}

func normalizeExplicitGroups(groups []string) ([]string, error) {
	result := make([]string, 0, len(groups))
	seen := map[string]bool{}
	for _, group := range groups {
		if err := validateExplicitGroup(group); err != nil {
			return nil, err
		}
		if seen[group] {
			return nil, fmt.Errorf("duplicate group %q", group)
		}
		seen[group] = true
		result = append(result, group)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeEffectiveGroups(groups []string) ([]string, error) {
	result := make([]string, 0, len(groups))
	seen := map[string]bool{}
	for _, group := range groups {
		if group == "" || len(group) > maxGroupBytes || group != strings.TrimSpace(group) {
			return nil, fmt.Errorf("invalid inherited group %q", group)
		}
		if seen[group] {
			return nil, fmt.Errorf("duplicate inherited group %q", group)
		}
		seen[group] = true
		result = append(result, group)
	}
	sort.Strings(result)
	return result, nil
}

func validateExplicitGroup(group string) error {
	if group == "" || group != strings.TrimSpace(group) || len(group) > maxGroupBytes || strings.HasPrefix(group, privateGroupPrefix) {
		return fmt.Errorf("invalid group %q", group)
	}
	for _, r := range group {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("._:/-", r) {
			continue
		}
		return fmt.Errorf("invalid group %q", group)
	}
	return nil
}

func validCatalogSessionID(value string) bool {
	if value == "" || len(value) > maxSessionIDBytes || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("._:-", r) {
			continue
		}
		return false
	}
	return true
}

func validProduct(value string) bool {
	_, ok := ProductByID(value)
	return ok
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func clonePreferences(source map[string]SessionPreferences) map[string]SessionPreferences {
	result := make(map[string]SessionPreferences, len(source))
	for key, value := range source {
		value.ExplicitGroups = append([]string(nil), value.ExplicitGroups...)
		value.InheritedGroups = append([]string(nil), value.InheritedGroups...)
		if value.Qwen != nil {
			qwen := *value.Qwen
			value.Qwen = &qwen
		}
		result[key] = value
	}
	return result
}

func clonePreference(value SessionPreferences) SessionPreferences {
	value.ExplicitGroups = append([]string(nil), value.ExplicitGroups...)
	value.InheritedGroups = append([]string(nil), value.InheritedGroups...)
	if value.Qwen != nil {
		qwen := *value.Qwen
		value.Qwen = &qwen
	}
	return value
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func validateCatalogQwenMetadata(product string, metadata *QwenSessionMetadata) error {
	if product != "qwen" {
		if metadata != nil {
			return errors.New("non-Qwen session contains Qwen metadata")
		}
		return nil
	}
	// Older catalogs created before first-class Qwen support have no managed
	// Qwen rows. Keep nil readable for forward migration while requiring the
	// payload on every new or updated managed Qwen launch.
	if metadata == nil {
		return nil
	}
	if !filepath.IsAbs(metadata.Cwd) || filepath.Clean(metadata.Cwd) != metadata.Cwd {
		return errors.New("qwen session metadata has an invalid canonical cwd")
	}
	profile := metadata.Profile
	if profile.Fingerprint == "" || (profile.QwenHomeSet && (!filepath.IsAbs(profile.QwenHome) || profile.QwenHome == "")) ||
		(profile.QwenRuntimeSet && (!filepath.IsAbs(profile.QwenRuntimeDir) || profile.QwenRuntimeDir == "")) {
		return errors.New("qwen session metadata has an invalid profile identity")
	}
	if metadata.LaunchPreference != "native_default" && metadata.LaunchPreference != "non_yolo" &&
		metadata.LaunchPreference != "yolo" && !strings.HasPrefix(metadata.LaunchPreference, "native:") {
		return errors.New("qwen session metadata has an invalid launch preference")
	}
	if strings.HasPrefix(metadata.LaunchPreference, "native:") && strings.TrimPrefix(metadata.LaunchPreference, "native:") == "" {
		return errors.New("qwen session metadata has an empty native launch mode")
	}
	expectedMode := map[string]string{
		"native_default": "native_default", "non_yolo": "default", "yolo": "yolo",
	}[metadata.LaunchPreference]
	if strings.HasPrefix(metadata.LaunchPreference, "native:") {
		expectedMode = strings.TrimPrefix(metadata.LaunchPreference, "native:")
	}
	if metadata.InitialModeRequest != expectedMode {
		return errors.New("qwen session metadata initial mode request does not match its launch preference")
	}
	return nil
}

func sameQwenResumeIdentity(left, right QwenSessionMetadata) bool {
	return left.Cwd == right.Cwd && reflect.DeepEqual(left.Profile, right.Profile)
}

func samePreferenceDecision(left, right SessionPreferences) bool {
	left.UpdatedAt, right.UpdatedAt = 0, 0
	left.Revision, right.Revision = "", ""
	return reflect.DeepEqual(clonePreference(left), clonePreference(right))
}

func samePreferenceRevision(left, right SessionPreferences) bool {
	return reflect.DeepEqual(clonePreference(left), clonePreference(right))
}

func newSessionPreferenceRevision() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
