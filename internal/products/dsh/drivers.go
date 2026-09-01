package dsh

import (
	"errors"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

// DriverConfig is the explicit product-local composition surface consumed by
// the sole central runtime composition root. It performs no registration.
type DriverConfig struct {
	ComponentSender ComponentSender
	Processes       productruntime.ProcessInspector
	Now             func() time.Time
	Peer            PeerConfig
	Lane            LaneConfig
	Doctor          DoctorConfig
}

type Drivers struct {
	Gateway *CordisGateway
	Peer    *PeerDriver
	Message *MessageDriver
	Lane    *LaneDriver
	Parent  *ParentAttester
	Doctor  *DoctorProbe
}

func NewDrivers(config DriverConfig) (Drivers, error) {
	if config.Processes == nil {
		return Drivers{}, errors.New("DSH driver composition requires the parent ancestry process inspector")
	}
	gateway, err := NewCordisGateway(config.ComponentSender, config.Now)
	if err != nil {
		return Drivers{}, err
	}
	doctor, err := NewDoctorProbe(config.Doctor)
	if err != nil {
		return Drivers{}, err
	}
	config.Peer.Gateway = gateway
	if config.Peer.TupleVerifier == nil {
		config.Peer.TupleVerifier = doctor
	}
	peer, err := NewPeerDriver(config.Peer)
	if err != nil {
		return Drivers{}, err
	}
	message, err := NewMessageDriver(gateway)
	if err != nil {
		return Drivers{}, err
	}
	parent, err := NewParentAttester(gateway, config.Processes)
	if err != nil {
		return Drivers{}, err
	}
	if config.Lane.TupleVerifier == nil {
		config.Lane.TupleVerifier = doctor
	}
	lane, err := NewLaneDriver(config.Lane)
	if err != nil {
		return Drivers{}, err
	}
	if peer.config.Executable != lane.config.Executable || lane.config.Executable != doctor.config.Executable ||
		peer.config.DSHHome != lane.config.DSHHome || lane.config.DSHHome != doctor.config.DSHHome {
		return Drivers{}, errors.New("DSH drivers must share one DSH executable and managed home")
	}
	return Drivers{Gateway: gateway, Peer: peer, Message: message, Lane: lane, Parent: parent, Doctor: doctor}, nil
}
