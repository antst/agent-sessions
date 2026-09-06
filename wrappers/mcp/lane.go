package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

const LaneSocketEnv = "AGENTBUS_LANE_SOCKET"

type LaneBackend struct{ path string }

func (*LaneBackend) Prepare(context.Context, json.RawMessage) error { return nil }

type laneFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *failure        `json:"error,omitempty"`
}

func NewLaneBackend() (*LaneBackend, error) {
	path := os.Getenv(LaneSocketEnv)
	if path == "" {
		return nil, errors.New("AGENTBUS_LANE_SOCKET is required")
	}
	return &LaneBackend{path: path}, nil
}

func (b *LaneBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	client, err := sessionkit.Dial(b.path)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Call(ctx, method, params)
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
	var request laneFrame
	response := laneFrame{JSONRPC: "2.0"}
	if err := json.NewDecoder(connection).Decode(&request); err != nil || request.JSONRPC != "2.0" || request.ID < 1 || request.Method == "" {
		response.Error = &failure{Code: -32600, Message: "invalid_frame"}
	} else if result, err := backend.Call(ctx, request.Method, request.Params); err != nil {
		response.ID, response.Error = request.ID, wireFailure(err)
	} else {
		response.ID, response.Result = request.ID, result
	}
	_ = json.NewEncoder(connection).Encode(response)
}

func wireFailure(err error) *failure {
	var protocol *sessionkit.ProtocolError
	if errors.As(err, &protocol) {
		return &failure{Code: protocol.Code, Message: protocol.Message, Data: protocol.Data}
	}
	return &failure{Code: -32603, Message: "internal"}
}
