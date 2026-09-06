package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/antst/sessionbus/internal/productcatalog"
)

type launcherProduct struct {
	descriptor productcatalog.Descriptor
}

func launcherProductByID(product string) (launcherProduct, bool) {
	descriptor, ok := productcatalog.ByID(product)
	return launcherProduct{descriptor: descriptor}, ok
}

func (product launcherProduct) resume(kind, sessionID string) (string, []string, bool) {
	if sessionID == "" || (kind != "interactive" && kind != "lane") {
		return "", nil, false
	}
	if kind == "interactive" && !product.descriptor.Has(productcatalog.CapabilityInteractive) ||
		kind == "lane" && !product.descriptor.Has(productcatalog.CapabilityLane) {
		return "", nil, false
	}
	arguments := []string{"resume", sessionID}
	executable := product.descriptor.PeerAlias
	if kind == "lane" {
		executable = product.descriptor.LaneAlias
	} else if product.descriptor.ResumeStyle == productcatalog.ResumeFlag {
		arguments = []string{"--resume", sessionID}
	}
	return executable, arguments, true
}

func productExecutable(environmentName, fallback string) (string, error) {
	configured := strings.TrimSpace(os.Getenv(environmentName))
	if configured == "" {
		configured = fallback
	}
	path, err := exec.LookPath(configured)
	if err != nil {
		if configured != fallback {
			return "", &ExitError{Code: 127, Err: fmt.Errorf("%s is unavailable: %s", environmentName, configured)}
		}
		return "", &ExitError{Code: 127, Err: errors.New(fallback + " was not found on PATH")}
	}
	return path, nil
}

// ResolveProductExecutable selects the exact native executable used by the
// established peer launchers. Keeping unified lanes on this boundary prevents
// their product detection from drifting from the already-hardened interactive
// launch path (most importantly Grok Build versus the unrelated chat client).
func ResolveProductExecutable(product string) (string, error) {
	switch product {
	case "codex":
		return codexExecutable()
	case "claude":
		return claudeExecutable()
	case "grok":
		return grokExecutable()
	case "qwen":
		return qwenExecutable()
	default:
		descriptor, ok := productcatalog.ByID(product)
		if !ok {
			return "", fmt.Errorf("unsupported product %q", product)
		}
		environment := strings.ToUpper(strings.ReplaceAll(product, "-", "_")) + "_PEER_" +
			strings.ToUpper(strings.ReplaceAll(descriptor.NativeExecutable, "-", "_")) + "_BIN"
		return productExecutable(environment, descriptor.NativeExecutable)
	}
}
