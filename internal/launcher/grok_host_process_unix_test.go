//go:build linux || darwin

package launcher

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const (
	grokHostProcessGroupHelperEnv  = "AGENT_SESSIONS_TEST_GROK_HOST_PROCESS_GROUP"
	grokHostProcessGroupRuntimeEnv = "AGENT_SESSIONS_TEST_GROK_HOST_RUNTIME"
	grokHostProcessGroupRootEnv    = "AGENT_SESSIONS_TEST_GROK_HOST_ROOT"
)

func TestGrokHostSurvivesTUIProcessGroupExit(t *testing.T) {
	if os.Getenv(grokHostProcessGroupHelperEnv) == "1" {
		runGrokHostProcessGroupHelper(t)
		return
	}

	root := t.TempDir()
	runtimePath := filepath.Join(root, "fake-agent-session-runtime")
	ready := fmt.Sprintf(
		`{"ready":true,"session_id":%q,"cwd":%q,"leader_socket":%q,"control_socket":%q}`+"\n",
		testGrokSessionID, root, filepath.Join(root, "leader.sock"), filepath.Join(root, "control.sock"),
	)
	script := "#!/bin/sh\nprintf '%s' \"$AGENT_SESSIONS_TEST_GROK_HOST_READY\"\nexec sleep 30\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestGrokHostSurvivesTUIProcessGroupExit$")
	helper.Env = append(os.Environ(),
		grokHostProcessGroupHelperEnv+"=1",
		grokHostProcessGroupRuntimeEnv+"="+runtimePath,
		grokHostProcessGroupRootEnv+"="+root,
		"AGENT_SESSIONS_TEST_GROK_HOST_READY="+ready,
	)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := make(chan error, 1)
	go func() { helperDone <- helper.Wait() }()
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		// The helper installs its signal handler before starting the host. Group
		// termination plus stdin EOF therefore makes it reap the host on every
		// parent-side assertion or parse failure, including failures before the
		// host PID reaches stdout.
		_ = syscall.Kill(-helper.Process.Pid, syscall.SIGTERM)
		_ = stdin.Close()
		select {
		case <-helperDone:
		case <-time.After(3 * time.Second):
			_ = helper.Process.Kill()
			<-helperDone
		}
	}()
	reader := bufio.NewReader(stdout)

	var hostPID, hostProcessGroup int
	if _, err := fmt.Fscan(reader, &hostPID, &hostProcessGroup); err != nil {
		t.Fatal(err)
	}
	if hostProcessGroup != hostPID || hostProcessGroup == helper.Process.Pid {
		t.Fatalf("grok-host pid/group = %d/%d, TUI group = %d", hostPID, hostProcessGroup, helper.Process.Pid)
	}

	if err := syscall.Kill(-helper.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var state string
	if _, err := fmt.Fscan(reader, &state); err != nil {
		t.Fatal(err)
	}
	if state != "alive" {
		t.Fatalf("grok-host did not survive TUI process-group exit: %s", state)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-helperDone:
		cleaned = true
		if err != nil {
			t.Fatalf("process-group helper cleanup: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process-group helper did not reap its detached host")
	}
}

func runGrokHostProcessGroupHelper(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	root := os.Getenv(grokHostProcessGroupRootEnv)
	host, err := startGrokHost(os.Getenv(grokHostProcessGroupRuntimeEnv), grokHostRequest{
		SessionID: testGrokSessionID, Cwd: root, OwnerPID: os.Getpid(),
		OwnerProcStart: "test-owner", LaunchToken: "test-token", PermissionMode: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	processGroup, err := syscall.Getpgid(host.process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%d %d\n", host.process.Pid, processGroup)
	<-signals
	time.Sleep(100 * time.Millisecond)
	state := "alive"
	if err := syscall.Kill(host.process.Pid, 0); err != nil {
		state = "dead"
	}
	fmt.Println(state)
	_, _ = os.Stdin.Read(make([]byte, 1))
	if state == "alive" {
		_ = host.process.Kill()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(host.process.Pid, 0); err == syscall.ESRCH {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("detached grok-host %d was not reaped", host.process.Pid)
	}
}
