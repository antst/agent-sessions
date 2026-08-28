package federation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// HostAdvertisement is the metadata-only identity and ready-operation set a
// host daemon publishes at the protocol handshake. Release identities are
// diagnostic evidence only; protocol equality is the compatibility gate.
type HostAdvertisement struct {
	HostID          string   `json:"host_id"`
	HostName        string   `json:"host_name"`
	ProtocolVersion int      `json:"protocol_version"`
	RuntimeVersion  string   `json:"runtime_version"`
	RuntimeIdentity string   `json:"runtime_identity"`
	Generation      uint64   `json:"generation"`
	Products        []string `json:"products"`
	Capabilities    []string `json:"capabilities"`
}

// ValidateProtocolHandshake rejects incompatible software before a host is
// registered or work is accepted. Runtime version and build identity are
// deliberately not inputs.
func ValidateProtocolHandshake(version int) error {
	if CompatibleProtocol(version) {
		return nil
	}
	return fmt.Errorf("federation protocol %d is incompatible; matching protocol %d is required", version, ProtocolVersion)
}

// NormalizeCapabilities returns the closed, sorted operation inventory. An
// unknown future operation does not change protocol compatibility and is not
// advertised as locally executable.
func NormalizeCapabilities(values []string) []string {
	known := make(map[string]struct{}, len(productcatalog.ProductDescriptors()))
	for _, product := range productcatalog.ProductDescriptors() {
		known[product.LaneCapability] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := known[value]; !ok {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// CapabilitiesForReadyProducts derives operations solely from products whose
// native adapters the host daemon has already proven ready.
func CapabilitiesForReadyProducts(products []string) ([]string, error) {
	capabilities := make([]string, 0, len(products))
	seen := make(map[string]struct{}, len(products))
	for _, productID := range products {
		product, ok := productcatalog.ProductByID(productID)
		if !ok {
			return nil, fmt.Errorf("unsupported ready product %q", productID)
		}
		if _, duplicate := seen[product.ID]; duplicate {
			continue
		}
		seen[product.ID] = struct{}{}
		capabilities = append(capabilities, product.LaneCapability)
	}
	return NormalizeCapabilities(capabilities), nil
}

// HostSupportsCapability reports operation availability without consulting a
// release or source identity.
func HostSupportsCapability(host Host, capability string) bool {
	for _, advertised := range host.Capabilities {
		if advertised == capability {
			return true
		}
	}
	return false
}

// LocalCatalog is the role-neutral in-memory projection of daemon-owned peer
// state. The daemon composes durable storage and calls Replace; this type does
// not introduce a second state owner.
type LocalCatalog struct {
	mu       sync.RWMutex
	hostID   string
	hostName string
	peers    map[string]Peer
}

// NewLocalCatalog creates the live projection for one durable host identity.
func NewLocalCatalog(hostID, hostName string) (*LocalCatalog, error) {
	if !validSimpleID(hostID) || strings.TrimSpace(hostName) == "" || strings.TrimSpace(hostName) != hostName {
		return nil, errors.New("local federation catalog requires canonical host identity")
	}
	return &LocalCatalog{hostID: hostID, hostName: hostName, peers: make(map[string]Peer)}, nil
}

// Replace atomically validates and publishes the daemon's full local peer
// projection. A rejected snapshot leaves the prior catalog unchanged.
func (catalog *LocalCatalog) Replace(peers []Peer) error {
	next := make(map[string]Peer, len(peers))
	sessions := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		if err := ValidateSnapshotPeer(peer, catalog.hostID); err != nil {
			return err
		}
		if _, duplicate := next[peer.ID]; duplicate {
			return errors.New("local federation snapshot contains a duplicate peer identity")
		}
		if _, duplicate := sessions[peer.SessionID]; duplicate {
			return errors.New("local federation snapshot contains a duplicate session identity")
		}
		sessions[peer.SessionID] = struct{}{}
		next[peer.ID] = clonePeer(peer)
	}
	catalog.mu.Lock()
	catalog.peers = next
	catalog.mu.Unlock()
	return nil
}

// Snapshot returns an independent, stable-order wire projection.
func (catalog *LocalCatalog) Snapshot() []Peer {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return sortedPeerMap(catalog.peers)
}

// ValidateSnapshotPeer applies the same identity/group/product gate at both
// the host catalog and hub registry boundaries.
func ValidateSnapshotPeer(peer Peer, hostID string) error {
	if peer.HostID != hostID || !validSimpleID(peer.SessionID) ||
		peer.ID != hostID+"/"+peer.SessionID || peer.GlobalID != GlobalSessionID(hostID, peer.SessionID) {
		return errors.New("snapshot contains an invalid peer identity")
	}
	if peer.PeerProtocol != GroupProtocolVersion || strings.TrimSpace(peer.Name) == "" ||
		strings.TrimSpace(peer.InstanceID) == "" {
		return errors.New("snapshot contains an incompatible grouped peer")
	}
	if _, ok := productcatalog.ProductByID(peer.Entrypoint); !ok {
		return errors.New("snapshot contains an unsupported product")
	}
	groups, err := normalizeRoutingGroups(peer.Groups)
	if err != nil || len(groups) == 0 || !containsSorted(groups, "session:"+hostID+"/"+peer.SessionID) {
		return errors.New("snapshot contains invalid peer groups")
	}
	return nil
}

func validSimpleID(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func normalizeRoutingGroups(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" {
			return nil, errors.New("group names must be non-empty and canonical")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func containsSorted(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

func clonePeer(peer Peer) Peer {
	peer.Groups = append([]string(nil), peer.Groups...)
	return peer
}

func sortedPeerMap(source map[string]Peer) []Peer {
	result := make([]Peer, 0, len(source))
	for _, peer := range source {
		result = append(result, clonePeer(peer))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
