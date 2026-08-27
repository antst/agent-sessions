package launcher

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/daemon"
)

const (
	internalAttachmentIDEnv = daemon.InternalAttachmentIDEnvironment
	internalCapabilityEnv   = daemon.InternalCapabilityEnvironment
	internalProductEnv      = daemon.InternalProductEnvironment
	internalSessionIDEnv    = daemon.InternalSessionIDEnvironment
)

type daemonPeerDependencies struct {
	prepare func(context.Context, daemon.AttachmentPrepareRequest) (daemon.AttachmentPrepareResult, error)
	detach  func(context.Context, string, string) error
	exec    func(string, []string, []string) error
}

func productionDaemonPeerDependencies() daemonPeerDependencies {
	return daemonPeerDependencies{prepare: daemon.PrepareManagedAttachment, detach: daemon.DetachManagedAttachment, exec: Exec}
}

func executeDaemonPreparedPeer(ctx context.Context, product string, prepared daemon.AttachmentPrepareResult, dependencies daemonPeerDependencies) error {
	if dependencies.exec == nil || dependencies.detach == nil || strings.TrimSpace(prepared.Launch.Executable) == "" ||
		strings.TrimSpace(prepared.Attachment.AttachmentID) == "" || strings.TrimSpace(prepared.Capability) == "" {
		return errors.New("daemon returned an incomplete native launch handoff")
	}
	environment := preparedPeerEnvironment(os.Environ(), product, prepared)
	if err := dependencies.exec(prepared.Launch.Executable, prepared.Launch.Arguments, environment); err != nil {
		return errors.Join(err, dependencies.detach(ctx, prepared.Attachment.AttachmentID, "native_exec_failed"))
	}
	return nil
}

func preparedPeerEnvironment(base []string, product string, prepared daemon.AttachmentPrepareResult) []string {
	replacements := make(map[string]string, len(prepared.Launch.Environment)+4)
	for key, value := range prepared.Launch.Environment {
		replacements[key] = value
	}
	replacements[internalAttachmentIDEnv] = prepared.Attachment.AttachmentID
	replacements[internalCapabilityEnv] = prepared.Capability
	replacements[internalProductEnv] = product
	if prepared.Launch.SessionID != "" {
		replacements[internalSessionIDEnv] = prepared.Launch.SessionID
	}
	result := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if _, replaced := replacements[name]; found && replaced {
			continue
		}
		result = append(result, entry)
	}
	names := make([]string, 0, len(replacements))
	for name := range replacements {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+replacements[name])
	}
	return result
}
