package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
)

const (
	SocketEnv    = "AGENTBUS_SOCKET"
	LocalKeyEnv  = "AGENTBUS_LOCAL_KEY"
	TokenEnv     = "AGENTBUS_LAUNCH_TOKEN"
	SessionIDEnv = "AGENTBUS_SESSION_ID"
	NameEnv      = "AGENTBUS_SESSION_NAME"
	GroupsEnv    = "AGENTBUS_GROUPS"
)

func LaneMode() bool { _, present := os.LookupEnv(TokenEnv); return present }

func LaunchTokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:16])
}

type PeerIdentity struct{ SessionID, Name string }

type ExecPlan struct {
	Path string
	Args []string
	Env  []string
}

type Child struct {
	command  *exec.Cmd
	lock     *SessionLock
	endpoint *PrivateEndpoint
	done     chan struct{}
	err      error
}

func StartChild(command *exec.Cmd, lock *SessionLock, endpoint *PrivateEndpoint) (*Child, error) {
	if command.Env == nil {
		command.Env = os.Environ()
	}
	for _, name := range []string{SocketEnv, LocalKeyEnv, TokenEnv, SessionIDEnv, NameEnv, GroupsEnv} {
		command.Env = replaceEnv(command.Env, name, "")
	}
	command.ExtraFiles = append(command.ExtraFiles, lock.File())
	if err := command.Start(); err != nil {
		return nil, err
	}
	child := &Child{command: command, lock: lock, endpoint: endpoint, done: make(chan struct{})}
	go func() { child.err = command.Wait(); close(child.done) }()
	return child, nil
}

func (c *Child) Done() <-chan struct{} { return c.done }
func (c *Child) Wait() error           { <-c.done; return c.err }
func (c *Child) Close(ctx context.Context, stop func(context.Context) error) error {
	_ = stop(ctx)
	_ = c.Wait()
	return errors.Join(c.endpoint.Close(), c.lock.Close())
}

// InteractivePlan removes only wrapper-owned group flags. Native option arity
// protects a following value that happens to look like a group flag.
func InteractivePlan(path string, args, environment []string, identity PeerIdentity, consumesValue func(string) bool) (ExecPlan, error) {
	if path == "" {
		return ExecPlan{}, errors.New("product executable is required")
	}
	forwarded, groups := make([]string, 0, len(args)), []string{}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			forwarded = append(forwarded, args[index:]...)
			break
		}
		if argument == "-g" || argument == "--group" {
			if index+1 == len(args) || strings.TrimSpace(args[index+1]) == "" {
				return ExecPlan{}, errors.New("-g/--group requires a non-empty value")
			}
			groups, index = append(groups, args[index+1]), index+1
			continue
		}
		if strings.HasPrefix(argument, "-g=") || strings.HasPrefix(argument, "--group=") {
			_, group, _ := strings.Cut(argument, "=")
			if strings.TrimSpace(group) == "" {
				return ExecPlan{}, errors.New("-g/--group requires a non-empty value")
			}
			groups = append(groups, group)
			continue
		}
		if argument == "-n" || argument == "--peer-name" {
			if index+1 == len(args) || strings.TrimSpace(args[index+1]) == "" {
				return ExecPlan{}, errors.New("-n/--peer-name requires a non-empty value")
			}
			identity.Name, index = args[index+1], index+1
			continue
		}
		if strings.HasPrefix(argument, "-n=") || strings.HasPrefix(argument, "--peer-name=") {
			_, identity.Name, _ = strings.Cut(argument, "=")
			if strings.TrimSpace(identity.Name) == "" {
				return ExecPlan{}, errors.New("-n/--peer-name requires a non-empty value")
			}
			continue
		}
		forwarded = append(forwarded, argument)
		if consumesValue != nil && consumesValue(argument) && index+1 < len(args) {
			index++
			forwarded = append(forwarded, args[index])
		}
	}
	encoded, _ := json.Marshal(groups)
	environment = replaceEnv(environment, GroupsEnv, string(encoded))
	environment = replaceEnv(environment, SessionIDEnv, identity.SessionID)
	environment = replaceEnv(environment, NameEnv, identity.Name)
	return ExecPlan{Path: path, Args: forwarded, Env: environment}, nil
}

type ArgumentRule struct {
	Name          string
	TakesValue    bool
	ConflictField string
	Conflict      func(value string) string
}

func BuildArguments(arguments []string, rules []ArgumentRule) ([]string, error) {
	result := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		name, value, attached := strings.Cut(argument, "=")
		var rule *ArgumentRule
		for candidate := range rules {
			if rules[candidate].Name == name {
				rule = &rules[candidate]
				break
			}
		}
		if rule == nil || attached && !rule.TakesValue {
			return nil, errors.New("unsupported argument " + argument)
		}
		result = append(result, argument)
		if rule.TakesValue && !attached {
			if index+1 == len(arguments) {
				return nil, errors.New(argument + " requires a value")
			}
			index++
			value = arguments[index]
			result = append(result, value)
		}
		field := rule.ConflictField
		if rule.Conflict != nil {
			field = rule.Conflict(value)
		}
		if field != "" {
			return nil, errors.New("argument conflicts with typed field " + field)
		}
	}
	return result, nil
}

func replaceEnv(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key != name {
			result = append(result, entry)
		}
	}
	if value == "" {
		return result
	}
	return append(result, name+"="+value)
}
