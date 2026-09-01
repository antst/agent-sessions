package kilocode

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/opencodefamily"
)

const (
	ProductID          = "kilo"
	TestedVersion      = "7.5.6"
	IntegrationVersion = "1"
)

type Config struct {
	Deps               productruntime.HostDeps
	Executable         string
	Gateway            opencodefamily.ComponentGateway
	Servers            opencodefamily.ServerManager
	ParentVerifier     opencodefamily.ExactParentVerifier
	PermissionDecision opencodefamily.PermissionDecision
	DoctorWorkDir      string
	IntegrationCheck   opencodefamily.IntegrationCheck
	VersionRunner      opencodefamily.VersionRunner
	RecoveryMode       opencodefamily.RecoveryPermissionMode
}

func NewRuntime(descriptor productcatalog.Descriptor, config Config) (productruntime.RuntimeProduct, error) {
	if descriptor.ID != ProductID {
		return productruntime.RuntimeProduct{}, productruntime.ErrProtocol
	}
	peer, err := NewPeerDriver(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	lane, err := NewLaneDriver(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	parent, err := NewParentAttester(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	doctor, err := NewDoctorProbe(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	return productruntime.RuntimeProduct{Descriptor: descriptor, Peer: peer, Message: peer, Lane: lane, Parent: parent, Doctor: doctor}, nil
}

type peerPair struct {
	cleanup  sync.Mutex
	server   kiloPeerServer
	nativeID string
	fresh    bool
	deleted  bool
}

type peerLaunchIntent struct {
	resumeNativeID string
	cwd            string
}

type kiloPeerServer interface {
	Client() *opencodefamily.Client
	ProcessRef() productruntime.OwnedProcessRef
	BuildKiloAttach(string, string, []string, []productruntime.EnvVar) (productruntime.NativeCommand, error)
	Close(context.Context) error
}

type kiloServerOpener func(context.Context, opencodefamily.ServerOpenRequest) (kiloPeerServer, error)

type peerLaunch struct {
	done chan struct{}
}

type PeerDriver struct {
	common     *opencodefamily.PeerDriver
	executable string
	servers    opencodefamily.ServerManager
	openServer kiloServerOpener
	mu         sync.Mutex
	pairs      map[string]*peerPair
	pending    map[string]peerLaunchIntent
	launching  map[string]*peerLaunch
}

func NewPeerDriver(config Config) (*PeerDriver, error) {
	executable := config.Executable
	if executable == "" {
		executable = "kilo"
	}
	if config.Servers == nil {
		return nil, productruntime.ErrProtocol
	}
	driver := &PeerDriver{
		executable: executable, servers: config.Servers,
		pairs: make(map[string]*peerPair), pending: make(map[string]peerLaunchIntent), launching: make(map[string]*peerLaunch),
	}
	driver.openServer = func(ctx context.Context, request opencodefamily.ServerOpenRequest) (kiloPeerServer, error) {
		return driver.servers.Open(ctx, request)
	}
	common, err := opencodefamily.NewPeerDriver(opencodefamily.PeerConfig{
		ProductID: ProductID, Executable: executable, IntegrationVersion: IntegrationVersion,
		Deps: config.Deps, Gateway: config.Gateway,
		Prepare: func(_ context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			if attachment.ID == "" || attachment.Product != ProductID || !validKiloCwd(attachment.Cwd) ||
				attachment.NativeSessionID != "" && !validKiloSessionID(attachment.NativeSessionID) {
				return daemon.NativeEvidence{}, productruntime.ErrProtocol
			}
			driver.mu.Lock()
			defer driver.mu.Unlock()
			if driver.launching[attachment.ID] != nil || driver.pairs[attachment.ID] != nil {
				return daemon.NativeEvidence{}, productruntime.ErrAmbiguousSession
			}
			if existing, ok := driver.pending[attachment.ID]; ok &&
				(existing.resumeNativeID != attachment.NativeSessionID || existing.cwd != attachment.Cwd) {
				return daemon.NativeEvidence{}, productruntime.ErrAmbiguousSession
			}
			driver.pending[attachment.ID] = peerLaunchIntent{resumeNativeID: attachment.NativeSessionID, cwd: attachment.Cwd}
			return daemon.NativeEvidence{ArtifactType: "component", ArtifactRevision: IntegrationVersion}, nil
		},
		BuildLaunch: driver.buildLaunch,
		Detach:      driver.detach,
	})
	if err != nil {
		return nil, err
	}
	driver.common = common
	return driver, nil
}

func (driver *PeerDriver) buildLaunch(ctx context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
	if rejectMiniOrTopology(request.Args) {
		return productruntime.NativeCommand{}, productruntime.ErrUnsupportedPolicy
	}
	driver.mu.Lock()
	_, exists := driver.pairs[request.AttachmentID]
	intent, prepared := driver.pending[request.AttachmentID]
	if exists {
		driver.mu.Unlock()
		return productruntime.NativeCommand{}, productruntime.ErrAmbiguousSession
	}
	if !prepared {
		driver.mu.Unlock()
		return productruntime.NativeCommand{}, productruntime.ErrStale
	}
	if intent.cwd != request.Cwd {
		driver.mu.Unlock()
		return productruntime.NativeCommand{}, productruntime.ErrAmbiguousSession
	}
	delete(driver.pending, request.AttachmentID)
	launch := &peerLaunch{done: make(chan struct{})}
	driver.launching[request.AttachmentID] = launch
	driver.mu.Unlock()
	defer driver.finishLaunching(request.AttachmentID, launch)
	bootstrapEnv, bootstrapSensitive := opencodefamily.BootstrapEnv(request)
	server, err := driver.openServer(ctx, opencodefamily.ServerOpenRequest{
		Key: "peer-" + request.AttachmentID, Cwd: request.Cwd,
		Env:          append(append([]productruntime.EnvVar(nil), request.Env...), bootstrapEnv...),
		SensitiveEnv: bootstrapSensitive,
	})
	if server == nil {
		if err == nil {
			err = productruntime.ErrUnavailable
		}
		return productruntime.NativeCommand{}, err
	}
	pair := &peerPair{server: server, nativeID: intent.resumeNativeID, fresh: intent.resumeNativeID == ""}
	driver.mu.Lock()
	if _, raced := driver.pairs[request.AttachmentID]; raced || driver.launching[request.AttachmentID] != launch {
		driver.mu.Unlock()
		return productruntime.NativeCommand{}, errors.Join(productruntime.ErrAmbiguousSession, driver.cleanupUnadoptedPair(context.Background(), pair))
	}
	driver.pairs[request.AttachmentID] = pair
	driver.mu.Unlock()
	if err != nil {
		return productruntime.NativeCommand{}, errors.Join(err, driver.rollbackPair(context.Background(), request.AttachmentID, pair))
	}
	client := server.Client()
	if client == nil {
		return productruntime.NativeCommand{}, errors.Join(productruntime.ErrUnavailable, driver.rollbackPair(context.Background(), request.AttachmentID, pair))
	}
	permissions, err := MapPermission(permissionmode.Default)
	if err != nil {
		return productruntime.NativeCommand{}, errors.Join(err, driver.rollbackPair(context.Background(), request.AttachmentID, pair))
	}
	var session opencodefamily.Session
	if intent.resumeNativeID == "" {
		session, err = client.CreateSession(ctx, "", permissions)
	} else {
		session, err = client.GetSession(ctx, intent.resumeNativeID)
	}
	if err != nil || intent.resumeNativeID != "" && session.ID != intent.resumeNativeID {
		if err == nil {
			err = productruntime.ErrAmbiguousSession
		}
		return productruntime.NativeCommand{}, errors.Join(err, driver.rollbackPair(context.Background(), request.AttachmentID, pair))
	}
	pair.cleanup.Lock()
	pair.nativeID = session.ID
	pair.cleanup.Unlock()
	if err := client.SelectKiloSession(ctx, session.ID); err != nil {
		return productruntime.NativeCommand{}, errors.Join(err, driver.rollbackPair(context.Background(), request.AttachmentID, pair))
	}
	command, err := server.BuildKiloAttach(request.Cwd, session.ID, request.Args, request.Env)
	if err != nil {
		return productruntime.NativeCommand{}, errors.Join(err, driver.rollbackPair(context.Background(), request.AttachmentID, pair))
	}
	command.Path = driver.executable
	return command, nil
}

func rejectMiniOrTopology(arguments []string) bool {
	for _, argument := range arguments {
		key := argument
		if index := strings.IndexByte(key, '='); index >= 0 {
			key = key[:index]
		}
		switch key {
		case "--mini", "--session", "-s", "--continue", "-c", "--fork", "--cloud-fork",
			"--dir", "--hostname", "--port", "--password", "-p", "--username", "-u",
			"--replay", "--no-replay", "--replay-limit", "attach", "serve", "daemon":
			return true
		}
		if strings.ContainsRune(argument, '\x00') {
			return true
		}
	}
	return false
}

func validKiloSessionID(value string) bool {
	if !strings.HasPrefix(value, "ses_") || len(value) <= len("ses_") || len(value) > 256 {
		return false
	}
	for _, character := range value[len("ses_"):] {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validKiloCwd(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && filepath.IsAbs(value) && filepath.Clean(value) == value &&
		len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
}

func (driver *PeerDriver) finishLaunching(attachmentID string, launch *peerLaunch) {
	driver.mu.Lock()
	if driver.launching[attachmentID] == launch {
		delete(driver.launching, attachmentID)
	}
	close(launch.done)
	driver.mu.Unlock()
}

func (driver *PeerDriver) detach(ctx context.Context, attachment daemon.ManagedAttachment) error {
	driver.mu.Lock()
	pair := driver.pairs[attachment.ID]
	delete(driver.pending, attachment.ID)
	launch := driver.launching[attachment.ID]
	driver.mu.Unlock()
	if launch != nil {
		select {
		case <-launch.done:
			driver.mu.Lock()
			pair = driver.pairs[attachment.ID]
			driver.mu.Unlock()
		case <-ctx.Done():
			return fmt.Errorf("%w: exact Kilo peer launch is still in flight", productruntime.ErrCleanupDebt)
		}
	}
	if pair == nil {
		return nil
	}
	pair.cleanup.Lock()
	defer pair.cleanup.Unlock()
	if err := pair.server.Close(ctx); err != nil {
		return fmt.Errorf("%w: exact isolated Kilo server close: %v", productruntime.ErrCleanupDebt, err)
	}
	driver.mu.Lock()
	if driver.pairs[attachment.ID] == pair {
		delete(driver.pairs, attachment.ID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *PeerDriver) cleanupUnadoptedPair(ctx context.Context, pair *peerPair) error {
	if pair == nil || pair.server == nil {
		return nil
	}
	pair.cleanup.Lock()
	defer pair.cleanup.Unlock()
	deleteNative := pair.fresh && pair.nativeID != "" && !pair.deleted
	if deleteNative {
		client := pair.server.Client()
		if client == nil {
			return fmt.Errorf("%w: exact fresh Kilo session cleanup client is unavailable", productruntime.ErrCleanupDebt)
		}
		if err := client.DeleteSession(ctx, pair.nativeID); err != nil && !errors.Is(err, productruntime.ErrStale) {
			return fmt.Errorf("%w: exact fresh Kilo session delete: %v", productruntime.ErrCleanupDebt, err)
		}
		pair.deleted = true
	}
	if err := pair.server.Close(ctx); err != nil {
		return fmt.Errorf("%w: exact isolated Kilo server close: %v", productruntime.ErrCleanupDebt, err)
	}
	return nil
}

func (driver *PeerDriver) rollbackPair(ctx context.Context, attachmentID string, pair *peerPair) error {
	if err := driver.cleanupUnadoptedPair(ctx, pair); err != nil {
		return err
	}
	driver.mu.Lock()
	if driver.pairs[attachmentID] == pair {
		delete(driver.pairs, attachmentID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *PeerDriver) rollback(ctx context.Context, attachment daemon.ManagedAttachment) error {
	driver.mu.Lock()
	delete(driver.pending, attachment.ID)
	launch := driver.launching[attachment.ID]
	pair := driver.pairs[attachment.ID]
	driver.mu.Unlock()
	if launch != nil {
		select {
		case <-launch.done:
			driver.mu.Lock()
			pair = driver.pairs[attachment.ID]
			driver.mu.Unlock()
		case <-ctx.Done():
			return fmt.Errorf("%w: exact Kilo peer launch cleanup is still in flight", productruntime.ErrCleanupDebt)
		}
	}
	return driver.rollbackPair(ctx, attachment.ID, pair)
}

func (driver *PeerDriver) AttachmentAdapter(deps productruntime.HostDeps) (daemon.AttachmentAdapter, error) {
	adapter, err := driver.common.AttachmentAdapter(deps)
	if err != nil {
		return daemon.AttachmentAdapter{}, err
	}
	commonAdopt := adapter.Adopt
	adapter.Adopt = func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) (daemon.NativeEvidence, error) {
		driver.mu.Lock()
		pair := driver.pairs[attachment.ID]
		driver.mu.Unlock()
		if pair == nil || observed.Process != pair.server.ProcessRef().Process || observed.ThreadID != pair.nativeID {
			return daemon.NativeEvidence{}, productruntime.ErrUnauthorized
		}
		accepted, err := commonAdopt(ctx, attachment, observed)
		if err != nil {
			return daemon.NativeEvidence{}, err
		}
		return accepted, nil
	}
	adapter.Rollback = driver.rollback
	return adapter, nil
}

func (driver *PeerDriver) BuildLaunch(ctx context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
	return driver.common.BuildLaunch(ctx, request)
}

func (driver *PeerDriver) Rename(ctx context.Context, attachment daemon.ManagedAttachment, name string) (productruntime.NativeName, error) {
	return driver.common.Rename(ctx, attachment, name)
}

func (driver *PeerDriver) Deliver(ctx context.Context, attachment daemon.ManagedAttachment, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	return driver.common.Deliver(ctx, attachment, request)
}

func NewLaneDriver(config Config) (*opencodefamily.LaneDriver, error) {
	return opencodefamily.NewLaneDriver(opencodefamily.LaneConfig{
		ProductID: ProductID, Dialect: opencodefamily.DialectKilo, Generation: config.Deps.Generation,
		Receipts: config.Deps.Receipts, Servers: config.Servers, MapPermission: MapPermission,
		RecoveryMode: config.RecoveryMode, DecidePermission: config.PermissionDecision, Now: config.Deps.Now,
	})
}

func NewParentAttester(config Config) (*opencodefamily.ParentAttester, error) {
	return opencodefamily.NewParentAttester(ProductID, config.ParentVerifier)
}

func NewDoctorProbe(config Config) (*opencodefamily.DoctorProbe, error) {
	executable := config.Executable
	if executable == "" {
		executable = "kilo"
	}
	return opencodefamily.NewDoctorProbe(opencodefamily.DoctorConfig{
		ProductID: ProductID, Executable: filepath.Base(executable), TestedVersion: TestedVersion,
		Dialect: opencodefamily.DialectKilo, WorkDir: config.DoctorWorkDir, Servers: config.Servers,
		RequiredRoutes: []string{
			"/session", "/session/{sessionID}", "/session/{sessionID}/message", "/event", "/config/providers",
			"/tui/select-session", "/tui/append-prompt", "/tui/submit-prompt", "/api/session/{sessionID}/prompt",
			"/api/session/{sessionID}/event", "/api/session/{sessionID}/interrupt", "/background-process",
		},
		RunVersion: config.VersionRunner, CheckIntegration: config.IntegrationCheck,
	})
}

func MapPermission(mode permissionmode.Mode) ([]opencodefamily.PermissionRule, error) {
	return opencodefamily.MapPermissionRules(mode)
}

var (
	_ productruntime.PeerDriver    = (*PeerDriver)(nil)
	_ productruntime.MessageDriver = (*PeerDriver)(nil)
)
