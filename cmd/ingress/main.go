package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/stateroot"
)

type options struct {
	groups []string
	target string
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	code := 2
	if err == nil {
		code, err = 1, run(opts, os.Stdin, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(code)
	}
}

func parseOptions(args []string) (options, error) {
	var opts options
	set := flag.NewFlagSet("ingress", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.Func("group", "session group (repeatable)", func(value string) error {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("group is empty")
		}
		opts.groups = append(opts.groups, value)
		return nil
	})
	set.StringVar(&opts.target, "target", "", "optional direct target")
	if err := set.Parse(args); err != nil || set.NArg() != 0 || len(opts.groups) == 0 || (opts.target != "" && strings.TrimSpace(opts.target) == "") {
		return options{}, fmt.Errorf("usage: ingress --group GROUP [--group GROUP ...] [--target TARGET]")
	}
	return opts, nil
}

func run(opts options, input io.Reader, stderr io.Writer) error {
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
	client := livepresence.StartClient(ctx, endpoint, livepresence.Report{
		UUID: fmt.Sprintf("ingress-%d", os.Getpid()), Name: "ingress", Groups: opts.groups,
		Product: "ingress", Info: livepresence.CwdInfo(cwd),
	}, nil)
	return scan(input, stderr, func(line int, body string) error {
		_, err := client.Call(ctx, fmt.Sprintf("ingress.%d", line), "message.send", messageParams(opts, body))
		return err
	})
}

func scan(input io.Reader, stderr io.Writer, send func(int, string) error) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), federation.MaxAgentContent+1)
	line := 0
	for scanner.Scan() {
		line++
		body := scanner.Text()
		if body == "" {
			continue
		}
		if !utf8.ValidString(body) {
			fmt.Fprintf(stderr, "line %d rejected: invalid UTF-8\n", line)
			continue
		}
		if err := send(line, body); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("line %d rejected: %w", line+1, err)
	}
	return nil
}

func messageParams(opts options, body string) map[string]any {
	params := map[string]any{"message": body}
	if opts.target != "" {
		params["target"] = opts.target
	} else {
		params["group"] = opts.groups[0]
	}
	return params
}
