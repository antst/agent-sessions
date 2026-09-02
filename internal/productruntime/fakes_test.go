package productruntime

import (
	"context"
)

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

type fakeDoctor struct{}

func (fakeDoctor) Probe(context.Context, ProbeRequest) (ProbeReport, error) {
	return ProbeReport{State: ProbeReady}, nil
}
