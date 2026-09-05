package protocol

import (
	"encoding/json"
	"math"
	"strconv"
)

const (
	MaxFrameBytes = 1 << 20
	MaxTextRunes  = 262144
	MaxRequestID  = 1<<53 - 1

	InvalidFrame     = -32600
	InvalidHello     = -32602
	Internal         = -32603
	UnknownSession   = -32001
	NotConnected     = -32002
	Busy             = -32003
	NotRunning       = -32004
	AlreadyConnected = -32005
	UnknownProduct   = -32007
	UnsupportedOpen  = -32008
	SpawnFailed      = -32009
	Timeout          = -32010
	NotCommitted     = -32011
	Superseded       = -32012
	NameTaken        = -32013
	UnknownHost      = -32014
	ForwardLost      = -32015
)

type ExtraArgument struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	TakesValue  bool   `json:"takes_value"`
}

type Integer int

func (value *Integer) UnmarshalJSON(raw []byte) error {
	number, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.Trunc(number) != number {
		return errInvalid
	}
	*value = Integer(number)
	return nil
}

type HelloDescription struct {
	Product             string          `json:"product"`
	Version             string          `json:"version,omitempty"`
	SupportedOpenFields []string        `json:"supported_open_fields"`
	ExtraArguments      []ExtraArgument `json:"extra_arguments"`
}

type WorkerHello struct {
	Protocol    Integer `json:"protocol"`
	LaunchToken string  `json:"launch_token"`
	HelloDescription
}

type PeerHello struct {
	Protocol  Integer        `json:"protocol"`
	Product   string         `json:"product"`
	SessionID string         `json:"session_id"`
	Name      string         `json:"name"`
	Groups    []string       `json:"groups"`
	Info      map[string]any `json:"info"`
}

type OpenOptions struct {
	Cwd             string   `json:"cwd,omitempty"`
	PermissionMode  string   `json:"permission_mode,omitempty"`
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Arguments       []string `json:"arguments,omitempty"`
}

type OpenRequest struct {
	Name            string      `json:"name"`
	Groups          []string    `json:"groups"`
	ResumeSessionID string      `json:"resume_session_id,omitempty"`
	Open            OpenOptions `json:"open"`
}

type OpenResult struct {
	SessionID string `json:"session_id"`
}

type TurnRunRequest struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
}

type TurnResult struct {
	Outcome          string `json:"outcome"`
	Result           string `json:"result"`
	Truncated        bool   `json:"truncated,omitempty"`
	NativeStopReason string `json:"native_stop_reason,omitempty"`
}

type DeliverySource struct {
	SessionID string   `json:"session_id"`
	Name      string   `json:"name"`
	Product   string   `json:"product"`
	Groups    []string `json:"groups"`
}

type DeliveryRequest struct {
	MessageID string         `json:"message_id"`
	From      DeliverySource `json:"from"`
	Body      string         `json:"body"`
}

type DeliveryReceipt struct {
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

type HostProducts struct {
	Host     string   `json:"host"`
	Products []string `json:"products"`
}

type SessionSummary struct {
	SessionID string         `json:"session_id"`
	Kind      string         `json:"kind"`
	Product   string         `json:"product"`
	Name      string         `json:"name"`
	Groups    []string       `json:"groups"`
	Connected bool           `json:"connected"`
	Running   bool           `json:"running"`
	Info      map[string]any `json:"info,omitempty"`
}

type SessionListRequest struct {
	SessionID string `json:"session_id,omitempty"`
}
type SessionListResult struct {
	Sessions []SessionSummary `json:"sessions"`
	Hosts    []HostProducts   `json:"hosts,omitempty"`
}

type MessageSendRequest struct {
	Target  string   `json:"target,omitempty"`
	Targets []string `json:"targets,omitempty"`
	Group   string   `json:"group,omitempty"`
	Message string   `json:"message"`
}

type MessageSendDelivery struct {
	Target      string `json:"target"`
	SessionID   string `json:"session_id,omitempty"`
	DeliveryID  string `json:"delivery_id,omitempty"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

type MessageSendResult struct {
	MessageID  string                `json:"message_id"`
	Deliveries []MessageSendDelivery `json:"deliveries"`
}

type LaneDescribeRequest struct {
	Product string `json:"product"`
	Host    string `json:"host,omitempty"`
}

type LaneDescribeResult = HelloDescription

type LaneSpawnRequest struct {
	Name            string       `json:"name,omitempty"`
	Product         string       `json:"product,omitempty"`
	Host            string       `json:"host,omitempty"`
	ResumeSessionID string       `json:"resume_session_id,omitempty"`
	ExtraGroups     []string     `json:"extra_groups,omitempty"`
	Open            *OpenOptions `json:"open,omitempty"`
}

type LaneSpawnResult struct {
	SessionID string `json:"session_id"`
}
type SessionTarget struct {
	SessionID string `json:"session_id"`
}
type SessionCloseRequest struct {
	SessionID string `json:"session_id"`
	Forget    bool   `json:"forget,omitempty"`
}

type SpawnFailedData struct {
	ExitCode   *int     `json:"exit_code,omitempty"`
	StderrTail []string `json:"stderr_tail"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }
