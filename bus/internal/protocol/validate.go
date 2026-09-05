package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

var errInvalid = errors.New("invalid session value")

func Direction(method string) (client, daemon bool) {
	switch method {
	case "session.hello", "session.list", "message.send", "lane.describe", "lane.spawn":
		return true, false
	case "session.superseded", "message.deliver", "session.open":
		return false, true
	case "turn.run", "turn.interrupt", "session.close":
		return true, true
	default:
		return false, false
	}
}

func Allows(method string, client bool) bool {
	fromClient, fromDaemon := Direction(method)
	return client && fromClient || !client && fromDaemon
}

func DecodeParams(method string, raw []byte) (any, error) {
	switch method {
	case "session.hello":
		return decodeHello(raw)
	case "session.superseded":
		return decodeEmpty(raw)
	case "session.list":
		var v SessionListRequest
		fields, err := decodeObject(raw, &v)
		return v, check(err, optionalText(fields, "session_id", v.SessionID, 0))
	case "message.send":
		return decodeMessageSend(raw)
	case "message.deliver":
		var v DeliveryRequest
		fields, err := decodeObject(raw, &v, "message_id", "from", "body")
		if err == nil {
			err = validateDeliverySource(fields["from"], v.From)
		}
		return v, check(err, text(v.MessageID, 0), runes(v.Body, MaxTextRunes))
	case "lane.describe":
		var v LaneDescribeRequest
		fields, err := decodeObject(raw, &v, "product")
		return v, check(err, text(v.Product, 0), optionalText(fields, "host", v.Host, 0))
	case "lane.spawn":
		return decodeSpawn(raw)
	case "session.open":
		return decodeOpen(raw)
	case "turn.run":
		var v TurnRunRequest
		_, err := decodeObject(raw, &v, "session_id", "input")
		return v, check(err, text(v.SessionID, 0), text(v.Input, MaxTextRunes))
	case "turn.interrupt":
		var v SessionTarget
		_, err := decodeObject(raw, &v, "session_id")
		return v, check(err, text(v.SessionID, 0))
	case "session.close":
		var v SessionCloseRequest
		_, err := decodeObject(raw, &v, "session_id")
		return v, check(err, text(v.SessionID, 0))
	default:
		return nil, errInvalid
	}
}

func DecodeResult(method string, raw []byte) (any, error) {
	switch method {
	case "session.hello", "session.superseded", "turn.interrupt", "session.close":
		return decodeEmpty(raw)
	case "session.list":
		return decodeListResult(raw)
	case "message.send":
		return decodeSendResult(raw)
	case "message.deliver":
		return decodeReceipt(raw)
	case "lane.describe":
		var v LaneDescribeResult
		fields, err := decodeObject(raw, &v, "product", "supported_open_fields", "extra_arguments")
		return v, check(err, validateDescription(fields, v))
	case "lane.spawn":
		var v LaneSpawnResult
		_, err := decodeObject(raw, &v, "session_id")
		return v, check(err, text(v.SessionID, 0))
	case "session.open":
		var v OpenResult
		_, err := decodeObject(raw, &v, "session_id")
		return v, check(err, text(v.SessionID, 128))
	case "turn.run":
		return decodeTurnResult(raw)
	default:
		return nil, errInvalid
	}
}

func EncodeParams(method string, value any) ([]byte, error) {
	return encodeChecked(method, value, DecodeParams)
}
func EncodeResult(method string, value any) ([]byte, error) {
	return encodeChecked(method, value, DecodeResult)
}

func UnmarshalResult(method string, raw []byte, target any) error {
	if _, err := DecodeResult(method, raw); err != nil || target == nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func EncodeError(value *RPCError) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err == nil {
		_, err = DecodeError(raw)
	}
	return raw, err
}

func DecodeError(raw []byte) (*RPCError, error) {
	var v RPCError
	fields, err := decodeObject(raw, &v, "code", "message")
	want := errorMessage(v.Code)
	if err == nil && (want == "" || v.Message != want) {
		err = errInvalid
	}
	if v.Code == SpawnFailed {
		if _, ok := fields["data"]; !ok {
			err = errInvalid
		} else {
			var data SpawnFailedData
			_, dataErr := decodeObject(v.Data, &data, "stderr_tail")
			if dataErr != nil {
				err = errInvalid
			}
		}
	} else if _, ok := fields["data"]; ok {
		err = errInvalid
	}
	return &v, err
}

func decodeHello(raw []byte) (any, error) {
	var fields map[string]json.RawMessage
	if err := strict(raw, &fields); err != nil || fields == nil {
		return nil, errInvalid
	}
	if _, worker := fields["launch_token"]; worker {
		var v WorkerHello
		got, err := decodeObject(raw, &v, "protocol", "product", "launch_token", "supported_open_fields", "extra_arguments")
		return v, check(err, exactProtocol(v.Protocol), text(v.LaunchToken, 0), validateDescription(got, v.HelloDescription))
	}
	var v PeerHello
	_, err := decodeObject(raw, &v, "protocol", "product", "session_id", "name", "groups", "info")
	return v, check(err, exactProtocol(v.Protocol), text(v.Product, 0), text(v.SessionID, 128), text(v.Name, 128), stringList(v.Groups, true))
}

func decodeMessageSend(raw []byte) (any, error) {
	var v MessageSendRequest
	fields, err := decodeObject(raw, &v, "message")
	selectors := present(fields, "target") + present(fields, "targets") + present(fields, "group")
	if selectors != 1 {
		err = errInvalid
	}
	if _, ok := fields["target"]; ok {
		err = check(err, text(v.Target, 0))
	}
	if _, ok := fields["targets"]; ok {
		err = check(err, nonemptyStrings(v.Targets))
	}
	if _, ok := fields["group"]; ok {
		err = check(err, text(v.Group, 0))
	}
	return v, check(err, text(v.Message, MaxTextRunes))
}

func decodeSpawn(raw []byte) (any, error) {
	var v LaneSpawnRequest
	fields, err := decodeObject(raw, &v)
	if present(fields, "resume_session_id") == 1 {
		if present(fields, "name")+present(fields, "product")+present(fields, "host")+present(fields, "extra_groups")+present(fields, "open") != 0 {
			err = errInvalid
		}
		return v, check(err, text(v.ResumeSessionID, 0))
	}
	for _, name := range []string{"name", "product", "open"} {
		if present(fields, name) == 0 {
			err = errInvalid
		}
	}
	if err == nil {
		var open OpenOptions
		_, err = decodeOpenOptions(fields["open"], &open)
	}
	return v, check(err, text(v.Name, 128), text(v.Product, 0), optionalText(fields, "host", v.Host, 0), optionalStrings(fields, "extra_groups", v.ExtraGroups, true))
}

func decodeOpen(raw []byte) (any, error) {
	var v OpenRequest
	fields, err := decodeObject(raw, &v, "name", "groups", "open")
	if err == nil {
		_, err = decodeOpenOptions(fields["open"], &v.Open)
	}
	return v, check(err, text(v.Name, 161), stringList(v.Groups, true), optionalText(fields, "resume_session_id", v.ResumeSessionID, 0))
}

func decodeListResult(raw []byte) (any, error) {
	var v SessionListResult
	fields, err := decodeObject(raw, &v, "sessions")
	var sessions []json.RawMessage
	if json.Unmarshal(fields["sessions"], &sessions) != nil {
		err = errInvalid
	}
	for index, item := range sessions {
		var session SessionSummary
		itemFields, itemErr := decodeObject(item, &session, "session_id", "kind", "product", "name", "groups", "connected", "running")
		if index < len(v.Sessions) {
			session = v.Sessions[index]
		}
		err = check(err, itemErr, validateSummary(itemFields, session))
	}
	if _, ok := fields["hosts"]; ok {
		var hosts []json.RawMessage
		if json.Unmarshal(fields["hosts"], &hosts) != nil {
			err = errInvalid
		}
		for _, item := range hosts {
			var host HostProducts
			_, itemErr := decodeObject(item, &host, "host", "products")
			err = check(err, itemErr, text(host.Host, 0), stringList(host.Products, true))
		}
	}
	return v, err
}

func decodeSendResult(raw []byte) (any, error) {
	var v MessageSendResult
	fields, err := decodeObject(raw, &v, "message_id", "deliveries")
	err = check(err, text(v.MessageID, 0))
	var deliveries []json.RawMessage
	if json.Unmarshal(fields["deliveries"], &deliveries) != nil {
		err = errInvalid
	}
	for _, item := range deliveries {
		var delivery MessageSendDelivery
		itemFields, itemErr := decodeObject(item, &delivery, "target", "disposition")
		err = check(err, itemErr, validateSendDelivery(itemFields, delivery))
	}
	return v, err
}

func decodeReceipt(raw []byte) (any, error) {
	var v DeliveryReceipt
	fields, err := decodeObject(raw, &v, "disposition")
	return v, check(err, validateReceipt(fields, v))
}

func decodeTurnResult(raw []byte) (any, error) {
	var v TurnResult
	fields, err := decodeObject(raw, &v, "outcome", "result")
	if v.Outcome != "completed" && v.Outcome != "interrupted" && v.Outcome != "failed" {
		err = errInvalid
	}
	return v, check(err, runes(v.Result, MaxTextRunes), optionalText(fields, "native_stop_reason", v.NativeStopReason, 0))
}

func decodeEmpty(raw []byte) (any, error) {
	var v struct{}
	_, err := decodeObject(raw, &v)
	return v, err
}

func decodeOpenOptions(raw []byte, value *OpenOptions) (map[string]json.RawMessage, error) {
	fields, err := decodeObject(raw, value)
	return fields, check(err,
		optionalText(fields, "cwd", value.Cwd, 0),
		optionalText(fields, "permission_mode", value.PermissionMode, 0),
		optionalText(fields, "model", value.Model, 0),
		optionalText(fields, "reasoning_effort", value.ReasoningEffort, 0))
}

func validateDescription(fields map[string]json.RawMessage, v HelloDescription) error {
	err := check(text(v.Product, 0), optionalText(fields, "version", v.Version, 0), openFields(v.SupportedOpenFields))
	var arguments []json.RawMessage
	if json.Unmarshal(fields["extra_arguments"], &arguments) != nil {
		err = errInvalid
	}
	for _, item := range arguments {
		var argument ExtraArgument
		_, itemErr := decodeObject(item, &argument, "name", "description", "takes_value")
		err = check(err, itemErr, text(argument.Name, 0), text(argument.Description, 0))
	}
	return err
}

func validateSummary(fields map[string]json.RawMessage, v SessionSummary) error {
	err := check(text(v.SessionID, 161), text(v.Product, 0), text(v.Name, 161), stringList(v.Groups, true))
	if info, ok := fields["info"]; ok {
		var object map[string]any
		if json.Unmarshal(info, &object) != nil || object == nil {
			err = errInvalid
		}
	}
	if v.Kind != "peer" && v.Kind != "lane" || !v.Connected && v.Running {
		err = errInvalid
	}
	return err
}

func validateSendDelivery(fields map[string]json.RawMessage, v MessageSendDelivery) error {
	if err := check(text(v.Target, 0), disposition(v.Disposition)); err != nil {
		return err
	}
	if v.Disposition != "rejected" {
		if present(fields, "session_id")+present(fields, "delivery_id") != 2 || present(fields, "reason") != 0 {
			return errInvalid
		}
		return check(text(v.SessionID, 0), text(v.DeliveryID, 0))
	}
	if err := text(v.Reason, 0); err != nil {
		return err
	}
	if v.Reason == "ambiguous" && (v.SessionID != "" || v.DeliveryID != "") {
		return errInvalid
	}
	return nil
}

func validateDeliverySource(raw json.RawMessage, v DeliverySource) error {
	_, err := decodeObject(raw, &v, "session_id", "name", "product", "groups")
	return check(err, text(v.SessionID, 0), text(v.Name, 161), text(v.Product, 0), stringList(v.Groups, true))
}

func validateReceipt(fields map[string]json.RawMessage, v DeliveryReceipt) error {
	if err := disposition(v.Disposition); err != nil {
		return err
	}
	_, hasReason := fields["reason"]
	if v.Disposition == "rejected" {
		return text(v.Reason, 0)
	}
	if hasReason {
		return errInvalid
	}
	return nil
}

func decodeObject(raw []byte, target any, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || strict(raw, target) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errInvalid
	}
	for name, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fields, errInvalid
		}
		_ = name
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return fields, errInvalid
		}
	}
	return fields, nil
}

func strict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errInvalid
	}
	return nil
}

func encodeChecked(method string, value any, decode func(string, []byte) (any, error)) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err == nil {
		_, err = decode(method, raw)
	}
	return raw, err
}

func exactProtocol(value Integer) error {
	if value != 1 {
		return errInvalid
	}
	return nil
}

func text(value string, maximum int) error {
	if value == "" || !utf8.ValidString(value) || maximum > 0 && utf8.RuneCountInString(value) > maximum {
		return errInvalid
	}
	return nil
}

func runes(value string, maximum int) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return errInvalid
	}
	return nil
}

func optionalText(fields map[string]json.RawMessage, name, value string, maximum int) error {
	if _, ok := fields[name]; !ok {
		return nil
	}
	return text(value, maximum)
}

func stringList(values []string, unique bool) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if text(value, 0) != nil || unique && seen[value] {
			return errInvalid
		}
		seen[value] = true
	}
	return nil
}

func nonemptyStrings(values []string) error {
	if len(values) == 0 {
		return errInvalid
	}
	return stringList(values, true)
}

func optionalStrings(fields map[string]json.RawMessage, name string, values []string, unique bool) error {
	if _, ok := fields[name]; !ok {
		return nil
	}
	return stringList(values, unique)
}

func openFields(values []string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] || value != "cwd" && value != "permission_mode" && value != "model" && value != "reasoning_effort" && value != "arguments" {
			return errInvalid
		}
		seen[value] = true
	}
	return nil
}

func disposition(value string) error {
	if value != "injected" && value != "queued_for_next_turn" && value != "rejected" {
		return errInvalid
	}
	return nil
}

func errorMessage(code int) string {
	switch code {
	case InvalidFrame:
		return "invalid_frame"
	case InvalidHello:
		return "invalid_hello"
	case Internal:
		return "internal"
	case UnknownSession:
		return "unknown_session"
	case NotConnected:
		return "not_connected"
	case Busy:
		return "busy"
	case NotRunning:
		return "not_running"
	case AlreadyConnected:
		return "already_connected"
	case UnknownProduct:
		return "unknown_product"
	case UnsupportedOpen:
		return "unsupported_open_field"
	case SpawnFailed:
		return "spawn_failed"
	case Timeout:
		return "timeout"
	case NotCommitted:
		return "not_committed"
	case Superseded:
		return "superseded"
	case NameTaken:
		return "name_taken"
	case UnknownHost:
		return "unknown_host"
	case ForwardLost:
		return "forward_lost"
	default:
		return ""
	}
}

func present(fields map[string]json.RawMessage, name string) int {
	if _, ok := fields[name]; ok {
		return 1
	}
	return 0
}

func absent(value string) error {
	if value != "" {
		return errInvalid
	}
	return nil
}

func check(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return errInvalid
		}
	}
	return nil
}
