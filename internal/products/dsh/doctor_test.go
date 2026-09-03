package dsh

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

type testCommandProbe struct{ cli, pnpm string }

func (testCommandProbe) LookPath(name string) (string, error) {
	switch name {
	case "dsh":
		return "/usr/bin/dsh", nil
	case "pnpm":
		return "/usr/bin/pnpm", nil
	default:
		return "", errors.New("missing")
	}
}

func (probe testCommandProbe) Output(_ context.Context, path string, _ []string, _ []string) ([]byte, error) {
	if path == "/usr/bin/dsh" {
		return []byte(probe.cli), nil
	}
	return []byte(probe.pnpm), nil
}

func TestDoctorRequiresExactNativeV1Tuple(t *testing.T) {
	doctor, err := NewDoctorProbe(DoctorConfig{Commands: testCommandProbe{cli: "dsh 0.1.2-rc.1\n", pnpm: "10.28.1\n"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil || report.State != productruntime.ProbeReady || !report.Features["native-v1"] || !report.Features["lane"] {
		t.Fatalf("Probe() = %#v, %v", report, err)
	}

	doctor, err = NewDoctorProbe(DoctorConfig{Commands: testCommandProbe{cli: "dsh 0.1.2-alpha.3\n", pnpm: "10.28.1\n"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err = doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil || report.State != productruntime.ProbeIncompatible || report.TupleOK == nil || *report.TupleOK {
		t.Fatalf("incompatible Probe() = %#v, %v", report, err)
	}
}
