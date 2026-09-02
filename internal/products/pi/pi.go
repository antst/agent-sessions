package pi

import (
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/pifamily"
)

const (
	ProductID          = pifamily.PiProductID
	TestedVersion      = pifamily.PiTestedVersion
	IntegrationVersion = pifamily.IntegrationVersion
)

type Config struct {
	Deps             productruntime.HostDeps
	Executable       string
	ExtensionPath    string
	Processes        pifamily.ProcessFactory
	DoctorRunner     pifamily.DoctorRunner
	IntegrationCheck pifamily.IntegrationCheck
	Now              func() time.Time
}

func NewRuntime(descriptor productcatalog.Descriptor, config Config) (productruntime.RuntimeProduct, error) {
	if descriptor.ID != ProductID {
		return productruntime.RuntimeProduct{}, productruntime.ErrProtocol
	}
	quirks, err := pifamily.QuirksFor(ProductID)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	lane, err := pifamily.NewLaneDriver(pifamily.LaneConfig{
		Quirks: quirks, Executable: config.Executable, Generation: config.Deps.Generation,
		Processes: config.Processes, MapPermission: MapPermission, Now: config.Now,
	})
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	doctor, err := NewDoctorProbe(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	return productruntime.RuntimeProduct{Descriptor: descriptor, Lane: lane, Doctor: doctor}, nil
}

func NewDoctorProbe(config Config) (*pifamily.DoctorProbe, error) {
	quirks, err := pifamily.QuirksFor(ProductID)
	if err != nil {
		return nil, err
	}
	return pifamily.NewDoctorProbe(pifamily.DoctorConfig{
		Quirks: quirks, Executable: config.Executable, ExtensionPath: config.ExtensionPath,
		Runner: config.DoctorRunner, CheckIntegration: config.IntegrationCheck,
	})
}
