package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

const LaneSocketEnv = "AGENTBUS_LANE_SOCKET"

type LaneBackend struct{ path string }

func (*LaneBackend) Prepare(context.Context, json.RawMessage) error { return nil }

type laneFrame struct {
	Action    string          `json:"action,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *failure        `json:"error,omitempty"`
}

func NewLaneBackend() (*LaneBackend, error) {
	path := os.Getenv(LaneSocketEnv)
	if path == "" {
		return nil, errors.New("AGENTBUS_LANE_SOCKET is required")
	}
	return &LaneBackend{path: path}, nil
}

func (b *LaneBackend) Action(ctx context.Context, action string, arguments json.RawMessage) (json.RawMessage, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", b.path)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err = json.NewEncoder(connection).Encode(laneFrame{Action: action, Arguments: arguments}); err != nil {
		return nil, err
	}
	var response laneFrame
	if err = json.NewDecoder(connection).Decode(&response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, &sessionkit.ProtocolError{Code: response.Error.Code, Message: response.Error.Message, Data: response.Error.Data}
	}
	return response.Result, nil
}

func ServeLane(ctx context.Context, listener net.Listener, backend Backend) error {
	caller := backendCaller(backend)
	if caller == nil {
		return errors.New("lane backend has no caller")
	}
	var calls sync.WaitGroup
	for {
		connection, err := listener.Accept()
		if err != nil {
			calls.Wait()
			return err
		}
		calls.Add(1)
		go func() {
			defer calls.Done()
			serveLaneCall(ctx, connection, caller, backend)
		}()
	}
}

func serveLaneCall(ctx context.Context, connection net.Conn, caller *sessionkit.Caller, backend Backend) {
	defer connection.Close()
	var request laneFrame
	response := laneFrame{}
	if err := json.NewDecoder(connection).Decode(&request); err != nil {
		response.Error = &failure{Code: -32600, Message: "invalid_frame"}
	} else if request.Action == "" {
		response.Error = &failure{Code: -32600, Message: "invalid_frame"}
	} else if err := backend.Prepare(ctx, nil); err != nil {
		response.Error = wireFailure(err)
	} else {
		if result, err := caller.Action(ctx, request.Action, request.Arguments); err != nil {
			response.Error = wireFailure(err)
		} else {
			response.Result = result
		}
	}
	_ = json.NewEncoder(connection).Encode(response)
}

func wireFailure(err error) *failure {
	var protocol *sessionkit.ProtocolError
	if errors.As(err, &protocol) {
		return &failure{Code: protocol.Code, Message: protocol.Message, Data: protocol.Data}
	}
	return &failure{Code: -32603, Message: err.Error()}
}
