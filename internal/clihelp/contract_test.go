package clihelp

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestCanonicalModesAliasesAndBinaryRolesAreComplete(t *testing.T) {
	contract := Contract()
	wantKeys := make([]string, 0, 60)
	wantKeys = append(wantKeys,
		"host.daemon", "host.help", "host.status", "host.doctor",
		"host.remove.inspect",
		"host.purge.inspect", "host.purge.apply", "host.install", "host.remove.apply", "host.connector.install", "host.connector.remove", "host.lane",
		"hub.serve", "hub.status", "hub.doctor", "hub.remove.inspect",
		"hub.purge.inspect", "hub.purge.apply", "hub.install", "hub.remove.apply",
		"peer", "peer.codex", "peer.claude", "peer.grok", "peer.qwen",
	)
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		for _, operation := range []string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"} {
			wantKeys = append(wantKeys, "lane."+product+"."+operation)
		}
		wantKeys = append(wantKeys, "connector."+product+".mcp")
	}
	gotKeys := make([]string, 0, len(contract.Commands))
	seen := map[string]bool{}
	for _, command := range contract.Commands {
		if command.Key == "" || seen[command.Key] {
			t.Fatalf("empty or duplicate command descriptor key %q", command.Key)
		}
		seen[command.Key] = true
		gotKeys = append(gotKeys, command.Key)
	}
	slices.Sort(gotKeys)
	slices.Sort(wantKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("canonical command modes = %q, want %q", gotKeys, wantKeys)
	}

	catalog := productcatalog.Catalog()
	if !slices.Equal(contract.HostAliases, catalog.HostAliases) {
		t.Errorf("CLI aliases = %q, product catalog aliases = %q", contract.HostAliases, catalog.HostAliases)
	}
	if !slices.Equal(contract.ReleaseExecutables, catalog.ReleaseExecutables) {
		t.Errorf("CLI binaries = %q, product catalog binaries = %q", contract.ReleaseExecutables, catalog.ReleaseExecutables)
	}
	for _, alias := range contract.HostAliases {
		if binary, ok := contract.ResolveInvocation(alias); !ok || binary != "agent-sessions" {
			t.Errorf("host alias %q resolves to %q, %v", alias, binary, ok)
		}
	}
	if binary, ok := contract.ResolveInvocation("agent-sessions-hub"); !ok || binary != "agent-sessions-hub" {
		t.Errorf("hub invocation resolves to %q, %v", binary, ok)
	}
}

func TestHubLifecycleModesAreDescriptorBackedAndRoleScoped(t *testing.T) {
	for _, test := range []struct {
		args []string
		key  string
	}{
		{args: []string{"lifecycle", "install", "--role", "hub"}, key: "hub.install"},
		{args: []string{"lifecycle", "remove", "--role", "hub"}, key: "hub.remove.apply"},
	} {
		command, remainder, err := ResolveCommand("agent-sessions-hub", test.args)
		if err != nil {
			t.Fatalf("resolve %v: %v", test.args, err)
		}
		if command.Key != test.key || len(remainder) != 2 || remainder[0] != "--role" || remainder[1] != "hub" {
			t.Fatalf("resolve %v = key %q remainder %q", test.args, command.Key, remainder)
		}
	}
}

func TestPeerLaneAndConnectorDescriptorsCoverTheExistingWorkflowSurface(t *testing.T) {
	contract := Contract()
	byKey := make(map[string]CommandDescriptor, len(contract.Commands))
	for _, command := range contract.Commands {
		byKey[command.Key] = command
	}
	peerOptions := []string{"--name", "--group", "--inherit-groups", "--no-inherit-groups", "--yolo", "--no-yolo", "--help"}
	laneOptions := []string{
		"--host",
		"--name", "--peer-name", "--cd", "--cwd", "--timeout", "--prompt-file",
		"--notify", "--no-notify", "--persistent", "--no-auto-archive", "--auto-archive-after",
		"--group", "--inherit-groups", "--no-inherit-groups", "--allow-duplicate-name",
		"--all", "--mine", "--json", "--help",
	}
	for _, product := range productcatalog.Catalog().Products {
		peer, ok := byKey["peer."+product.ID]
		if !ok {
			t.Errorf("missing %s peer descriptor", product.ID)
		} else {
			assertOptionSubset(t, peer.Key, peerOptions, peer.OptionNames)
		}
		productLaneOptions := []string{}
		for _, operation := range []string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"} {
			key := "lane." + product.ID + "." + operation
			lane, laneOK := byKey[key]
			if !laneOK {
				t.Errorf("missing %s", key)
				continue
			}
			productLaneOptions = append(productLaneOptions, lane.OptionNames...)
		}
		assertOptionSubset(t, product.ID+" lane descriptors", laneOptions, productLaneOptions)
		connector, connectorOK := byKey["connector."+product.ID+"."+product.Connector.Mode]
		if !connectorOK {
			t.Errorf("missing %s connector descriptor", product.ID)
		} else if slices.Contains(connector.OptionNames, "--json") || slices.Contains(connector.OptionNames, "--help") {
			t.Errorf("%s internal stdio connector exposes administrative options: %+v", product.ID, connector.OptionNames)
		}
	}
}

func assertOptionSubset(t *testing.T, key string, want, got []string) {
	t.Helper()
	for _, option := range want {
		if !slices.Contains(got, option) {
			t.Errorf("%s omits parsed/help option %s", key, option)
		}
	}
}

func TestEveryInstantiatedParserOptionMatchesGeneratedHelp(t *testing.T) {
	contract := Contract()
	for _, command := range contract.Commands {
		described := append([]string(nil), command.OptionNames...)
		parsed, err := ParserOptionNames(command.Key)
		if err != nil {
			t.Errorf("inspect parser %s: %v", command.Key, err)
			continue
		}
		help, err := HelpOptionNames(command.Key)
		if err != nil {
			t.Errorf("inspect generated help %s: %v", command.Key, err)
			continue
		}
		slices.Sort(described)
		slices.Sort(parsed)
		slices.Sort(help)
		if !slices.Equal(parsed, described) {
			t.Errorf("%s parsed options = %q, descriptor = %q", command.Key, parsed, described)
		}
		if !slices.Equal(help, described) {
			t.Errorf("%s help options = %q, descriptor = %q", command.Key, help, described)
		}
	}
}

func TestEnvironmentJSONAndNumericExitContractsAreExact(t *testing.T) {
	contract := Contract()
	wantEnvironment := []string{
		"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"QWEN_HOME", "QWEN_RUNTIME_DIR",
	}
	if !slices.Equal(contract.EnvironmentNames, wantEnvironment) {
		t.Errorf("environment inventory = %q, want %q", contract.EnvironmentNames, wantEnvironment)
	}

	wantExitClasses := []ExitClass{
		{Code: 0, Name: "success"},
		{Code: 1, Name: "internal"},
		{Code: 2, Name: "usage"},
		{Code: 3, Name: "unavailable"},
		{Code: 4, Name: "refused"},
		{Code: 5, Name: "incompatible"},
		{Code: 6, Name: "retryable"},
	}
	if len(contract.ExitClasses) != len(wantExitClasses) {
		t.Fatalf("exit classes = %+v, want %+v", contract.ExitClasses, wantExitClasses)
	}
	for index, want := range wantExitClasses {
		got := contract.ExitClasses[index]
		if got.Code != want.Code || got.Name != want.Name {
			t.Errorf("exit class[%d] = %+v, want core fields %+v", index, got, want)
		}
	}

	if !slices.Equal(contract.JSONEnvelope.SuccessFields, []string{"schema_version", "ok", "command", "result"}) ||
		!slices.Equal(contract.JSONEnvelope.FailureFields, []string{"schema_version", "ok", "command", "error"}) ||
		!slices.Equal(contract.JSONEnvelope.ErrorFields, []string{"class", "code", "retryable", "message", "next_action"}) {
		t.Errorf("JSON envelope = %+v", contract.JSONEnvelope)
	}
	wantResults := map[string][]string{
		"help":           {"binaries", "modes"},
		"status":         {"runtime_version", "runtime_identity", "generation", "pid", "proc_start", "endpoint", "service", "products", "attachments", "lanes", "federation", "debt"},
		"doctor":         {"healthy", "checks"},
		"hub.status":     {"runtime_version", "runtime_identity", "pid", "proc_start", "listener", "service", "protocol_version", "connected_hosts", "routing", "debt"},
		"hub.doctor":     {"healthy", "checks"},
		"remove.inspect": {"role", "revision", "blockers", "targets", "preserved"},
		"purge.inspect":  {"role", "plan_revision", "targets", "exclusions"},
		"purge.apply":    {"role", "plan_revision", "deleted", "debt"},
	}
	if len(contract.JSONResultFields) != len(wantResults) {
		t.Errorf("JSON result schema count = %d, want %d", len(contract.JSONResultFields), len(wantResults))
	}
	for command, want := range wantResults {
		if got := contract.JSONResultFields[command]; !slices.Equal(got, want) {
			t.Errorf("JSON result fields for %s = %q, want %q", command, got, want)
		}
	}
}

func TestCheckedCLIDocumentationIsGeneratedFromTheContract(t *testing.T) {
	root := filepath.Join("..", "..")
	checked, err := os.ReadFile(filepath.Join(root, "docs", "CLI.md"))
	if err != nil {
		t.Fatal(err)
	}
	generated, err := RenderMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	if string(checked) != generated {
		t.Error("docs/CLI.md differs from the canonical CLI descriptor rendering")
	}
}
