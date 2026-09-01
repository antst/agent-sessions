package diagnostics

import (
	"strings"
	"testing"
)

func TestStatusAndDoctorHaveFixedTruthfulSchema(t *testing.T) {
	input := Input{
		RuntimeReady: true, Generation: 7, CatalogRevision: 18, ServiceState: "running",
		ReleasePresent: true, EndpointPresent: true,
		Revisions:     Revisions{Attachments: 5, Lanes: 7, Federation: 9},
		Records:       Records{Attachments: 4, ActiveAttachments: 3, Lanes: 8, ActiveLanes: 2, Turns: 12, UncollectedTurns: 1},
		ProductStates: map[string]string{"codex": "ready", "claude": "missing", "grok": "opaque raw diagnostic"},
	}
	input.Operation = "status"
	status, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if status.Schema != Schema || status.Operation != "status" || !status.Ready || len(status.Checks) != 0 || len(status.Products) != 4 {
		t.Fatalf("status report = %+v", status)
	}
	if status.Products[1].ID != "codex" || status.Products[1].Readiness != "available" {
		t.Fatalf("normalized products = %+v", status.Products)
	}
	input.Operation = "doctor"
	doctor, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Operation != "doctor" || len(doctor.Checks) != 6 || doctor.Checks[2].Status != "unavailable" {
		t.Fatalf("doctor report = %+v", doctor)
	}
}

func TestReportDropsCredentialMessageTranscriptAndArbitraryMetadataCanaries(t *testing.T) {
	canary := "SECRET_PROMPT_RESULT_TRANSCRIPT_TOKEN"
	body, err := Marshal(Input{
		Operation: "doctor", RuntimeReady: true, Generation: 1, ServiceState: canary,
		EndpointPresent: true, ProductStates: map[string]string{
			"codex": canary, canary: canary,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), canary) {
		t.Fatalf("diagnostic output leaked content canary: %s", body)
	}
	if len(body) > MaxReportBytes {
		t.Fatalf("diagnostic output size = %d", len(body))
	}
}

func TestReportRemainsBoundedWithUntrustedProductMap(t *testing.T) {
	states := map[string]string{}
	for index := 0; index < 10000; index++ {
		states[strings.Repeat("x", 64)+string(rune(index))] = strings.Repeat("y", 4096)
	}
	body, err := Marshal(Input{Operation: "status", ServiceState: "running", RuntimeReady: true, EndpointPresent: true, ProductStates: states})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxReportBytes {
		t.Fatalf("diagnostic output size = %d", len(body))
	}
}
