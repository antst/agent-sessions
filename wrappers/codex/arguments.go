package codex

import (
	"errors"
	"slices"
	"strings"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

var processRules = []host.ArgumentRule{
	{Name: "-c", TakesValue: true, Conflict: configConflict},
	{Name: "--config", TakesValue: true, Conflict: configConflict},
	{Name: "--enable", TakesValue: true},
	{Name: "--disable", TakesValue: true},
	{Name: "--search"},
}

func processArguments(arguments []string) ([]string, error) {
	return host.BuildArguments(arguments, processRules)
}

func configConflict(value string) string {
	key, _, _ := strings.Cut(strings.TrimSpace(value), "=")
	key = strings.Map(func(character rune) rune {
		if character == ' ' || character == '\t' || character == '\'' || character == '"' {
			return -1
		}
		return character
	}, key)
	for prefix, field := range map[string]string{
		"cwd": "cwd", "model": "model", "model_reasoning_effort": "reasoning_effort",
		"approval_policy": "permission_mode", "sandbox": "permission_mode", "sandbox_mode": "permission_mode",
		"thread_id": "session_id", "thread_name": "name", "mcp_servers.agent_sessions": "mcp",
	} {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			return field
		}
	}
	return ""
}

func permission(value string) (string, string, error) {
	switch value {
	case "", "default":
		return "never", "", nil
	case "bypassPermissions":
		return "never", "danger-full-access", nil
	default:
		return "", "", errors.New("unsupported value permission_mode=" + value)
	}
}

func namePart(name string) (string, error) {
	index := strings.LastIndexByte(name, '@')
	if index < 1 {
		return "", errors.New("Codex lane name is invalid")
	}
	return name[:index], nil
}

func InteractivePlan(arguments, environment []string) (host.ExecPlan, error) {
	name, forwarded, err := peerName(arguments)
	if err != nil {
		return host.ExecPlan{}, err
	}
	if !slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		environment = append(environment, host.SocketEnv+"="+sessionkit.Socket())
	}
	return host.InteractivePlan("codex", forwarded, environment, host.PeerIdentity{Name: name}, codexOptionTakesValue)
}

func peerName(arguments []string) (string, []string, error) {
	forwarded := make([]string, 0, len(arguments))
	name := ""
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		key, value, attached := strings.Cut(argument, "=")
		if key == "-n" || key == "--peer-name" {
			if !attached {
				if index+1 == len(arguments) {
					return "", nil, errors.New(argument + " requires a value")
				}
				index++
				value = arguments[index]
			}
			if strings.TrimSpace(value) == "" {
				return "", nil, errors.New(argument + " requires a value")
			}
			name = value
			continue
		}
		forwarded = append(forwarded, argument)
		if argument == "--" {
			forwarded = append(forwarded, arguments[index+1:]...)
			break
		}
		if !attached && codexOptionTakesValue(key) && index+1 < len(arguments) {
			index++
			forwarded = append(forwarded, arguments[index])
		}
	}
	return name, forwarded, nil
}

func codexOptionTakesValue(name string) bool {
	_, found := map[string]bool{
		"-a": true, "--ask-for-approval": true, "-c": true, "--config": true,
		"-C": true, "--cd": true, "--disable": true, "--enable": true,
		"-m": true, "--model": true, "-p": true, "--profile": true,
		"-s": true, "--sandbox": true,
	}[name]
	return found
}
