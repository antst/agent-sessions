package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
)

const LaneSocketEnv = "AGENTBUS_LANE_SOCKET"

type LaneBackend struct{ path string }

type laneCall struct {
	Action    string          `json:"action"`
	Arguments json.RawMessage `json:"arguments"`
}

type laneReply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *failure        `json:"error,omitempty"`
}

func NewLaneBackend() (*LaneBackend, error) {
	path := os.Getenv(LaneSocketEnv)
	if path == "" {
		return nil, errors.New("AGENTBUS_LANE_SOCKET is required")
	}
	return &LaneBackend{path: path}, nil
}

func (b *LaneBackend) Call(ctx context.Context, action string, arguments json.RawMessage) (json.RawMessage, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", b.path)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer func() { stop(); _ = connection.Close() }()
	if err = json.NewEncoder(connection).Encode(laneCall{Action: action, Arguments: arguments}); err != nil {
		return nil, err
	}
	var reply laneReply
	if err = json.NewDecoder(connection).Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, reply.Error
	}
	return reply.Result, nil
}

func ServeLane(ctx context.Context, listener net.Listener, backend Backend) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		go serveLaneCall(ctx, connection, backend)
	}
}

func serveLaneCall(ctx context.Context, connection net.Conn, backend Backend) {
	defer connection.Close()
	var call laneCall
	reply := laneReply{}
	if err := json.NewDecoder(connection).Decode(&call); err != nil {
		reply.Error = errorFailure(err)
	} else if !validAction(call.Action) {
		reply.Error = &failure{Code: -32602, Message: "Invalid params"}
	} else if result, err := backend.Call(ctx, call.Action, call.Arguments); err != nil {
		reply.Error = errorFailure(err)
	} else {
		reply.Result = result
	}
	_ = json.NewEncoder(connection).Encode(reply)
}
