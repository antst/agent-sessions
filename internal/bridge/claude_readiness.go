package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
)

type claudeAuthenticationStatus struct {
	LoggedIn    bool   `json:"loggedIn"`
	AuthMethod  string `json:"authMethod"`
	APIProvider string `json:"apiProvider"`
}

type ClaudeLaneReadiness struct {
	LoggedIn    bool   `json:"logged_in"`
	AuthMethod  string `json:"auth_method,omitempty"`
	APIProvider string `json:"api_provider,omitempty"`
}

// InspectClaudeLaneReadiness asks Claude directly about the currently selected
// native profile. No daemon-owned profile or session record participates.
func InspectClaudeLaneReadiness(claudeBin string) (ClaudeLaneReadiness, error) {
	source, err := claudeprofile.CurrentSource()
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR")); configured != "" {
		source, err = claudeprofile.SharedSource(configured)
	}
	if err != nil {
		return ClaudeLaneReadiness{}, err
	}
	status, err := inspectClaudeAuthenticationWithEnvironment(claudeBin, claudeAuthenticationEnvironment(os.Environ(), source))
	return ClaudeLaneReadiness(status), err
}

func inspectClaudeAuthenticationWithEnvironment(claudeBin string, environment []string) (claudeAuthenticationStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, claudeBin, "auth", "status", "--json")
	command.Env = environment
	var stderr strings.Builder
	command.Stderr = &stderr
	body, commandErr := command.Output()
	if ctx.Err() != nil {
		return claudeAuthenticationStatus{}, errors.New("authentication check for Claude Code timed out")
	}
	var status claudeAuthenticationStatus
	if err := json.Unmarshal(body, &status); err != nil {
		if commandErr != nil {
			return status, fmt.Errorf("authentication check for Claude Code failed: %w: %s", commandErr, strings.TrimSpace(stderr.String()))
		}
		return status, fmt.Errorf("decode Claude Code authentication status: %w", err)
	}
	if commandErr != nil && status.LoggedIn {
		return status, commandErr
	}
	return status, nil
}

func claudeAuthenticationEnvironment(environment []string, source claudeprofile.Source) []string {
	blocked := map[string]bool{
		"CLAUDE_CODE_SESSION_ID": true, "CLAUDE_PID": true, "CLAUDE_CODE_MESSAGING_SOCKET": true,
		"CLAUDE_CODE_ENTRYPOINT": true, "CLAUDECODE": true, "CLAUDE_CODE_CHILD_SESSION": true,
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": true, "CLAUDE_CODE_HARBOR_KITE": true,
		"CLAUDE_CODE_SIMPLE": true, "CLAUDE_CONFIG_DIR": true,
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR": true, "CLAUDE_SECURESTORAGE_CONFIG_DIR": true,
	}
	result := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] {
			result = append(result, entry)
		}
	}
	result = append(result, "CLAUDE_PEER_CLAUDE_CONFIG_DIR="+source.ConfigRoot)
	if source.ConfigEnvSet {
		result = append(result, "CLAUDE_CONFIG_DIR="+source.ConfigEnvValue)
	}
	if source.SecureEnvSet {
		result = append(result, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+source.SecureConfig)
	}
	return result
}
