package launcher

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

type managedPeerPlan struct {
	path        string
	args        []string
	environment []string
}

// RunManagedPeer is the stateless launch path for products whose native
// plugin, extension, or MCP connector reports the product-owned session over
// presence.sock. The daemon is not involved in session creation or resume.
func RunManagedPeer(product string, args []string) error {
	path, err := ResolveProductExecutable(product)
	if err != nil {
		return err
	}
	descriptor, ok := productcatalog.ByID(product)
	if !ok || !descriptor.Has(productcatalog.CapabilityInteractive) {
		return fmt.Errorf("unsupported managed peer product %q", product)
	}
	switch descriptor.NativeRegistration.Strategy {
	case "opencode-global-plugin", "kilo-global-plugin":
		args, err = resolveProductResume(product, path, "", args, "--session", isOpenCodeSessionID, listProductSessions)
		if err != nil {
			return err
		}
	case "pi-package":
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return fmt.Errorf("resolve working directory: %w", cwdErr)
		}
		args, err = resolveProductResume(product, path, cwd, args, "--session", isPiNativeSessionSelector, listPiSessions)
		if err != nil {
			return err
		}
	case "omp-extension":
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return fmt.Errorf("resolve working directory: %w", cwdErr)
		}
		args, err = resolveProductResume(product, path, cwd, args, "--resume", isPiNativeSessionSelector, listOMPSessions)
		if err != nil {
			return err
		}
	}
	root := ""
	switch descriptor.NativeRegistration.Strategy {
	case "pi-package", "omp-extension", "codebuddy-wrapper-plugin-mcp":
		root, err = managedIntegrationRoot()
		if err != nil {
			return err
		}
	}
	plan, err := buildManagedPeerPlan(product, args, os.Environ(), root, path, newManagedSessionID)
	if err != nil {
		return err
	}
	return Exec(plan.path, plan.args, plan.environment)
}

type productSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Updated   int64  `json:"updated"`
	Modified  string `json:"modified"`
}

func listProductSessions(executable, _ string) ([]productSession, error) {
	command := exec.Command(executable, "--pure", "session", "list", "--format", "json") //nolint:gosec // resolved installed product executable.
	payload, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list product sessions: %w", err)
	}
	var sessions []productSession
	if err := json.Unmarshal(payload, &sessions); err != nil {
		return nil, fmt.Errorf("decode product session list: %w", err)
	}
	return sessions, nil
}

const packageSessionListScript = `
import { pathToFileURL } from "node:url";
const { SessionManager } = await import(pathToFileURL(process.argv[1]).href);
const rows = await SessionManager.list(process.argv[2]);
process.stdout.write(JSON.stringify(rows.map((row) => ({
  id: row.id,
  title: row.title ?? row.name ?? "",
  directory: row.cwd,
  modified: row.modified instanceof Date ? row.modified.toISOString() : String(row.modified),
}))));
`

func listPiSessions(executable, cwd string) ([]productSession, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve Pi executable: %w", err)
	}
	productAPI := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "index.js"))
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("resolve Node.js for Pi session list: %w", err)
	}
	command := exec.Command(node, "--input-type=module", "--eval", packageSessionListScript, productAPI, cwd) //nolint:gosec // pinned product API and caller cwd.
	command.Dir = cwd
	payload, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list Pi sessions through product API: %w", err)
	}
	var sessions []productSession
	if err := json.Unmarshal(payload, &sessions); err != nil {
		return nil, fmt.Errorf("decode Pi product session list: %w", err)
	}
	return sessions, nil
}

func listOMPSessions(executable, cwd string) ([]productSession, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve OMP executable: %w", err)
	}
	productAPI := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "src", "index.ts"))
	bun, err := exec.LookPath("bun")
	if err != nil {
		return nil, fmt.Errorf("resolve Bun for OMP session list: %w", err)
	}
	command := exec.Command(bun, "--eval", packageSessionListScript, productAPI, cwd) //nolint:gosec // pinned product API and caller cwd.
	command.Dir = cwd
	payload, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list OMP sessions through product API: %w", err)
	}
	var sessions []productSession
	if err := json.Unmarshal(payload, &sessions); err != nil {
		return nil, fmt.Errorf("decode OMP product session list: %w", err)
	}
	return sessions, nil
}

func isOpenCodeSessionID(selector string) bool {
	return strings.HasPrefix(selector, "ses_")
}

func isPiNativeSessionSelector(selector string) bool {
	return threadIDPattern.MatchString(selector) || filepath.IsAbs(selector) || strings.ContainsAny(selector, `/\\`) || filepath.Ext(selector) == ".jsonl"
}

func resolveProductResume(
	product string,
	executable string,
	cwd string,
	arguments []string,
	option string,
	isNativeSelector func(string) bool,
	list func(string, string) ([]productSession, error),
) ([]string, error) {
	selector, present, err := optionValue(arguments, option)
	if err != nil || !present || isNativeSelector(selector) {
		return arguments, err
	}
	sessions, err := list(executable, cwd)
	if err != nil {
		return nil, err
	}
	matches := make([]productSession, 0, 1)
	for _, session := range sessions {
		if session.Title == selector {
			matches = append(matches, session)
		}
	}
	if len(matches) == 0 {
		return nil, usageError(fmt.Sprintf("%s session name %q was not found in the product session list", product, selector))
	}
	if len(matches) > 1 {
		details := make([]string, 0, len(matches))
		for _, match := range matches {
			updated := fmt.Sprintf("%d", match.Updated)
			if match.Modified != "" {
				updated = match.Modified
			}
			details = append(details, fmt.Sprintf("%s (directory=%s updated=%s)", match.ID, match.Directory, updated))
		}
		return nil, usageError(fmt.Sprintf("%s session name %q is ambiguous: %s", product, selector, strings.Join(details, ", ")))
	}
	resolved := append([]string(nil), arguments...)
	for index, argument := range beforeDoubleDash(resolved) {
		if argument == option {
			resolved[index+1] = matches[0].ID
			break
		}
		if strings.HasPrefix(argument, option+"=") {
			resolved[index] = option + "=" + matches[0].ID
			break
		}
	}
	return resolved, nil
}

func buildManagedPeerPlan(
	product string,
	args, environment []string,
	pluginRoot, executable string,
	idSource func() (string, error),
) (managedPeerPlan, error) {
	descriptor, ok := productcatalog.ByID(product)
	if !ok || !descriptor.Has(productcatalog.CapabilityInteractive) {
		return managedPeerPlan{}, fmt.Errorf("unsupported managed peer product %q", product)
	}
	forwarded, context, err := scanPeerWrapperOptions(product, args)
	if err != nil {
		return managedPeerPlan{}, err
	}
	forwarded, peerName, err := extractPeerNameArgs(forwarded)
	if err != nil {
		return managedPeerPlan{}, err
	}
	environment = managedLiveEnvironment(environment, product, peerName, context.groups)
	forwarded, err = projectNativeLaunchPolicy(descriptor, forwarded, context.forceNoYolo)
	if err != nil {
		return managedPeerPlan{}, err
	}

	switch descriptor.NativeRegistration.Strategy {
	case "opencode-global-plugin", "kilo-global-plugin":
		// These products load the release-managed global plugin themselves.
		resumeID, present, resumeErr := optionValue(forwarded, "--session")
		if resumeErr != nil {
			return managedPeerPlan{}, resumeErr
		}
		if present {
			environment = envutil.Set(environment, peerSessionIDEnv, resumeID)
		}
	case "pi-package":
		forwarded = append([]string{"--extension", integrationAsset(pluginRoot, descriptor.ID, "agent-sessions.mjs")}, forwarded...)
	case "omp-extension":
		forwarded = append([]string{"--extension=" + integrationAsset(pluginRoot, descriptor.ID, "agent-sessions.mjs")}, forwarded...)
	case "codebuddy-wrapper-plugin-mcp":
		if hasOption(forwarded, "--mcp-config") || hasOption(forwarded, "--strict-mcp-config") {
			return managedPeerPlan{}, usageError("CodeBuddy MCP routing is owned by the managed wrapper")
		}
		sessionID, ok, err := optionValue(forwarded, "--session-id")
		if err != nil {
			return managedPeerPlan{}, err
		}
		if !ok {
			if idSource == nil {
				return managedPeerPlan{}, errors.New("CodeBuddy session id source is unavailable")
			}
			sessionID, err = idSource()
			if err != nil {
				return managedPeerPlan{}, err
			}
			forwarded = append(forwarded, "--session-id", sessionID)
		}
		environment = envutil.Set(environment, peerSessionIDEnv, sessionID)
		forwarded = append(forwarded, "--strict-mcp-config", "--mcp-config", integrationAsset(pluginRoot, descriptor.ID, "mcp.json"))
	default:
		return managedPeerPlan{}, fmt.Errorf("unsupported managed peer product %q", product)
	}
	return managedPeerPlan{path: executable, args: forwarded, environment: environment}, nil
}

func projectNativeLaunchPolicy(descriptor productcatalog.Descriptor, arguments []string, forceNoYolo bool) ([]string, error) {
	projected := make([]string, 0, len(arguments)+len(descriptor.NativeToolGrantArgs)+len(descriptor.NativeYoloArgs))
	yoloSeen := false
	for index, argument := range arguments {
		if argument == "--" {
			projected = append(projected, arguments[index:]...)
			break
		}
		if argument != "--yolo" {
			projected = append(projected, argument)
			continue
		}
		if yoloSeen {
			return nil, usageError("--yolo was specified more than once")
		}
		if forceNoYolo {
			return nil, usageError("--yolo conflicts with --no-yolo")
		}
		if descriptor.NativeYoloArgs == nil {
			return nil, usageError("--yolo is not mapped for " + descriptor.ID)
		}
		yoloSeen = true
		projected = append(projected, descriptor.NativeYoloArgs...)
	}
	if len(descriptor.NativeToolGrantArgs) > 0 && !containsArgumentSequence(beforeDoubleDash(projected), descriptor.NativeToolGrantArgs) {
		insert := len(projected)
		for index, argument := range projected {
			if argument == "--" {
				insert = index
				break
			}
		}
		withGrant := make([]string, 0, len(projected)+len(descriptor.NativeToolGrantArgs))
		withGrant = append(withGrant, projected[:insert]...)
		withGrant = append(withGrant, descriptor.NativeToolGrantArgs...)
		withGrant = append(withGrant, projected[insert:]...)
		projected = withGrant
	}
	return projected, nil
}

func containsArgumentSequence(arguments, sequence []string) bool {
	if len(sequence) == 0 || len(sequence) > len(arguments) {
		return false
	}
	for start := 0; start+len(sequence) <= len(arguments); start++ {
		if slices.Equal(arguments[start:start+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func managedLiveEnvironment(environment []string, product, name string, groups []string) []string {
	environment = daemonPeerEnvironment(environment, "", product)
	environment = envutil.Set(environment, "AGENT_SESSIONS_PRODUCT_ID", product)
	return liveReportEnvironment(environment, name, groups)
}

func integrationAsset(root, product, name string) string {
	return filepath.Join(root, "integrations", product, name)
}

func managedIntegrationRoot() (string, error) {
	for _, name := range []string{"AGENT_SESSIONS_PLUGIN_ROOT", "CODEX_PEER_PLUGIN_ROOT"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return filepath.Abs(value)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve launcher executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	for _, candidate := range []string{
		filepath.Join(filepath.Dir(executable), ".."),
		filepath.Join(filepath.Dir(executable), "..", ".."),
	} {
		candidate, err = filepath.Abs(candidate)
		if err == nil {
			if info, statErr := os.Stat(filepath.Join(candidate, "integrations")); statErr == nil && info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", errors.New("Agent Sessions integration payload root is unavailable")
}

func optionValue(arguments []string, name string) (string, bool, error) {
	for index, argument := range beforeDoubleDash(arguments) {
		if argument == name {
			if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
				return "", false, usageError(name + " requires a non-empty value")
			}
			return arguments[index+1], true, nil
		}
		if strings.HasPrefix(argument, name+"=") {
			value := strings.TrimSpace(strings.TrimPrefix(argument, name+"="))
			if value == "" {
				return "", false, usageError(name + " requires a non-empty value")
			}
			return value, true, nil
		}
	}
	return "", false, nil
}

func hasOption(arguments []string, name string) bool {
	_, ok, _ := optionValue(arguments, name)
	if ok {
		return true
	}
	for _, argument := range beforeDoubleDash(arguments) {
		if argument == name {
			return true
		}
	}
	return false
}

func newManagedSessionID() (string, error) {
	var body [16]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", err
	}
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(body[0:4]),
		hex.EncodeToString(body[4:6]),
		hex.EncodeToString(body[6:8]),
		hex.EncodeToString(body[8:10]),
		hex.EncodeToString(body[10:16]),
	}, "-"), nil
}
