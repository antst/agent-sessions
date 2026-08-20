//go:build darwin

package procinfo

import (
	"os"
	"testing"
)

func TestListIncludesCurrentProcess(t *testing.T) {
	processes, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if process.PID == os.Getpid() {
			if process.Status != Known || process.Start == "" || process.StrongStart == "" {
				t.Fatalf("current process identity = %+v", process)
			}
			return
		}
	}
	t.Fatal("current process is absent from Darwin process snapshot")
}
