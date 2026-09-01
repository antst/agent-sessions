package pifamily

import (
	"reflect"
	"testing"
)

func TestClosedQuirkTablePinsRealProtocolDifferences(t *testing.T) {
	pi, err := QuirksFor(PiProductID)
	if err != nil {
		t.Fatal(err)
	}
	omp, err := QuirksFor(OMPProductID)
	if err != nil {
		t.Fatal(err)
	}
	if pi.TestedVersion != PiTestedVersion || pi.ReadyStrategy != ReadyByStateProbe || pi.TerminalEvent != "agent_settled" || pi.NativeSteerFraming {
		t.Fatalf("unexpected Pi row: %+v", pi)
	}
	if omp.TestedVersion != OMPTestedVersion || omp.ReadyStrategy != ReadyByEvent || omp.TerminalEvent != "agent_end" || !omp.NativeSteerFraming {
		t.Fatalf("unexpected OMP row: %+v", omp)
	}
	if got := pi.extensionArguments("/managed/extension.mjs"); !reflect.DeepEqual(got, []string{"--extension", "/managed/extension.mjs"}) {
		t.Fatalf("Pi extension args = %q", got)
	}
	if got := omp.extensionArguments("/managed/extension.mjs"); !reflect.DeepEqual(got, []string{"--extension=/managed/extension.mjs"}) {
		t.Fatalf("OMP extension args = %q", got)
	}
	if _, err := QuirksFor("future-fork"); err == nil {
		t.Fatal("unknown family member did not fail closed")
	}
	mutated := pi
	mutated.TerminalEvent = "agent_end"
	if err := mutated.Validate(); err == nil {
		t.Fatal("manufactured quirk row was accepted")
	}
}

func TestReservedNativeArgumentsFailClosed(t *testing.T) {
	for _, argument := range []string{"--mode", "--mode=rpc", "--session", "--session-id=x", "--extension=/tmp/x", "--approval-mode=yolo", "--tools", "--"} {
		if !reservedArgument(argument) {
			t.Errorf("reservedArgument(%q) = false", argument)
		}
	}
}
