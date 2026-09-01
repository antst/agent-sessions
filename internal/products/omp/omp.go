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
	ComponentSocket  string
	Processes        pifamily.ProcessFactory
	RecoveryPlans    pifamily.RecoveryPlanner
	Sender           pifamily.ComponentSender
	Renamer          pifamily.ComponentRenamer
	Bindings         pifamily.BindingSource
	ParentTools      pifamily.ParentToolHandler
	Component        *pifamily.ComponentRuntime
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
	runtime := config.Component
	if runtime == nil {
		return productruntime.RuntimeProduct{}, productruntime.ErrProtocol
	}
	if runtime.ProductID() != ProductID {
		return productruntime.RuntimeProduct{}, productruntime.ErrProtocol
	}
	peer, err := pifamily.NewPeerDriver(pifamily.PeerConfig{
		Quirks: quirks, Executable: config.Executable, ExtensionPath: config.ExtensionPath,
		ComponentSocket: config.ComponentSocket, Runtime: runtime,
	})
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	message, err := pifamily.NewMessageDriver(runtime)
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
	parent, err := pifamily.NewParentAttester(quirks, runtime, config.Bindings, config.Deps.Processes)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	doctor, err := NewDoctorProbe(config)
	if err != nil {
		return productruntime.RuntimeProduct{}, err
	}
	return productruntime.RuntimeProduct{
		Descriptor: descriptor, Peer: peer, Message: message,
		Lane: lane, Parent: parent, Doctor: doctor,
	}, nil
}

// NewComponentObserver constructs the ephemeral post-admission observer used
// by both the product drivers and central broker routing. Composition must pass
// this exact pointer back in Config.Component for the live connection.
func NewComponentObserver(config Config) (*pifamily.ComponentRuntime, error) {
	quirks, err := pifamily.QuirksFor(ProductID)
	if err != nil {
		return nil, err
	}
	if config.ParentTools == nil {
		return nil, productruntime.ErrProtocol
	}
	return pifamily.NewComponentRuntime(pifamily.ComponentRuntimeConfig{
		Quirks: quirks, Sender: config.Sender, Renamer: config.Renamer, Bindings: config.Bindings,
		Tools: config.ParentTools, Now: config.Now,
	})
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
