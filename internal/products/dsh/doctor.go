package dsh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/antst/sessionbus/internal/productruntime"
)

type CommandProbe interface {
	LookPath(string) (string, error)
	Output(context.Context, string, []string, []string) ([]byte, error)
}

type OSCommandProbe struct{}

func (OSCommandProbe) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (OSCommandProbe) Output(ctx context.Context, path string, arguments, environment []string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, arguments...) //nolint:gosec // exact typed doctor executable.
	command.Env = append([]string(nil), environment...)
	return command.Output()
}

type DoctorConfig struct {
	Executable     string
	PNPMExecutable string
	Environment    []string
	Commands       CommandProbe
	Timeout        time.Duration
}

type DoctorProbe struct{ config DoctorConfig }

func NewDoctorProbe(config DoctorConfig) (*DoctorProbe, error) {
	if config.Executable == "" {
		config.Executable = "dsh"
	}
	if config.PNPMExecutable == "" {
		config.PNPMExecutable = RequiredPNPM
	}
	if config.Commands == nil {
		config.Commands = OSCommandProbe{}
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Timeout < time.Millisecond || config.Timeout > time.Minute || filepath.Base(config.Executable) != "dsh" ||
		filepath.Base(config.PNPMExecutable) != RequiredPNPM {
		return nil, errors.New("DSH doctor configuration is invalid")
	}
	return &DoctorProbe{config: config}, nil
}

func (doctor *DoctorProbe) VerifyTuple(ctx context.Context, _ string) (Tuple, error) {
	cliPath, err := doctor.config.Commands.LookPath(doctor.config.Executable)
	if err != nil || filepath.Base(cliPath) != "dsh" {
		return Tuple{}, fmt.Errorf("%w: DSH CLI is missing", productruntime.ErrUnavailable)
	}
	pnpmPath, err := doctor.config.Commands.LookPath(doctor.config.PNPMExecutable)
	if err != nil || filepath.Base(pnpmPath) != RequiredPNPM {
		return Tuple{}, fmt.Errorf("%w: pnpm is required for DSH", productruntime.ErrUnavailable)
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctor.config.Timeout)
	defer cancel()
	environment := safeDoctorEnv(doctor.environment())
	cliOutput, err := doctor.config.Commands.Output(probeCtx, cliPath, []string{"--version"}, environment)
	if err != nil {
		return Tuple{}, fmt.Errorf("%w: DSH CLI version probe failed", productruntime.ErrUnavailable)
	}
	pnpmOutput, err := doctor.config.Commands.Output(probeCtx, pnpmPath, []string{"--version"}, environment)
	if err != nil {
		return Tuple{}, fmt.Errorf("%w: pnpm version probe failed", productruntime.ErrUnavailable)
	}
	tuple := Tuple{CLI: extractPinnedVersion(string(cliOutput)), PackageManager: RequiredPNPM, PNPMVersion: strings.TrimSpace(string(pnpmOutput))}
	return tuple, tuple.Validate()
}

func (doctor *DoctorProbe) Probe(ctx context.Context, request productruntime.ProbeRequest) (productruntime.ProbeReport, error) {
	if ctx == nil || request.ProductID != ProductID || !validProbeDepth(request.Depth) {
		return productruntime.ProbeReport{}, fmt.Errorf("%w: DSH doctor received product %q", productruntime.ErrProtocol, request.ProductID)
	}
	report := productruntime.ProbeReport{Features: map[string]bool{
		"native-cli": false, "pnpm": false, "exact-tuple": false, "native-v1": false, "lane": false,
	}}
	executable := doctor.config.Executable
	if request.ExecutablePath != "" {
		executable = request.ExecutablePath
	}
	resolved, err := doctor.config.Commands.LookPath(executable)
	if err != nil || filepath.Base(resolved) != "dsh" {
		report.State, report.Detail = productruntime.ProbeMissing, productruntime.NewRedactedString("DSH CLI not found")
		return report, nil
	}
	report.Features["native-cli"] = true
	if _, err := doctor.config.Commands.LookPath(doctor.config.PNPMExecutable); err != nil {
		report.State, report.Detail = productruntime.ProbeMissing, productruntime.NewRedactedString("pnpm is required for DSH")
		return report, nil
	}
	report.Features["pnpm"] = true
	tuple, err := doctor.VerifyTuple(ctx, "")
	report.NativeVersion = tuple.CLI
	tupleOK := err == nil
	report.TupleOK = &tupleOK
	if err != nil {
		report.State, report.Detail = productruntime.ProbeIncompatible, productruntime.NewRedactedString(err.Error())
		return report, nil
	}
	report.Features["exact-tuple"] = true
	report.Features["native-v1"] = true
	report.Features["lane"] = true
	report.State = productruntime.ProbeReady
	return report, nil
}

func validProbeDepth(depth productruntime.ProbeDepth) bool {
	return depth == productruntime.ProbePresence || depth == productruntime.ProbeVersion ||
		depth == productruntime.ProbeFeature || depth == productruntime.ProbeIntegration
}

func (doctor *DoctorProbe) environment() []string {
	if doctor.config.Environment != nil {
		return append([]string(nil), doctor.config.Environment...)
	}
	return os.Environ()
}

var versionPattern = regexp.MustCompile(`(?:^|\s)(\d+\.\d+\.\d+-[0-9A-Za-z.-]+)(?:\s|$)`)

func extractPinnedVersion(output string) string {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) == 2 {
		return match[1]
	}
	return strings.TrimSpace(output)
}

func safeDoctorEnv(environment []string) []string {
	allowed := map[string]bool{"HOME": true, "PATH": true, "DSH_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true}
	result := make([]string, 0, len(allowed))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && allowed[name] {
			result = append(result, entry)
		}
	}
	return result
}
