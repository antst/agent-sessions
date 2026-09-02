package launcher

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	root, err := managedIntegrationRoot()
	if err != nil {
		return err
	}
	plan, err := buildManagedPeerPlan(product, args, os.Environ(), root, path, newManagedSessionID)
	if err != nil {
		return err
	}
	return Exec(plan.path, plan.args, plan.environment)
}

func buildManagedPeerPlan(
	product string,
	args, environment []string,
	pluginRoot, executable string,
	idSource func() (string, error),
) (managedPeerPlan, error) {
	forwarded, context, err := scanPeerWrapperOptions(product, args)
	if err != nil {
		return managedPeerPlan{}, err
	}
	forwarded, peerName, err := extractPeerNameArgs(forwarded)
	if err != nil {
		return managedPeerPlan{}, err
	}
	environment = managedLiveEnvironment(environment, product, peerName, context.groups)
	descriptor, ok := productcatalog.ByID(product)
	if !ok {
		return managedPeerPlan{}, fmt.Errorf("unsupported managed peer product %q", product)
	}

	switch descriptor.NativeRegistration.Strategy {
	case "opencode-global-plugin", "kilo-global-plugin":
		// These products load the release-managed global plugin themselves.
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
	case "dsh-owned-profile":
		if !hasOption(forwarded, "--profile") {
			forwarded = append([]string{"--profile", "agent-sessions"}, forwarded...)
		}
	default:
		return managedPeerPlan{}, fmt.Errorf("unsupported managed peer product %q", product)
	}
	return managedPeerPlan{path: executable, args: forwarded, environment: environment}, nil
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
