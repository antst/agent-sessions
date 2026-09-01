package codebuddy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

type SecretSource interface {
	Generate(context.Context) (productruntime.SensitiveValue, error)
}

type SecretSourceFunc func(context.Context) (productruntime.SensitiveValue, error)

func (function SecretSourceFunc) Generate(ctx context.Context) (productruntime.SensitiveValue, error) {
	return function(ctx)
}

type EndpointAllocator interface {
	Allocate(context.Context) (string, error)
}

type EndpointAllocatorFunc func(context.Context) (string, error)

func (function EndpointAllocatorFunc) Allocate(ctx context.Context) (string, error) {
	return function(ctx)
}

type CommandResult struct {
	Stdout string
	Stderr string
}

type CommandRunner interface {
	Run(context.Context, string, ...string) (CommandResult, error)
}

type CommandRunnerFunc func(context.Context, string, ...string) (CommandResult, error)

func (function CommandRunnerFunc) Run(ctx context.Context, path string, args ...string) (CommandResult, error) {
	return function(ctx, path, args...)
}

type ActiveAttachmentSource interface {
	ActiveCodeBuddyAttachments(context.Context) ([]daemon.ManagedAttachment, error)
}

type ActiveAttachmentSourceFunc func(context.Context) ([]daemon.ManagedAttachment, error)

func (function ActiveAttachmentSourceFunc) ActiveCodeBuddyAttachments(ctx context.Context) ([]daemon.ManagedAttachment, error) {
	return function(ctx)
}

type RecoveryRequestSource interface {
	LaneOpenRequest(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error)
}

type RecoveryRequestSourceFunc func(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error)

func (function RecoveryRequestSourceFunc) LaneOpenRequest(ctx context.Context, request productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error) {
	return function(ctx, request)
}

type EntrypointMatcher func(executable string, argv []string) bool

type Config struct {
	Deps               productruntime.HostDeps
	Executable         string
	MCPConfigPath      string
	Registry           WorkerRegistry
	SocketOwner        SocketOwnerVerifier
	Processes          ProcessProbe
	Attachments        ActiveAttachmentSource
	Recovery           RecoveryRequestSource
	Secrets            SecretSource
	Endpoints          EndpointAllocator
	Commands           CommandRunner
	Entrypoint         EntrypointMatcher
	Limits             productserver.Limits
	Now                func() time.Time
	PollInterval       time.Duration
	MaxAncestryDepth   int
	AllowSandboxBypass bool
}

type Drivers struct {
	Peer    *PeerDriver
	Message *MessageDriver
	Lane    *LaneDriver
	Parent  *ParentAttester
	Doctor  *DoctorProbe
}

// NewDrivers constructs the complete product-local driver set. It performs no
// registration and starts no process; the central composition root owns that.
func NewDrivers(config Config, deps productruntime.HostDeps) (Drivers, error) {
	normalized, err := normalizeConfig(config, deps)
	if err != nil {
		return Drivers{}, err
	}
	if deps.Generation == 0 || deps.OwnedProcesses == nil || deps.Receipts == nil || normalized.Attachments == nil || normalized.Recovery == nil {
		return Drivers{}, fmtConfig("complete runtime host dependencies, attachment source, and exact lane recovery source are required")
	}
	mechanics := &peerMechanics{config: normalized}
	peer := &PeerDriver{mechanics: mechanics}
	lane := newLaneDriver(normalized, deps)
	return Drivers{
		Peer:    peer,
		Message: &MessageDriver{mechanics: mechanics, now: normalized.Now},
		Lane:    lane,
		Parent:  &ParentAttester{processes: normalized.Processes, attachments: normalized.Attachments, maxDepth: normalized.MaxAncestryDepth},
		Doctor:  &DoctorProbe{config: normalized, deps: deps},
	}, nil
}

// NewRuntime is the product-local composition constructor. It validates the
// exact pinned experimental descriptor without registering it globally.
func NewRuntime(descriptor productcatalog.Descriptor, config Config) (productruntime.RuntimeProduct, error) {
	if descriptor.ID != ProductID || descriptor.TestedVersion != PinnedVersion ||
		descriptor.SupportState != productcatalog.SupportExperimental || descriptor.Compatibility.Policy != productcatalog.VersionExact {
		return productruntime.RuntimeProduct{}, productruntime.ErrIncompatible
	}
	drivers, err := NewDrivers(config, config.Deps)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	return productruntime.RuntimeProduct{
		Descriptor: descriptor, Peer: drivers.Peer, Message: drivers.Message,
		Lane: drivers.Lane, Parent: drivers.Parent, Doctor: drivers.Doctor,
	}, nil
}

func normalizeConfig(config Config, deps productruntime.HostDeps) (Config, error) {
	if config.Executable == "" {
		config.Executable = "codebuddy"
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.PollInterval == 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	if config.PollInterval < time.Millisecond {
		return Config{}, fmtConfig("poll interval is too small")
	}
	if config.MaxAncestryDepth == 0 {
		config.MaxAncestryDepth = 32
	}
	if config.MaxAncestryDepth < 1 || config.MaxAncestryDepth > 256 {
		return Config{}, fmtConfig("ancestry bound is outside 1..256")
	}
	if config.Processes == nil && deps.Processes != nil {
		config.Processes = NewHostProcessProbe(deps.Processes)
	}
	if config.SocketOwner == nil {
		config.SocketOwner = NewPlatformSocketOwnerVerifier()
	}
	if config.Secrets == nil {
		config.Secrets = SecretSourceFunc(randomSecret)
	}
	if config.Endpoints == nil {
		config.Endpoints = EndpointAllocatorFunc(allocateLoopbackEndpoint)
	}
	if config.Commands == nil {
		config.Commands = CommandRunnerFunc(runBoundedCommand)
	}
	if config.Entrypoint == nil {
		config.Entrypoint = DefaultEntrypointMatcher
	}
	if config.Registry == nil || config.Processes == nil {
		return Config{}, fmtConfig("worker registry and process probe are required")
	}
	if strings.TrimSpace(config.MCPConfigPath) == "" {
		return Config{}, fmtConfig("MCP config path is required")
	}
	if !filepath.IsAbs(config.MCPConfigPath) {
		return Config{}, fmtConfig("MCP config path must be absolute")
	}
	return config, nil
}

func fmtConfig(detail string) error {
	return errors.Join(ErrInvalidConfiguration, errors.New(detail))
}

func randomSecret(ctx context.Context) (productruntime.SensitiveValue, error) {
	if ctx == nil {
		return productruntime.SensitiveValue{}, context.Canceled
	}
	select {
	case <-ctx.Done():
		return productruntime.SensitiveValue{}, ctx.Err()
	default:
	}
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return productruntime.SensitiveValue{}, err
	}
	return productruntime.NewSensitiveValue(base64.RawURLEncoding.EncodeToString(buffer)), nil
}

func allocateLoopbackEndpoint(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", context.Canceled
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return "http://" + address, nil
}

func endpointPort(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidConfiguration
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() || parsed.Port() == "" {
		return "", ErrInvalidConfiguration
	}
	return parsed.Port(), nil
}

func runBoundedCommand(ctx context.Context, path string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, path, args...)
	var stdout, stderr boundedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if stdout.overflow || stderr.overflow {
		err = errors.Join(err, errors.New("command output byte bound exceeded"))
	}
	return CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type boundedBuffer struct {
	value    []byte
	overflow bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	const maximum = 64 << 10
	original := len(value)
	remaining := maximum - len(buffer.value)
	if len(value) > remaining {
		buffer.overflow = true
	}
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		buffer.value = append(buffer.value, value...)
	}
	return original, nil
}

func (buffer *boundedBuffer) String() string { return string(buffer.value) }
