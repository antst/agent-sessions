package codebuddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

type DoctorProbe struct {
	config Config
	deps   productruntime.HostDeps
}

func NewDoctorProbe(config Config, deps productruntime.HostDeps) (*DoctorProbe, error) {
	normalized, err := normalizeConfig(config, deps)
	if err != nil {
		return nil, err
	}
	return &DoctorProbe{config: normalized, deps: deps}, nil
}

func (doctor *DoctorProbe) Probe(ctx context.Context, request productruntime.ProbeRequest) (productruntime.ProbeReport, error) {
	if doctor == nil || ctx == nil || request.ProductID != ProductID || !validProbeDepth(request.Depth) {
		return productruntime.ProbeReport{}, ErrInvalidConfiguration
	}
	executable := strings.TrimSpace(request.ExecutablePath)
	if executable == "" {
		executable = doctor.config.Executable
	}
	result, err := doctor.config.Commands.Run(ctx, executable, "--version")
	if err != nil {
		state := productruntime.ProbeError
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			state = productruntime.ProbeMissing
		}
		return probeReport(state, "", baseFeatures(false), "CodeBuddy executable is unavailable"), nil
	}
	version := strings.TrimSpace(result.Stdout)
	if version == "" {
		version = strings.TrimSpace(result.Stderr)
	}
	if version != PinnedVersion {
		return probeReport(productruntime.ProbeIncompatible, version, baseFeatures(false), "CodeBuddy version does not match the pinned beta protocol"), nil
	}
	features := baseFeatures(false)
	features["native-cli"] = true
	if request.Depth == productruntime.ProbePresence || request.Depth == productruntime.ProbeVersion {
		return probeReport(productruntime.ProbeReady, version, features, "CodeBuddy pinned executable is present"), nil
	}
	if doctor.deps.OwnedProcesses == nil {
		return probeReport(productruntime.ProbeUnconfigured, version, features, "CodeBuddy feature probe requires an owned-process supervisor"), nil
	}
	featureSet, featureErr := doctor.probeServer(ctx, executable, request.Depth == productruntime.ProbeIntegration)
	for name, ready := range featureSet {
		features[name] = ready
	}
	if featureErr != nil {
		state := productruntime.ProbeError
		if errors.Is(featureErr, ErrOpenAPIDrift) {
			state = productruntime.ProbeIncompatible
		}
		return probeReport(state, version, features, "CodeBuddy offline protocol probe failed"), nil
	}
	return probeReport(productruntime.ProbeReady, version, features, "Offline protocol ready; Tencent-authenticated model-turn acceptance remains pending and experimental"), nil
}

func (doctor *DoctorProbe) probeServer(ctx context.Context, executable string, integration bool) (map[string]bool, error) {
	features := baseFeatures(false)
	root, err := os.MkdirTemp("", "agent-sessions-codebuddy-doctor-")
	if err != nil {
		return features, err
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0o700); err != nil {
		return features, err
	}
	environment, err := isolatedEnvironment(root)
	if err != nil {
		return features, err
	}
	endpoint, err := doctor.config.Endpoints.Allocate(ctx)
	if err != nil {
		return features, err
	}
	port, err := endpointPort(endpoint)
	if err != nil {
		return features, err
	}
	password, err := doctor.config.Secrets.Generate(ctx)
	if err != nil || password.Empty() {
		return features, errors.Join(ErrInvalidConfiguration, err)
	}
	auth, err := productserver.NewBearerAuth(password)
	if err != nil {
		return features, err
	}
	command := productruntime.NativeCommand{
		Path: executable,
		Args: []string{"--serve", "--auth", "password", "--port", port,
			"--strict-mcp-config", "--mcp-config", doctor.config.MCPConfigPath},
		Env: append(environment,
			productruntime.EnvVar{Name: GatewayAuthEnv, Value: "password"},
			productruntime.EnvVar{Name: ProductEnv, Value: ProductID}),
		SensitiveEnv: []productruntime.SensitiveEnvVar{{Name: GatewayPasswordEnv, Value: password}},
		Cwd:          root,
	}
	server, err := productserver.StartOwnedServer(ctx, productserver.OwnedServerConfig{
		Command: command, Endpoint: endpoint, Auth: auth, Limits: doctor.config.Limits,
		Supervisor: doctor.deps.OwnedProcesses,
		Ready: func(probeCtx context.Context, safe *productserver.Client) error {
			return wrapOwnedClient(safe).Health(probeCtx)
		},
	})
	if err != nil {
		if server != nil {
			return features, errors.Join(productruntime.ErrCleanupDebt, err)
		}
		return features, err
	}
	client := wrapOwnedClient(server.Client())
	document, probeErr := client.OpenAPI(ctx)
	if probeErr == nil {
		probeErr = validateOpenAPI(document, features)
		features["durable-resume"] = doctor.config.Recovery != nil
	}
	if probeErr == nil && integration {
		probeErr = probeOfflineJobRoundTrip(ctx, client, root)
		features["offline-job-roundtrip"] = probeErr == nil
	}
	closeErr := server.Close(ctx)
	if closeErr != nil {
		probeErr = errors.Join(probeErr, productruntime.ErrCleanupDebt, closeErr)
	}
	persisted, scanErr := treeContains(root, []byte(password.Reveal()))
	if scanErr != nil {
		probeErr = errors.Join(probeErr, scanErr)
	}
	if persisted {
		probeErr = errors.Join(probeErr, ErrSecretPersisted)
	}
	return features, probeErr
}

func probeOfflineJobRoundTrip(ctx context.Context, client *APIClient, cwd string) (probeErr error) {
	baseline, err := client.ListJobs(ctx, cwd)
	if err != nil {
		return err
	}
	baselineIDs := jobIdentitySet(baseline)
	const marker = "agent-sessions-offline-doctor"
	var cleanupJob AgentJob
	defer func() {
		if cleanupJob.ID == "" {
			return
		}
		deleted, deleteErr := client.DeleteJob(ctx, cleanupJob.ID)
		if deleteErr != nil || !deleted.Deleted {
			probeErr = errors.Join(probeErr, productruntime.ErrCleanupDebt, deleteErr)
			if deleteErr == nil && !deleted.Deleted {
				probeErr = errors.Join(probeErr, errors.New("offline doctor job deletion was refused"))
			}
		}
	}()
	reportedJob, err := client.DispatchJob(ctx, DispatchJobRequest{
		Prompt: "Agent Sessions offline protocol probe; authenticated model output is not credited.",
		Cwd:    cwd, PermissionMode: "default", Name: marker,
	})
	if err != nil {
		// A malformed/lost response may still have written. Recover only an
		// exact new marked job for guarded cleanup, never as readiness credit.
		jobs, listErr := client.ListJobs(ctx, cwd)
		if listErr == nil {
			candidates := exactDoctorJobs(jobs, baselineIDs, marker, cwd, "", "")
			if len(candidates) == 1 {
				cleanupJob = candidates[0]
			}
		}
		return errors.Join(err, listErr)
	}
	if reportedJob.Name != marker {
		return fmt.Errorf("%w: dispatch did not retain the doctor marker", productruntime.ErrProtocol)
	}
	jobs, err := client.ListJobs(ctx, cwd)
	if err != nil {
		return err
	}
	candidates := exactDoctorJobs(jobs, baselineIDs, marker, cwd, reportedJob.ID, reportedJob.SessionID)
	if len(candidates) != 1 {
		return fmt.Errorf("%w: offline job list did not corroborate exact dispatch", productruntime.ErrProtocol)
	}
	cleanupJob = candidates[0]
	return nil
}

func exactDoctorJobs(jobs []AgentJob, baseline map[string]struct{}, marker, cwd, jobID, sessionID string) []AgentJob {
	result := make([]AgentJob, 0, 1)
	for _, candidate := range jobs {
		if _, existed := baseline[candidate.ID]; existed || candidate.Name != marker || !candidate.valid() || filepath.Clean(candidate.Cwd) != filepath.Clean(cwd) {
			continue
		}
		if jobID != "" && candidate.ID != jobID || sessionID != "" && candidate.SessionID != sessionID {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

var requiredOpenAPIOperations = map[string]map[string]string{
	"/api/v1/health":               {"get": "getHealth"},
	"/api/v1/jobs":                 {"get": "listAgentJobs", "post": "dispatchAgentJob"},
	"/api/v1/jobs/resume":          {"post": "resumeAgentJob"},
	"/api/v1/jobs/{id}":            {"get": "getAgentJob", "delete": "deleteAgentJob"},
	"/api/v1/jobs/{id}/reply":      {"post": "replyAgentJob"},
	"/api/v1/jobs/{id}/respawn":    {"post": "respawnAgentJob"},
	"/api/v1/jobs/{id}/stop":       {"post": "stopAgentJob"},
	"/api/v1/jobs/{id}/stream":     {"get": "streamAgentJobTranscript"},
	"/api/v1/process/start":        {"post": "processStart"},
	"/api/v1/sessions/live":        {"get": "getLiveSession"},
	"/api/v1/sessions/{id}/rename": {"post": "renameSession"},
	"/api/v1/sessions/{id}/reply":  {"post": "replySession"},
	"/api/v1/workers":              {"get": "listWorkers"},
}

func validateOpenAPI(document OpenAPIDocument, features map[string]bool) error {
	if document.Info.Title != OpenAPITitle || document.Info.Version != PinnedVersion {
		return ErrOpenAPIDrift
	}
	paths := make([]string, 0, len(requiredOpenAPIOperations))
	for path := range requiredOpenAPIOperations {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		methods := document.Paths[path]
		for method, operation := range requiredOpenAPIOperations[path] {
			if methods == nil || methods[method].OperationID != operation {
				return fmt.Errorf("%w: %s %s", ErrOpenAPIDrift, strings.ToUpper(method), path)
			}
		}
	}
	if err := validateDeferredBindingSchemas(document); err != nil {
		return err
	}
	for _, feature := range []string{"peer", "lane", "parent", "password-auth", "csrf-header", "openapi",
		"job-dispatch", "job-reply", "job-stream", "job-stop", "job-respawn", "job-archive", "deferred-session-binding"} {
		features[feature] = true
	}
	return nil
}

func validateDeferredBindingSchemas(document OpenAPIDocument) error {
	job := document.Components.Schemas["AgentJob"]
	if job.Type != "object" || !schemaHasStringProperties(job, "id", "sessionId", "name", "cwd") {
		return fmt.Errorf("%w: AgentJob lacks deferred-binding identity fields", ErrOpenAPIDrift)
	}
	list := document.Components.Schemas["AgentJobList"]
	jobs, ok := list.Properties["jobs"]
	if list.Type != "object" || !ok || jobs.Type != "array" || jobs.Items == nil || jobs.Items.Ref != "#/components/schemas/AgentJob" || !containsString(list.Required, "jobs") {
		return fmt.Errorf("%w: AgentJobList shape changed", ErrOpenAPIDrift)
	}
	operation := document.Paths["/api/v1/jobs"]["post"]
	media, ok := operation.RequestBody.Content["application/json"]
	request := media.Schema
	if !ok || request.Type != "object" || !schemaHasStringProperties(request, "prompt", "cwd", "permissionMode", "name") || !containsString(request.Required, "prompt") {
		return fmt.Errorf("%w: dispatchAgentJob request shape changed", ErrOpenAPIDrift)
	}
	return nil
}

func schemaHasStringProperties(schema OpenAPISchema, names ...string) bool {
	for _, name := range names {
		property, ok := schema.Properties[name]
		if !ok || property.Type != "string" {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func baseFeatures(ready bool) map[string]bool {
	return map[string]bool{
		"native-cli": ready, "peer": ready, "lane": ready, "parent": ready,
		"password-auth": ready, "csrf-header": ready, "openapi": ready,
		"job-dispatch": ready, "job-reply": ready, "job-stream": ready,
		"job-stop": ready, "job-respawn": ready, "job-archive": ready,
		"durable-resume": ready, "deferred-session-binding": ready, "offline-job-roundtrip": ready,
		// This cell is intentionally never inferred from an offline probe.
		"tencent-model-turn": false,
	}
}

func probeReport(state productruntime.ProbeState, version string, features map[string]bool, detail string) productruntime.ProbeReport {
	copyFeatures := make(map[string]bool, len(features))
	for name, ready := range features {
		copyFeatures[name] = ready
	}
	return productruntime.ProbeReport{
		State: state, NativeVersion: version, Features: copyFeatures,
		Detail: productruntime.NewRedactedString(detail),
	}
}

func validProbeDepth(depth productruntime.ProbeDepth) bool {
	return depth == productruntime.ProbePresence || depth == productruntime.ProbeVersion || depth == productruntime.ProbeFeature || depth == productruntime.ProbeIntegration
}

func isolatedEnvironment(root string) ([]productruntime.EnvVar, error) {
	paths := map[string]string{
		"HOME":            root,
		"XDG_STATE_HOME":  filepath.Join(root, "xdg-state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg-config"),
		"XDG_CACHE_HOME":  filepath.Join(root, "xdg-cache"),
		"XDG_DATA_HOME":   filepath.Join(root, "xdg-data"),
	}
	keys := make([]string, 0, len(paths))
	for name := range paths {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	result := make([]productruntime.EnvVar, 0, len(paths))
	for _, name := range keys {
		path := paths[name]
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
		result = append(result, productruntime.EnvVar{Name: name, Value: path})
	}
	return result, nil
}

func treeContains(root string, needle []byte) (bool, error) {
	if len(needle) == 0 {
		return false, ErrInvalidConfiguration
	}
	const maximumFiles = 2048
	const maximumBytes = 16 << 20
	files, total := 0, int64(0)
	found := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if found {
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("doctor state contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		files++
		total += info.Size()
		if files > maximumFiles || total > maximumBytes {
			return fmt.Errorf("doctor state scan bound exceeded")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(body)) != info.Size() {
			return errors.Join(readErr, closeErr)
		}
		found = bytes.Contains(body, needle)
		return nil
	})
	return found, err
}

var _ productruntime.DoctorProbe = (*DoctorProbe)(nil)
