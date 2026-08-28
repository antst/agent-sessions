package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
)

// applyAdoptedHostConfiguration makes the atomically committed stopped-agent
// identity effective before attachments, delivery routing, or federation are
// recovered. A crash before this point leaves admission closed; every restart
// reprojects the same migration authority before those components start.
func (runtime *Runtime) applyAdoptedHostConfiguration(ctx context.Context) error {
	snapshot, err := LoadAdoptedState(ctx, runtime.options.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load adopted host configuration: %w", err)
	}
	if snapshot.Configuration == nil {
		return nil
	}
	adopted := snapshot.Configuration
	configuration := runtime.options.Configuration
	configuration.HostID = adopted.HostID
	configuration.HostName = adopted.HostName
	configuration.HubAddress = adopted.HubAddress
	configuration.RemoteLanesEnabled = adopted.RemoteLanesEnabled
	configuration.ProductOverrides = maps.Clone(adopted.ProductOverrides)
	if configuration.UpdatedAt < adopted.UpdatedAt {
		configuration.UpdatedAt = adopted.UpdatedAt
	}
	if err := configuration.Validate(runtime.options.Paths); err != nil {
		return fmt.Errorf("validate adopted host configuration: %w", err)
	}
	runtime.options.Configuration = configuration
	return nil
}
