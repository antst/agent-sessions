package codebuddy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestDoctorPinsOpenAPIJobRoundTripAndNeverPassesTencentCell(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	supervisor.handler = doctorNativeHandler
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	config.Commands = CommandRunnerFunc(func(context.Context, string, ...string) (CommandResult, error) {
		return CommandResult{Stdout: PinnedVersion + "\n"}, nil
	})
	doctor, err := NewDoctorProbe(config, productruntime.HostDeps{OwnedProcesses: supervisor})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil || report.State != productruntime.ProbeReady || report.NativeVersion != PinnedVersion {
		t.Fatalf("report = %#v, %v", report, err)
	}
	for _, feature := range []string{"openapi", "peer", "lane", "parent", "job-respawn", "job-archive", "deferred-session-binding", "offline-job-roundtrip"} {
		if !report.Features[feature] {
			t.Fatalf("feature %q was not ready: %#v", feature, report.Features)
		}
	}
	if report.Features["tencent-model-turn"] {
		t.Fatal("offline doctor marked Tencent model-turn acceptance passed")
	}
	supervisor.native.mu.Lock()
	requests := append([]string(nil), supervisor.native.requests...)
	supervisor.native.mu.Unlock()
	for _, request := range requests {
		if strings.Contains(request, "/api/v1/runs") {
			t.Fatalf("doctor used the pinned binary's stale /runs contract: %#v", requests)
		}
	}
	command := supervisor.commands[0]
	if argumentValue(command.Args, "--session-id") != "" {
		t.Fatalf("doctor invented a native session flag: %#v", command.Args)
	}
	for _, variable := range command.Env {
		if variable.Name == SessionIDEnv {
			t.Fatalf("doctor invented a native session environment value: %#v", command.Env)
		}
	}
	if !strings.Contains(report.Detail.String(), "pending") || !strings.Contains(report.Detail.String(), "experimental") {
		t.Fatalf("doctor detail = %q", report.Detail.String())
	}
}

func TestDoctorOpenAPIDriftIsIncompatibleNotReady(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/openapi.json" {
			writeJSON(response, http.StatusOK, map[string]any{"info": map[string]any{"title": OpenAPITitle, "version": "2.144.0"}, "paths": map[string]any{}})
			return
		}
		doctorNativeHandler(native, response, request)
	}
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	config.Commands = CommandRunnerFunc(func(context.Context, string, ...string) (CommandResult, error) {
		return CommandResult{Stdout: PinnedVersion}, nil
	})
	doctor, _ := NewDoctorProbe(config, productruntime.HostDeps{OwnedProcesses: supervisor})
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeFeature})
	if err != nil || report.State != productruntime.ProbeIncompatible || report.Features["openapi"] {
		t.Fatalf("drift report = %#v, %v", report, err)
	}
}

func TestDoctorNeverDeletesBaselineJobNamedByHostileDispatchResponse(t *testing.T) {
	supervisor := newFakeOwnedSupervisor()
	baseline := AgentJob{
		ID: "job-1", SessionID: "user-session", State: "working", Name: "agent-sessions-offline-doctor",
		Cwd: "/work", StartedAt: 1, UpdatedAt: 1,
	}
	supervisor.handler = func(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/jobs":
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"jobs": []AgentJob{baseline}}})
			return
		case "POST /api/v1/jobs":
			var body DispatchJobRequest
			_ = json.NewDecoder(request.Body).Decode(&body)
			hostile := baseline
			hostile.Name, hostile.Cwd = body.Name, body.Cwd
			writeJSON(response, http.StatusOK, map[string]any{"data": hostile})
			return
		}
		defaultNativeHandler(native, response, request)
	}
	driver, err := NewLaneDriver(codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{}), productruntime.HostDeps{
		Generation: 15, OwnedProcesses: supervisor,
		Receipts: memoryReceiptReader{values: map[string][]byte{"unused": []byte("unused")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "doctor-cleanup", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := driver.lanes[reference.LaneID]
	if err := probeOfflineJobRoundTrip(context.Background(), runtime.client, "/work"); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("baseline-ID response was not rejected: %v", err)
	}
	supervisor.native.mu.Lock()
	deleteCount := supervisor.native.deleteCount
	supervisor.native.mu.Unlock()
	if deleteCount != 0 {
		t.Fatalf("doctor deleted a pre-existing user job %d times", deleteCount)
	}
	if err := driver.Archive(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
}

func doctorNativeHandler(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
	switch request.Method + " " + request.URL.Path {
	case "GET /api/v1/health":
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"status": "ok", "pid": 4001}})
	case "GET /api/openapi.json":
		paths := map[string]any{}
		for path, methods := range requiredOpenAPIOperations {
			operationMap := map[string]any{}
			for method, operation := range methods {
				shape := map[string]any{"operationId": operation}
				if path == "/api/v1/jobs" && method == "post" {
					shape["requestBody"] = map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
						"type": "object", "required": []string{"prompt"}, "properties": map[string]any{
							"prompt": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"},
							"permissionMode": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"},
						},
					}}}}
				}
				operationMap[method] = shape
			}
			paths[path] = operationMap
		}
		components := map[string]any{"schemas": map[string]any{
			"AgentJob": map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string"}, "sessionId": map[string]any{"type": "string"},
				"name": map[string]any{"type": "string"}, "cwd": map[string]any{"type": "string"},
			}},
			"AgentJobList": map[string]any{"type": "object", "required": []string{"jobs"}, "properties": map[string]any{
				"jobs": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/AgentJob"}},
			}},
		}}
		writeJSON(response, http.StatusOK, map[string]any{
			"info": map[string]any{"title": OpenAPITitle, "version": PinnedVersion}, "paths": paths, "components": components,
		})
	default:
		defaultNativeHandler(native, response, request)
	}
}
