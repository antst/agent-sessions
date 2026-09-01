//go:build linux || darwin

package structuredprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const (
	processHelperModeEnv  = "AGENT_SESSIONS_STRUCTURED_PROCESS_HELPER"
	processHelperChildEnv = "AGENT_SESSIONS_STRUCTURED_PROCESS_CHILD"
)

func TestStructuredProcessHelper(t *testing.T) {
	mode := os.Getenv(processHelperModeEnv)
	if mode == "" {
		return
	}
	if os.Getenv(processHelperChildEnv) == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	switch mode {
	case "group":
		processGroup, err := syscall.Getpgid(os.Getpid())
		if err != nil {
			os.Exit(91)
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"pid\":%d,\"process_group\":%d}\n", os.Getpid(), processGroup)
	case "tree":
		child := exec.Command(os.Args[0], "-test.run=^TestStructuredProcessHelper$")
		child.Env = append(os.Environ(), processHelperChildEnv+"=1")
		child.Stdout, child.Stderr = io.Discard, io.Discard
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"child_pid\":%d}\n", child.Process.Pid)
	case "orphan":
		child := exec.Command(os.Args[0], "-test.run=^TestStructuredProcessHelper$")
		child.Env = append(os.Environ(), processHelperChildEnv+"=1")
		child.Stdout, child.Stderr = io.Discard, io.Discard
		if err := child.Start(); err != nil {
			os.Exit(94)
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"child_pid\":%d}\n", child.Process.Pid)
		return
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		_, _ = fmt.Fprintln(os.Stdout, "ready")
	case "exit-17":
		os.Exit(17)
	case "frame-exit":
		_, _ = fmt.Fprintln(os.Stdout, "final")
		os.Exit(0)
	case "delayed-frame":
		time.Sleep(60 * time.Millisecond)
		_, _ = fmt.Fprintln(os.Stdout, "ready")
	case "multi-frame-exit":
		for index := 0; index < 8; index++ {
			_, _ = fmt.Fprintf(os.Stdout, "frame-%d\n", index)
		}
		os.Exit(0)
	case "frame-flood":
		for index := 0; index < 64; index++ {
			_, _ = fmt.Fprintf(os.Stdout, "flood-%d\n", index)
		}
	case "queued-hang":
		for index := 0; index < 4; index++ {
			_, _ = fmt.Fprintf(os.Stdout, "queued-%d\n", index)
		}
	default:
		os.Exit(93)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestSupervisorStartsExactPrivateProcessGroupAndReturnsExitEvidence(t *testing.T) {
	supervisor := newTestSupervisor(t, 100*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("group"))
	if err != nil {
		t.Fatal(err)
	}
	ref := process.Ref()
	if ref.Process.PID <= 1 || ref.Process.Start == "" || ref.Process.StrongStart == "" {
		t.Fatalf("inexact child reference: %#v", ref)
	}
	if ref.ProcessGroup != ref.Process.PID {
		t.Fatalf("process group = %d; want private leader %d", ref.ProcessGroup, ref.Process.PID)
	}

	frame, err := process.ReadFrame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		PID          int `json:"pid"`
		ProcessGroup int `json:"process_group"`
	}
	if err := json.Unmarshal(frame, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.PID != ref.Process.PID || observed.ProcessGroup != ref.ProcessGroup {
		t.Fatalf("native identity = %#v; want %#v", observed, ref)
	}

	if err := process.Signal(context.Background(), productruntime.ProcessTerminate); err != nil {
		t.Fatal(err)
	}
	evidence, err := process.WaitEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Ref != ref || evidence.Exit.ExitLike != 143 || evidence.Exit.Signal != "terminate" || evidence.ExitedAt.Before(evidence.StartedAt) {
		t.Fatalf("exit evidence = %#v", evidence)
	}
	second, err := process.WaitEvidence(context.Background())
	if err != nil || second != evidence {
		t.Fatalf("repeated evidence = %#v, %v; want %#v", second, err, evidence)
	}
}

func TestSupervisorCancellationTerminatesWholeOwnedGroup(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	process, err := supervisor.StartProcess(ctx, helperCommand("tree"))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := process.ReadFrame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		ChildPID int `json:"child_pid"`
	}
	if err := json.Unmarshal(frame, &observed); err != nil {
		t.Fatal(err)
	}
	child, err := procinfo.CaptureIdentity(observed.ChildPID)
	if err != nil {
		t.Fatal(err)
	}
	childProcessGroup, err := syscall.Getpgid(observed.ChildPID)
	if err != nil {
		t.Fatal(err)
	}
	if childProcessGroup != process.Ref().ProcessGroup {
		t.Fatalf("descendant group = %d; want owned group %d", childProcessGroup, process.Ref().ProcessGroup)
	}

	cancel()
	if _, err := process.WaitEvidence(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForStaleIdentity(t, child)
}

func TestSupervisorCleansResidualGroupAfterLeaderExit(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := process.ReadFrame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		ChildPID int `json:"child_pid"`
	}
	if err := json.Unmarshal(frame, &observed); err != nil {
		t.Fatal(err)
	}
	child, err := procinfo.CaptureIdentity(observed.ChildPID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := process.WaitEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Exit.ExitLike != 0 {
		t.Fatalf("leader exit = %#v; want natural success", evidence.Exit)
	}
	waitForStaleIdentity(t, child)
}

func TestSupervisorCleanupEscalatesAndWorksAfterSupervisorRestart(t *testing.T) {
	owner := newTestSupervisor(t, 40*time.Millisecond)
	process, err := owner.StartProcess(context.Background(), helperCommand("ignore-term"))
	if err != nil {
		t.Fatal(err)
	}
	if frame, err := process.ReadFrame(context.Background()); err != nil || string(frame) != "ready" {
		t.Fatalf("ready frame = %q, %v", frame, err)
	}

	restarted := newTestSupervisor(t, 40*time.Millisecond)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := restarted.Cleanup(cleanupCtx, process.Ref()); err != nil {
		t.Fatal(err)
	}
	evidence, err := process.WaitEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Exit.ExitLike != 137 || evidence.Exit.Signal != "kill" {
		t.Fatalf("escalated exit = %#v", evidence.Exit)
	}
}

func TestSupervisorRejectsForgedOwnershipWithoutSignaling(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("group"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.ReadFrame(context.Background()); err != nil {
		t.Fatal(err)
	}
	forged := process.Ref()
	forged.Process.Start += "-forged"
	if err := supervisor.Signal(context.Background(), forged, productruntime.ProcessKill); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("forged signal error = %v; want ErrNotOwned", err)
	}
	if observation := procinfo.ObserveIdentity(process.Ref().Process); observation.Status != procinfo.IdentityMatches {
		t.Fatalf("forged signal affected child: %#v", observation)
	}
	if err := supervisor.Cleanup(context.Background(), process.Ref()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorReturnsNonzeroExitAsEvidenceAndWaitIsCancelable(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("exit-17"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := process.WaitEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Exit.ExitLike != 17 || evidence.Exit.Signal != "" {
		t.Fatalf("natural nonzero exit = %#v", evidence.Exit)
	}

	blocking, err := supervisor.StartProcess(context.Background(), helperCommand("group"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocking.ReadFrame(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := blocking.WaitEvidence(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded wait = %v; want deadline exceeded", err)
	}
	if err := supervisor.Cleanup(context.Background(), blocking.Ref()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorPreservesFinalFrameAfterProcessExit(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("frame-exit"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.WaitEvidence(context.Background()); err != nil {
		t.Fatal(err)
	}
	frame, err := process.ReadFrame(context.Background())
	if err != nil || string(frame) != "final" {
		t.Fatalf("final frame after exit = %q, %v", frame, err)
	}
	if _, err := process.ReadFrame(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read after final frame = %v; want EOF", err)
	}
}

func TestSupervisorWaitDoesNotRequireDrainingMultipleFinalFrames(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("multi-frame-exit"))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := process.WaitEvidence(waitCtx); err != nil {
		t.Fatalf("wait without reading frames = %v", err)
	}
	for index := 0; index < 8; index++ {
		frame, err := process.ReadFrame(context.Background())
		if err != nil || string(frame) != fmt.Sprintf("frame-%d", index) {
			t.Fatalf("queued frame %d = %q, %v", index, frame, err)
		}
	}
	if _, err := process.ReadFrame(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read after queued frames = %v; want EOF", err)
	}
}

func TestSupervisorFailsBoundedlyWhenNativeFramesOutrunConsumer(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("frame-flood"))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	evidence, err := process.WaitEvidence(waitCtx)
	if !errors.Is(err, ErrFrameBackpressure) || !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("non-reading wait = %#v, %v; want explicit protocol backpressure error", evidence, err)
	}
	if evidence.Exit.ExitLike == 0 {
		t.Fatalf("backpressured child was not terminated: %#v", evidence.Exit)
	}
	for {
		_, readErr := process.ReadFrame(context.Background())
		if errors.Is(readErr, ErrFrameBackpressure) && errors.Is(readErr, productruntime.ErrProtocol) {
			break
		}
		if readErr != nil {
			t.Fatalf("drain after backpressure = %v", readErr)
		}
	}
}

func TestSupervisorCleanupDoesNotRequireFrameConsumer(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("queued-hang"))
	if err != nil {
		t.Fatal(err)
	}
	queueDeadline := time.Now().Add(time.Second)
	for len(process.state.frames) < 4 && time.Now().Before(queueDeadline) {
		time.Sleep(time.Millisecond)
	}
	if queued := len(process.state.frames); queued != 4 {
		t.Fatalf("queued frames before cleanup = %d; want 4", queued)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.Cleanup(cleanupCtx); err != nil {
		t.Fatalf("cleanup without reading frames = %v", err)
	}
	if _, err := process.WaitEvidence(context.Background()); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		frame, err := process.ReadFrame(context.Background())
		if err != nil || string(frame) != fmt.Sprintf("queued-%d", index) {
			t.Fatalf("preserved cleanup frame %d = %q, %v", index, frame, err)
		}
	}
	if _, err := process.ReadFrame(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read after cleanup frames = %v; want EOF", err)
	}
}

func TestSupervisorCapturesVeryShortLivedChild(t *testing.T) {
	executable, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true executable is unavailable")
	}
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), productruntime.NativeCommand{Path: executable})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := process.WaitEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Ref.Process.Start == "" || evidence.Ref.Process.StrongStart == "" || evidence.Exit.ExitLike != 0 {
		t.Fatalf("short-lived child evidence = %#v", evidence)
	}
}

func TestSupervisorPrunesHeavyStateForManyCompletedChildren(t *testing.T) {
	executable, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true executable is unavailable")
	}
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	var oldestRef productruntime.OwnedProcessRef
	var latest *Process
	for index := 0; index < completedEvidenceLimit+32; index++ {
		process, err := supervisor.StartProcess(context.Background(), productruntime.NativeCommand{Path: executable})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldestRef = process.Ref()
		}
		latest = process
		if _, err := latest.WaitEvidence(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	supervisor.mu.Lock()
	active := len(supervisor.processes)
	retained := len(supervisor.completed)
	supervisor.mu.Unlock()
	if active != 0 {
		t.Fatalf("supervisor retained %d heavy completed process states", active)
	}
	if retained != completedEvidenceLimit {
		t.Fatalf("supervisor retained %d compact evidence records; want %d", retained, completedEvidenceLimit)
	}
	latestEvidence, err := latest.WaitEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := supervisor.WaitEvidence(context.Background(), latest.Ref())
	if err != nil || repeated != latestEvidence {
		t.Fatalf("repeat compact evidence = %#v, %v; want %#v", repeated, err, latestEvidence)
	}
	if _, err := supervisor.WaitEvidence(context.Background(), oldestRef); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("evicted evidence wait = %v; want ErrNotOwned", err)
	}
}

func TestProcessReadCancellationPreservesFollowingFrame(t *testing.T) {
	supervisor := newTestSupervisor(t, 50*time.Millisecond)
	process, err := supervisor.StartProcess(context.Background(), helperCommand("delayed-frame"))
	if err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := process.ReadFrame(readCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled process read = %v; want deadline exceeded", err)
	}
	frame, err := process.ReadFrame(context.Background())
	if err != nil || string(frame) != "ready" {
		t.Fatalf("frame after canceled read = %q, %v", frame, err)
	}
	if err := process.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newTestSupervisor(t *testing.T, grace time.Duration) *Supervisor {
	t.Helper()
	supervisor, err := NewSupervisor(Options{TerminationGrace: grace, MaxFrameBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return supervisor
}

func helperCommand(mode string) productruntime.NativeCommand {
	return productruntime.NativeCommand{
		Path: os.Args[0],
		Args: []string{"-test.run=^TestStructuredProcessHelper$"},
		Env:  []productruntime.EnvVar{{Name: processHelperModeEnv, Value: mode}},
	}
}

func waitForStaleIdentity(t *testing.T, identity procinfo.Identity) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if procinfo.ObserveIdentity(identity).Status == procinfo.IdentityStale {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process remained live: %#v", identity)
}

var _ productruntime.OwnedProcessSupervisor = (*Supervisor)(nil)
