package launchhandoff

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
)

var (
	protocolMagic = [4]byte{'A', 'S', 'L', 'H'}
	envName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const (
	frameClaim byte = iota + 1
	frameCommand
	frameAck
	frameGo
	frameError
)

const (
	errorInvalid byte = iota + 1
	errorUnauthorized
	errorStale
	errorClaimed
	errorUnavailable
	errorProtocol
)

func encodeClaim(ticket Ticket) ([]byte, error) {
	if ticket.Contract != ContractVersion || !validTicketID(ticket.ID) {
		return nil, ErrInvalid
	}
	var output bytes.Buffer
	writeHeader(&output, frameClaim)
	if err := writeString(&output, ticket.ID); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func decodeClaim(body []byte) (Ticket, error) {
	reader, err := readHeader(body, frameClaim)
	if err != nil {
		return Ticket{}, err
	}
	id, err := readString(reader, 128)
	if err != nil || !validTicketID(id) || reader.Len() != 0 {
		return Ticket{}, ErrProtocol
	}
	return Ticket{ID: id, Contract: ContractVersion}, nil
}

func encodeCommand(command productruntime.NativeCommand, limits Limits) ([]byte, error) {
	if err := validateCommand(command, limits); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writeHeader(&output, frameCommand)
	for _, value := range []string{command.Path, command.Cwd} {
		if err := writeString(&output, value); err != nil {
			return nil, err
		}
	}
	if err := writeStrings(&output, command.Args); err != nil {
		return nil, err
	}
	if err := writeEnv(&output, command.Env); err != nil {
		return nil, err
	}
	if len(command.SensitiveEnv) > int(^uint16(0)) {
		return nil, ErrInvalid
	}
	_ = binary.Write(&output, binary.BigEndian, uint16(len(command.SensitiveEnv)))
	for _, variable := range command.SensitiveEnv {
		if err := writeString(&output, variable.Name); err != nil {
			return nil, err
		}
		if err := writeString(&output, variable.Value.Reveal()); err != nil {
			return nil, err
		}
	}
	if output.Len() > limits.MaxCommandBytes {
		zero(output.Bytes())
		return nil, ErrCapacity
	}
	return output.Bytes(), nil
}

func decodeCommand(body []byte, limits Limits) (productruntime.NativeCommand, error) {
	if len(body) == 0 || len(body) > limits.MaxCommandBytes {
		return productruntime.NativeCommand{}, ErrProtocol
	}
	reader, err := readHeader(body, frameCommand)
	if err != nil {
		return productruntime.NativeCommand{}, err
	}
	command := productruntime.NativeCommand{}
	if command.Path, err = readString(reader, limits.MaxFieldBytes); err != nil {
		return productruntime.NativeCommand{}, err
	}
	if command.Cwd, err = readString(reader, limits.MaxFieldBytes); err != nil {
		return productruntime.NativeCommand{}, err
	}
	if command.Args, err = readStrings(reader, limits.MaxArguments, limits.MaxFieldBytes); err != nil {
		return productruntime.NativeCommand{}, err
	}
	if command.Env, err = readEnv(reader, limits.MaxEnvironment, limits.MaxEnvNameBytes, limits.MaxFieldBytes); err != nil {
		return productruntime.NativeCommand{}, err
	}
	count, err := readCount(reader, limits.MaxSensitiveEnv)
	if err != nil {
		return productruntime.NativeCommand{}, err
	}
	command.SensitiveEnv = make([]productruntime.SensitiveEnvVar, 0, count)
	for range count {
		name, nameErr := readString(reader, limits.MaxEnvNameBytes)
		value, valueErr := readString(reader, limits.MaxFieldBytes)
		if nameErr != nil || valueErr != nil {
			return productruntime.NativeCommand{}, ErrProtocol
		}
		command.SensitiveEnv = append(command.SensitiveEnv, productruntime.SensitiveEnvVar{
			Name: name, Value: productruntime.NewSensitiveValue(value),
		})
	}
	if reader.Len() != 0 || validateCommand(command, limits) != nil {
		return productruntime.NativeCommand{}, ErrProtocol
	}
	return command, nil
}

func encodeAck(digest [sha256.Size]byte) []byte {
	var output bytes.Buffer
	writeHeader(&output, frameAck)
	_, _ = output.Write(digest[:])
	return output.Bytes()
}

func decodeAck(body []byte, expected [sha256.Size]byte) error {
	reader, err := readHeader(body, frameAck)
	if err != nil || reader.Len() != sha256.Size {
		return ErrProtocol
	}
	var actual [sha256.Size]byte
	_, _ = io.ReadFull(reader, actual[:])
	if actual != expected {
		return ErrProtocol
	}
	return nil
}

func simpleFrame(kind byte) []byte {
	var output bytes.Buffer
	writeHeader(&output, kind)
	return output.Bytes()
}

func encodeError(err error) []byte {
	category := errorUnavailable
	switch {
	case errors.Is(err, ErrInvalid):
		category = errorInvalid
	case errors.Is(err, ErrUnauthorized):
		category = errorUnauthorized
	case errors.Is(err, ErrStale):
		category = errorStale
	case errors.Is(err, ErrClaimed):
		category = errorClaimed
	case errors.Is(err, ErrProtocol):
		category = errorProtocol
	}
	body := simpleFrame(frameError)
	return append(body, category)
}

func decodeServerError(body []byte) error {
	reader, err := readHeader(body, frameError)
	if err != nil || reader.Len() != 1 {
		return ErrProtocol
	}
	category, _ := reader.ReadByte()
	switch category {
	case errorInvalid:
		return ErrInvalid
	case errorUnauthorized:
		return ErrUnauthorized
	case errorStale:
		return ErrStale
	case errorClaimed:
		return ErrClaimed
	case errorUnavailable:
		return ErrUnavailable
	case errorProtocol:
		return ErrProtocol
	default:
		return ErrProtocol
	}
}

func validateCommand(command productruntime.NativeCommand, limits Limits) error {
	if !limits.valid() || invalidRequiredField(command.Path, limits.MaxFieldBytes) || invalidRequiredField(command.Cwd, limits.MaxFieldBytes) ||
		len(command.Args) > limits.MaxArguments || len(command.Env) > limits.MaxEnvironment || len(command.SensitiveEnv) > limits.MaxSensitiveEnv {
		return ErrInvalid
	}
	for _, argument := range command.Args {
		if invalidOptionalField(argument, limits.MaxFieldBytes) {
			return ErrInvalid
		}
	}
	names := make(map[string]struct{}, len(command.Env)+len(command.SensitiveEnv))
	for _, variable := range command.Env {
		if !validEnv(variable.Name, variable.Value, limits) {
			return ErrInvalid
		}
		if _, duplicate := names[variable.Name]; duplicate {
			return ErrInvalid
		}
		names[variable.Name] = struct{}{}
	}
	public := make([]string, 0, 2+len(command.Args)+len(command.Env)*2)
	public = append(public, command.Path, command.Cwd)
	public = append(public, command.Args...)
	for _, variable := range command.Env {
		public = append(public, variable.Name, variable.Value)
	}
	for _, variable := range command.SensitiveEnv {
		secret := variable.Value.Reveal()
		if !validEnv(variable.Name, secret, limits) || secret == "" {
			return ErrInvalid
		}
		if _, duplicate := names[variable.Name]; duplicate {
			return ErrInvalid
		}
		names[variable.Name] = struct{}{}
		for _, value := range public {
			if strings.Contains(value, secret) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func exactEnvironment(command productruntime.NativeCommand, limits Limits) ([]string, error) {
	if err := validateCommand(command, limits); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(command.Env)+len(command.SensitiveEnv))
	for _, variable := range command.Env {
		result = append(result, variable.Name+"="+variable.Value)
	}
	for _, variable := range command.SensitiveEnv {
		result = append(result, variable.Name+"="+variable.Value.Reveal())
	}
	return result, nil
}

func writeHeader(output *bytes.Buffer, kind byte) {
	_, _ = output.Write(protocolMagic[:])
	_ = binary.Write(output, binary.BigEndian, ContractVersion)
	_ = output.WriteByte(kind)
}

func readHeader(body []byte, kind byte) (*bytes.Reader, error) {
	if len(body) < 7 || !bytes.Equal(body[:4], protocolMagic[:]) || binary.BigEndian.Uint16(body[4:6]) != ContractVersion || body[6] != kind {
		return nil, ErrProtocol
	}
	return bytes.NewReader(body[7:]), nil
}

func writeString(output *bytes.Buffer, value string) error {
	if len(value) > int(^uint32(0)) {
		return ErrInvalid
	}
	_ = binary.Write(output, binary.BigEndian, uint32(len(value))) //nolint:gosec // checked above.
	_, err := output.WriteString(value)
	return err
}

func readString(reader *bytes.Reader, maximum int) (string, error) {
	var size uint32
	if binary.Read(reader, binary.BigEndian, &size) != nil || uint64(size) > uint64(maximum) || uint64(size) > uint64(reader.Len()) {
		return "", ErrProtocol
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		zero(body)
		return "", ErrProtocol
	}
	return stringAndZero(body), nil
}

func stringAndZero(body []byte) string {
	value := string(body)
	zero(body)
	return value
}

func writeStrings(output *bytes.Buffer, values []string) error {
	if len(values) > int(^uint16(0)) {
		return ErrInvalid
	}
	_ = binary.Write(output, binary.BigEndian, uint16(len(values))) //nolint:gosec // checked above.
	for _, value := range values {
		if err := writeString(output, value); err != nil {
			return err
		}
	}
	return nil
}

func readStrings(reader *bytes.Reader, maximum, fieldMaximum int) ([]string, error) {
	count, err := readCount(reader, maximum)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, count)
	for range count {
		value, valueErr := readString(reader, fieldMaximum)
		if valueErr != nil {
			return nil, valueErr
		}
		result = append(result, value)
	}
	return result, nil
}

func writeEnv(output *bytes.Buffer, values []productruntime.EnvVar) error {
	if len(values) > int(^uint16(0)) {
		return ErrInvalid
	}
	_ = binary.Write(output, binary.BigEndian, uint16(len(values))) //nolint:gosec // checked above.
	for _, value := range values {
		if err := writeString(output, value.Name); err != nil {
			return err
		}
		if err := writeString(output, value.Value); err != nil {
			return err
		}
	}
	return nil
}

func readEnv(reader *bytes.Reader, maximum, nameMaximum, fieldMaximum int) ([]productruntime.EnvVar, error) {
	count, err := readCount(reader, maximum)
	if err != nil {
		return nil, err
	}
	result := make([]productruntime.EnvVar, 0, count)
	for range count {
		name, nameErr := readString(reader, nameMaximum)
		value, valueErr := readString(reader, fieldMaximum)
		if nameErr != nil || valueErr != nil {
			return nil, ErrProtocol
		}
		result = append(result, productruntime.EnvVar{Name: name, Value: value})
	}
	return result, nil
}

func readCount(reader *bytes.Reader, maximum int) (int, error) {
	var count uint16
	if binary.Read(reader, binary.BigEndian, &count) != nil || int(count) > maximum {
		return 0, ErrProtocol
	}
	return int(count), nil
}

func invalidRequiredField(value string, maximum int) bool {
	return value == "" || len(value) > maximum || strings.IndexByte(value, 0) >= 0
}

func invalidOptionalField(value string, maximum int) bool {
	return len(value) > maximum || strings.IndexByte(value, 0) >= 0
}

func validEnv(name, value string, limits Limits) bool {
	return len(name) <= limits.MaxEnvNameBytes && envName.MatchString(name) && len(value) <= limits.MaxFieldBytes && strings.IndexByte(value, 0) < 0
}

func validTicketID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func zero(body []byte) {
	for index := range body {
		body[index] = 0
	}
}

func protocolDigest(body []byte) [sha256.Size]byte { return sha256.Sum256(body) }
