package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const agyLaunchTokenEnv = "AGENT_SESSIONS_AGY_LAUNCH_TOKEN"

type agyPlan struct {
	peerName        string
	forwarded       []string
	passthroughOnly bool
	permissionMode  string
}

// RunAgyPeer starts an Antigravity process whose hooks may publish only the
// exact live launcher process as a peer. Native informational commands and
// subcommands remain transparent passthroughs.
func RunAgyPeer(args []string) error {
	plan, err := parseAgyPeerArgs(args)
	if err != nil {
		return err
	}
	agy, err := agyExecutable()
	if err != nil {
		return err
	}
	if plan.passthroughOnly {
		return Exec(agy, plan.forwarded, nil)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	runtimePath, err := peerRuntimeExecutable()
	if err != nil {
		return err
	}
	ownerPID := os.Getpid()
	ownerStart, err := capture(runtimePath, "launch", "proc-start", strconv.Itoa(ownerPID))
	if err != nil {
		return fmt.Errorf("capture launcher process identity: %w", err)
	}
	prepareArgs := []string{
		"agy-launch", "prepare", "--cwd", cwd,
		"--owner-pid", strconv.Itoa(ownerPID), "--owner-proc-start", strings.TrimSpace(ownerStart),
		"--permission-mode", plan.permissionMode,
	}
	if plan.peerName != "" {
		prepareArgs = append(prepareArgs, "--name", plan.peerName)
	}
	token, err := capture(runtimePath, prepareArgs...)
	if err != nil {
		return errors.New("failed to prepare the Antigravity peer launch")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("native runtime returned an empty Antigravity launch token")
	}
	environment := append([]string{}, os.Environ()...)
	environment = replaceEnvironment(environment, agyLaunchTokenEnv, token)
	return Exec(agy, plan.forwarded, environment)
}

func parseAgyPeerArgs(args []string) (agyPlan, error) {
	plan := agyPlan{permissionMode: "default"}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			plan.forwarded = append(plan.forwarded, args[index:]...)
			index = len(args)
		case argument == "-n" || argument == "--peer-name":
			index++
			if index >= len(args) {
				return agyPlan{}, usageError("-n/--peer-name requires a non-empty value")
			}
			plan.peerName = strings.TrimSpace(args[index]) // #nosec G602 -- index is checked against len(args) immediately above.
			if plan.peerName == "" {
				return agyPlan{}, usageError("-n/--peer-name requires a non-empty value")
			}
		case strings.HasPrefix(argument, "-n=") || strings.HasPrefix(argument, "--peer-name="):
			plan.peerName = strings.TrimSpace(strings.SplitN(argument, "=", 2)[1])
			if plan.peerName == "" {
				return agyPlan{}, usageError("-n/--peer-name requires a non-empty value")
			}
		default:
			plan.forwarded = append(plan.forwarded, argument)
		}
	}
	plan.passthroughOnly = agyPassthrough(plan.forwarded)
	if plan.passthroughOnly && plan.peerName != "" {
		return agyPlan{}, usageError("-n/--peer-name applies only to an interactive or headless Antigravity session")
	}
	for _, argument := range beforeDoubleDash(plan.forwarded) {
		if argument == "--dangerously-skip-permissions" {
			plan.permissionMode = "bypassPermissions"
		}
	}
	return plan, nil
}

func agyPassthrough(args []string) bool {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			return true
		}
		if strings.HasPrefix(argument, "-") {
			if agyOptionConsumesNext(argument) {
				index++
			}
			continue
		}
		switch argument {
		case "agent", "agents", "changelog", "help", "install", "models", "plugin", "plugins", "update":
			return true
		default:
			return false
		}
	}
	return false
}

func agyOptionConsumesNext(argument string) bool {
	options := []string{
		"--add-dir", "--agent", "--conversation", "--effort", "--json-schema", "--log-file",
		"--mode", "--model", "--output-format", "--print", "--print-timeout", "--project",
		"--prompt", "--prompt-interactive",
	}
	for _, option := range options {
		if argument == option {
			return true
		}
		if strings.HasPrefix(argument, option+"=") {
			return false
		}
	}
	if len(argument) > 2 && (strings.HasPrefix(argument, "-p") || strings.HasPrefix(argument, "-i")) {
		return false
	}
	return argument == "-p" || argument == "-i"
}

func agyExecutable() (string, error) {
	if path := os.Getenv("AGY_PEER_AGY_BIN"); path != "" {
		return path, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		userCLI := filepath.Join(home, ".local", "bin", "agy")
		if info, statErr := os.Stat(userCLI); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return userCLI, nil
		}
	}
	path, err := exec.LookPath("agy")
	if err != nil {
		return "", &ExitError{Code: 127, Err: errors.New("agy was not found on PATH")}
	}
	if isAntigravityGUIExecutable(path) {
		return "", &ExitError{Code: 127, Err: errors.New("agy on PATH is Antigravity's GUI helper, not the headless Agy CLI; install the CLI at ~/.local/bin/agy or set AGY_PEER_AGY_BIN")}
	}
	return path, nil
}

func isAntigravityGUIExecutable(path string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return strings.HasSuffix(filepath.ToSlash(filepath.Clean(path)), "/.antigravity/antigravity/bin/agy")
}

func peerRuntimeExecutable() (string, error) {
	if path := os.Getenv("AGY_PEER_NATIVE_RUNTIME"); path != "" {
		return path, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve agy-peer executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve agy-peer symlink: %w", err)
	}
	path := filepath.Join(filepath.Dir(executable), "agent-session-runtime")
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("native runtime is unavailable beside agy-peer: %s", path)
	}
	return path, nil
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}
