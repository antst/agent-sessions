// Package clihelp owns the typed command, option, environment, JSON, and exit
// inventory for both shipped executables.
package clihelp

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/pflag"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// CommandVisibility classifies who may invoke a descriptor surface.
type CommandVisibility string

const (
	// VisibilityPublic identifies an operator-facing host command.
	VisibilityPublic CommandVisibility = "public"
	// VisibilityService identifies the foreground user-service entry point.
	VisibilityService CommandVisibility = "service-internal"
	// VisibilityConnector identifies a stateless vendor relay.
	VisibilityConnector CommandVisibility = "connector-internal"
	// VisibilityHub identifies a central-hub-only command.
	VisibilityHub CommandVisibility = "hub-only"
)

// CommandDescriptor is one canonical binary/mode/parser/help contract.
type CommandDescriptor struct {
	Key                string            `json:"key"`
	Binary             string            `json:"binary"`
	Invocation         string            `json:"invocation"`
	Summary            string            `json:"summary"`
	Visibility         CommandVisibility `json:"visibility"`
	OptionNames        []string          `json:"option_names"`
	PassVendorArgs     bool              `json:"pass_vendor_args"`
	DaemonRole         string            `json:"daemon_role"`
	Online             bool              `json:"online"`
	JSONResultContract string            `json:"json_result_contract,omitempty"`
}

// ExitClass defines one stable numeric public outcome.
type ExitClass struct {
	Code        int
	Name        string
	Description string
}

// JSONEnvelopeDescriptor names the exact fields in public JSON envelopes.
type JSONEnvelopeDescriptor struct {
	SuccessFields []string
	FailureFields []string
	ErrorFields   []string
}

// Descriptor is the closed command contract shared by both binaries.
type Descriptor struct {
	Commands           []CommandDescriptor
	HostAliases        []string
	ReleaseExecutables []string
	EnvironmentNames   []string
	ExitClasses        []ExitClass
	JSONEnvelope       JSONEnvelopeDescriptor
	JSONResultFields   map[string][]string
}

// ParsedOptions is the descriptor-owned administrative option projection.
type ParsedOptions struct {
	JSON      bool
	Plan      string
	Arguments []string
}

// Contract returns an independent copy of the canonical command inventory.
func Contract() Descriptor {
	catalog := productcatalog.Catalog()
	descriptor := Descriptor{
		HostAliases:        append([]string(nil), catalog.HostAliases...),
		ReleaseExecutables: append([]string(nil), catalog.ReleaseExecutables...),
		EnvironmentNames: []string{
			"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
			"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "QWEN_HOME", "QWEN_RUNTIME_DIR",
		},
		ExitClasses: []ExitClass{
			{0, "success", "requested read or committed operation completed"},
			{1, "internal", "bounded attributable implementation failure"},
			{2, "usage", "invalid command, option, or positional shape"},
			{3, "unavailable", "required daemon, service manager, hub, or native dependency is unavailable"},
			{4, "refused", "exact blocker, conflict, denial, or unsafe precondition"},
			{5, "incompatible", "unsupported protocol, state, or release contract"},
			{6, "retryable", "operation was not accepted or remains durable debt"},
		},
		JSONEnvelope: JSONEnvelopeDescriptor{
			SuccessFields: []string{"schema_version", "ok", "command", "result"},
			FailureFields: []string{"schema_version", "ok", "command", "error"},
			ErrorFields:   []string{"class", "code", "retryable", "message", "next_action"},
		},
		JSONResultFields: map[string][]string{
			"help":           {"binaries", "modes"},
			"status":         {"runtime_version", "runtime_identity", "generation", "pid", "proc_start", "endpoint", "service", "products", "attachments", "lanes", "federation", "debt"},
			"doctor":         {"healthy", "checks"},
			"hub.status":     {"runtime_version", "runtime_identity", "pid", "proc_start", "listener", "service", "protocol_version", "connected_hosts", "routing", "debt"},
			"hub.doctor":     {"healthy", "checks"},
			"remove.inspect": {"role", "revision", "blockers", "targets", "preserved"},
			"purge.inspect":  {"role", "plan_revision", "targets", "exclusions"},
			"purge.apply":    {"role", "plan_revision", "deleted", "debt"},
		},
	}
	descriptor.Commands = commandInventory(catalog.Products)
	return cloneDescriptor(descriptor)
}

func commandInventory(products []productcatalog.ProductDescriptor) []CommandDescriptor {
	jsonHelp := []string{"--json", "--help", "-h"}
	planOptions := []string{"--plan", "--json", "--help", "-h"}
	commands := make([]CommandDescriptor, 0, 16+11*len(products))
	commands = append(commands,
		command("host.daemon", "agent-sessions", "agent-sessions daemon", "run the foreground user service", VisibilityService, nil, "service", false, ""),
		command("host.help", "agent-sessions", "agent-sessions help [MODE]", "render the canonical command contract", VisibilityPublic, jsonHelp, "admin", false, "help"),
		command("host.status", "agent-sessions", "agent-sessions status", "show metadata-only host status", VisibilityPublic, jsonHelp, "admin", true, "status"),
		command("host.doctor", "agent-sessions", "agent-sessions doctor", "diagnose host readiness without starting it", VisibilityPublic, jsonHelp, "admin", true, "doctor"),
		command("host.remove.inspect", "agent-sessions", "agent-sessions remove inspect", "inspect host removal blockers and targets", VisibilityPublic, jsonHelp, "admin", true, "remove.inspect"),
		command("host.purge.inspect", "agent-sessions", "agent-sessions purge inspect", "create or inspect a revision-bound host purge plan", VisibilityPublic, planOptions, "admin", false, "purge.inspect"),
		command("host.purge.apply", "agent-sessions", "agent-sessions purge apply", "apply an exact host purge plan", VisibilityPublic, planOptions, "admin", false, "purge.apply"),
		command("host.install", "agent-sessions", "agent-sessions lifecycle install --role host --source-root ROOT --prefix PREFIX --version VERSION", "install or upgrade only the unified host role transaction", VisibilityService, hostInstallOptions(), "service", false, ""),
		command("host.remove.apply", "agent-sessions", "agent-sessions lifecycle remove --role host --prefix PREFIX", "remove only the quiescent unified host role transaction", VisibilityService, hostRemoveOptions(), "service", false, ""),
		command("host.connector.install", "agent-sessions", "agent-sessions connector install --product PRODUCT --source-root ROOT", "install one explicit native product connector transaction", VisibilityPublic, connectorInstallOptions(), "service", false, ""),
		command("host.connector.remove", "agent-sessions", "agent-sessions connector remove --product PRODUCT", "remove one explicit native product connector through its supported installer", VisibilityPublic, connectorRemoveOptions(), "service", false, ""),
		command("host.lane", "agent-sessions", "agent-sessions lane --host HOST --product PRODUCT -- COMMAND [ARGS...]", "run one lane operation through the connected host daemon", VisibilityPublic, []string{"--host", "--product", "--help", "-h"}, "launcher", true, ""),
		command("hub.serve", "agent-sessions-hub", "agent-sessions-hub", "run the central federation hub", VisibilityHub, []string{"--listen", "--help", "-h"}, "service", false, ""),
		command("hub.status", "agent-sessions-hub", "agent-sessions-hub status", "show metadata-only hub status", VisibilityHub, jsonHelp, "admin", true, "hub.status"),
		command("hub.doctor", "agent-sessions-hub", "agent-sessions-hub doctor", "diagnose hub readiness without starting it", VisibilityHub, jsonHelp, "admin", true, "hub.doctor"),
		command("hub.remove.inspect", "agent-sessions-hub", "agent-sessions-hub remove inspect", "inspect hub removal blockers and targets", VisibilityHub, jsonHelp, "admin", true, "remove.inspect"),
		command("hub.purge.inspect", "agent-sessions-hub", "agent-sessions-hub purge inspect", "create or inspect a revision-bound hub purge plan", VisibilityHub, planOptions, "admin", false, "purge.inspect"),
		command("hub.purge.apply", "agent-sessions-hub", "agent-sessions-hub purge apply", "apply an exact hub purge plan", VisibilityHub, planOptions, "admin", false, "purge.apply"),
		command("hub.install", "agent-sessions-hub", "agent-sessions-hub lifecycle install --role hub --source-root ROOT --prefix PREFIX --version VERSION", "install or upgrade only the central hub role transaction", VisibilityService, hubInstallOptions(), "service", false, ""),
		command("hub.remove.apply", "agent-sessions-hub", "agent-sessions-hub lifecycle remove --role hub --prefix PREFIX", "remove only the central hub role transaction", VisibilityService, hubRemoveOptions(), "service", false, ""),
		command("peer", "agent-sessions", "peer PRODUCT", "launch or resume an interactive peer", VisibilityPublic, peerOptions(), "launcher", true, ""),
	)
	for _, product := range products {
		commands = append(commands,
			command("peer."+product.ID, "agent-sessions", product.PeerAlias, "launch or resume a "+product.Label+" peer", VisibilityPublic, peerOptions(), "launcher", true, ""),
		)
		for _, operation := range []string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"} {
			commands = append(commands, command(
				"lane."+product.ID+"."+operation, "agent-sessions", product.LaneAlias+" "+operation,
				operation+" a "+product.Label+" lane", VisibilityPublic, laneOptions(product.ID, operation), "launcher", true, "",
			))
		}
		commands = append(commands, command(
			"connector."+product.ID+"."+product.Connector.Mode, "agent-sessions",
			"agent-sessions connector "+product.ID+" "+product.Connector.Mode,
			"run the stateless "+product.Label+" connector relay", VisibilityConnector, nil, "connector", true, "",
		))
	}
	return commands
}

func connectorInstallOptions() []string {
	return []string{"--product", "--source-root", "--native", "--grok-user-root", "--help", "-h"}
}

func hostInstallOptions() []string {
	return []string{
		"--role", "--source-root", "--prefix", "--version", "--codex", "--claude", "--grok", "--qwen", "--help", "-h",
	}
}

func hostRemoveOptions() []string {
	return []string{"--role", "--prefix", "--codex", "--claude", "--grok", "--qwen", "--help", "-h"}
}

func hubInstallOptions() []string {
	return []string{"--role", "--source-root", "--prefix", "--version", "--listen", "--help", "-h"}
}

func hubRemoveOptions() []string {
	return []string{"--role", "--prefix", "--help", "-h"}
}

func connectorRemoveOptions() []string {
	return []string{"--product", "--native", "--grok-user-root", "--help", "-h"}
}

func command(key, binary, invocation, summary string, visibility CommandVisibility, options []string, role string, online bool, result string) CommandDescriptor {
	return CommandDescriptor{
		Key: key, Binary: binary, Invocation: invocation, Summary: summary, Visibility: visibility,
		OptionNames: append([]string(nil), options...), PassVendorArgs: strings.HasPrefix(key, "peer"),
		DaemonRole: role, Online: online, JSONResultContract: result,
	}
}

func peerOptions() []string {
	return []string{"--name", "-n", "--group", "-g", "--inherit-groups", "--no-inherit-groups", "--yolo", "--no-yolo", "--help", "-h"}
}

func laneOptions(product, operation string) []string {
	base := []string{"--host", "--json", "--help", "-h"}
	switch operation {
	case "run", "start", "resume":
		options := []string{
			"--name", "-n", "--peer-name", "--cd", "-C", "--cwd", "--timeout", "--prompt-file",
			"--notify", "--no-notify", "--persistent", "--no-auto-archive", "--auto-archive-after",
			"--group", "-g", "--inherit-groups", "--no-inherit-groups", "--allow-duplicate-name",
		}
		switch product {
		case "codex":
			options = append(options,
				"--model", "-m", "--effort", "--reasoning-effort", "--sandbox", "--approval-policy",
				"--config", "-c", "--web", "--no-web", "--schema", "--worktree", "--skip-git-repo-check",
			)
		case "claude":
			options = append(options,
				"--model", "-m", "--effort", "--permission-mode", "--max-budget-usd", "--tools",
				"--allowed-tools", "--disallowed-tools", "--schema", "--bare", "--worktree",
			)
		case "grok":
			options = append(options,
				"--model", "-m", "--effort", "--reasoning-effort", "--permission-mode", "--always-approve", "--yolo",
			)
		case "qwen":
			options = append(options, "--qwen-home", "--yolo", "--no-yolo", "--approval-mode")
		}
		return append(options, base...)
	case "wait":
		return append([]string{"--timeout"}, base...)
	case "status", "list", "doctor":
		return append([]string{"--all", "--mine"}, base...)
	case "interrupt", "archive":
		return append([]string{"--all", "--mine"}, base...)
	default:
		return base
	}
}

// ParserOptionNames instantiates the descriptor parser and lists its options.
func ParserOptionNames(key string) ([]string, error) {
	parser, err := instantiateParser(key)
	if err != nil {
		return nil, err
	}
	var names []string
	parser.VisitAll(func(flag *pflag.Flag) {
		names = append(names, "--"+flag.Name)
		if flag.Shorthand != "" {
			names = append(names, "-"+flag.Shorthand)
		}
	})
	return names, nil
}

// HelpOptionNames extracts the options emitted by generated command help.
func HelpOptionNames(key string) ([]string, error) {
	help, err := RenderHelp(key)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(help, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "-") {
			names = append(names, strings.Fields(line)[0])
		}
	}
	return names, nil
}

func commandOptions(key string) ([]string, error) {
	for _, command := range Contract().Commands {
		if command.Key == key {
			return append([]string(nil), command.OptionNames...), nil
		}
	}
	return nil, fmt.Errorf("unknown command descriptor %q", key)
}

func instantiateParser(key string) (*pflag.FlagSet, error) {
	options, err := commandOptions(key)
	if err != nil {
		return nil, err
	}
	parser := pflag.NewFlagSet(key, pflag.ContinueOnError)
	parser.SetInterspersed(true)
	shorthands := map[string]string{
		"--name": "n", "--group": "g", "--cd": "C", "--model": "m", "--config": "c", "--help": "h",
	}
	valueOptions := map[string]bool{
		"--name": true, "--peer-name": true, "--group": true, "--cd": true, "--cwd": true,
		"--timeout": true, "--prompt-file": true, "--notify": true, "--auto-archive-after": true,
		"--model": true, "--effort": true, "--reasoning-effort": true, "--sandbox": true,
		"--approval-policy": true, "--config": true, "--schema": true, "--permission-mode": true,
		"--max-budget-usd": true, "--tools": true, "--allowed-tools": true, "--disallowed-tools": true,
		"--approval-mode": true, "--qwen-home": true, "--plan": true, "--listen": true,
		"--host": true, "--product": true, "--source-root": true, "--native": true, "--grok-user-root": true,
		"--role": true, "--prefix": true, "--version": true, "--codex": true, "--claude": true, "--grok": true, "--qwen": true,
	}
	declared := make(map[string]bool)
	for _, option := range options {
		if len(option) == 2 && option[0] == '-' && option[1] != '-' {
			continue
		}
		if !strings.HasPrefix(option, "--") || len(option) < 3 {
			return nil, fmt.Errorf("invalid option descriptor %q for %s", option, key)
		}
		name := strings.TrimPrefix(option, "--")
		if declared[name] {
			return nil, fmt.Errorf("duplicate option descriptor %q for %s", option, key)
		}
		declared[name] = true
		if valueOptions[option] {
			parser.StringP(name, shorthands[option], "", "descriptor-owned value")
		} else {
			parser.BoolP(name, shorthands[option], false, "descriptor-owned switch")
		}
	}
	return parser, nil
}

// ParseOptions validates one command's Agent Sessions-owned options. Vendor
// passthrough surfaces retain their separate native argument boundary.
func ParseOptions(key string, arguments []string) (ParsedOptions, error) {
	parser, err := instantiateParser(key)
	if err != nil {
		return ParsedOptions{}, err
	}
	parser.SetOutput(io.Discard)
	if err := parser.Parse(arguments); err != nil {
		return ParsedOptions{}, err
	}
	result := ParsedOptions{Arguments: append([]string(nil), parser.Args()...)}
	if parser.Lookup("json") != nil {
		result.JSON, _ = parser.GetBool("json")
	}
	if parser.Lookup("plan") != nil {
		result.Plan, _ = parser.GetString("plan")
	}
	if (key == "host.purge.inspect" || key == "host.purge.apply" || key == "hub.purge.inspect" || key == "hub.purge.apply") && result.Plan == "" {
		return ParsedOptions{}, errors.New("--plan requires a value")
	}
	return result, nil
}

// ResolveInvocation maps a shipped argv[0] name to its owning binary image.
func (descriptor Descriptor) ResolveInvocation(name string) (string, bool) {
	return productcatalog.Catalog().ResolveExecutable(filepath.Base(name))
}

// CommandByKey resolves one exact canonical mode.
func CommandByKey(key string) (CommandDescriptor, bool) {
	for _, command := range Contract().Commands {
		if command.Key == key {
			return command, true
		}
	}
	return CommandDescriptor{}, false
}

// ResolveCommand applies only the descriptor-owned command boundary. It does
// not parse or rewrite vendor arguments after a peer handoff.
func ResolveCommand(argv0 string, args []string) (CommandDescriptor, []string, error) {
	name := filepath.Base(argv0)
	catalog := productcatalog.Catalog()
	if name == "agent-sessions-hub" {
		return resolveHubCommand(args)
	}
	if name == "agent-sessions" {
		return resolveHostCommand(args)
	}
	for _, product := range catalog.Products {
		if name == product.PeerAlias {
			return mustCommand("peer." + product.ID), args, nil
		}
		if name == product.LaneAlias {
			if len(args) == 0 {
				return CommandDescriptor{}, nil, errors.New("lane operation is required")
			}
			return commandWithRemainder("lane."+product.ID+"."+args[0], args[1:])
		}
	}
	if name == "peer" {
		if len(args) == 0 {
			return mustCommand("peer"), nil, nil
		}
		if _, ok := productcatalog.ProductByID(args[0]); ok {
			return commandWithRemainder("peer."+args[0], args[1:])
		}
		return mustCommand("peer"), args, nil
	}
	return CommandDescriptor{}, nil, fmt.Errorf("unknown invocation %q", name)
}

//nolint:gocyclo // The canonical host dispatcher keeps every public and internal top-level mode explicit.
func resolveHostCommand(args []string) (CommandDescriptor, []string, error) {
	if len(args) == 0 {
		return CommandDescriptor{}, nil, errors.New("host command is required")
	}
	switch args[0] {
	case "--help", "-h":
		return commandWithRemainder("host.help", nil)
	case "daemon", "help", "status", "doctor":
		return commandWithRemainder("host."+args[0], args[1:])
	case "remove", "purge":
		if len(args) < 2 {
			return CommandDescriptor{}, nil, fmt.Errorf("%s operation is required", args[0])
		}
		return commandWithRemainder("host."+args[0]+"."+args[1], args[2:])
	case "lifecycle":
		if len(args) < 2 || (args[1] != "install" && args[1] != "remove") {
			return CommandDescriptor{}, nil, errors.New("lifecycle install or remove operation is required")
		}
		key := map[string]string{"install": "host.install", "remove": "host.remove.apply"}[args[1]]
		return commandWithRemainder(key, args[2:])
	case "peer":
		return ResolveCommand("peer", args[1:])
	case "lane":
		return commandWithRemainder("host.lane", args[1:])
	case "connector":
		if len(args) >= 2 && (args[1] == "install" || args[1] == "remove") {
			return commandWithRemainder("host.connector."+args[1], args[2:])
		}
		if len(args) < 3 {
			return CommandDescriptor{}, nil, errors.New("connector product and mode are required")
		}
		return commandWithRemainder("connector."+args[1]+"."+args[2], args[3:])
	default:
		return CommandDescriptor{}, nil, fmt.Errorf("unknown host command %q", args[0])
	}
}

func resolveHubCommand(args []string) (CommandDescriptor, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return mustCommand("hub.serve"), args, nil
	}
	switch args[0] {
	case "status", "doctor":
		return commandWithRemainder("hub."+args[0], args[1:])
	case "remove", "purge":
		if len(args) < 2 {
			return CommandDescriptor{}, nil, fmt.Errorf("hub %s operation is required", args[0])
		}
		return commandWithRemainder("hub."+args[0]+"."+args[1], args[2:])
	case "lifecycle":
		if len(args) < 2 || (args[1] != "install" && args[1] != "remove") {
			return CommandDescriptor{}, nil, errors.New("hub lifecycle install or remove operation is required")
		}
		key := map[string]string{"install": "hub.install", "remove": "hub.remove.apply"}[args[1]]
		return commandWithRemainder(key, args[2:])
	default:
		return CommandDescriptor{}, nil, fmt.Errorf("unknown hub command %q", args[0])
	}
}

func commandWithRemainder(key string, remainder []string) (CommandDescriptor, []string, error) {
	command, ok := CommandByKey(key)
	if !ok {
		return CommandDescriptor{}, nil, fmt.Errorf("unsupported command %q", key)
	}
	return command, remainder, nil
}

func mustCommand(key string) CommandDescriptor {
	command, ok := CommandByKey(key)
	if !ok {
		panic("missing canonical command " + key)
	}
	return command
}

// RenderHelp renders human command help from one descriptor.
func RenderHelp(key string) (string, error) {
	command, ok := CommandByKey(key)
	if !ok {
		return "", fmt.Errorf("unknown command descriptor %q", key)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "usage: %s\n\n%s\n", command.Invocation, command.Summary)
	if command.PassVendorArgs {
		output.WriteString("\nArguments after -- are passed unchanged to the native vendor.\n")
	}
	if len(command.OptionNames) != 0 {
		output.WriteString("\nAgent Sessions options:\n")
		for _, option := range command.OptionNames {
			fmt.Fprintf(&output, "  %s\n", option)
		}
	}
	return output.String(), nil
}

// RenderMarkdown renders the checked command reference.
func RenderMarkdown() (string, error) {
	contract := Contract()
	commands := append([]CommandDescriptor(nil), contract.Commands...)
	sort.Slice(commands, func(i, j int) bool { return commands[i].Key < commands[j].Key })
	var output strings.Builder
	output.WriteString("# Agent Sessions command reference\n\n")
	output.WriteString("Generated from `internal/clihelp`; edit the descriptor, not this table.\n\n")
	output.WriteString("## Binaries and modes\n\n| Key | Invocation | Visibility | Online | Summary |\n|---|---|---|---:|---|\n")
	for _, command := range commands {
		fmt.Fprintf(&output, "| `%s` | `%s` | %s | %t | %s |\n", command.Key, command.Invocation, command.Visibility, command.Online, command.Summary)
	}
	output.WriteString("\n## Environment\n\n")
	for _, name := range contract.EnvironmentNames {
		fmt.Fprintf(&output, "- `%s`\n", name)
	}
	output.WriteString("\n## Exit classes\n\n| Code | Class | Meaning |\n|---:|---|---|\n")
	for _, class := range contract.ExitClasses {
		fmt.Fprintf(&output, "| %d | `%s` | %s |\n", class.Code, class.Name, class.Description)
	}
	return output.String(), nil
}

func cloneDescriptor(source Descriptor) Descriptor {
	clone := source
	clone.Commands = append([]CommandDescriptor(nil), source.Commands...)
	for index := range clone.Commands {
		clone.Commands[index].OptionNames = append([]string(nil), source.Commands[index].OptionNames...)
	}
	clone.HostAliases = append([]string(nil), source.HostAliases...)
	clone.ReleaseExecutables = append([]string(nil), source.ReleaseExecutables...)
	clone.EnvironmentNames = append([]string(nil), source.EnvironmentNames...)
	clone.ExitClasses = append([]ExitClass(nil), source.ExitClasses...)
	clone.JSONEnvelope.SuccessFields = append([]string(nil), source.JSONEnvelope.SuccessFields...)
	clone.JSONEnvelope.FailureFields = append([]string(nil), source.JSONEnvelope.FailureFields...)
	clone.JSONEnvelope.ErrorFields = append([]string(nil), source.JSONEnvelope.ErrorFields...)
	clone.JSONResultFields = make(map[string][]string, len(source.JSONResultFields))
	for key, fields := range source.JSONResultFields {
		clone.JSONResultFields[key] = append([]string(nil), fields...)
	}
	return clone
}
