package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

type Frame struct {
	ID      int64
	Method  string
	Params  json.RawMessage
	Result  json.RawMessage
	Error   *RPCError
	Request bool
}

func DecodeFrame(body []byte) (Frame, error) {
	var fields map[string]json.RawMessage
	var frame Frame
	if json.Unmarshal(body, &fields) == nil {
		frame.ID, _ = numericID(fields["id"])
		_ = json.Unmarshal(fields["method"], &frame.Method)
	}
	var wire struct {
		JSONRPC json.RawMessage `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  json.RawMessage `json:"method,omitempty"`
		Params  json.RawMessage `json:"params,omitempty"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   json.RawMessage `json:"error,omitempty"`
	}
	if strict(body, &wire) != nil || null(wire.JSONRPC) || null(wire.ID) {
		return frame, errInvalid
	}
	var version string
	var ok bool
	if json.Unmarshal(wire.JSONRPC, &version) != nil || version != "2.0" {
		return frame, errInvalid
	}
	if frame.ID, ok = numericID(wire.ID); !ok {
		return frame, errInvalid
	}
	if len(wire.Method) != 0 {
		if null(wire.Method) || null(wire.Params) || len(wire.Result) != 0 || len(wire.Error) != 0 || json.Unmarshal(wire.Method, &frame.Method) != nil || frame.Method == "" {
			return frame, errInvalid
		}
		frame.Request, frame.Params = true, wire.Params
		return frame, nil
	}
	if len(wire.Params) != 0 || (len(wire.Result) == 0) == (len(wire.Error) == 0) || null(wire.Result) || null(wire.Error) {
		return frame, errInvalid
	}
	if len(wire.Result) != 0 {
		frame.Result = wire.Result
		return frame, nil
	}
	var err error
	frame.Error, err = DecodeError(wire.Error)
	if err != nil {
		return frame, errInvalid
	}
	return frame, nil
}

func EncodeRequest(id int64, method string, params []byte) ([]byte, error) {
	return encodeFrame(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", id, method, params})
}

func RequestBytes(id int64, method string, value any) ([]byte, error) {
	params, err := EncodeParams(method, value)
	if err != nil {
		return nil, err
	}
	return EncodeRequest(id, method, params)
}

func EncodeResultFrame(id int64, result []byte) ([]byte, error) {
	return encodeFrame(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{"2.0", id, result})
}

func ResultBytes(id int64, method string, value any) ([]byte, error) {
	result, err := EncodeResult(method, value)
	if err != nil {
		return nil, err
	}
	return EncodeResultFrame(id, result)
}

func EncodeFailureFrame(id int64, value *RPCError) ([]byte, error) {
	return encodeFrame(struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      int64     `json:"id"`
		Error   *RPCError `json:"error"`
	}{"2.0", id, value})
}

func ErrorBytes(id int64, code int, data any) ([]byte, error) {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	value := &RPCError{Code: code, Message: errorMessage(code), Data: raw}
	if _, err := EncodeError(value); err != nil {
		return nil, err
	}
	return EncodeFailureFrame(id, value)
}

func encodeFrame(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	encoded = append(encoded, '\n')
	if err != nil || len(encoded) > MaxFrameBytes {
		return nil, errInvalid
	}
	return encoded, nil
}

func numericID(raw []byte) (int64, bool) {
	if len(raw) == 0 || null(raw) {
		return 0, false
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || value < 1 || value > MaxRequestID || math.Trunc(value) != value {
		return 0, false
	}
	return int64(value), true
}

func null(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
