//go:build linux

package servicecontrol

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

func validatePlatformRoleDescriptor(descriptor RoleDescriptor) error {
	if descriptor.ServiceName == "" || filepath.Base(descriptor.ServiceName) != descriptor.ServiceName ||
		!strings.HasSuffix(descriptor.ServiceName, ".service") || strings.ContainsAny(descriptor.ServiceName, " \t\r\n/\\") {
		return fmt.Errorf("%s service unit %q is not one safe .service name", descriptor.Role, descriptor.ServiceName)
	}
	return nil
}

func platformServiceCommand(descriptor RoleDescriptor, operation serviceOperation) (string, []string, error) {
	switch operation {
	case serviceEnable, serviceStart, serviceRestart, serviceStop, serviceDisable:
		return "systemctl", []string{"--user", string(operation), descriptor.ServiceName}, nil
	case serviceStatus:
		return "systemctl", []string{"--user", "is-active", descriptor.ServiceName}, nil
	default:
		return "", nil, fmt.Errorf("unsupported systemd operation %q", operation)
	}
}

func renderSystemdCapture(boundary, variant string, fields map[string]any) ([]byte, error) {
	if boundary != "journal" && boundary != "stdout" && boundary != "stderr" {
		return nil, fmt.Errorf("unmanifested systemd capture boundary %q", boundary)
	}
	kind, ok := map[string]diagnostics.OutputKind{
		"normal": diagnostics.OutputNormal, "debug": diagnostics.OutputDebug,
		"failure": diagnostics.OutputError, "crash": diagnostics.OutputCrashReport,
	}[variant]
	if !ok {
		return nil, errors.New("unmanifested systemd capture variant " + variant)
	}
	return diagnostics.Render(kind, "service.systemd."+boundary, fields)
}
