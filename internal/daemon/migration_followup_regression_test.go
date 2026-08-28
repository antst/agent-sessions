package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestProductionLegacyReattestProcessObservesPIDWhenServiceIsAbsent(t *testing.T) {
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	identity := procinfo.Read(os.Getpid())
	if identity.Status != procinfo.Known {
		t.Fatalf("test process identity = %+v", identity)
	}
	candidate := LegacyRuntimeCandidate{
		ServiceManager: "systemd-user", ServiceUnit: "peer-federator-agent.service",
		ServiceStatus: "loaded", PID: os.Getpid(), ProcessStatus: "known",
		ProcStart: "stale-start", StrongStart: "stale-strong-start",
	}
	observed, err := (&productionLegacyRetirementLifecycle{}).ReattestProcess(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ServiceStatus != "absent" || observed.ProcessStatus != "known" ||
		observed.ProcStart != identity.Start || observed.StrongStart != identity.StrongStart {
		t.Fatalf("service-absent process reattestation = %+v, process identity = %+v", observed, identity)
	}
}

func TestReflectLegacyAdoptionRequestEmptyRejectsDeliveryMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LegacyAdoptionRequest)
	}{
		{name: "deliveries", mutate: func(request *LegacyAdoptionRequest) {
			request.Deliveries = []DeliveryRecord{{}}
		}},
		{name: "delivery cursors", mutate: func(request *LegacyAdoptionRequest) {
			request.DeliveryCursors = []LegacyDeliveryCursor{{}}
		}},
		{name: "delivery notices", mutate: func(request *LegacyAdoptionRequest) {
			request.DeliveryNotices = []LegacyDeliveryNotice{{}}
		}},
		{name: "preparations", mutate: func(request *LegacyAdoptionRequest) {
			request.Preparations = []LegacyPreparationRecord{{}}
		}},
		{name: "configuration", mutate: func(request *LegacyAdoptionRequest) {
			request.Configuration = &LegacyHostConfiguration{}
		}},
	}
	if !reflectLegacyAdoptionRequestEmpty(LegacyAdoptionRequest{}) {
		t.Fatal("zero adoption request was not empty")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request LegacyAdoptionRequest
			test.mutate(&request)
			if reflectLegacyAdoptionRequestEmpty(request) {
				t.Fatalf("nonempty %s were ignored", test.name)
			}
		})
	}
}
