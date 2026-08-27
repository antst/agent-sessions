package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type launcherProduct struct {
	descriptor productcatalog.ProductDescriptor
}

func launcherProductByID(product string) (launcherProduct, bool) {
	descriptor, ok := productcatalog.ProductByID(product)
	return launcherProduct{descriptor: descriptor}, ok
}

func (product launcherProduct) resume(kind, sessionID string) (string, []string, bool) {
	arguments, ok := product.descriptor.ResumeArguments(kind, sessionID)
	if !ok {
		return "", nil, false
	}
	executable := product.descriptor.PeerAlias
	if kind == "lane" {
		executable = product.descriptor.LaneAlias
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
