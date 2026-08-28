//go:build linux

package procinfo

import "testing"

func TestReconcileListedProcessOmitsOnlyProvenExitAndFailsClosedOtherwise(t *testing.T) {
	tests := []struct {
		name       string
		initial    Info
		reobserved Info
		present    bool
		wantError  bool
	}{
		{name: "known", initial: Info{Status: Known, State: "S", Start: "1", StrongStart: "1"}, present: true},
		{name: "exited during enumeration", initial: Info{Status: Unknown}, reobserved: Info{Status: Absent}},
		{name: "became observable", initial: Info{Status: Unknown}, reobserved: Info{Status: Known, State: "R", Start: "2", StrongStart: "2"}, present: true},
		{name: "remains unknown", initial: Info{Status: Unknown}, reobserved: Info{Status: Unknown}, wantError: true},
		{name: "zombie", initial: Info{Status: Known, State: "Z"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations := 0
			process, present, err := reconcileListedProcess(42, test.initial, func(pid int) Info {
				observations++
				if pid != 42 {
					t.Fatalf("re-observed PID = %d", pid)
				}
				return test.reobserved
			})
			if (err != nil) != test.wantError || present != test.present {
				t.Fatalf("reconcile = %+v, present=%v, err=%v", process, present, err)
			}
			wantObservations := 0
			if test.initial.Status == Unknown {
				wantObservations = 1
			}
			if observations != wantObservations {
				t.Fatalf("re-observations = %d, want %d", observations, wantObservations)
			}
			if present && (process.PID != 42 || process.Status != Known) {
				t.Fatalf("included process = %+v", process)
			}
		})
	}
}
