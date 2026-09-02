package opencode

import (
	"path/filepath"

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
	Servers            opencodefamily.ServerManager
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
	lane, err := NewLaneDriver(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	doctor, err := NewDoctorProbe(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	return productruntime.RuntimeProduct{Descriptor: descriptor, Lane: lane, Doctor: doctor}, nil
}

func NewLaneDriver(config Config) (*opencodefamily.LaneDriver, error) {
	return opencodefamily.NewLaneDriver(opencodefamily.LaneConfig{
		ProductID: ProductID, Dialect: opencodefamily.DialectOpenCode, Generation: config.Deps.Generation,
		Receipts: config.Deps.Receipts, Servers: config.Servers, MapPermission: MapPermission,
		RecoveryMode: config.RecoveryMode, DecidePermission: config.PermissionDecision, Now: config.Deps.Now,
	})
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
