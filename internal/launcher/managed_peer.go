package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	args, err = projectNativeArgumentRules(descriptor, path, cwd, args, terminalProductSessionChooser)
	if err != nil {
		return err
	}
	root := ""
	switch descriptor.NativeRegistration.Strategy {
	case "pi-package", "omp-extension":
		root, err = managedIntegrationRoot()
		if err != nil {
			return err
		}
	}
	plan, err := buildManagedPeerPlan(product, args, os.Environ(), root, path)
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

func listProductSessions(executable, _ string) ([]productSession, error) {
	return ListProductSessions(context.Background(), executable)
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

type productSessionChooser func(string, string, []productSession) (string, error)

type productArgumentHandler struct {
	isNativeSelector func(string) bool
	list             func(string, string) ([]productSession, error)
}

var productArgumentHandlers = map[string]productArgumentHandler{
	"opencode-session-list": {isNativeSelector: isOpenCodeSessionID, list: listProductSessions},
	"pi-session-list":       {isNativeSelector: isPiNativeSessionSelector, list: listPiSessions},
	"omp-session-list":      {isNativeSelector: isPiNativeSessionSelector, list: listOMPSessions},
}

func projectNativeArgumentTranslations(descriptor productcatalog.Descriptor, arguments []string) ([]string, error) {
	projected := append([]string(nil), arguments...)
	for _, rule := range descriptor.NativeArgumentRules {
		if rule.Kind != productcatalog.NativeArgumentTranslation {
			continue
		}
		var err error
		projected, err = translateNativeOption(projected, rule.Option, rule.Replacement)
		if err != nil {
			return nil, err
		}
	}
	return projected, nil
}

func resolveNativeArgumentHandlerValue(
	descriptor productcatalog.Descriptor,
	executable, cwd, option, selector string,
	handlers map[string]productArgumentHandler,
	choose productSessionChooser,
) (string, error) {
	for _, rule := range descriptor.NativeArgumentRules {
		if rule.Kind != productcatalog.NativeArgumentHandler || rule.Option != option {
			continue
		}
		handler, ok := handlers[rule.Handler]
		if !ok {
			return "", fmt.Errorf("native argument handler %q is unavailable", rule.Handler)
		}
		return resolveProductSession(
			descriptor.ID, executable, cwd, selector,
			handler.isNativeSelector, handler.list, choose,
		)
	}
	return selector, nil
}

func projectNativeArgumentRules(
	descriptor productcatalog.Descriptor,
	executable, cwd string,
	arguments []string,
	choose productSessionChooser,
) ([]string, error) {
	projected := append([]string(nil), arguments...)
	for _, rule := range descriptor.NativeArgumentRules {
		var err error
		switch rule.Kind {
		case productcatalog.NativeArgumentTranslation:
			projected, err = translateNativeOption(projected, rule.Option, rule.Replacement)
		case productcatalog.NativeArgumentHandler:
			handler, ok := productArgumentHandlers[rule.Handler]
			if !ok {
				return nil, fmt.Errorf("native argument handler %q is unavailable", rule.Handler)
			}
			projected, err = resolveProductResume(
				descriptor.ID, executable, cwd, projected, rule.Option,
				handler.isNativeSelector, handler.list, choose,
			)
		default:
			return nil, fmt.Errorf("native argument rule kind %q is unsupported", rule.Kind)
		}
		if err != nil {
			return nil, err
		}
	}
	return projected, nil
}

func translateNativeOption(arguments []string, option string, replacement []string) ([]string, error) {
	result := make([]string, 0, len(arguments)+len(replacement))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			return append(result, arguments[index:]...), nil
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
		result = append(result, value)
		if consumed {
			index++
		}
	}
	return result, nil
}

func resolveProductResume(
	product, executable, cwd string,
	arguments []string,
	option string,
	isNativeSelector func(string) bool,
	list func(string, string) ([]productSession, error),
	choose productSessionChooser,
) ([]string, error) {
	selector, present, err := optionValue(arguments, option)
	if err != nil || !present || isNativeSelector(selector) {
		return arguments, err
	}
	selected, err := resolveProductSession(product, executable, cwd, selector, isNativeSelector, list, choose)
	if err != nil {
		return nil, err
	}
	return replaceNativeOptionValue(arguments, option, selected), nil
}

func resolveProductSession(
	product, executable, cwd, selector string,
	isNativeSelector func(string) bool,
	list func(string, string) ([]productSession, error),
	choose productSessionChooser,
) (string, error) {
	if isNativeSelector(selector) {
		return selector, nil
	}
	sessions, err := list(executable, cwd)
	if err != nil {
		return "", err
	}
	matches := make([]productSession, 0, 1)
	for _, session := range sessions {
		if session.Title == selector {
			matches = append(matches, session)
		}
	}
	selected, err := choose(product, selector, matches)
	if err != nil {
		return "", err
	}
	return selected, nil
}

func terminalProductSessionChooser(product, selector string, matches []productSession) (string, error) {
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	return chooseProductSession(product, selector, matches, os.Stdin, os.Stderr, interactive)
}

func chooseProductSession(
	product, selector string,
	matches []productSession,
	input io.Reader,
	output io.Writer,
	interactive bool,
) (string, error) {
	if len(matches) == 0 {
		return "", usageError(fmt.Sprintf("%s session name %q was not found in the product session list", product, selector))
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	details := formatProductSessionCandidates(matches)
	if !interactive {
		return "", usageError(fmt.Sprintf("%s session name %q has multiple matches; choose an exact session ID: %s", product, selector, strings.Join(details, ", ")))
	}
	_, _ = fmt.Fprintf(output, "%s has multiple sessions named %q:\n", product, selector)
	for index, detail := range details {
		_, _ = fmt.Fprintf(output, "  %d. %s\n", index+1, detail)
	}
	_, _ = fmt.Fprintf(output, "Select session [1-%d]: ", len(matches))
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(matches) {
		return "", usageError("session selection is invalid")
	}
	return matches[choice-1].ID, nil
}

func formatProductSessionCandidates(matches []productSession) []string {
	details := make([]string, 0, len(matches))
	for _, match := range matches {
		updated := fmt.Sprintf("%d", match.Updated)
		if match.Modified != "" {
			updated = match.Modified
		}
		details = append(details, fmt.Sprintf("%s (directory=%s updated=%s)", match.ID, match.Directory, updated))
	}
	return details
}

func replaceNativeOptionValue(arguments []string, option, value string) []string {
	resolved := append([]string(nil), arguments...)
	for index, argument := range beforeDoubleDash(resolved) {
		switch {
		case argument == option:
			resolved[index+1] = value
			return resolved
		case strings.HasPrefix(argument, option+"="):
			resolved[index] = option + "=" + value
			return resolved
		case len(option) == 2 && strings.HasPrefix(argument, option) && argument != option:
			resolved[index] = option + value
			return resolved
		}
	}
	return resolved
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
	pluginRoot, executable string,
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
