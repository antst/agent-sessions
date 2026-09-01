package productruntime

import (
	"context"

	"github.com/antst/agent-sessions/internal/daemon"
)

type fakePeerDriver struct{}

func (fakePeerDriver) AttachmentAdapter(HostDeps) (daemon.AttachmentAdapter, error) {
	return daemon.AttachmentAdapter{}, nil
}
func (fakePeerDriver) BuildLaunch(context.Context, PeerLaunchRequest) (NativeCommand, error) {
	return NativeCommand{Path: "/native"}, nil
}
func (fakePeerDriver) Rename(context.Context, daemon.ManagedAttachment, string) (NativeName, error) {
	return NativeName{}, nil
}

type fakeMessageDriver struct{}

func (fakeMessageDriver) Deliver(context.Context, daemon.ManagedAttachment, DeliveryRequest) (NativeAcceptance, error) {
	return NativeAcceptance{}, nil
}

type fakeLaneDriver struct{}

func (fakeLaneDriver) Capabilities() LaneCapabilitySet { return LaneCapabilitySet{} }
func (fakeLaneDriver) Open(context.Context, LaneOpenRequest) (NativeSessionRef, error) {
	return NativeSessionRef{}, nil
}
func (fakeLaneDriver) StartTurn(context.Context, NativeSessionRef, TurnStartRequest) (NativeTurnRef, error) {
	return NativeTurnRef{}, nil
}
func (fakeLaneDriver) WaitTurn(context.Context, NativeTurnRef) (NativeTerminal, error) {
	return NativeTerminal{}, nil
}
func (fakeLaneDriver) Steer(context.Context, NativeTurnRef, TurnStartRequest) (NativeAcceptance, error) {
	return NativeAcceptance{}, ErrUnsupportedSteer
}
func (fakeLaneDriver) Interrupt(context.Context, NativeTurnRef) error  { return nil }
func (fakeLaneDriver) Archive(context.Context, NativeSessionRef) error { return nil }
func (fakeLaneDriver) Recover(context.Context, LaneRecoveryRequest) (NativeSessionRef, error) {
	return NativeSessionRef{}, ErrUnsupportedRecovery
}

type fakeParentAttester struct{}

func (fakeParentAttester) Attest(context.Context, ConnectorAttempt) (ParentBinding, error) {
	return ParentBinding{}, nil
}

type fakeDoctor struct{}

func (fakeDoctor) Probe(context.Context, ProbeRequest) (ProbeReport, error) {
	return ProbeReport{State: ProbeReady}, nil
}
