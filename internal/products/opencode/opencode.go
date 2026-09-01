package opencode

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/opencodefamily"
)

const (
	ProductID          = "opencode"
	TestedVersion      = "1.18.25"
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

func NewPeerDriver(config Config) (*opencodefamily.PeerDriver, error) {
	executable := config.Executable
	if executable == "" {
		executable = "opencode"
	}
	return opencodefamily.NewPeerDriver(opencodefamily.PeerConfig{
		ProductID: ProductID, Executable: executable, IntegrationVersion: IntegrationVersion,
		Deps: config.Deps, Gateway: config.Gateway,
		BuildLaunch: func(_ context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
			if unsafePeerArguments(request.Args) {
				return productruntime.NativeCommand{}, productruntime.ErrUnsupportedPolicy
			}
			environment, sensitive := opencodefamily.ComponentEnv(request)
			return productruntime.NativeCommand{
				Path: executable, Args: append([]string(nil), request.Args...), Cwd: request.Cwd,
				Env: append(append([]productruntime.EnvVar(nil), request.Env...), environment...), SensitiveEnv: sensitive,
			}, nil
		},
	})
}

func unsafePeerArguments(arguments []string) bool {
	for _, argument := range arguments {
		key := argument
		if index := strings.IndexByte(key, '='); index >= 0 {
			key = key[:index]
		}
		switch key {
		case "serve", "run", "attach", "daemon", "--hostname", "--port", "--mini":
			return true
		}
		if strings.ContainsRune(argument, '\x00') {
			return true
		}
	}
	return false
}

func NewLaneDriver(config Config) (*opencodefamily.LaneDriver, error) {
	return opencodefamily.NewLaneDriver(opencodefamily.LaneConfig{
		ProductID: ProductID, Dialect: opencodefamily.DialectOpenCode, Generation: config.Deps.Generation,
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
		executable = "opencode"
	}
	return opencodefamily.NewDoctorProbe(opencodefamily.DoctorConfig{
		ProductID: ProductID, Executable: filepath.Base(executable), TestedVersion: TestedVersion,
		Dialect: opencodefamily.DialectOpenCode, WorkDir: config.DoctorWorkDir, Servers: config.Servers,
		RequiredRoutes: []string{
			"/session", "/session/{sessionID}", "/session/{sessionID}/prompt_async",
			"/session/{sessionID}/message", "/session/{sessionID}/abort", "/session/{sessionID}/permissions/{permissionID}",
			"/event", "/config/providers",
		},
		RunVersion: config.VersionRunner, CheckIntegration: config.IntegrationCheck,
	})
}

func MapPermission(mode permissionmode.Mode) ([]opencodefamily.PermissionRule, error) {
	return opencodefamily.MapPermissionRules(mode)
}
