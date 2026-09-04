package pifamily

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
)

const maxDoctorOutput = 64 << 10

var semanticVersion = regexp.MustCompile(`(?:^|[[:space:]])v?([^[:space:]]+\.[^[:space:]]+\.[^[:space:]]+)(?:$|[[:space:]])`)

type DoctorRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) (string, error)
	Stat(string) (os.FileInfo, error)
}

type IntegrationCheck func(context.Context) (bool, string, error)

type DoctorConfig struct {
	Quirks           Quirks
	Executable       string
	ExtensionPath    string
	Runner           DoctorRunner
	CheckIntegration IntegrationCheck
}

type DoctorProbe struct{ config DoctorConfig }

func NewDoctorProbe(config DoctorConfig) (*DoctorProbe, error) {
	if err := config.Quirks.Validate(); err != nil {
		return nil, err
	}
	if config.Executable == "" {
		config.Executable = config.Quirks.Executable
	}
	if strings.TrimSpace(config.ExtensionPath) == "" {
		return nil, errors.New("Pi-family doctor requires the managed extension path")
	}
	if config.Runner == nil {
		config.Runner = osDoctorRunner{}
	}
	return &DoctorProbe{config: config}, nil
}

func (probe *DoctorProbe) Probe(ctx context.Context, request productruntime.ProbeRequest) (productruntime.ProbeReport, error) {
	if ctx == nil || request.ProductID != probe.config.Quirks.ProductID || !validProbeDepth(request.Depth) {
		return productruntime.ProbeReport{}, productruntime.ErrProtocol
	}
	features := map[string]bool{"native-cli": false, "rpc": false, "peer": false, "lane": false, "parent": false}
	report := productruntime.ProbeReport{Features: features}
	executable := request.ExecutablePath
	if executable == "" {
		var err error
		executable, err = probe.config.Runner.LookPath(probe.config.Executable)
		if err != nil {
			report.State = productruntime.ProbeMissing
			report.Detail = productruntime.NewRedactedString("native executable is not on PATH")
			return report, nil
		}
	} else if info, err := probe.config.Runner.Stat(executable); err != nil || info == nil || !info.Mode().IsRegular() {
		report.State = productruntime.ProbeMissing
		report.Detail = productruntime.NewRedactedString("selected native executable is absent or not a regular file")
		return report, nil
	}
	if filepath.Base(executable) != filepath.Base(probe.config.Executable) {
		report.State = productruntime.ProbeIncompatible
		report.Detail = productruntime.NewRedactedString("executable basename does not match the selected product")
		return report, nil
	}
	features["native-cli"] = true
	report.State = productruntime.ProbeReady
	if request.Depth == productruntime.ProbePresence {
		return report, nil
	}
	output, err := probe.config.Runner.Run(ctx, executable, "--version")
	if err != nil {
		report.State = productruntime.ProbeError
		report.Detail = productruntime.NewRedactedString("native version probe failed")
		return report, nil
	}
	version := extractVersion(output)
	report.NativeVersion = version
	if !productruntime.VersionAtLeast(version, probe.config.Quirks.TestedVersion) {
		report.State = productruntime.ProbeIncompatible
		report.Detail = productruntime.NewRedactedString(fmt.Sprintf("native version %s is below minimum supported version %s", version, probe.config.Quirks.TestedVersion))
		return report, nil
	}
	features["minimum-version"] = true
	if request.Depth == productruntime.ProbeVersion {
		return report, nil
	}
	help, err := probe.config.Runner.Run(ctx, executable, "--help")
	if err != nil || !hasRequiredHelp(probe.config.Quirks, help) {
		report.State = productruntime.ProbeIncompatible
		report.Detail = productruntime.NewRedactedString("native CLI does not expose the required RPC, extension, resume, and permission options")
		return report, nil
	}
	features["rpc"] = true
	features["lane"] = true
	if request.Depth == productruntime.ProbeFeature {
		return report, nil
	}
	info, err := probe.config.Runner.Stat(probe.config.ExtensionPath)
	if err != nil || info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString("managed live-session extension is absent or unreadable")
		value := false
		report.TupleOK = &value
		return report, nil
	}
	if probe.config.CheckIntegration == nil {
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString("live-session integration check is not configured")
		value := false
		report.TupleOK = &value
		return report, nil
	}
	ready, detail, err := probe.config.CheckIntegration(ctx)
	if err != nil || !ready {
		if strings.TrimSpace(detail) == "" {
			detail = "live-session integration is not ready"
		}
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString(detail)
		value := false
		report.TupleOK = &value
		return report, nil
	}
	features["peer"] = true
	features["parent"] = true
	features["registered-tool"] = true
	value := true
	report.TupleOK = &value
	return report, nil
}

func validProbeDepth(depth productruntime.ProbeDepth) bool {
	return depth == productruntime.ProbePresence || depth == productruntime.ProbeVersion ||
		depth == productruntime.ProbeFeature || depth == productruntime.ProbeIntegration
}

func extractVersion(output string) string {
	matches := semanticVersion.FindStringSubmatch(output)
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

func hasRequiredHelp(quirks Quirks, output string) bool {
	required := []string{"--mode", "--extension", "--session"}
	if quirks.ProductID == PiProductID {
		required = append(required, "--tools")
	} else {
		required = append(required, "--approval-mode")
	}
	for _, option := range required {
		if !strings.Contains(output, option) {
			return false
		}
	}
	return true
}

type osDoctorRunner struct{}

func (osDoctorRunner) LookPath(name string) (string, error)  { return exec.LookPath(name) }
func (osDoctorRunner) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }
func (osDoctorRunner) Run(ctx context.Context, executable string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	output := &boundedBuffer{remaining: maxDoctorOutput}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	return output.String(), err
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if buffer.remaining > 0 {
		if len(value) > buffer.remaining {
			value = value[:buffer.remaining]
		}
		written, _ := buffer.buffer.Write(value)
		buffer.remaining -= written
	}
	// Report full consumption so an overly chatty child cannot turn the bound
	// into a broken-pipe oracle. The retained diagnostic remains bounded.
	return original, nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }

var (
	_ io.Writer                  = (*boundedBuffer)(nil)
	_ productruntime.DoctorProbe = (*DoctorProbe)(nil)
)
