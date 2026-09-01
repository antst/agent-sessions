//go:build linux || darwin

package launchhandoff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/productruntime"
)

// Consume obtains exactly one command. The socket is CLOEXEC from creation and
// is closed before this function returns, including every error path.
func Consume(ctx context.Context, endpoint string, ticket Ticket, limits Limits) (productruntime.NativeCommand, error) {
	if ctx == nil || endpoint == "" || !limits.valid() {
		return productruntime.NativeCommand{}, ErrInvalid
	}
	connection, err := localtransport.DialBytes(endpoint, uint32(limits.MaxCommandBytes)) //nolint:gosec // validated positive.
	if err != nil {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: dial", ErrUnavailable)
	}
	defer func() { _ = connection.Close() }()
	deadline := time.Now().Add(limits.HandshakeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	claim, err := encodeClaim(ticket)
	if err != nil {
		return productruntime.NativeCommand{}, err
	}
	if err := connection.WriteFrame(claim); err != nil {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: claim", ErrUnavailable)
	}
	body, err := connection.ReadFrame()
	if err != nil {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: command", ErrUnavailable)
	}
	if _, headerErr := readHeader(body, frameError); headerErr == nil {
		serverErr := decodeServerError(body)
		zero(body)
		return productruntime.NativeCommand{}, serverErr
	}
	digest := protocolDigest(body)
	command, err := decodeCommand(body, limits)
	zero(body)
	if err != nil {
		return productruntime.NativeCommand{}, err
	}
	ack := encodeAck(digest)
	if err := connection.WriteFrame(ack); err != nil {
		zero(ack)
		clearCommand(&command)
		return productruntime.NativeCommand{}, fmt.Errorf("%w: acknowledge", ErrUnavailable)
	}
	zero(ack)
	confirmation, err := connection.ReadFrame()
	if err != nil {
		clearCommand(&command)
		return productruntime.NativeCommand{}, fmt.Errorf("%w: confirmation", ErrUnavailable)
	}
	defer zero(confirmation)
	if _, err := readHeader(confirmation, frameGo); err != nil || len(confirmation) != 7 {
		if serverErr := decodeServerError(confirmation); serverErr != ErrProtocol {
			clearCommand(&command)
			return productruntime.NativeCommand{}, serverErr
		}
		clearCommand(&command)
		return productruntime.NativeCommand{}, ErrProtocol
	}
	if err := connection.Close(); err != nil {
		clearCommand(&command)
		return productruntime.NativeCommand{}, fmt.Errorf("%w: close before exec", ErrUnavailable)
	}
	return command, nil
}

// ConsumeAndExec consumes the handoff and replaces the wrapper image through
// the platform syscall exec implementation. It never exposes an injectable
// production callback.
func ConsumeAndExec(ctx context.Context, endpoint string, ticket Ticket, limits Limits) error {
	return consumeAndExec(ctx, endpoint, ticket, limits, nativeExec)
}

// consumeAndExec is the unit-test seam. The child environment is exactly the
// envelope environment; ambient wrapper variables are neither read nor merged.
func consumeAndExec(ctx context.Context, endpoint string, ticket Ticket, limits Limits, execute execFunc) error {
	if execute == nil {
		return ErrInvalid
	}
	command, err := Consume(ctx, endpoint, ticket, limits)
	if err != nil {
		return err
	}
	environment, err := exactEnvironment(command, limits)
	if err != nil {
		clearCommand(&command)
		return err
	}
	err = execute(command.Path, append([]string(nil), command.Args...), environment, command.Cwd)
	for index := range environment {
		environment[index] = ""
	}
	clearCommand(&command)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return nil
}
