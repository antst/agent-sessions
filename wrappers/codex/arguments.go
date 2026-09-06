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
	originalEnvironment := environment
	if !slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		environment = append(environment, host.SocketEnv+"="+sessionkit.Socket())
	}
	passthrough, remote := false, false
	plan, err := host.InteractivePlan("codex", arguments, environment, host.PeerIdentity{}, func(argument string) bool {
		key, _, attached := strings.Cut(argument, "=")
		if key == "--remote" || key == "--remote-auth-token-env" {
			remote = true
		}
		if argument == "-h" || argument == "--help" || argument == "-V" || argument == "--version" || codexSubcommand(argument) {
			passthrough = true
		}
		return !attached && codexOptionTakesValue(key)
	})
	if err != nil {
		return host.ExecPlan{}, err
	}
	if passthrough {
		return host.ExecPlan{Path: "codex", Args: arguments, Env: originalEnvironment}, nil
	}
	if remote {
		return host.ExecPlan{}, errors.New("caller-controlled --remote options are not supported")
	}
	return plan, nil
}

func codexSubcommand(argument string) bool {
	switch argument {
	case "agents", "exec", "e", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "remote-control", "completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "unarchive", "cloud", "app", "exec-server", "features", "help", "migrate-rollouts":
		return true
	}
	return false
}

func codexOptionTakesValue(name string) bool {
	_, found := map[string]bool{
		"-a": true, "--ask-for-approval": true, "-c": true, "--config": true,
		"-C": true, "--cd": true, "--disable": true, "--enable": true,
		"-i": true, "--image": true, "-m": true, "--model": true,
		"--local-provider": true, "-p": true, "--profile": true,
		"--remote": true, "--remote-auth-token-env": true,
		"-s": true, "--sandbox": true, "--add-dir": true,
	}[name]
	return found
}
