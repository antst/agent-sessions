package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/antst/sessionbus/internal/livepresence"
	"github.com/antst/sessionbus/internal/productruntime"
	"github.com/antst/sessionbus/internal/stateroot"
)

type options struct {
	name   string
	groups []string
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	code := 2
	if err == nil {
		code, err = 1, run(opts, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(code)
	}
}

func parseOptions(args []string) (options, error) {
	var opts options
	set := flag.NewFlagSet("verbatim-writer", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&opts.name, "name", "", "session name")
	set.Func("group", "session group (repeatable)", func(value string) error {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("group is empty")
		}
		opts.groups = append(opts.groups, value)
		return nil
	})
	if err := set.Parse(args); err != nil || set.NArg() != 0 || strings.TrimSpace(opts.name) == "" || len(opts.groups) == 0 {
		return options{}, fmt.Errorf("usage: verbatim-writer --name NAME --group GROUP [--group GROUP ...]")
	}
	return opts, nil
}

func run(opts options, output io.Writer) error {
	endpoint, err := stateroot.PresenceSocket()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminal := make(chan error, 1)
	var outputMu sync.Mutex
	livepresence.StartClient(ctx, endpoint, livepresence.Report{
		UUID: fmt.Sprintf("verbatim-writer-%d", os.Getpid()), Name: opts.name, Groups: opts.groups,
		Product: "verbatim-writer", Info: livepresence.CwdInfo(cwd),
	}, func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "message.deliver" {
			return nil, fmt.Errorf("unsupported method %s", method)
		}
		outputMu.Lock()
		err := writeDelivery(output, params)
		outputMu.Unlock()
		if err != nil {
			select {
			case terminal <- err:
			default:
			}
		}
		return json.RawMessage(`{}`), err
	})
	return <-terminal
}

func writeDelivery(output io.Writer, params json.RawMessage) error {
	message, err := livepresence.DecodeDeliver(params)
	if err != nil {
		return err
	}
	record := struct {
		Time      string                             `json:"time"`
		MessageID string                             `json:"message_id"`
		From      productruntime.NativeMessageSource `json:"from"`
		Body      string                             `json:"body"`
	}{time.Now().UTC().Format(time.RFC3339Nano), message.ID, message.From, message.Body}
	return json.NewEncoder(output).Encode(record)
}
