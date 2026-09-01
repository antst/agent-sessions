package federation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	privateGroupPrefix = "session:"
	maxGroupBytes      = 192
	maxSessionIDBytes  = 128
)

// Admission is one immutable routing decision. Broadcast membership is
// snapshotted here before any destination callback runs.
type Admission struct {
	Source  Peer
	Targets []Peer
}

// EffectiveGroups adds the mandatory host/session anchor to explicit and
// inherited membership while rejecting malformed or duplicate input.
func EffectiveGroups(hostID, sessionID string, explicit, inherited []string) ([]string, error) {
	if !validSimpleID(hostID) || !validSessionID(sessionID) {
		return nil, errors.New("effective groups require valid host and session identities")
	}
	values := make([]string, 0, len(explicit)+len(inherited)+1)
	for _, group := range explicit {
		if err := validateExplicitGroup(group); err != nil {
			return nil, err
		}
		values = append(values, group)
	}
	for _, group := range inherited {
		if err := validateEffectiveGroup(group); err != nil {
			return nil, err
		}
		values = append(values, group)
	}
	values = append(values, PrivateGroup(hostID, sessionID))
	return uniqueSorted(values)
}

// ChildGroups creates the fixed immediate-parent anchor and optionally copies
// the parent's other effective groups. It never infers ancestry from caller
// supplied names.
func ChildGroups(
	hostID, childSessionID, parentHostID, parentSessionID string,
	explicit, parentEffective []string,
	inheritParent bool,
) ([]string, error) {
	if !validSimpleID(parentHostID) || !validSessionID(parentSessionID) {
		return nil, errors.New("child groups require a valid parent identity")
	}
	anchor := PrivateGroup(parentHostID, parentSessionID)
	if !contains(parentEffective, anchor) {
		return nil, errors.New("parent effective groups omit its private anchor")
	}
	inherited := []string{anchor}
	if inheritParent {
		inherited = append([]string(nil), parentEffective...)
	}
	return EffectiveGroups(hostID, childSessionID, explicit, inherited)
}

// PrivateGroup returns the mandatory private host/session anchor.
func PrivateGroup(hostID, sessionID string) string {
	return privateGroupPrefix + hostID + "/" + sessionID
}

// Admit validates a discover, send, or broadcast request and returns its
// group-filtered immutable target snapshot.
func Admit(frame AgentFrame, source Peer, peers []Peer) (Admission, error) { //nolint:gocyclo
	if frame.Version != AgentFrameVersion {
		return Admission{}, errors.New("agent frame protocol is incompatible")
	}
	if strings.TrimSpace(frame.MessageID) == "" {
		return Admission{}, errors.New("agent frame requires message_id")
	}
	if len(frame.Content) > MaxAgentContent {
		return Admission{}, errors.New("agent frame content exceeds 1 MiB")
	}
	if strings.TrimSpace(source.ID) == "" || len(source.Groups) == 0 {
		return Admission{}, errors.New("source is not a live grouped peer")
	}
	visible := make([]Peer, 0, len(peers))
	for _, peer := range peers {
		if peer.ID != source.ID && groupsIntersect(source.Groups, peer.Groups) {
			visible = append(visible, clonePeer(peer))
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
	result := Admission{Source: clonePeer(source)}
	switch frame.Type {
	case "discover":
		if len(frame.Targets) != 0 || frame.Group != "" || frame.Content != "" {
			return Admission{}, errors.New("discover frame contains routing fields")
		}
		result.Targets = visible
		return result, nil
	case "send":
		if len(frame.Targets) == 0 || frame.Group != "" || frame.Content == "" {
			return Admission{}, errors.New("send requires targets and content")
		}
		if duplicateStrings(frame.Targets) {
			return Admission{}, errors.New("send targets contain duplicates")
		}
		resolved := map[string]bool{}
		for _, raw := range frame.Targets {
			peer, err := Resolve(raw, visible)
			if err != nil {
				return Admission{}, err
			}
			if resolved[peer.ID] {
				return Admission{}, errors.New("send targets resolve to a duplicate peer")
			}
			resolved[peer.ID] = true
			result.Targets = append(result.Targets, peer)
		}
		return result, nil
	case "broadcast":
		if frame.Group == "" || len(frame.Targets) != 0 || frame.Content == "" {
			return Admission{}, errors.New("broadcast requires group and content")
		}
		if !contains(source.Groups, frame.Group) {
			return Admission{}, errors.New("broadcast sender is not a member of the group")
		}
		for _, peer := range peers {
			if peer.ID != source.ID && contains(peer.Groups, frame.Group) {
				result.Targets = append(result.Targets, clonePeer(peer))
			}
		}
		sort.Slice(result.Targets, func(i, j int) bool { return result.Targets[i].ID < result.Targets[j].ID })
		return result, nil
	default:
		return Admission{}, fmt.Errorf("unsupported agent frame type %q", frame.Type)
	}
}

// Resolve applies exact identity first and unique visible-name resolution
// second. Hidden peers never participate because callers pass an admitted
// visible roster.
func Resolve(raw string, peers []Peer) (Peer, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "session:"))
	if raw == "" {
		return Peer{}, errors.New("peer target is empty")
	}
	matches := make([]Peer, 0, 1)
	for _, peer := range peers {
		for _, value := range []string{peer.ID, peer.SessionID, peer.GlobalID, peer.Name, peer.DisplayName} {
			if raw == value || strings.TrimPrefix(value, "session:") == raw {
				matches = append(matches, clonePeer(peer))
				break
			}
		}
	}
	if len(matches) == 0 {
		return Peer{}, fmt.Errorf("no live peer session or lane matching %q", raw)
	}
	if len(matches) != 1 {
		return Peer{}, fmt.Errorf("peer name %q is ambiguous; use an exact host/session identity", raw)
	}
	return matches[0], nil
}

func groupsIntersect(left, right []string) bool {
	seen := make(map[string]bool, len(left))
	for _, group := range left {
		seen[group] = true
	}
	for _, group := range right {
		if seen[group] {
			return true
		}
	}
	return false
}

func validateExplicitGroup(group string) error {
	if strings.HasPrefix(group, privateGroupPrefix) {
		return fmt.Errorf("explicit group %q uses the reserved private prefix", group)
	}
	return validateEffectiveGroup(group)
}

func validateEffectiveGroup(group string) error {
	if group == "" || group != strings.TrimSpace(group) || len(group) > maxGroupBytes {
		return fmt.Errorf("invalid group %q", group)
	}
	for _, value := range group {
		if unicode.IsLetter(value) || unicode.IsNumber(value) || strings.ContainsRune("._:/-", value) {
			continue
		}
		return fmt.Errorf("invalid group %q", group)
	}
	return nil
}

func uniqueSorted(values []string) ([]string, error) {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			return nil, fmt.Errorf("duplicate group %q", value)
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func duplicateStrings(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validSimpleID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

func validSessionID(value string) bool {
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
