package daemon

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestHostRuntimeRecordSchemaRoundTrip(t *testing.T) {
	want := validHostRuntimeRecord()
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal host runtime record: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode host runtime fields: %v", err)
	}
	gotKeys := make([]string, 0, len(fields))
	for key := range fields {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"committed_at", "control_endpoint", "generation", "host_id", "host_name",
		"pid", "proc_start", "runtime_identity", "runtime_version",
		"schema_version", "service_manager", "service_unit", "started_at", "state",
		"state_revision", "strong_start",
	}
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("host runtime JSON fields = %v, want %v", gotKeys, wantKeys)
	}

	var got HostRuntimeRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal host runtime record: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate round-tripped host runtime record: %v", err)
	}
}

func TestHostRuntimeRecordRejectsUnsupportedOrIncompleteAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*HostRuntimeRecord){
		"unsupported schema":      func(record *HostRuntimeRecord) { record.SchemaVersion++ },
		"zero generation":         func(record *HostRuntimeRecord) { record.Generation = 0 },
		"zero revision":           func(record *HostRuntimeRecord) { record.StateRevision = 0 },
		"missing runtime version": func(record *HostRuntimeRecord) { record.RuntimeVersion = "" },
		"missing runtime id":      func(record *HostRuntimeRecord) { record.RuntimeIdentity = "" },
		"missing host id":         func(record *HostRuntimeRecord) { record.HostID = "" },
		"zero pid":                func(record *HostRuntimeRecord) { record.PID = 0 },
		"missing process start":   func(record *HostRuntimeRecord) { record.ProcStart = "" },
		"missing strong start":    func(record *HostRuntimeRecord) { record.StrongStart = "" },
		"relative endpoint":       func(record *HostRuntimeRecord) { record.ControlEndpoint = "runtime/daemon.sock" },
		"unknown manager":         func(record *HostRuntimeRecord) { record.ServiceManager = "hand-rolled" },
		"missing service unit":    func(record *HostRuntimeRecord) { record.ServiceUnit = "" },
		"zero started at":         func(record *HostRuntimeRecord) { record.StartedAt = 0 },
		"unknown state":           func(record *HostRuntimeRecord) { record.State = HostRuntimeState("running-ish") },
		"ready uncommitted":       func(record *HostRuntimeRecord) { record.CommittedAt = 0 },
		"systemd unit mismatch":   func(record *HostRuntimeRecord) { record.ServiceUnit = "net.antst.agent-sessions" },
		"launchd unit mismatch": func(record *HostRuntimeRecord) {
			record.ServiceManager = "launchd-user"
			record.ServiceUnit = "agent-sessions.service"
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := validHostRuntimeRecord()
			mutate(&record)
			if err := record.Validate(); err == nil {
				t.Fatalf("invalid host runtime record unexpectedly validated: %+v", record)
			}
		})
	}
}

func TestHostRuntimeRecordAcceptsLifecycleStates(t *testing.T) {
	for _, state := range []HostRuntimeState{
		HostRuntimeStarting,
		HostRuntimeRecovering,
		HostRuntimeReady,
		HostRuntimeStopping,
		HostRuntimeDebt,
	} {
		t.Run(string(state), func(t *testing.T) {
			record := validHostRuntimeRecord()
			record.State = state
			if state == HostRuntimeStarting || state == HostRuntimeRecovering {
				record.CommittedAt = 0
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("validate state %q: %v", state, err)
			}
		})
	}
}

func TestHostRuntimeRecordAcceptsSupportedUserServiceManagers(t *testing.T) {
	for manager, unit := range map[string]string{
		"systemd-user": "agent-sessions.service",
		"launchd-user": "net.antst.agent-sessions",
	} {
		t.Run(manager, func(t *testing.T) {
			record := validHostRuntimeRecord()
			record.ServiceManager = manager
			record.ServiceUnit = unit
			if err := record.Validate(); err != nil {
				t.Fatalf("validate %s host runtime record: %v", manager, err)
			}
		})
	}
}

func validHostRuntimeRecord() HostRuntimeRecord {
	return HostRuntimeRecord{
		SchemaVersion:   HostRuntimeSchemaVersion,
		Generation:      7,
		RuntimeVersion:  "0.3.0",
		RuntimeIdentity: "sha256:runtime",
		HostID:          "host-0123456789abcdef",
		HostName:        "builder",
		PID:             4242,
		ProcStart:       "123456789",
		StrongStart:     "boot-id:123456789",
		ControlEndpoint: "/tmp/agent-sessions-1000/daemon.sock",
		ServiceManager:  "systemd-user",
		ServiceUnit:     "agent-sessions.service",
		StartedAt:       1_787_600_000_000,
		CommittedAt:     1_787_600_000_100,
		State:           HostRuntimeReady,
		StateRevision:   11,
	}
}
