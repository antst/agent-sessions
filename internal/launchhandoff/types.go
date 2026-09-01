// Package launchhandoff transfers one transient native command from the
// daemon to the exact already-running peer wrapper that requested it.
//
// The ticket is only a selector. Authority comes from kernel peer credentials
// and a fresh exact process-identity comparison at consume time. Command and
// sensitive environment material never enter JSON, durable state, or disk.
package launchhandoff

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const (
	ContractVersion uint16 = 1

	defaultMaxPending       = 64
	defaultMaxCommandBytes  = 256 << 10
	defaultMaxAggregate     = 1 << 20
	defaultMaxArguments     = 256
	defaultMaxEnvironment   = 256
	defaultMaxSensitiveEnv  = 32
	defaultMaxEnvNameBytes  = 128
	defaultMaxFieldBytes    = 64 << 10
	defaultPendingTTL       = 30 * time.Second
	defaultRollbackTimeout  = 5 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
)

var (
	ErrUnavailable  = errors.New("launch handoff unavailable")
	ErrInvalid      = errors.New("launch handoff invalid")
	ErrUnauthorized = errors.New("launch handoff unauthorized")
	ErrStale        = errors.New("launch handoff stale")
	ErrClaimed      = errors.New("launch handoff already claimed")
	ErrProtocol     = errors.New("launch handoff protocol failure")
	ErrCapacity     = errors.New("launch handoff capacity exceeded")
)

type Limits struct {
	MaxPending       int
	MaxCommandBytes  int
	MaxAggregate     int
	MaxArguments     int
	MaxEnvironment   int
	MaxSensitiveEnv  int
	MaxEnvNameBytes  int
	MaxFieldBytes    int
	PendingTTL       time.Duration
	RollbackTimeout  time.Duration
	HandshakeTimeout time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxPending: defaultMaxPending, MaxCommandBytes: defaultMaxCommandBytes,
		MaxAggregate: defaultMaxAggregate, MaxArguments: defaultMaxArguments,
		MaxEnvironment: defaultMaxEnvironment, MaxSensitiveEnv: defaultMaxSensitiveEnv,
		MaxEnvNameBytes: defaultMaxEnvNameBytes, MaxFieldBytes: defaultMaxFieldBytes,
		PendingTTL: defaultPendingTTL, RollbackTimeout: defaultRollbackTimeout,
		HandshakeTimeout: defaultHandshakeTimeout,
	}
}

func (l Limits) valid() bool {
	return l.MaxPending > 0 && l.MaxCommandBytes > 0 && l.MaxAggregate >= l.MaxCommandBytes &&
		uint64(l.MaxCommandBytes) <= uint64(^uint32(0)) &&
		l.MaxArguments > 0 && l.MaxArguments <= int(^uint16(0)) &&
		l.MaxEnvironment > 0 && l.MaxEnvironment <= int(^uint16(0)) &&
		l.MaxSensitiveEnv > 0 && l.MaxSensitiveEnv <= int(^uint16(0)) &&
		l.MaxEnvNameBytes > 0 && l.MaxFieldBytes > 0 && l.MaxFieldBytes <= l.MaxCommandBytes &&
		l.PendingTTL > 0 && l.RollbackTimeout > 0 && l.HandshakeTimeout > 0
}

// Ticket is safe control-plane metadata. It selects a staged record but grants
// no authority and contains no command or credential material.
type Ticket struct {
	ID       string `json:"handoff_id"`
	Contract uint16 `json:"handoff_contract"`
}

type RollbackFunc func(context.Context) error

// AmbiguousHandoff identifies a GO write that may have authorized native exec.
// It contains no command, credential, endpoint, or process-owned mutable fact.
type AmbiguousHandoff struct {
	AttachmentID string
	Ticket       Ticket
}

// AmbiguousFinalizer must reconcile live adoption versus proven absence and
// durably hand off cleanup debt when neither can be proven. It is deliberately
// distinct from RollbackFunc: a possible GO write must never blindly destroy
// a native process that may already have execed.
type AmbiguousFinalizer func(context.Context, AmbiguousHandoff) error

// FinalizationPlan is an opaque pre-GO capability set. NewFinalizationPlan is
// the only constructor. At GO classification it is consumed into exactly one
// compile-time resolution variant; the partial-write variant has no field or
// method through which destructive rollback can be reached.
type FinalizationPlan struct {
	zeroWrite     rollbackCapability
	possibleWrite ambiguousCapability
}

type rollbackCapability struct{ run RollbackFunc }
type ambiguousCapability struct{ run AmbiguousFinalizer }

func NewFinalizationPlan(rollback RollbackFunc, ambiguous AmbiguousFinalizer) (FinalizationPlan, error) {
	if rollback == nil || ambiguous == nil {
		return FinalizationPlan{}, ErrInvalid
	}
	return FinalizationPlan{
		zeroWrite:     rollbackCapability{run: rollback},
		possibleWrite: ambiguousCapability{run: ambiguous},
	}, nil
}

func (p FinalizationPlan) valid() bool {
	return p.zeroWrite.run != nil && p.possibleWrite.run != nil
}

// WrapperIdentity is the exact same-UID process that prepared the attachment.
// UID comes from the original control-socket peer credential. Process is
// recaptured from the launch-socket kernel peer PID before every claim.
type WrapperIdentity struct {
	UID     int
	Process procinfo.Identity
}

// StageRequest is the single unified ephemeral record admitted by Broker.
// Rollback must call the daemon's exact attachment transaction rather than a
// product driver directly.
type StageRequest struct {
	AttachmentID string
	Wrapper      WrapperIdentity
	Command      productruntime.NativeCommand
	Finalizers   FinalizationPlan
}

type ProcessCapture func(int) (procinfo.Identity, error)

type Config struct {
	StateRoot      string
	Limits         Limits
	Random         io.Reader
	Now            func() time.Time
	CaptureProcess ProcessCapture
}

// execFunc is the unit-test seam beneath the exported syscall.Exec-bound API.
// Args excludes argv[0], matching productruntime.NativeCommand.
type execFunc func(path string, args []string, env []string, cwd string) error
