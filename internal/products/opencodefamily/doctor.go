package opencodefamily

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/antst/sessionbus/internal/productruntime"
)

type VersionRunner func(context.Context, string) (string, error)
type IntegrationCheck func(context.Context) (bool, string, error)

type DoctorConfig struct {
	ProductID        string
	Executable       string
	TestedVersion    string
	Dialect          Dialect
	WorkDir          string
	Servers          ServerManager
	RequiredRoutes   []string
	RunVersion       VersionRunner
	CheckIntegration IntegrationCheck
}

type DoctorProbe struct{ config DoctorConfig }

func NewDoctorProbe(config DoctorConfig) (*DoctorProbe, error) {
	if config.ProductID == "" || config.Executable == "" || config.TestedVersion == "" ||
		config.Dialect != DialectOpenCode && config.Dialect != DialectKilo {
		return nil, productruntime.ErrProtocol
	}
	if config.RunVersion == nil {
		config.RunVersion = runVersion
	}
	config.RequiredRoutes = append([]string(nil), config.RequiredRoutes...)
	return &DoctorProbe{config: config}, nil
}

func runVersion(ctx context.Context, executable string) (string, error) {
	command := exec.CommandContext(ctx, executable, "--version")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return "", err
	}
	lines := strings.Fields(output.String())
	if len(lines) == 0 {
		return "", productruntime.ErrProtocol
	}
	return lines[len(lines)-1], nil
}

func (probe *DoctorProbe) Probe(ctx context.Context, request productruntime.ProbeRequest) (report productruntime.ProbeReport, returnErr error) {
	if request.ProductID != probe.config.ProductID {
		return productruntime.ProbeReport{}, productruntime.ErrProtocol
	}
	switch request.Depth {
	case productruntime.ProbePresence, productruntime.ProbeVersion, productruntime.ProbeFeature, productruntime.ProbeIntegration:
	default:
		return productruntime.ProbeReport{}, productruntime.ErrProtocol
	}
	executable := request.ExecutablePath
	if executable == "" {
		resolved, err := exec.LookPath(probe.config.Executable)
		if err != nil {
			return productruntime.ProbeReport{State: productruntime.ProbeMissing, Features: map[string]bool{"native-cli": false}, Detail: productruntime.NewRedactedString("native executable is not on PATH")}, nil
		}
		executable = resolved
	}
	if filepath.Base(executable) != filepath.Base(probe.config.Executable) {
		return productruntime.ProbeReport{State: productruntime.ProbeIncompatible, Features: map[string]bool{"native-cli": false}, Detail: productruntime.NewRedactedString("executable basename does not match product")}, nil
	}
	features := map[string]bool{"native-cli": true}
	report = productruntime.ProbeReport{State: productruntime.ProbeReady, Features: features}
	if request.Depth == productruntime.ProbePresence {
		return report, nil
	}
	version, err := probe.config.RunVersion(ctx, executable)
	report.NativeVersion = strings.TrimSpace(version)
	if err != nil {
		report.State = productruntime.ProbeError
		report.Detail = productruntime.NewRedactedString("native version probe failed")
		return report, nil
	}
	if report.NativeVersion != probe.config.TestedVersion {
		report.State = productruntime.ProbeIncompatible
		report.Detail = productruntime.NewRedactedString(fmt.Sprintf("native version %s is not tested version %s", report.NativeVersion, probe.config.TestedVersion))
		return report, nil
	}
	features["native-version"] = true
	if request.Depth == productruntime.ProbeVersion {
		return report, nil
	}
	if probe.config.Servers == nil || !validDirectory(probe.config.WorkDir) {
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString("bounded feature probe server is not configured")
		return report, nil
	}
	server, err := probe.config.Servers.Open(ctx, ServerOpenRequest{Key: "doctor-" + probe.config.ProductID, Cwd: probe.config.WorkDir})
	if err != nil {
		report.State = probeState(err)
		report.Detail = productruntime.NewRedactedString("ephemeral native feature probe failed")
		return report, nil
	}
	defer func() {
		if err := server.Close(context.Background()); err != nil {
			report.State = productruntime.ProbeError
			report.Detail = productruntime.NewRedactedString("ephemeral native feature probe failed to stop")
		}
	}()
	client := server.Client()
	if client == nil {
		report.State = productruntime.ProbeError
		return report, nil
	}
	routes, err := client.ProbeDocument(ctx, probe.config.RequiredRoutes)
	if err != nil {
		report.State = probeState(err)
		return report, nil
	}
	for route, present := range routes {
		features["route:"+route] = present
		if !present {
			report.State = productruntime.ProbeIncompatible
			report.Detail = productruntime.NewRedactedString("required documented route is absent: " + route)
			return report, nil
		}
	}
	permissions, mapErr := MapPermissionRules("default")
	if mapErr != nil {
		report.State = productruntime.ProbeError
		return report, nil
	}
	session, err := client.CreateSession(ctx, "agent-sessions-doctor", permissions)
	if err != nil {
		report.State = probeState(err)
		return report, nil
	}
	sessionDeleted := false
	defer func() {
		if !sessionDeleted {
			if err := client.DeleteSession(context.Background(), session.ID); err != nil {
				report.State = productruntime.ProbeError
				report.Detail = productruntime.NewRedactedString("doctor session cleanup was not confirmed")
			}
		}
	}()
	if _, err := client.GetSession(ctx, session.ID); err != nil {
		report.State = probeState(err)
		return report, nil
	}
	if err := client.DeleteSession(ctx, session.ID); err != nil {
		report.State = probeState(err)
		return report, nil
	}
	sessionDeleted = true
	features["session-round-trip"] = true
	providers, err := client.ProvidersAvailable(ctx)
	if err != nil {
		report.State = probeState(err)
		return report, nil
	}
	features["model-provider"] = providers
	if !providers {
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString("no native model provider is available")
		return report, nil
	}
	features["peer"], features["lane"] = true, true
	if request.Depth != productruntime.ProbeIntegration {
		return report, nil
	}
	if probe.config.CheckIntegration == nil {
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString("managed component integration is not configured")
		return report, nil
	}
	ready, detail, err := probe.config.CheckIntegration(ctx)
	if err != nil || !ready {
		report.State = productruntime.ProbeUnconfigured
		report.Detail = productruntime.NewRedactedString(detail)
		return report, nil
	}
	features["integration"] = true
	return report, nil
}

func probeState(err error) productruntime.ProbeState {
	switch {
	case errors.Is(err, productruntime.ErrIncompatible), errors.Is(err, productruntime.ErrProtocol):
		return productruntime.ProbeIncompatible
	case errors.Is(err, productruntime.ErrUnavailable):
		return productruntime.ProbeMissing
	case errors.Is(err, productruntime.ErrUnauthorized):
		return productruntime.ProbeUnconfigured
	default:
		return productruntime.ProbeError
	}
}

var _ productruntime.DoctorProbe = (*DoctorProbe)(nil)
