package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	sdk "github.com/antst/agent-sessions/bus/sdk/go"
)

type activeRun struct {
	cancel   context.CancelFunc
	injected []string
}

type example struct {
	mu     sync.Mutex
	active *activeRun
	queued []string
	call   func(context.Context, string, any, any) error
}

func main() {
	product := &example{}
	worker := sdk.NewWorker(product)
	product.call = worker.Call
	if err := worker.Serve(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (*example) Hello(context.Context) (sdk.HelloDescription, error) {
	return sdk.HelloDescription{
		Product:             "example-peer",
		Version:             "1",
		SupportedOpenFields: []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"},
		ExtraArguments: []sdk.ExtraArgument{{
			Name: "session_id", Description: "product session id", TakesValue: true,
		}},
	}, nil
}

func (*example) Open(_ context.Context, request sdk.OpenRequest) (sdk.OpenResult, error) {
	id := request.ResumeSessionID
	for _, argument := range request.Open.Arguments {
		key, value, found := strings.Cut(argument, "=")
		if !found || key != "session_id" || value == "" {
			return sdk.OpenResult{}, errors.New("arguments must be session_id=<id>")
		}
		id = value
	}
	if id == "" {
		value := make([]byte, 16)
		if _, err := rand.Read(value); err != nil {
			return sdk.OpenResult{}, err
		}
		id = hex.EncodeToString(value)
	}
	return sdk.OpenResult{SessionID: id}, nil
}

func (p *example) Run(ctx context.Context, run *sdk.Run, input string) (result sdk.TurnResult, err error) {
	ctx, active, queued := p.begin(ctx, run)
	defer func() { result.Result = p.finish(run, active, result.Result) }()
	switch {
	case input == "block":
		<-ctx.Done()
		result.Outcome = "interrupted"
	case strings.HasPrefix(input, "call "):
		method, raw, found := strings.Cut(strings.TrimPrefix(input, "call "), " ")
		if !found || !json.Valid([]byte(raw)) {
			return sdk.TurnResult{}, errors.New("call requires method and params-json")
		}
		var response json.RawMessage
		if err = p.call(ctx, method, json.RawMessage(raw), &response); err == nil {
			result = sdk.TurnResult{Outcome: "completed", Result: string(response)}
		}
	case strings.HasPrefix(input, "fail "):
		err = errors.New(strings.TrimPrefix(input, "fail "))
	default:
		result = sdk.TurnResult{Outcome: "completed", Result: input}
	}
	if queued != "" {
		result.Result = queued + "\n" + result.Result
	}
	return result, err
}

func (p *example) begin(parent context.Context, run *sdk.Run) (context.Context, *activeRun, string) {
	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	active := &activeRun{cancel: cancel}
	p.active, run.Native = active, active
	queued := strings.Join(p.queued, "\n")
	p.queued = nil
	if run.Interrupted() {
		active.cancel()
		active.cancel = nil
	}
	p.mu.Unlock()
	return ctx, active, queued
}

func (p *example) finish(run *sdk.Run, active *activeRun, result string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == active {
		p.active = nil
	}
	run.Native = nil
	if len(active.injected) != 0 {
		if result != "" {
			result += "\n"
		}
		result += strings.Join(active.injected, "\n")
	}
	return result
}

func (p *example) Interrupt(_ context.Context, run *sdk.Run) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	active, _ := run.Native.(*activeRun)
	if active != nil && active.cancel != nil {
		active.cancel()
		active.cancel = nil
	}
	return nil
}

func (p *example) Deliver(_ context.Context, request sdk.DeliveryRequest) (sdk.DeliveryReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != nil {
		p.active.injected = append(p.active.injected, request.Body)
		return sdk.DeliveryReceipt{Disposition: "injected"}, nil
	}
	p.queued = append(p.queued, request.Body)
	return sdk.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func (*example) Close(context.Context, sdk.SessionCloseRequest) error { return nil }
