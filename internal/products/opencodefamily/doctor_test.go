package opencodefamily

import (
	"context"
	"net/http"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestDoctorFailsClosedOnVersionAndFeatureDrift(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "GET /doc":
			_, _ = response.Write([]byte(`{"paths":{"/session":{},"/event":{},"/config/providers":{}}}`))
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_doctor"}`))
		case "GET /session/ses_doctor":
			_, _ = response.Write([]byte(`{"id":"ses_doctor"}`))
		case "DELETE /session/ses_doctor":
			_, _ = response.Write([]byte("true"))
		case "GET /config/providers":
			_, _ = response.Write([]byte(`{"providers":[{"id":"opencode"}],"default":{"opencode":"free"}}`))
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	servers := &testServerManager{client: client}
	version := "1.18.25"
	doctor, err := NewDoctorProbe(DoctorConfig{
		ProductID: "opencode", Executable: "opencode", TestedVersion: "1.18.25", Dialect: DialectOpenCode,
		WorkDir: "/work/project", Servers: servers, RequiredRoutes: []string{"/session", "/event", "/config/providers"},
		RunVersion:       func(context.Context, string) (string, error) { return version, nil },
		CheckIntegration: func(context.Context) (bool, string, error) { return true, "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: "opencode", ExecutablePath: "/opt/opencode", Depth: productruntime.ProbeIntegration})
	if err != nil || report.State != productruntime.ProbeReady || !report.Features["session-round-trip"] || !report.Features["integration"] {
		t.Fatalf("ready report = %#v, %v", report, err)
	}
	version = "1.18.26"
	report, err = doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: "opencode", ExecutablePath: "/opt/opencode", Depth: productruntime.ProbeFeature})
	if err != nil || report.State != productruntime.ProbeIncompatible {
		t.Fatalf("version drift report = %#v, %v", report, err)
	}
}
