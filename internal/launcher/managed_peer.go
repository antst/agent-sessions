package launcher

import (
	"context"
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
	args, err = projectNativeArgumentRules(descriptor, args)
	if err != nil {
		return err
	}
	asset := ""
	switch descriptor.NativeRegistration.Strategy {
	case "pi-package", "omp-extension":
		asset, err = ManagedIntegrationAsset(product, "agent-sessions.mjs")
		if err != nil {
			return err
		}
	}
	plan, err := buildManagedPeerPlan(product, args, os.Environ(), asset, path)
	if err != nil {
		return err
	}
	return Exec(plan.path, plan.args, plan.environment)
}

type ProductSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Updated   int64  `json:"updated"`
	Modified  string `json:"modified"`
}

type productSession = ProductSession

func ListProductSessions(ctx context.Context, executable string) ([]ProductSession, error) {
	command := exec.CommandContext(ctx, executable, "--pure", "session", "list", "--format", "json") //nolint:gosec // resolved installed product executable.
	payload, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list product sessions: %w", err)
	}
	var sessions []ProductSession
	if err := json.Unmarshal(payload, &sessions); err != nil {
		return nil, fmt.Errorf("decode product session list: %w", err)
	}
	return sessions, nil
}

const packageSessionListScript = `
import { pathToFileURL } from "node:url";
const { SessionManager } = await import(pathToFileURL(process.argv[1]).href);
const rows = process.argv[2] === "all"
  ? await SessionManager.listAll()
  : await SessionManager.list(process.argv[3]);
process.stdout.write(JSON.stringify(rows.map((row) => ({
  id: row.id,
  title: row.title ?? row.name ?? "",
  directory: row.cwd,
  modified: row.modified instanceof Date ? row.modified.toISOString() : String(row.modified),
}))));
`

func listPiSessionsWithScope(ctx context.Context, executable, scope, cwd string) ([]ProductSession, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve Pi executable: %w", err)
	}
	productAPI := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "index.js"))
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("resolve Node.js for Pi session list: %w", err)
	}
	command := exec.CommandContext(ctx, node, "--input-type=module", "--eval", packageSessionListScript, productAPI, scope, cwd) //nolint:gosec // pinned product API and explicit scope.
	if cwd != "" {
		command.Dir = cwd
	}
	payload, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list Pi sessions through product API: %w", err)
	}
	var sessions []ProductSession
	if err := json.Unmarshal(payload, &sessions); err != nil {
		return nil, fmt.Errorf("decode Pi product session list: %w", err)
	}
	return sessions, nil
}

func ListAllPiSessions(ctx context.Context, executable string) ([]ProductSession, error) {
	return listPiSessionsWithScope(ctx, executable, "all", "")
}

func listOMPSessionsWithScope(ctx context.Context, executable, scope, cwd string) ([]ProductSession, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve OMP executable: %w", err)
	}
	productAPI := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "src", "index.ts"))
	bun, err := exec.LookPath("bun")
	if err != nil {
		return nil, fmt.Errorf("resolve Bun for OMP session list: %w", err)
	}
	command := exec.CommandContext(ctx, bun, "--eval", packageSessionListScript, productAPI, scope, cwd) //nolint:gosec // pinned product API and explicit scope.
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

func ListAllOMPSessions(ctx context.Context, executable string) ([]ProductSession, error) {
	return listOMPSessionsWithScope(ctx, executable, "all", "")
}

func projectNativeArgumentTranslations(
	descriptor productcatalog.Descriptor,
	surface productcatalog.NativeArgumentSurface,
	arguments []string,
) ([]string, error) {
	if err := requireNativeSelectionSupport(descriptor, surface, arguments); err != nil {
		return nil, err
	}
	projected := append([]string(nil), arguments...)
	for _, rule := range descriptor.NativeArgumentRules {
		if rule.Surface != surface || rule.Kind != productcatalog.NativeArgumentTranslation {
			continue
		}
		var err error
		projected, err = translateNativeOption(projected, rule.Option, rule.Replacement, rule.ValuePrefix)
		if err != nil {
			return nil, err
		}
	}
	return projected, nil
}

func projectNativeArgumentRules(
	descriptor productcatalog.Descriptor,
	arguments []string,
) ([]string, error) {
	if err := requireNativeSelectionSupport(descriptor, productcatalog.NativeArgumentPeer, arguments); err != nil {
		return nil, err
	}
	projected := append([]string(nil), arguments...)
	for _, rule := range descriptor.NativeArgumentRules {
		if rule.Surface != productcatalog.NativeArgumentPeer {
			continue
		}
		var err error
		switch rule.Kind {
		case productcatalog.NativeArgumentTranslation:
			projected, err = translateNativeOption(projected, rule.Option, rule.Replacement, rule.ValuePrefix)
		case productcatalog.NativeArgumentHandler:
			return nil, fmt.Errorf("peer native argument handler %q is forbidden; resume belongs to the product", rule.Handler)
		default:
			return nil, fmt.Errorf("native argument rule kind %q is unsupported", rule.Kind)
		}
		if err != nil {
			return nil, err
		}
	}
	return projected, nil
}

func translateNativeOption(arguments []string, option string, replacement []string, valuePrefix string) ([]string, error) {
	if len(replacement) == 1 && replacement[0] == option && valuePrefix == "" {
		return append([]string(nil), arguments...), nil
	}
	result := make([]string, 0, len(arguments)+len(replacement))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			return append(result, arguments[index:]...), nil
		}
		if option == "--resume" && argument == option {
			result = append(result, replacement...)
			if index+1 < len(arguments) && !strings.HasPrefix(arguments[index+1], "-") {
				result = append(result, valuePrefix+arguments[index+1])
				index++
			}
			continue
		}
		value, matched, consumed, err := argumentOptionValue(arguments, index, option)
		if err != nil {
			return nil, err
		}
		if !matched {
			result = append(result, argument)
			continue
		}
		result = append(result, replacement...)
		result = append(result, valuePrefix+value)
		if consumed {
			index++
		}
	}
	return result, nil
}

var nativeSelectionLabels = map[string]string{
	"--agent":            "agent",
	"--effort":           "effort",
	"--reasoning-effort": "effort",
}

func requireNativeSelectionSupport(
	descriptor productcatalog.Descriptor,
	surface productcatalog.NativeArgumentSurface,
	arguments []string,
) error {
	for _, argument := range beforeDoubleDash(arguments) {
		option := argument
		if strings.Contains(option, "=") {
			option, _, _ = strings.Cut(option, "=")
		}
		label, owned := nativeSelectionLabels[option]
		if !owned {
			continue
		}
		supported := false
		for _, rule := range descriptor.NativeArgumentRules {
			if rule.Surface == surface && rule.Option == option {
				supported = true
				break
			}
		}
		if !supported {
			return usageError(fmt.Sprintf("%s has no native %s selector", descriptor.ID, label))
		}
	}
	return nil
}

type laneArgumentHandler func(productcatalog.Descriptor, []string, productcatalog.NativeArgumentRule) ([]string, error)

var laneArgumentHandlers = map[string]laneArgumentHandler{
	"dsh-effort-with-model": func(descriptor productcatalog.Descriptor, arguments []string, rule productcatalog.NativeArgumentRule) ([]string, error) {
		_, selected, err := optionValue(arguments, rule.Option)
		if err != nil || !selected {
			return arguments, err
		}
		_, present, err := optionValue(arguments, "--model")
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, usageError(descriptor.ID + " effort requires --model in the same invocation")
		}
		return translateNativeOption(arguments, rule.Option, []string{"--effort"}, "")
	},
}

// ProjectNativeLaneArguments applies only descriptor-owned uniform lane
// options. Product-specific native argv remains otherwise untouched.
func ProjectNativeLaneArguments(product string, arguments []string) ([]string, error) {
	descriptor, ok := productcatalog.ByID(product)
	if !ok {
		return nil, usageError("unsupported lane product: " + product)
	}
	if err := requireNativeSelectionSupport(descriptor, productcatalog.NativeArgumentLane, arguments); err != nil {
		return nil, err
	}
	projected := append([]string(nil), arguments...)
	for _, rule := range descriptor.NativeArgumentRules {
		if rule.Surface != productcatalog.NativeArgumentLane {
			continue
		}
		var err error
		switch rule.Kind {
		case productcatalog.NativeArgumentTranslation:
			projected, err = translateNativeOption(projected, rule.Option, rule.Replacement, rule.ValuePrefix)
		case productcatalog.NativeArgumentHandler:
			handler, ok := laneArgumentHandlers[rule.Handler]
			if !ok {
				return nil, fmt.Errorf("native lane argument handler %q is unavailable", rule.Handler)
			}
			projected, err = handler(descriptor, projected, rule)
		default:
			return nil, fmt.Errorf("native argument rule kind %q is unsupported", rule.Kind)
		}
		if err != nil {
			return nil, err
		}
	}
	return projected, nil
}

func argumentOptionValue(arguments []string, index int, option string) (string, bool, bool, error) {
	argument := arguments[index]
	switch {
	case argument == option:
		if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" || arguments[index+1] == "--" {
			return "", true, false, usageError(option + " requires a non-empty value")
		}
		return arguments[index+1], true, true, nil
	case strings.HasPrefix(argument, option+"="):
		value := strings.TrimSpace(strings.TrimPrefix(argument, option+"="))
		if value == "" {
			return "", true, false, usageError(option + " requires a non-empty value")
		}
		return value, true, false, nil
	case len(option) == 2 && strings.HasPrefix(argument, option) && argument != option:
		value := strings.TrimSpace(strings.TrimPrefix(argument, option))
		if value == "" {
			return "", true, false, usageError(option + " requires a non-empty value")
		}
		return value, true, false, nil
	default:
		return "", false, false, nil
	}
}

func buildManagedPeerPlan(
	product string,
	args, environment []string,
	integrationPath, executable string,
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
		forwarded = append([]string{"--extension", integrationPath}, forwarded...)
	case "omp-extension":
		forwarded = append([]string{"--extension=" + integrationPath}, forwarded...)
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
	return liveReportEnvironment(environment, name, groups)
}

func integrationAsset(root, product, name string) string {
	return filepath.Join(root, "integrations", product, name)
}

// ManagedIntegrationAsset resolves one release-owned integration payload.
func ManagedIntegrationAsset(product, name string) (string, error) {
	root, err := managedIntegrationRoot()
	if err != nil {
		return "", err
	}
	return integrationAsset(root, product, name), nil
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
		if len(name) == 2 && strings.HasPrefix(argument, name) && argument != name {
			value := strings.TrimSpace(strings.TrimPrefix(argument, name))
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
