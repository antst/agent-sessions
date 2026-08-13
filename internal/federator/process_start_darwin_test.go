//go:build darwin

package federator

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestDarwinProcessStartMatchesPSLstart(t *testing.T) {
	started := processStart(os.Getpid())
	if started == "" {
		t.Fatal("current process has no start token")
	}
	command := exec.Command("/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid()))
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if expected := strings.TrimSpace(string(body)); started != expected {
		t.Fatalf("process start = %q, ps lstart = %q", started, expected)
	}
}
