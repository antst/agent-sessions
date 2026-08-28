package federation

import (
	"errors"
	"sort"
	"strings"
	"sync"
)

// RegistrySnapshot is one independent metadata-only view of the central hub
// registry. Revision changes only after a successful host/snapshot mutation.
type RegistrySnapshot struct {
	Revision uint64 `json:"revision"`
	Hosts    []Host `json:"hosts"`
	Peers    []Peer `json:"peers"`
}

type registeredHost struct {
	advertisement HostAdvertisement
	peers         map[string]Peer
}

// HubRegistry owns central host and global peer membership. Transport
// connections hold an opaque registration generation and cannot unregister a
// newer replacement for the same durable host ID.
type HubRegistry struct {
	mu         sync.RWMutex
	revision   uint64
	generation uint64
	hosts      map[string]registeredHost
	owners     map[string]uint64
}

// NewHubRegistry creates an empty one-hub global registry.
func NewHubRegistry() *HubRegistry {
	return &HubRegistry{hosts: make(map[string]registeredHost), owners: make(map[string]uint64)}
}

// RegisterHost validates the exact protocol and replaces any prior transport
// owner without introducing a release namespace. It returns the ownership
// generation required for later snapshot and unregister operations.
func (registry *HubRegistry) RegisterHost(advertisement HostAdvertisement) (uint64, error) {
	normalized, err := normalizeHostAdvertisement(advertisement)
	if err != nil {
		return 0, err
	}
	advertisement = normalized
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.generation++
	owner := registry.generation
	registry.hosts[advertisement.HostID] = registeredHost{
		advertisement: cloneAdvertisement(advertisement), peers: make(map[string]Peer),
	}
	registry.owners[advertisement.HostID] = owner
	registry.revision++
	return owner, nil
}

// ReplaceHostSnapshot atomically replaces one current connection's peer set.
func (registry *HubRegistry) ReplaceHostSnapshot(hostID string, owner uint64, peers []Peer) error {
	next := make(map[string]Peer, len(peers))
	sessions := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		if err := ValidateSnapshotPeer(peer, hostID); err != nil {
			return err
		}
		if _, duplicate := next[peer.ID]; duplicate {
			return errors.New("snapshot contains a duplicate peer identity")
		}
		if _, duplicate := sessions[peer.SessionID]; duplicate {
			return errors.New("snapshot contains a duplicate session identity")
		}
		sessions[peer.SessionID] = struct{}{}
		next[peer.ID] = clonePeer(peer)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	host, exists := registry.hosts[hostID]
	if !exists || registry.owners[hostID] != owner {
		return errors.New("snapshot owner is not the current host connection")
	}
	host.peers = next
	registry.hosts[hostID] = host
	registry.revision++
	return nil
}

// UnregisterHost removes a host only when the disconnecting transport still
// owns its current generation.
func (registry *HubRegistry) UnregisterHost(hostID string, owner uint64) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.owners[hostID] != owner {
		return false
	}
	delete(registry.owners, hostID)
	delete(registry.hosts, hostID)
	registry.revision++
	return true
}

// Snapshot returns the one global sorted host/peer space.
func (registry *HubRegistry) Snapshot() RegistrySnapshot {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	result := RegistrySnapshot{Revision: registry.revision}
	for _, registered := range registry.hosts {
		result.Hosts = append(result.Hosts, Host{
			ID: registered.advertisement.HostID, Name: registered.advertisement.HostName,
			Capabilities: append([]string(nil), registered.advertisement.Capabilities...),
		})
		for _, peer := range registered.peers {
			result.Peers = append(result.Peers, clonePeer(peer))
		}
	}
	sort.Slice(result.Hosts, func(i, j int) bool { return result.Hosts[i].ID < result.Hosts[j].ID })
	sort.Slice(result.Peers, func(i, j int) bool { return result.Peers[i].ID < result.Peers[j].ID })
	return result
}

// Peer resolves one exact global peer ID without exposing registry maps.
func (registry *HubRegistry) Peer(peerID string) (Peer, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	for _, host := range registry.hosts {
		if peer, ok := host.peers[peerID]; ok {
			return clonePeer(peer), true
		}
	}
	return Peer{}, false
}

// Host returns one current metadata-only host registration.
func (registry *HubRegistry) Host(hostID string) (Host, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	registered, ok := registry.hosts[hostID]
	if !ok {
		return Host{}, false
	}
	return Host{
		ID: registered.advertisement.HostID, Name: registered.advertisement.HostName,
		Capabilities: append([]string(nil), registered.advertisement.Capabilities...),
	}, true
}

func validateHostAdvertisement(advertisement HostAdvertisement) error {
	_, err := normalizeHostAdvertisement(advertisement)
	return err
}

func normalizeHostAdvertisement(advertisement HostAdvertisement) (HostAdvertisement, error) {
	if err := ValidateProtocolHandshake(advertisement.ProtocolVersion); err != nil {
		return HostAdvertisement{}, err
	}
	if !validSimpleID(advertisement.HostID) || strings.TrimSpace(advertisement.HostName) == "" ||
		strings.TrimSpace(advertisement.HostName) != advertisement.HostName {
		return HostAdvertisement{}, errors.New("host advertisement requires canonical host identity")
	}
	if advertisement.Generation == 0 || strings.TrimSpace(advertisement.RuntimeVersion) == "" ||
		strings.TrimSpace(advertisement.RuntimeIdentity) == "" {
		return HostAdvertisement{}, errors.New("host advertisement requires exact runtime generation and identity")
	}
	products := append([]string(nil), advertisement.Products...)
	sort.Strings(products)
	normalizedProducts := products[:0]
	for _, product := range products {
		if len(normalizedProducts) > 0 && normalizedProducts[len(normalizedProducts)-1] == product {
			continue
		}
		normalizedProducts = append(normalizedProducts, product)
	}
	wantCapabilities, err := CapabilitiesForReadyProducts(normalizedProducts)
	if err != nil {
		return HostAdvertisement{}, err
	}
	gotCapabilities := NormalizeCapabilities(advertisement.Capabilities)
	if !equalStrings(gotCapabilities, wantCapabilities) {
		return HostAdvertisement{}, errors.New("host advertisement capabilities do not match ready products")
	}
	advertisement.Products = append([]string(nil), normalizedProducts...)
	advertisement.Capabilities = gotCapabilities
	return advertisement, nil
}

func cloneAdvertisement(advertisement HostAdvertisement) HostAdvertisement {
	advertisement.Products = append([]string(nil), advertisement.Products...)
	advertisement.Capabilities = append([]string(nil), advertisement.Capabilities...)
	return advertisement
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
