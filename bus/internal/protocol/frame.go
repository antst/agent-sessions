package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"unicode/utf8"
)

type Frame struct {
	ID             int64
	Method         string
	Params, Result json.RawMessage
	Error          *RPCError
}

type wireFrame struct {
	JSONRPC json.RawMessage `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  json.RawMessage `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func DecodeFrame(body []byte) (frame Frame, err error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) == nil {
		frame.ID, _ = numericID(fields["id"])
		_ = json.Unmarshal(fields["method"], &frame.Method)
	}
	var wire wireFrame
	if !utf8.Valid(body) || strict(body, &wire) != nil || null(wire.JSONRPC) || null(wire.ID) {
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
		frame.Params = wire.Params
		return frame, nil
	}
	if len(wire.Params) != 0 || (len(wire.Result) == 0) == (len(wire.Error) == 0) || null(wire.Result) || null(wire.Error) {
		return frame, errInvalid
	}
	if len(wire.Result) != 0 {
		frame.Result = wire.Result
		return frame, nil
	}
	frame.Error, err = DecodeError(wire.Error)
	return frame, err
}

func EncodeRequest(id int64, method string, params []byte) ([]byte, error) {
	return encodeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(params)})
}
func RequestBytes(id int64, method string, value any) ([]byte, error) {
	raw, err := EncodeParams(method, value)
	if err != nil {
		return nil, err
	}
	return EncodeRequest(id, method, raw)
}
func ResultBytes(id int64, method string, value any) ([]byte, error) {
	raw, err := EncodeResult(method, value)
	if err != nil {
		return nil, err
	}
	return encodeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(raw)})
}
func ErrorBytes(id int64, code int, data any) ([]byte, error) {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	value := &RPCError{Code: code, Message: errorMessages[code], Data: raw}
	if _, err := EncodeError(value); err != nil {
		return nil, err
	}
	return encodeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "error": value})
}
func encodeFrame(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw)+1 > MaxFrameBytes {
		return nil, errInvalid
	}
	return append(raw, '\n'), nil
}
func strict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errInvalid
	}
	return nil
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
	return int64(value), err == nil && value >= 1 && value <= MaxRequestID && math.Trunc(value) == value
}
func null(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
