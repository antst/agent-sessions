package releaseinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

var (
	ErrInstallDrift      = errors.New("native integration identity drift")
	ErrInstallDebt       = errors.New("native integration cleanup debt")
	ErrInstallInProgress = errors.New("native integration transaction requires recovery")
)

// NativeToken, NativeRevision, and NativeDigest make the only serializable
// native identity components explicit. Validation accepts a bounded product
// token, a secret-shaped-content-rejecting revision, and a SHA-256 digest;
// paths, argv, environment, credentials, endpoints, and configuration bodies
// have no representable identity field.
type NativeToken string
type NativeRevision string
type NativeDigest string

// NativeIdentity is the complete secret-free authority used for exact native
// removal.
type NativeIdentity struct {
	ResourceKey NativeToken    `json:"resource_key"`
	Kind        NativeToken    `json:"kind"`
	Revision    NativeRevision `json:"revision"`
	Digest      NativeDigest   `json:"digest"`
}

func (identity NativeIdentity) Equal(other NativeIdentity) bool { return identity == other }

// MarshalJSON makes the secret-free identity grammar a serialization
// boundary, not merely a store-level convention.
func (identity NativeIdentity) MarshalJSON() ([]byte, error) {
	if err := validateIdentity(identity); err != nil {
		return nil, err
	}
	type wireIdentity NativeIdentity
	return json.Marshal(wireIdentity(identity))
}

type Discovery struct {
	Present  bool
	Identity *NativeIdentity
}

type Baseline struct {
	Current *NativeIdentity
}

type CapturedChange struct {
	Baseline Baseline
	Change   Change
}

type RemovalRequest struct {
	Installed NativeIdentity
	Prior     *NativeIdentity
}

// CleanupRequest identifies deterministic AS-owned staging for one
// transaction. ReconcileCleanup must be idempotent and return nil only after
// it has verified that the staged assets are absent.
type CleanupRequest struct {
	TransactionID string
	Planned       NativeIdentity
}

// Change owns one prepared native mutation. Stage and Validate must have no
// native side effects; Register is the first native mutation.
type Change interface {
	Stage(context.Context) error
	Validate(context.Context) error
	PlannedIdentity(context.Context) (NativeIdentity, error)
	Register(context.Context) (NativeIdentity, error)
	Verify(context.Context, NativeIdentity) error
	Rollback(context.Context, Baseline, *NativeIdentity) error
	Abort(context.Context) error
}

// Strategy is an explicit product-neutral native registration implementation.
// Capture returns the exact baseline and a change bound to that baseline.
type Strategy interface {
	Key() string
	ValidateDescriptor(productcatalog.Descriptor) error
	Discover(context.Context, productcatalog.Descriptor) (Discovery, error)
	Capture(context.Context, productcatalog.Descriptor, Discovery) (CapturedChange, error)
	Remove(context.Context, productcatalog.Descriptor, RemovalRequest) error
	ReconcileCleanup(context.Context, productcatalog.Descriptor, CleanupRequest) error
}

// Registry is immutable after explicit construction. Package init side effects
// and fallback strategies are intentionally unsupported.
type Registry struct {
	strategies    map[string]Strategy
	registrations map[string]productcatalog.NativeRegistration
}

func NewRegistry(inventory []productcatalog.Descriptor, strategies ...Strategy) (*Registry, error) {
	if err := productcatalog.ValidateInventory(inventory); err != nil {
		return nil, fmt.Errorf("validate install inventory: %w", err)
	}
	required := make(map[string][]productcatalog.Descriptor)
	for _, descriptor := range inventory {
		if descriptor.NativeRegistration.Strategy == "" {
			continue
		}
		required[descriptor.NativeRegistration.Strategy] = append(required[descriptor.NativeRegistration.Strategy], descriptor)
	}
	resolved := make(map[string]Strategy, len(strategies))
	for index, strategy := range strategies {
		if strategy == nil {
			return nil, fmt.Errorf("install strategy %d is nil", index)
		}
		key := strategy.Key()
		if err := productcatalog.ValidateToken(key); err != nil {
			return nil, fmt.Errorf("install strategy key %q: %w", key, err)
		}
		if _, exists := resolved[key]; exists {
			return nil, fmt.Errorf("duplicate install strategy %q", key)
		}
		if _, used := required[key]; !used {
			return nil, fmt.Errorf("extra install strategy %q", key)
		}
		resolved[key] = strategy
		for _, descriptor := range required[key] {
			if err := strategy.ValidateDescriptor(descriptor); err != nil {
				return nil, fmt.Errorf("validate %s install descriptor: %w", descriptor.ID, err)
			}
		}
	}
	for key := range required {
		if resolved[key] == nil {
			return nil, fmt.Errorf("missing install strategy %q", key)
		}
	}
	registrations := make(map[string]productcatalog.NativeRegistration, len(inventory))
	for _, descriptor := range inventory {
		registrations[descriptor.ID] = productcatalog.NativeRegistration{
			Strategy:  descriptor.NativeRegistration.Strategy,
			Args:      append([]string(nil), descriptor.NativeRegistration.Args...),
			AssetOnly: descriptor.NativeRegistration.AssetOnly,
		}
	}
	return &Registry{strategies: resolved, registrations: registrations}, nil
}

func (registry *Registry) validateInventory(inventory []productcatalog.Descriptor) error {
	if registry == nil || len(registry.registrations) != len(inventory) {
		return errors.New("install registry inventory does not match catalog")
	}
	seen := make(map[string]bool, len(inventory))
	for _, descriptor := range inventory {
		expected, ok := registry.registrations[descriptor.ID]
		if !ok || expected.Strategy != descriptor.NativeRegistration.Strategy || expected.AssetOnly != descriptor.NativeRegistration.AssetOnly || !equalStrings(expected.Args, descriptor.NativeRegistration.Args) {
			return fmt.Errorf("install registration drift for product %q", descriptor.ID)
		}
		if expected.Strategy == "" {
			seen[descriptor.ID] = true
			continue
		}
		strategy, ok := registry.Resolve(expected.Strategy)
		if !ok {
			return fmt.Errorf("missing install strategy %q", expected.Strategy)
		}
		if err := strategy.ValidateDescriptor(descriptor); err != nil {
			return fmt.Errorf("validate %s install descriptor: %w", descriptor.ID, err)
		}
		seen[descriptor.ID] = true
	}
	if len(seen) != len(registry.registrations) {
		return errors.New("install registry inventory is incomplete")
	}
	return nil
}

func (registry *Registry) Resolve(key string) (Strategy, bool) {
	if registry == nil {
		return nil, false
	}
	strategy, ok := registry.strategies[key]
	return strategy, ok
}

func sortedDescriptors(inventory []productcatalog.Descriptor) []productcatalog.Descriptor {
	result := append([]productcatalog.Descriptor(nil), inventory...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneNativeIdentity(identity *NativeIdentity) *NativeIdentity {
	if identity == nil {
		return nil
	}
	result := *identity
	return &result
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
