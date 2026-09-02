package omp

import (
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/pifamily"
)

const (
	ProductID          = pifamily.OMPProductID
	TestedVersion      = pifamily.OMPTestedVersion
	IntegrationVersion = pifamily.IntegrationVersion
)

type Config struct {
	Deps             productruntime.HostDeps
	Executable       string
	ExtensionPath    string
	Processes        pifamily.ProcessFactory
	RecoveryPlans    pifamily.RecoveryPlanner
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
		Processes: config.Processes, Receipts: config.Deps.Receipts, MapPermission: MapPermission,
		RecoveryPlans: config.RecoveryPlans, Now: config.Now,
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
