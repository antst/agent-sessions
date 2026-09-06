package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/antst/sessionbus/internal/products/dsh"
)

type coordinatorDSHPresence struct{ coordinator *hostCoordinator }

func (bridge coordinatorDSHPresence) server() (*livePresenceServer, error) {
	bridge.coordinator.mu.Lock()
	presence := bridge.coordinator.presence
	bridge.coordinator.mu.Unlock()
	if presence == nil {
		return nil, errors.New("Agent Sessions presence server is unavailable")
	}
	return presence, nil
}

func (bridge coordinatorDSHPresence) WaitLane(ctx context.Context, sessionID string) error {
	presence, err := bridge.server()
	if err != nil {
		return err
	}
	return presence.Wait(ctx, sessionID, dsh.ProductID, true)
}

func (bridge coordinatorDSHPresence) CallLane(
	ctx context.Context,
	sessionID, callID, method string,
	params any,
) (json.RawMessage, error) {
	presence, err := bridge.server()
	if err != nil {
		return nil, err
	}
	return presence.Call(ctx, sessionID, callID, method, params)
}
