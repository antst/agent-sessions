package codebuddy

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type ProcessProbe interface {
	CaptureIdentity(context.Context, int) (procinfo.Identity, error)
	ObserveIdentity(context.Context, procinfo.Identity) (procinfo.IdentityObservation, error)
	Executable(context.Context, procinfo.Identity) (string, error)
	CommandLine(context.Context, procinfo.Identity) ([]string, error)
	DescendsFrom(context.Context, procinfo.Identity, procinfo.Identity, int) (bool, error)
}

type hostProcessProbe struct {
	inspector productruntime.ProcessInspector
}

func NewHostProcessProbe(inspector productruntime.ProcessInspector) ProcessProbe {
	if inspector == nil {
		return nil
	}
	return &hostProcessProbe{inspector: inspector}
}

func (probe *hostProcessProbe) CaptureIdentity(ctx context.Context, pid int) (procinfo.Identity, error) {
	return probe.inspector.CaptureIdentity(ctx, pid)
}

func (probe *hostProcessProbe) ObserveIdentity(ctx context.Context, identity procinfo.Identity) (procinfo.IdentityObservation, error) {
	return probe.inspector.ObserveIdentity(ctx, identity)
}

func (probe *hostProcessProbe) Executable(ctx context.Context, identity procinfo.Identity) (string, error) {
	return probe.inspector.Executable(ctx, identity)
}

func (probe *hostProcessProbe) CommandLine(ctx context.Context, identity procinfo.Identity) ([]string, error) {
	return platformCommandLine(ctx, identity)
}

func (probe *hostProcessProbe) DescendsFrom(ctx context.Context, child, ancestor procinfo.Identity, depth int) (bool, error) {
	return probe.inspector.DescendsFrom(ctx, child, ancestor, depth)
}

func DefaultEntrypointMatcher(executable string, argv []string) bool {
	base := strings.ToLower(filepath.Base(executable))
	if base == "codebuddy" || base == "codebuddy.exe" {
		return true
	}
	if base != "node" && base != "node.exe" && !strings.HasPrefix(base, "node-") {
		return false
	}
	// The npm shebang launches Node with the CodeBuddy bin script as argv[1].
	// Do not scan later user/prompt arguments for product-looking filenames.
	if len(argv) < 2 {
		return false
	}
	candidate := strings.ToLower(filepath.Base(argv[1]))
	return candidate == "codebuddy" || candidate == "codebuddy.js" || candidate == "codebuddy-code"
}
