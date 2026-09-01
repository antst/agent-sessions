//go:build !linux && !darwin

package launchhandoff

import (
	"context"

	"github.com/antst/agent-sessions/internal/productruntime"
)

type Broker struct{}

func Endpoint(string) string                       { return "" }
func NewBroker(Config) (*Broker, error)            { return nil, ErrUnavailable }
func (*Broker) Endpoint() string                   { return "" }
func (*Broker) Stage(StageRequest) (Ticket, error) { return Ticket{}, ErrUnavailable }
func (*Broker) Run(context.Context) error          { return ErrUnavailable }
func (*Broker) Close() error                       { return nil }

func Consume(context.Context, string, Ticket, Limits) (productruntime.NativeCommand, error) {
	return productruntime.NativeCommand{}, ErrUnavailable
}
func ConsumeAndExec(context.Context, string, Ticket, Limits) error { return ErrUnavailable }
