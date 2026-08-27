package daemon

import (
	"errors"
	"path/filepath"

	"github.com/antst/agent-sessions/internal/servicecontrol"
)

// HostServiceRole returns the host-only service descriptor rooted at one
// installation prefix. Hub ownership is deliberately absent from this package.
func HostServiceRole(prefix string) (servicecontrol.RoleDescriptor, error) {
	if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix || prefix == string(filepath.Separator) {
		return servicecontrol.RoleDescriptor{}, errors.New("host install prefix must be clean, absolute and non-root")
	}
	return servicecontrol.RoleDescriptor{
		Role: "host", ServiceName: "agent-sessions.service", Label: "net.antst.agent-sessions",
		DefinitionPath:   hostServiceDefinitionPath(),
		Program:          filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions"),
		ProgramArguments: []string{"daemon"},
	}, nil
}
