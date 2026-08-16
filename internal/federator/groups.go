package federator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
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
	// SessionKindInteractive is a resumable top-level product peer.
	SessionKindInteractive = "interactive"
	// SessionKindLane is a resumable supervised worker lane.
	SessionKindLane = "lane"
)

// SessionPreferences is the small durable portion of one peer registration.
// Live addresses, process identities, names, and status remain runtime state.
type SessionPreferences struct {
	SessionID           string   `json:"session_id"`
	Product             string   `json:"product"`
	Kind                string   `json:"kind,omitempty"`
	ExplicitGroups      []string `json:"explicit_groups,omitempty"`
	InheritedGroups     []string `json:"inherited_groups,omitempty"`
	ParentSession       string   `json:"parent_session_id,omitempty"`
	ParentHostID        string   `json:"parent_host_id,omitempty"`
	InheritParentGroups bool     `json:"inherit_parent_groups"`
	AlwaysApprove       bool     `json:"always_approve"`
	UpdatedAt           int64    `json:"updated_at"`
}

// SessionPreferenceUpdate distinguishes omitted resume flags from explicit
// replacements. Omitted values restore the existing durable preference.
type SessionPreferenceUpdate struct {
	SessionID              string
	Product                string
	Kind                   string
	ExplicitGroups         []string
	GroupsSpecified        bool
	ParentSession          string
	ParentHostID           string
	ParentGroups           []string
	ParentSpecified        bool
	InheritParentGroups    bool
	InheritGroupsSpecified bool
	AlwaysApprove          bool
	AlwaysApproveSpecified bool
}

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
}

//nolint:gocyclo // Catalog reopening validates every durable invariant before use.
func openSessionCatalog(path, hostID string) (*sessionCatalog, error) {
	hostID = cleanID(hostID)
	if hostID == "" {
		return nil, errors.New("session catalog requires a host id")
	}
	catalog := &sessionCatalog{
		path: path, hostID: hostID, sessions: map[string]SessionPreferences{},
		now: func() int64 { return time.Now().UnixMilli() },
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
		catalog.sessions[key] = preference
	}
	return catalog, nil
}

//nolint:gocyclo // One transaction applies presence-sensitive resume and parent rules atomically.
func (c *sessionCatalog) update(update SessionPreferenceUpdate) (SessionPreferences, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	preference.UpdatedAt = c.now()
	candidate := clonePreferences(c.sessions)
	candidate[update.SessionID] = preference
	groups := c.effectiveGroups(preference)
	if err := c.write(candidate); err != nil {
		return SessionPreferences{}, nil, err
	}
	c.sessions = candidate
	return preference, groups, nil
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
	if value == "" || len(value) > 32 || value != strings.ToLower(value) {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return false
	}
	return true
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
		result[key] = value
	}
	return result
}
