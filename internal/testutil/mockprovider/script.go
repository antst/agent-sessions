package mockprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	defaultModel   = "agent-sessions-mock"
	defaultCreated = int64(1_700_000_000)
)

// Script is an ordered, single-use sequence of model turns. New takes a deep
// copy, so callers may reuse or mutate their input after the server starts.
// Requests reserve turns in capture-sequence order.
type Script struct {
	Model   string
	Created int64
	Turns   []Turn
}

// Turn describes one chat-completion response. Text and ToolCalls model normal
// output. Block pauses this request until Server.Release is called with its
// captured sequence. Error, MalformedStream, and Disconnect are mutually
// exclusive terminal modes and cannot be combined with normal output.
type Turn struct {
	Name            string
	Text            []string
	ToolCalls       []ToolCall
	Block           bool
	Error           *ErrorResponse
	MalformedStream []string
	Disconnect      bool
}

// ToolCall is an OpenAI function tool call. ID may be empty; the server then
// supplies a deterministic ID from the request sequence and call index.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ErrorResponse is an OpenAI-style error envelope returned with Status.
type ErrorResponse struct {
	Status  int
	Code    string
	Type    string
	Message string
}

// Limits bounds untrusted HTTP input and in-memory request capture. Zero fields
// retain the package defaults. Negative fields are invalid.
type Limits struct {
	MaxRequestBytes     int64
	MaxCapturedRequests int
	MaxMessages         int
	MaxTools            int
}

// Option configures a Server.
type Option func(*config) error

// WithLimits overrides the non-zero input and capture limits.
func WithLimits(limits Limits) Option {
	return func(config *config) error {
		if limits.MaxRequestBytes < 0 || limits.MaxCapturedRequests < 0 || limits.MaxMessages < 0 || limits.MaxTools < 0 {
			return fmt.Errorf("mockprovider: limits must not be negative")
		}
		if limits.MaxRequestBytes > 0 {
			config.limits.MaxRequestBytes = limits.MaxRequestBytes
		}
		if limits.MaxCapturedRequests > 0 {
			config.limits.MaxCapturedRequests = limits.MaxCapturedRequests
		}
		if limits.MaxMessages > 0 {
			config.limits.MaxMessages = limits.MaxMessages
		}
		if limits.MaxTools > 0 {
			config.limits.MaxTools = limits.MaxTools
		}
		return nil
	}
}

type config struct {
	limits Limits
}

func defaultConfig() config {
	return config{limits: Limits{
		MaxRequestBytes:     1 << 20,
		MaxCapturedRequests: 128,
		MaxMessages:         256,
		MaxTools:            128,
	}}
}

func prepareScript(script Script) (Script, error) {
	if len(script.Turns) == 0 {
		return Script{}, fmt.Errorf("mockprovider: script must contain at least one turn")
	}
	prepared := Script{
		Model:   script.Model,
		Created: script.Created,
		Turns:   make([]Turn, len(script.Turns)),
	}
	if prepared.Model == "" {
		prepared.Model = defaultModel
	}
	if prepared.Created == 0 {
		prepared.Created = defaultCreated
	}
	for index, turn := range script.Turns {
		copy, err := prepareTurn(index, turn)
		if err != nil {
			return Script{}, err
		}
		prepared.Turns[index] = copy
	}
	return prepared, nil
}

func prepareTurn(index int, turn Turn) (Turn, error) {
	terminalModes := 0
	if turn.Error != nil {
		terminalModes++
	}
	if turn.MalformedStream != nil {
		terminalModes++
	}
	if turn.Disconnect {
		terminalModes++
	}
	hasOutput := turn.Text != nil || len(turn.ToolCalls) > 0
	if terminalModes > 1 || terminalModes == 1 && hasOutput {
		return Turn{}, fmt.Errorf("mockprovider: turn %d combines conflicting response modes", index+1)
	}

	prepared := Turn{
		Name:       turn.Name,
		Block:      turn.Block,
		Disconnect: turn.Disconnect,
	}
	if prepared.Name == "" {
		prepared.Name = fmt.Sprintf("turn-%06d", index+1)
	}
	if turn.Text != nil {
		prepared.Text = append([]string(nil), turn.Text...)
	}
	if turn.ToolCalls != nil {
		prepared.ToolCalls = append([]ToolCall(nil), turn.ToolCalls...)
	}
	for callIndex, call := range prepared.ToolCalls {
		if call.Name == "" {
			return Turn{}, fmt.Errorf("mockprovider: turn %d tool call %d has no name", index+1, callIndex+1)
		}
		if call.Arguments == "" {
			prepared.ToolCalls[callIndex].Arguments = "{}"
			continue
		}
		if !json.Valid([]byte(call.Arguments)) {
			return Turn{}, fmt.Errorf("mockprovider: turn %d tool call %d arguments are not JSON", index+1, callIndex+1)
		}
	}
	if turn.Error != nil {
		if turn.Error.Status < http.StatusBadRequest || turn.Error.Status > 599 {
			return Turn{}, fmt.Errorf("mockprovider: turn %d error status %d is not 4xx or 5xx", index+1, turn.Error.Status)
		}
		copy := *turn.Error
		if copy.Code == "" {
			copy.Code = "mock_error"
		}
		if copy.Type == "" {
			copy.Type = "mock_error"
		}
		if copy.Message == "" {
			copy.Message = http.StatusText(copy.Status)
		}
		prepared.Error = &copy
	}
	if turn.MalformedStream != nil {
		prepared.MalformedStream = append([]string(nil), turn.MalformedStream...)
	}
	return prepared, nil
}
