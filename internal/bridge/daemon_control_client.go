package bridge

import (
	"context"
	"errors"
	"strings"
	"sync"

	daemonpkg "github.com/antst/sessionbus/internal/daemon"
)

type daemonGenerationSource func(context.Context) (uint64, error)

type daemonControlClient struct {
	endpoint   string
	generation daemonGenerationSource
	mu         sync.Mutex
	current    uint64
}

func newDaemonControlClient(endpoint string, generation daemonGenerationSource) (*daemonControlClient, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("daemon control endpoint is empty")
	}
	if generation == nil {
		return nil, errors.New("daemon generation source is unavailable")
	}
	return &daemonControlClient{endpoint: endpoint, generation: generation}, nil
}

func (c *daemonControlClient) call(ctx context.Context, request daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
	generation, err := c.readGeneration(ctx, false)
	if err != nil {
		return daemonpkg.ControlResponse{}, err
	}
	request.Generation = generation
	response, err := daemonpkg.CallControl(ctx, c.endpoint, request)
	if err != nil || response.Error == nil || response.Error.Code != daemonpkg.ErrorStaleGeneration {
		return response, err
	}
	generation, err = c.readGeneration(ctx, true)
	if err != nil {
		return daemonpkg.ControlResponse{}, err
	}
	request.Generation = generation
	request.ID = randomID()
	return daemonpkg.CallControl(ctx, c.endpoint, request)
}

func (c *daemonControlClient) readGeneration(ctx context.Context, refresh bool) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != 0 && !refresh {
		return c.current, nil
	}
	generation, err := c.generation(ctx)
	if err != nil || generation == 0 {
		return 0, errors.New("daemon generation is unavailable")
	}
	c.current = generation
	return generation, nil
}
