package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RunRemoteLaneCLI submits one canonical remote lane operation through the
// already-running unified daemon. It owns no listener, session, or fallback.
func RunRemoteLaneCLI(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	host, product, commandArguments, err := parseRemoteLaneCLI(arguments)
	if err != nil {
		return err
	}
	command := commandArguments[0]
	var prompt string
	if command == "run" || command == "start" || command == "resume" {
		body, readErr := io.ReadAll(io.LimitReader(input, maxLaneCommandInputBytes+1))
		if readErr != nil {
			return fmt.Errorf("read lane input: %w", readErr)
		}
		if len(body) > maxLaneCommandInputBytes {
			return fmt.Errorf("lane input exceeds %d bytes", maxLaneCommandInputBytes)
		}
		prompt = string(body)
	}
	response, err := QueryLocalControl(ctx, InheritedLauncherIdentity(product), "lane.command", LaneCommandRequest{
		Product: product, Command: command, Host: host,
		Arguments: append([]string(nil), commandArguments[1:]...), Input: prompt,
	})
	if err != nil {
		return err
	}
	if len(response.Result) == 0 {
		return errors.New("daemon returned an empty lane result")
	}
	if _, err := output.Write(append(response.Result, '\n')); err != nil {
		return fmt.Errorf("write lane result: %w", err)
	}
	return nil
}

func parseRemoteLaneCLI(arguments []string) (string, string, []string, error) {
	var host, product string
	remaining := arguments
	for len(remaining) > 0 {
		option := remaining[0]
		remaining = remaining[1:]
		switch option {
		case "--":
			if host == "" || product == "" || len(remaining) == 0 {
				return "", "", nil, errors.New("remote lane requires --host HOST --product PRODUCT -- COMMAND")
			}
			if !supportedLaneCommandProduct(product) {
				return "", "", nil, fmt.Errorf("unsupported lane product %q", product)
			}
			return host, product, append([]string(nil), remaining...), nil
		case "--host", "--product":
			value, tail, valueErr := shiftRemoteLaneArgument(remaining, option)
			if valueErr != nil {
				return "", "", nil, valueErr
			}
			remaining = tail
			if option == "--host" {
				if host != "" {
					return "", "", nil, errors.New("remote lane --host may be specified only once")
				}
				host = value
			} else {
				if product != "" {
					return "", "", nil, errors.New("remote lane --product may be specified only once")
				}
				product = value
			}
		default:
			return "", "", nil, fmt.Errorf("unexpected remote lane option %q", option)
		}
	}
	return "", "", nil, errors.New("remote lane requires --host HOST --product PRODUCT -- COMMAND")
}

func shiftRemoteLaneArgument(arguments []string, option string) (string, []string, error) {
	if len(arguments) == 0 {
		return "", nil, fmt.Errorf("%s requires a value", option)
	}
	value := strings.TrimSpace(arguments[0])
	if value == "" || strings.HasPrefix(value, "-") {
		return "", nil, fmt.Errorf("%s requires a non-empty value", option)
	}
	return value, arguments[1:], nil
}
