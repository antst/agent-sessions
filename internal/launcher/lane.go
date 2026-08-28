package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

const launcherLaneInputLimit = 1024 * 1024

var (
	queryLaneDaemon           = daemon.QueryLocalControl
	laneInput       io.Reader = os.Stdin
	laneOutput      io.Writer = os.Stdout
)

// RunLane is a short-lived client of the existing unified daemon. It never
// discovers or execs a native runtime and never manages daemon lifetime.
func RunLane(role string, args []string) error {
	product, ok := launcherProductByLaneRole(role)
	if !ok {
		return fmt.Errorf("unsupported lane role %q", role)
	}
	if laneHelpRequested(args) {
		// Canonical help is rendered by cmd/agent-sessions before this boundary.
		return nil
	}
	if len(args) == 0 {
		return errors.New("lane operation is required")
	}
	command := strings.TrimSpace(args[0])
	host, arguments, err := extractLaneHost(args[1:])
	if err != nil {
		return err
	}
	if host != "" && launcherLanePromptFile(arguments) != "" {
		return errors.New("--prompt-file is not supported for remote lanes; provide bounded prompt input on stdin")
	}
	input, err := readLauncherLaneInput(command, arguments)
	if err != nil {
		return err
	}
	duration := 5 * time.Minute
	if command == "run" || command == "resume" || command == "wait" {
		duration = 24 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	response, err := queryLaneDaemon(ctx, daemon.InheritedLauncherIdentity(product.descriptor.ID), "lane.command", daemon.LaneCommandRequest{
		Product: product.descriptor.ID, Command: command, Host: host,
		Arguments: append([]string(nil), arguments...), Input: input,
	})
	if err != nil {
		return err
	}
	if len(response.Result) == 0 {
		return errors.New("daemon returned an empty lane result")
	}
	_, err = laneOutput.Write(append(append([]byte(nil), response.Result...), '\n'))
	return err
}

func extractLaneHost(arguments []string) (string, []string, error) {
	result := make([]string, 0, len(arguments))
	host := ""
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if strings.HasPrefix(argument, "--host=") {
			if host != "" {
				return "", nil, errors.New("lane --host may be specified only once")
			}
			host = strings.TrimSpace(strings.TrimPrefix(argument, "--host="))
			if host == "" {
				return "", nil, errors.New("lane --host requires a non-empty host id")
			}
			continue
		}
		if argument == "--host" {
			if host != "" {
				return "", nil, errors.New("lane --host may be specified only once")
			}
			remaining := arguments[index+1:]
			if len(remaining) == 0 {
				return "", nil, errors.New("lane --host requires a host id")
			}
			index++
			host = strings.TrimSpace(remaining[0])
			if host == "" || strings.HasPrefix(host, "-") {
				return "", nil, errors.New("lane --host requires a non-empty host id")
			}
			continue
		}
		result = append(result, argument)
	}
	return host, result, nil
}

func readLauncherLaneInput(command string, arguments []string) (string, error) {
	if command != "run" && command != "start" && command != "resume" {
		return "", nil
	}
	reader := laneInput
	var file *os.File
	if path := launcherLanePromptFile(arguments); path != "" {
		opened, err := os.Open(path) //nolint:gosec // explicit operator-owned prompt path.
		if err != nil {
			return "", err
		}
		file, reader = opened, opened
		defer func() { _ = file.Close() }()
	}
	body, err := io.ReadAll(io.LimitReader(reader, launcherLaneInputLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > launcherLaneInputLimit {
		return "", fmt.Errorf("lane input exceeds %d bytes", launcherLaneInputLimit)
	}
	return string(body), nil
}

func launcherLanePromptFile(arguments []string) string {
	for index, argument := range arguments {
		if strings.HasPrefix(argument, "--prompt-file=") {
			return strings.TrimPrefix(argument, "--prompt-file=")
		}
		if argument == "--prompt-file" && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}

func laneHelpRequested(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, argument := range args {
		if argument == "-h" || argument == "--help" {
			return true
		}
	}
	return false
}

func launcherProductByLaneRole(role string) (launcherProduct, bool) {
	for _, descriptor := range productcatalog.ProductDescriptors() {
		if descriptor.LaneRuntimeRole == role {
			return launcherProduct{descriptor: descriptor}, true
		}
	}
	return launcherProduct{}, false
}
