package sessionkit

import (
	"context"
	"encoding/json"
	"net"

	"github.com/antst/agent-sessions/bus/internal/rpc"
)

// Client is a framed connection without a session hello.
type Client struct{ wire *rpc.Conn }

func Dial(socket string) (*Client, error) {
	fd, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	wire := rpc.New(fd, true, func(context.Context, *rpc.Request) { _ = fd.Close() })
	return &Client{wire: wire}, nil
}

func (c *Client) Call(ctx context.Context, method string, params any) (result json.RawMessage, err error) {
	err = c.wire.Call(ctx, method, params, &result)
	return
}

func (c *Client) Close() error { return c.wire.Close() }
