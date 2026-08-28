package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/diagnostics"
	sharedfederation "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func renderHostLog(kind diagnostics.OutputKind, event string, fields map[string]any) ([]byte, error) {
	return diagnostics.Render(kind, event, fields)
}

func renderHostStatus(fields map[string]any, machine bool) ([]byte, error) {
	return renderHostProjection("host.status", fields, machine)
}

func renderHostDoctor(fields map[string]any, machine bool) ([]byte, error) {
	return renderHostProjection("host.doctor", fields, machine)
}

func renderHostProjection(event string, fields map[string]any, machine bool) ([]byte, error) {
	body, err := diagnostics.Render(diagnostics.OutputNormal, event, fields)
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
		value, err := json.Marshal(envelope.Metadata[key])
		if err != nil {
			return nil, fmt.Errorf("render host projection field %s: %w", key, err)
		}
		output.WriteByte(' ')
		output.WriteString(key)
		output.WriteByte('=')
		output.Write(value)
	}
	return []byte(output.String()), nil
}

// HostStatusProjection is the stable metadata-only host status shape.
type HostStatusProjection struct {
	RuntimeVersion  string           `json:"runtime_version"`
	RuntimeIdentity string           `json:"runtime_identity"`
	Generation      uint64           `json:"generation"`
	PID             int              `json:"pid"`
	ProcStart       string           `json:"proc_start"`
	Endpoint        string           `json:"endpoint"`
	Service         map[string]any   `json:"service"`
	Products        map[string]any   `json:"products"`
	Attachments     int              `json:"attachments"`
	Lanes           int              `json:"lanes"`
	Federation      map[string]any   `json:"federation"`
	Migration       map[string]any   `json:"migration"`
	Debt            []map[string]any `json:"debt"`
}

// StatusProjection snapshots the current in-process host authority.
func (runtime *Runtime) StatusProjection() HostStatusProjection {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	federationComponent := runtime.federation
	products := make(map[string]any, len(productcatalog.Catalog().Products))
	for _, product := range productcatalog.Catalog().Products {
		state := "not_installed"
		executable := runtime.options.Configuration.ProductOverrides[product.ID].Executable
		if executable == "" {
			executable, _ = exec.LookPath(product.ID)
		}
		if executable != "" {
			if info, err := os.Lstat(executable); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				state = "installed_unready"
			}
		}
		products[product.ID] = map[string]any{"state": state}
	}
	return HostStatusProjection{
		RuntimeVersion: runtime.options.RuntimeVersion, RuntimeIdentity: runtime.options.RuntimeIdentity,
		Generation: runtime.generation, PID: runtime.options.PID, ProcStart: runtime.options.ProcStart,
		Endpoint: runtime.options.Paths.ControlEndpoint,
		Service:  map[string]any{"manager": runtime.options.ServiceManager, "unit": runtime.options.ServiceUnit},
		Products: products, Federation: hostFederationStatus(runtime.options.Configuration, federationComponent),
		Migration: map[string]any{"state": "none"}, Debt: []map[string]any{},
	}
}

// HostDoctorCheck describes one bounded, cause-specific readiness check.
type HostDoctorCheck struct {
	ID         string `json:"id"`
	Healthy    bool   `json:"healthy"`
	ErrorCode  string `json:"error_code,omitempty"`
	NextAction string `json:"next_action,omitempty"`
}

// HostDoctorProjection aggregates read-only host readiness checks.
type HostDoctorProjection struct {
	Healthy bool              `json:"healthy"`
	Checks  []HostDoctorCheck `json:"checks"`
}

// DoctorProjection evaluates readiness without starting or mutating a service.
func (runtime *Runtime) DoctorProjection() HostDoctorProjection {
	runtime.mu.RLock()
	ready := runtime.admission == AdmissionReady
	identity := runtime.options.RuntimeIdentity != "" && runtime.options.ProcStart != ""
	service := runtime.options.ServiceManager != "" && runtime.options.ServiceUnit != ""
	configuration := runtime.options.Configuration
	federationComponent := runtime.federation
	runtimeVersion, runtimeIdentity := runtime.options.RuntimeVersion, runtime.options.RuntimeIdentity
	runtime.mu.RUnlock()
	checks := []HostDoctorCheck{
		doctorCheck("service_manager", service, "service_unavailable", daemonInspectionCommand()),
		doctorCheck("runtime_identity", identity, "identity_incomplete", daemonInspectionCommand()),
		doctorCheck("state_schema", ready, "runtime_not_ready", daemonInspectionCommand()),
		doctorCheck("product_inventory", true, "", ""),
		hostFederationDoctorCheck(configuration, federationComponent, runtimeVersion, runtimeIdentity),
		doctorCheck("lifecycle_debt", true, "", ""),
	}
	healthy := true
	for _, check := range checks {
		healthy = healthy && check.Healthy
	}
	return HostDoctorProjection{Healthy: healthy, Checks: checks}
}

func hostFederationStatus(configuration DaemonConfig, component *federationComponent) map[string]any {
	state := FederationStateRecord{}
	stateName := string(sharedfederation.AgentDisabled)
	if component != nil {
		state = component.snapshot()
		stateName = state.State
	} else if configuration.HubAddress != "" {
		stateName = "not_recovered"
	}
	return map[string]any{
		"configured":                  configuration.HubAddress != "",
		"host_id":                     configuration.HostID,
		"host_name":                   configuration.HostName,
		"hub_address":                 configuration.HubAddress,
		"remote_lanes_enabled":        configuration.RemoteLanesEnabled,
		"state":                       stateName,
		"protocol_version":            state.ProtocolVersion,
		"connection_generation":       state.ConnectionGeneration,
		"advertised_runtime_version":  state.AdvertisedRuntimeVersion,
		"advertised_runtime_identity": state.AdvertisedRuntimeIdentity,
		"advertised_products":         append([]string{}, state.AdvertisedProducts...),
		"advertised_capabilities":     append([]string{}, state.AdvertisedCapabilities...),
		"remote_roster_revision":      state.RemoteRosterRevision,
		"last_connected_at":           state.LastConnectedAt,
		"last_error_code":             state.LastErrorCode,
	}
}

func hostFederationDoctorCheck(
	configuration DaemonConfig,
	component *federationComponent,
	runtimeVersion string,
	runtimeIdentity string,
) HostDoctorCheck {
	if configuration.HubAddress == "" {
		return doctorCheck("federation_protocol", true, "", "")
	}
	if component == nil {
		return doctorCheck(
			"federation_protocol", false, "federation_not_recovered",
			"restart the host service to recover the configured hub connection",
		)
	}
	state := component.snapshot()
	if state.ProtocolVersion != sharedfederation.ProtocolVersion ||
		state.State == string(sharedfederation.AgentIncompatible) || state.LastErrorCode == "protocol_mismatch" {
		return doctorCheck(
			"federation_protocol", false, "federation_protocol_mismatch",
			"install host and hub releases with matching federation protocol versions, then retry",
		)
	}
	if state.HostID != configuration.HostID || state.HostName != configuration.HostName ||
		state.HubAddress != configuration.HubAddress || state.AdvertisedRuntimeVersion != runtimeVersion ||
		state.AdvertisedRuntimeIdentity != runtimeIdentity {
		return doctorCheck(
			"federation_protocol", false, "federation_identity_mismatch",
			"restart the host service to republish the configured host and runtime identity",
		)
	}
	if state.State != string(sharedfederation.AgentConnected) {
		return doctorCheck(
			"federation_protocol", false, "federation_unavailable",
			"verify the configured hub address and hub service, then retry",
		)
	}
	return doctorCheck("federation_protocol", true, "", "")
}

func doctorCheck(id string, healthy bool, code, action string) HostDoctorCheck {
	if healthy {
		return HostDoctorCheck{ID: id, Healthy: true}
	}
	return HostDoctorCheck{ID: id, Healthy: false, ErrorCode: code, NextAction: action}
}
