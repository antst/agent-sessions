package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

var peerDaemonCommand = exec.CommandContext

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

func InteractivePlan(arguments, environment []string) (host.ExecPlan, bool, error) {
	remote := false
	plan, passthrough, err := host.ClassifiedInteractivePlan("codex", arguments, environment, host.PeerIdentity{}, func(argument string) bool {
		key, _, attached := strings.Cut(argument, "=")
		if key == "--remote" || key == "--remote-auth-token-env" {
			remote = true
		}
		return !attached && codexOptionTakesValue(key)
	}, func(argument string) bool {
		return argument == "-h" || argument == "--help" || argument == "-V" || argument == "--version" || codexSubcommand(argument)
	})
	if err != nil {
		return host.ExecPlan{}, false, err
	}
	if passthrough {
		return plan, false, nil
	}
	if remote {
		return host.ExecPlan{}, false, errors.New("caller-controlled --remote options are not supported")
	}
	if !slices.ContainsFunc(plan.Env, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		plan.Env = append(plan.Env, host.SocketEnv+"="+sessionkit.Socket())
	}
	socket, err := appServerSocket()
	if err != nil {
		return host.ExecPlan{}, false, err
	}
	plan.Args = append([]string{"--remote", "unix://" + socket}, plan.Args...)
	return plan, true, nil
}

func appServerSocket() (string, error) {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(user, ".codex")
	}
	return filepath.Join(home, "app-server-control", "app-server-control.sock"), nil
}

func StartPeerDaemon(ctx context.Context, path string) error {
	command := peerDaemonCommand(ctx, path, "app-server", "daemon", "start")
	command.Env = slices.DeleteFunc(os.Environ(), func(value string) bool {
		key, _, _ := strings.Cut(value, "=")
		return strings.HasPrefix(key, "AGENTBUS_")
	})
	if err := command.Run(); err != nil {
		return fmt.Errorf("start Codex App Server: %w", err)
	}
	return nil
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
