package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

type CommandProbe interface {
	LookPath(string) (string, error)
	Output(context.Context, string, []string, []string) ([]byte, error)
}

type IntegrationCheck func(context.Context) (bool, string, error)

type OSCommandProbe struct{}

func (OSCommandProbe) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (OSCommandProbe) Output(ctx context.Context, path string, arguments, environment []string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, arguments...) //nolint:gosec // exact typed doctor executable.
	command.Env = append([]string(nil), environment...)
	return command.Output()
}

type DoctorConfig struct {
	Executable       string
	PNPMExecutable   string
	ACPProfile       string
	DSHHome          string
	ProfileIdentity  string
	ACPAppManifest   string
	PluginManifest   string
	ProfileManifest  string
	ProbeCwd         string
	Environment      []string
	Commands         CommandProbe
	Processes        ACPProcessFactory
	CheckIntegration IntegrationCheck
	Timeout          time.Duration
}

type DoctorProbe struct{ config DoctorConfig }

func NewDoctorProbe(config DoctorConfig) (*DoctorProbe, error) {
	if config.Executable == "" {
		config.Executable = "dsh"
	}
	if config.PNPMExecutable == "" {
		config.PNPMExecutable = RequiredPNPM
	}
	if config.ACPProfile == "" {
		config.ACPProfile = "acp"
	}
	if config.Commands == nil {
		config.Commands = OSCommandProbe{}
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Timeout < time.Millisecond || config.Timeout > time.Minute || config.ACPAppManifest == "" || config.PluginManifest == "" || config.ProfileManifest == "" ||
		config.ACPProfile != strings.TrimSpace(config.ACPProfile) || strings.ContainsAny(config.ACPProfile, "\x00/\\") {
		return nil, errors.New("DSH doctor requires bounded timeout and exact tuple manifest paths")
	}
	if filepath.Base(config.Executable) != "dsh" || filepath.Base(config.PNPMExecutable) != RequiredPNPM {
		return nil, errors.New("DSH doctor executable identities are invalid")
	}
	if err := validateConfiguredProfileManifestShape(config.DSHHome, config.ACPProfile, config.ProfileManifest); err != nil {
		return nil, err
	}
	for _, path := range []string{config.ACPAppManifest, config.PluginManifest} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errors.New("DSH doctor tuple manifest paths must be absolute and clean")
		}
	}
	if config.ProbeCwd != "" && !validCwd(config.ProbeCwd) {
		return nil, errors.New("DSH doctor probe cwd must be absolute and clean")
	}
	if err := validateProfileIdentity(config.ACPProfile); err != nil {
		return nil, err
	}
	return &DoctorProbe{config: config}, nil
}

func (doctor *DoctorProbe) VerifyTuple(ctx context.Context, profileIdentity string) (Tuple, error) {
	profileIdentity = doctor.selectedProfile(profileIdentity)
	cliPath, err := doctor.resolveExecutable(doctor.config.Executable)
	if err != nil {
		return Tuple{}, fmt.Errorf("%w: DSH CLI is missing", productruntime.ErrUnavailable)
	}
	if filepath.Base(cliPath) != "dsh" {
		return Tuple{}, fmt.Errorf("%w: DSH CLI executable identity changed", productruntime.ErrIncompatible)
	}
	return doctor.verifyResolvedTuple(ctx, profileIdentity, cliPath)
}

func (doctor *DoctorProbe) verifyResolvedTuple(ctx context.Context, profileIdentity, cliPath string) (Tuple, error) {
	profileIdentity = doctor.selectedProfile(profileIdentity)
	profileManifest, err := validateManagedProfile(doctor.config.DSHHome, profileIdentity)
	if err != nil {
		return Tuple{}, err
	}
	pnpmPath, err := doctor.resolveExecutable(doctor.config.PNPMExecutable)
	if err != nil {
		return Tuple{}, fmt.Errorf("%w: pnpm is required for DSH", productruntime.ErrUnavailable)
	}
	if filepath.Base(pnpmPath) != RequiredPNPM {
		return Tuple{}, fmt.Errorf("%w: DSH requires the pnpm executable", productruntime.ErrIncompatible)
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctor.config.Timeout)
	defer cancel()
	doctorEnvironment := setStringEnv(safeDoctorEnv(doctor.environment()), "DSH_HOME", doctor.config.DSHHome)
	cliOutput, err := doctor.config.Commands.Output(probeCtx, cliPath, []string{"--version"}, doctorEnvironment)
	if err != nil {
		return Tuple{}, fmt.Errorf("%w: DSH CLI version probe failed", productruntime.ErrUnavailable)
	}
	pnpmOutput, err := doctor.config.Commands.Output(probeCtx, pnpmPath, []string{"--version"}, doctorEnvironment)
	if err != nil {
		return Tuple{}, fmt.Errorf("%w: pnpm version probe failed", productruntime.ErrUnavailable)
	}
	tuple := Tuple{
		CLI: extractPinnedVersion(string(cliOutput)), PackageManager: RequiredPNPM, PNPMVersion: strings.TrimSpace(string(pnpmOutput)),
		ACPApp:  manifestVersion(doctor.config.ACPAppManifest, ACPAppPackage),
		Plugin:  manifestVersion(doctor.config.PluginManifest, PluginPackage),
		Profile: manifestVersion(profileManifest, ProfilePackage),
	}
	if err := tuple.Validate(); err != nil {
		return tuple, err
	}
	return tuple, nil
}

func (doctor *DoctorProbe) Probe(ctx context.Context, request productruntime.ProbeRequest) (productruntime.ProbeReport, error) {
	if ctx == nil || request.ProductID != ProductID || !validProbeDepth(request.Depth) {
		return productruntime.ProbeReport{}, fmt.Errorf("%w: DSH doctor received product %q", productruntime.ErrProtocol, request.ProductID)
	}
	report := productruntime.ProbeReport{Features: map[string]bool{
		"native-cli": false, "pnpm": false, "exact-tuple": false, "peer": false,
		"lane": false, "parent": false, "acp-initialize": false, "acp-session-new": false,
	}}
	executable := doctor.config.Executable
	if request.ExecutablePath != "" {
		executable = request.ExecutablePath
	}
	resolvedExecutable, err := doctor.resolveExecutable(executable)
	if err != nil {
		report.State, report.Detail = productruntime.ProbeMissing, productruntime.NewRedactedString("DSH CLI not found")
		return report, nil
	}
	if filepath.Base(resolvedExecutable) != "dsh" {
		report.State, report.Detail = productruntime.ProbeIncompatible, productruntime.NewRedactedString("DSH CLI executable identity changed")
		return report, nil
	}
	report.Features["native-cli"] = true
	if _, err := doctor.resolveExecutable(doctor.config.PNPMExecutable); err != nil {
		report.State, report.Detail = productruntime.ProbeMissing, productruntime.NewRedactedString("pnpm is required for DSH")
		return report, nil
	}
	report.Features["pnpm"] = true
	if request.Depth == productruntime.ProbePresence {
		report.State = productruntime.ProbeReady
		return report, nil
	}

	tuple, err := doctor.verifyResolvedTuple(ctx, doctor.selectedProfile(doctor.config.ProfileIdentity), resolvedExecutable)
	report.NativeVersion = tuple.CLI
	tupleOK := err == nil
	report.TupleOK = &tupleOK
	if err != nil {
		if errors.Is(err, errManagedProfileUnavailable) {
			report.State = productruntime.ProbeUnconfigured
		} else if errors.Is(err, productruntime.ErrIncompatible) {
			report.State = productruntime.ProbeIncompatible
		} else {
			report.State = productruntime.ProbeError
		}
		report.Detail = productruntime.NewRedactedString(err.Error())
		return report, nil
	}
	report.Features["exact-tuple"] = true
	if request.Depth == productruntime.ProbeVersion {
		report.State = productruntime.ProbeReady
		return report, nil
	}
	if doctor.config.Processes == nil {
		report.State, report.Detail = productruntime.ProbeUnconfigured, productruntime.NewRedactedString("DSH keyless ACP feature probe is not configured")
		return report, nil
	}
	if err := doctor.keylessACPProbe(ctx, resolvedExecutable); err != nil {
		report.State, report.Detail = productruntime.ProbeError, productruntime.NewRedactedString(err.Error())
		return report, nil
	}
	report.Features["acp-initialize"] = true
	report.Features["acp-session-new"] = true
	report.Features["lane"] = true
	if request.Depth == productruntime.ProbeFeature {
		report.State = productruntime.ProbeReady
		return report, nil
	}
	if doctor.config.CheckIntegration == nil {
		report.State, report.Detail = productruntime.ProbeUnconfigured, productruntime.NewRedactedString("DSH live-session integration check is not configured")
		return report, nil
	}
	ready, detail, integrationErr := doctor.config.CheckIntegration(ctx)
	if integrationErr != nil || !ready {
		if strings.TrimSpace(detail) == "" {
			detail = "DSH live-session integration is not ready"
		}
		report.State, report.Detail = productruntime.ProbeUnconfigured, productruntime.NewRedactedString(detail)
		return report, nil
	}
	report.Features["peer"], report.Features["parent"] = true, true
	report.State = productruntime.ProbeReady
	return report, nil
}

func validProbeDepth(depth productruntime.ProbeDepth) bool {
	return depth == productruntime.ProbePresence || depth == productruntime.ProbeVersion ||
		depth == productruntime.ProbeFeature || depth == productruntime.ProbeIntegration
}

func (doctor *DoctorProbe) keylessACPProbe(ctx context.Context, executable string) (failure error) {
	profile := doctor.selectedProfile(doctor.config.ProfileIdentity)
	profileManifest, err := validateManagedProfile(doctor.config.DSHHome, profile)
	if err != nil {
		return err
	}
	cwd := doctor.config.ProbeCwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// session/new materializes durable product state even after session/close.
	// Point DSH at a disposable home whose sole profile is a symlink to the
	// already tuple-verified configured profile. Removal never traverses that
	// symlink, so the installed profile and the user's native store stay inert.
	probeBase := doctorEnvironmentValue(doctor.environment(), "HOME")
	if probeBase == "" {
		probeBase, _ = os.UserHomeDir()
	}
	if !filepath.IsAbs(probeBase) {
		return fmt.Errorf("%w: DSH doctor HOME is not absolute", productruntime.ErrUnsupportedPolicy)
	}
	probeHome, err := os.MkdirTemp(probeBase, ".agent-sessions-dsh-doctor-")
	if err != nil {
		return fmt.Errorf("create isolated DSH doctor home: %w", err)
	}
	defer func() { failure = errors.Join(failure, os.RemoveAll(probeHome)) }()
	profiles := filepath.Join(probeHome, "profiles")
	if err := os.Mkdir(profiles, 0o700); err != nil {
		return fmt.Errorf("create isolated DSH doctor profiles: %w", err)
	}
	profileRoot, err := filepath.Abs(filepath.Dir(profileManifest))
	if err != nil {
		return fmt.Errorf("resolve configured DSH profile: %w", err)
	}
	if err := os.Symlink(profileRoot, filepath.Join(profiles, profile)); err != nil {
		return fmt.Errorf("link exact DSH doctor profile: %w", err)
	}
	environment := setEnvVar(doctorRuntimeEnv(doctor.environment()), "DSH_PERMISSION_MODE", string(SandboxWorkspaceWrite))
	environment = setEnvVar(environment, "DSH_HOME", probeHome)
	command := productruntime.NativeCommand{
		Path: executable, Args: []string{"--profile", profile},
		Env: environment, Cwd: cwd,
	}
	processCtx, cancelProcess := context.WithCancel(context.Background())
	process, err := doctor.config.Processes.StartACPProcess(processCtx, command)
	if err != nil {
		cancelProcess()
		return fmt.Errorf("start DSH ACP keyless probe: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := process.Cleanup(cleanupCtx)
		cancel()
		cancelProcess()
		failure = errors.Join(failure, cleanupErr)
	}()
	client, err := NewACPClient(process, nil)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctor.config.Timeout)
	defer cancel()
	if err := client.Initialize(probeCtx); err != nil {
		return err
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	policy, _ := MapPermission("default")
	if err := client.Request(probeCtx, "session/new", sessionParams("", cwd, policy), &created); err != nil {
		return err
	}
	if created.SessionID == "" {
		return fmt.Errorf("%w: keyless DSH session/new returned no identity", productruntime.ErrProtocol)
	}
	if err := client.Request(probeCtx, "session/close", map[string]string{"sessionId": created.SessionID}, nil); err != nil {
		return err
	}
	return nil
}

func (doctor *DoctorProbe) selectedProfile(profile string) string {
	if profile != "" {
		return profile
	}
	if doctor.config.ProfileIdentity != "" {
		return doctor.config.ProfileIdentity
	}
	return doctor.config.ACPProfile
}

func (doctor *DoctorProbe) environment() []string {
	if doctor.config.Environment == nil {
		return os.Environ()
	}
	return append([]string(nil), doctor.config.Environment...)
}

func doctorEnvironmentValue(environment []string, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, ok := strings.Cut(environment[index], "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

func (doctor *DoctorProbe) resolveExecutable(name string) (string, error) {
	if filepath.IsAbs(name) {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", errors.New("DSH doctor executable path is not executable")
		}
		return name, nil
	}
	return doctor.config.Commands.LookPath(name)
}

var exactVersionPattern = regexp.MustCompile("(?:^|[^0-9A-Za-z.-])(" + regexp.QuoteMeta(PinnedVersion) + ")(?:$|[^0-9A-Za-z.-])")

func extractPinnedVersion(output string) string {
	match := exactVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) == 2 {
		return match[1]
	}
	return strings.TrimSpace(output)
}

func manifestVersion(path, expectedName string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 1<<20 {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) > 1<<20 {
		return ""
	}
	var manifest struct {
		Name             string            `json:"name"`
		Version          string            `json:"version"`
		PackageManager   string            `json:"packageManager"`
		Dependencies     map[string]string `json:"dependencies"`
		PeerDependencies map[string]string `json:"peerDependencies"`
	}
	if json.Unmarshal(body, &manifest) != nil || manifest.Name != expectedName {
		return ""
	}
	packageManager := RequiredPNPM + "@" + PinnedPNPM
	if expectedName == PluginPackage && (manifest.PackageManager != packageManager ||
		manifest.Dependencies["@deepseek-ai/dsh-llm"] != PinnedVersion ||
		manifest.Dependencies["@deepseek-ai/dsh-tools"] != PinnedVersion ||
		manifest.Dependencies["@deepseek-ai/dsh-sandbox-policy"] != PinnedVersion ||
		manifest.Dependencies["@deepseek-ai/dsh-user-approval"] != PinnedVersion ||
		manifest.PeerDependencies[CLIPackage] != PinnedVersion || manifest.PeerDependencies[ACPAppPackage] != PinnedVersion) {
		return ""
	}
	if expectedName == ProfilePackage && (manifest.PackageManager != packageManager ||
		manifest.Dependencies[ACPAppPackage] != PinnedVersion || manifest.Dependencies[PluginPackage] != PinnedVersion) {
		return ""
	}
	return manifest.Version
}

func setStringEnv(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		entryName, _, _ := strings.Cut(entry, "=")
		if entryName != name {
			result = append(result, entry)
		}
	}
	return append(result, name+"="+value)
}

func safeDoctorEnv(environment []string) []string {
	allowed := map[string]bool{
		"HOME": true, "USER": true, "LOGNAME": true, "PATH": true, "LANG": true,
		"LC_ALL": true, "XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true, "XDG_DATA_HOME": true,
		"DSH_HOME": true,
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		if allowed[name] {
			result = append(result, entry)
		}
	}
	return result
}

func doctorRuntimeEnv(environment []string) []productruntime.EnvVar {
	safe := safeDoctorEnv(environment)
	result := make([]productruntime.EnvVar, 0, len(safe))
	for _, entry := range safe {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			result = append(result, productruntime.EnvVar{Name: name, Value: value})
		}
	}
	return result
}

var _ productruntime.DoctorProbe = (*DoctorProbe)(nil)
var _ TupleVerifier = (*DoctorProbe)(nil)
