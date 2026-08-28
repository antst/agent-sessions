package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	hubConfigurationDirectoryName = "agent-sessions-hub"
	hubStateDirectoryName         = "agent-sessions-hub"
	hubConfigurationRecord        = "configuration"
	hubRuntimeRecord              = "runtime/status"
	hubRecordBytes                = 256 * 1024
)

// HubPaths are the configuration and durable state roots owned solely by the
// separately installed central hub. They never alias the host daemon roots.
type HubPaths struct {
	ConfigurationRoot string
	StateRoot         string
}

// ResolveHubPaths resolves the fixed per-user hub roots without creating or
// mutating them.
func ResolveHubPaths() (HubPaths, error) {
	home, err := hubAbsoluteEnvironmentPath("HOME", "")
	if err != nil {
		return HubPaths{}, err
	}
	configurationBase, err := hubAbsoluteEnvironmentPath("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return HubPaths{}, err
	}
	stateBase, err := hubAbsoluteEnvironmentPath("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err != nil {
		return HubPaths{}, err
	}
	return HubPaths{
		ConfigurationRoot: filepath.Join(configurationBase, hubConfigurationDirectoryName),
		StateRoot:         filepath.Join(stateBase, hubStateDirectoryName),
	}, nil
}

func hubAbsoluteEnvironmentPath(name, fallback string) (string, error) {
	value, present := os.LookupEnv(name)
	if !present || value == "" {
		value = fallback
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) == string(filepath.Separator) {
		return "", fmt.Errorf("%s must name a clean absolute non-root path", name)
	}
	return filepath.Clean(value), nil
}

// HubConfiguration is non-secret central-hub configuration. Host identity,
// products and connector state cannot be represented here.
type HubConfiguration struct {
	SchemaVersion int    `json:"schema_version"`
	Listen        string `json:"listen"`
}

// HubRuntimeStatus is the durable metadata-only observation published by the
// running hub. It does not own or duplicate the registry itself.
type HubRuntimeStatus struct {
	SchemaVersion   int              `json:"schema_version"`
	RuntimeVersion  string           `json:"runtime_version"`
	RuntimeIdentity string           `json:"runtime_identity"`
	PID             int              `json:"pid"`
	ProcStart       string           `json:"proc_start"`
	Listener        string           `json:"listener"`
	Service         map[string]any   `json:"service"`
	ProtocolVersion int              `json:"protocol_version"`
	ConnectedHosts  int              `json:"connected_hosts"`
	Routing         map[string]any   `json:"routing"`
	Debt            []map[string]any `json:"debt"`
}

// HubStatusProjection is the exact public status schema from the CLI
// contract. Its fields are intentionally independent from host status.
type HubStatusProjection struct {
	RuntimeVersion  string           `json:"runtime_version"`
	RuntimeIdentity string           `json:"runtime_identity"`
	PID             int              `json:"pid"`
	ProcStart       string           `json:"proc_start"`
	Listener        string           `json:"listener"`
	Service         map[string]any   `json:"service"`
	ProtocolVersion int              `json:"protocol_version"`
	ConnectedHosts  int              `json:"connected_hosts"`
	Routing         map[string]any   `json:"routing"`
	Debt            []map[string]any `json:"debt"`
}

// HubDoctorCheck is one bounded, cause-specific read-only check.
type HubDoctorCheck struct {
	ID         string `json:"id"`
	Healthy    bool   `json:"healthy"`
	ErrorCode  string `json:"error_code,omitempty"`
	NextAction string `json:"next_action,omitempty"`
}

// HubDoctorProjection is the stable read-only hub diagnosis schema.
type HubDoctorProjection struct {
	Healthy bool             `json:"healthy"`
	Checks  []HubDoctorCheck `json:"checks"`
}

func openHubStore(root string) (*statestore.Store, error) {
	return statestore.Open(statestore.Options{Root: root, MaxRecordBytes: hubRecordBytes})
}

func saveHubRuntimeStatus(ctx context.Context, paths HubPaths, status HubRuntimeStatus) error {
	store, err := openHubStore(paths.StateRoot)
	if err != nil {
		return err
	}
	var prior HubRuntimeStatus
	revision, err := store.Read(ctx, hubRuntimeRecord, &prior)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err = store.CompareAndSwap(ctx, hubRuntimeRecord, revision, status)
	return err
}

// ReadHubStatus reads the last metadata-only hub observation without starting
// or repairing either service.
func ReadHubStatus(ctx context.Context) (HubStatusProjection, error) {
	paths, err := ResolveHubPaths()
	if err != nil {
		return HubStatusProjection{}, err
	}
	info, err := os.Lstat(paths.StateRoot)
	if err != nil {
		return HubStatusProjection{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return HubStatusProjection{}, errors.New("hub state root is not an owner-only real directory")
	}
	store, err := openHubStore(paths.StateRoot)
	if err != nil {
		return HubStatusProjection{}, err
	}
	var status HubRuntimeStatus
	if _, err := store.Read(ctx, hubRuntimeRecord, &status); err != nil {
		return HubStatusProjection{}, err
	}
	return hubStatusProjection(status), nil
}

func hubStatusProjection(status HubRuntimeStatus) HubStatusProjection {
	service := cloneHubMap(status.Service)
	routing := cloneHubMap(status.Routing)
	debt := make([]map[string]any, 0, len(status.Debt))
	for _, item := range status.Debt {
		debt = append(debt, cloneHubMap(item))
	}
	return HubStatusProjection{
		RuntimeVersion: status.RuntimeVersion, RuntimeIdentity: status.RuntimeIdentity,
		PID: status.PID, ProcStart: status.ProcStart, Listener: status.Listener,
		Service: service, ProtocolVersion: status.ProtocolVersion, ConnectedHosts: status.ConnectedHosts,
		Routing: routing, Debt: debt,
	}
}

func hubStatusProcessMatches(status HubStatusProjection) bool {
	process := procinfo.Read(status.PID)
	return process.Status == procinfo.Known && process.Start == status.ProcStart
}

// InspectHubDoctor evaluates durable metadata without starting or mutating the
// hub or host service.
func InspectHubDoctor(ctx context.Context) HubDoctorProjection {
	status, err := ReadHubStatus(ctx)
	available := err == nil
	processMatches := available && hubStatusProcessMatches(status)
	checks := []HubDoctorCheck{
		hubDoctorCheck("service_manager", available && len(status.Service) != 0, "service_unavailable", "inspect the agent-sessions-hub user service"),
		hubDoctorCheck("runtime_identity", available && status.RuntimeIdentity != "" && status.ProcStart != "", "identity_incomplete", "restart the agent-sessions-hub user service"),
		hubDoctorCheck("process_identity", processMatches, "process_identity_mismatch", "restart the agent-sessions-hub user service and verify its exact process identity"),
		hubDoctorCheck("listener", available && status.Listener != "", "hub_listener_unavailable", "verify the configured listener address and service state"),
		hubDoctorCheck("state_schema", available, "hub_state_unavailable", "inspect the hub state root and retry"),
		hubDoctorCheck("federation_protocol", available && status.ProtocolVersion == ProtocolVersion, "protocol_mismatch", "deploy a hub with the required protocol version"),
		hubDoctorCheck("lifecycle_debt", available && len(status.Debt) == 0, "lifecycle_debt", "resolve the reported hub lifecycle debt and retry"),
	}
	healthy := true
	for _, check := range checks {
		healthy = healthy && check.Healthy
	}
	return HubDoctorProjection{Healthy: healthy, Checks: checks}
}

func hubDoctorCheck(id string, healthy bool, code, action string) HubDoctorCheck {
	if healthy {
		return HubDoctorCheck{ID: id, Healthy: true}
	}
	return HubDoctorCheck{ID: id, Healthy: false, ErrorCode: code, NextAction: action}
}

func renderHubDiagnostic(kind diagnostics.OutputKind, event string, fields map[string]any) ([]byte, error) {
	return diagnostics.Render(kind, event, hubDiagnosticFields(fields))
}

func renderHubStatus(fields map[string]any, machine bool) ([]byte, error) {
	return renderHubProjection("hub.status", fields, machine)
}

func renderHubDoctor(fields map[string]any, machine bool) ([]byte, error) {
	return renderHubProjection("hub.doctor", fields, machine)
}

// RenderHubStatus renders the public projection through the shared bounded
// diagnostics envelope.
func RenderHubStatus(status HubStatusProjection, machine bool) ([]byte, error) {
	return renderHubStatus(map[string]any{
		"runtime_version": status.RuntimeVersion, "runtime_identity": status.RuntimeIdentity,
		"pid": status.PID, "proc_start": status.ProcStart, "listener": status.Listener,
		"service": status.Service, "protocol_version": status.ProtocolVersion,
		"connected_hosts": status.ConnectedHosts, "routing": status.Routing, "debt": status.Debt,
	}, machine)
}

// RenderHubDoctor renders the public projection through the same bounded
// diagnostics envelope without gaining service-start authority.
func RenderHubDoctor(doctor HubDoctorProjection, machine bool) ([]byte, error) {
	checks := make([]any, 0, len(doctor.Checks))
	for _, check := range doctor.Checks {
		checks = append(checks, map[string]any{
			"id": check.ID, "healthy": check.Healthy, "error_code": check.ErrorCode, "next_action": check.NextAction,
		})
	}
	return renderHubDoctor(map[string]any{"role": "hub", "healthy": doctor.Healthy, "checks": checks}, machine)
}

func renderHubProjection(event string, fields map[string]any, machine bool) ([]byte, error) {
	body, err := diagnostics.Render(diagnostics.OutputNormal, event, hubDiagnosticFields(fields))
	if err != nil || machine {
		return body, err
	}
	var envelope diagnostics.Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(envelope.Metadata))
	for key := range envelope.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var output strings.Builder
	output.WriteString(event)
	for _, key := range keys {
		value, marshalErr := json.Marshal(envelope.Metadata[key])
		if marshalErr != nil {
			return nil, marshalErr
		}
		output.WriteByte(' ')
		output.WriteString(key)
		output.WriteByte('=')
		output.Write(value)
	}
	return []byte(output.String()), nil
}

func hubDiagnosticFields(fields map[string]any) map[string]any {
	allowed := map[string]struct{}{
		"request_id": {}, "operation": {}, "role": {}, "runtime_version": {}, "runtime_identity": {},
		"pid": {}, "proc_start": {}, "service": {}, "state": {}, "revision": {}, "protocol_version": {},
		"listener": {}, "connected_hosts": {}, "routing": {}, "debt": {}, "healthy": {}, "checks": {},
		"error_code": {}, "cause_detail": {}, "retryable": {}, "next_action": {}, "duration_ms": {},
	}
	result := make(map[string]any)
	for key, value := range fields {
		if _, ok := allowed[key]; ok {
			result[key] = value
		}
	}
	return result
}

func cloneHubMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	body, err := json.Marshal(source)
	if err != nil {
		return map[string]any{}
	}
	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		return map[string]any{}
	}
	return result
}

// HubAdmissionRequest identifies one durable mutation boundary. It contains
// metadata only and deliberately has no work payload field.
type HubAdmissionRequest struct {
	Operation string
	HostID    string
	RequestID string
}

// HubAdmissionHooks keep resource preflight ahead of the durable commit.
type HubAdmissionHooks struct {
	Preflight func(context.Context, HubAdmissionRequest) error
	Commit    func(context.Context, HubAdmissionRequest) (uint64, error)
}

// HubAdmissionResult reports success only after the caller's durable commit.
type HubAdmissionResult struct {
	Accepted bool
	Revision uint64
}

type hubAdmissionRefusalError struct{ cause error }

func (failure *hubAdmissionRefusalError) Error() string { return failure.cause.Error() }
func (failure *hubAdmissionRefusalError) Unwrap() error { return failure.cause }

// Accepted reports that resource preflight failed before acceptance.
func (*hubAdmissionRefusalError) Accepted() bool { return false }

// Retryable reports that the caller may retry after resources recover.
func (*hubAdmissionRefusalError) Retryable() bool { return true }

// AdmitHubWork fails before acceptance on resource pressure and reports
// success only after the caller-provided durable mutation commits.
func AdmitHubWork(ctx context.Context, request HubAdmissionRequest, hooks HubAdmissionHooks) (HubAdmissionResult, error) {
	if strings.TrimSpace(request.Operation) == "" || strings.TrimSpace(request.HostID) == "" || strings.TrimSpace(request.RequestID) == "" {
		return HubAdmissionResult{}, errors.New("hub admission requires operation, host, and request identity")
	}
	if hooks.Preflight == nil || hooks.Commit == nil {
		return HubAdmissionResult{}, errors.New("hub admission requires preflight and durable commit hooks")
	}
	if err := hooks.Preflight(ctx, request); err != nil {
		return HubAdmissionResult{}, &hubAdmissionRefusalError{cause: err}
	}
	revision, err := hooks.Commit(ctx, request)
	if err != nil {
		return HubAdmissionResult{}, err
	}
	if revision == 0 {
		return HubAdmissionResult{}, errors.New("hub durable commit returned an empty revision")
	}
	return HubAdmissionResult{Accepted: true, Revision: revision}, nil
}
