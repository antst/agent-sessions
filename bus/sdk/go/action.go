package sessionkit

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/antst/sessionbus/bus/internal/protocol"
)

var Actions = []string{"list", "send", "spawn", "describe", "run", "start", "wait", "status", "interrupt", "close", "forget"}

func (c *Caller) Action(ctx context.Context, action string, args json.RawMessage) (json.RawMessage, error) {
	method := map[string]string{
		"list": "session.list", "send": "message.send", "spawn": "lane.spawn", "describe": "lane.describe",
		"run": "turn.run", "interrupt": "turn.interrupt", "close": "session.close",
	}[action]
	if method != "" {
		params, err := protocol.DecodeParams(method, args)
		var result json.RawMessage
		if err == nil {
			err = c.call(ctx, method, params, &result)
		}
		return result, err
	}
	switch action {
	case "start":
		return localAction(args, c.Start)
	case "wait":
		return localAction(args, c.Wait)
	case "status":
		return localAction(args, c.Status)
	case "forget":
		return localAction(args, func(request SessionCloseRequest) (struct{}, error) {
			request.Forget = true
			return struct{}{}, c.Close(ctx, request)
		})
	default:
		return nil, errors.New("unknown action")
	}
}

func localAction[T, R any](raw json.RawMessage, call func(T) (R, error)) (json.RawMessage, error) {
	var request T
	err := protocol.DecodeJSON(raw, &request)
	if err != nil {
		return nil, err
	}
	result, err := call(request)
	if err == nil {
		return json.Marshal(result)
	}
	return nil, err
}
