package kilocode

import (
	"path/filepath"

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
	Servers            opencodefamily.ServerManager
	PermissionDecision opencodefamily.PermissionDecision
	DoctorWorkDir      string
	IntegrationCheck   opencodefamily.IntegrationCheck
	VersionRunner      opencodefamily.VersionRunner
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
		ProductID: ProductID, Dialect: opencodefamily.DialectKilo, Generation: config.Deps.Generation,
		Servers: config.Servers, MapPermission: MapPermission,
		DecidePermission: config.PermissionDecision, Now: config.Deps.Now,
	})
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
