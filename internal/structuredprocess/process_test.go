//go:build linux || darwin

package structuredprocess

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/productruntime"
)

func TestSupervisorRunsFramedChildAndReturnsNativeExit(t *testing.T) {
	supervisor, err := NewSupervisor(Options{TerminationGrace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })

	process, err := supervisor.StartProcess(context.Background(), productruntime.NativeCommand{
		Path: "/bin/sh",
		Args: []string{"-c", `IFS= read -r line; printf '%s\n' "$line"; exit 7`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if process.Ref().Process.PID <= 1 {
		t.Fatalf("child pid = %d", process.Ref().Process.PID)
	}
	frame := []byte(`{"ping":true}`)
	if err := process.WriteFrame(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	got, err := process.ReadFrame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(frame) {
		t.Fatalf("frame = %q, want %q", got, frame)
	}
	exit, err := process.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.ExitLike != 7 || exit.Signal != "" {
		t.Fatalf("exit = %#v", exit)
	}
	if err := process.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorPassesWorkingDirectoryAndEnvironment(t *testing.T) {
	supervisor, err := NewSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	workingDirectory := t.TempDir()
	process, err := supervisor.StartProcess(context.Background(), productruntime.NativeCommand{
		Path: "/bin/sh",
		Args: []string{"-c", `printf '%s|%s|%s\n' "$PLAIN" "$SECRET" "$PWD"`},
		Cwd:  workingDirectory,
		Env:  []productruntime.EnvVar{{Name: "PLAIN", Value: "visible"}},
		SensitiveEnv: []productruntime.SensitiveEnvVar{{
			Name:  "SECRET",
			Value: productruntime.NewSensitiveValue("hidden"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := process.ReadFrame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("visible|hidden|%s", filepath.Clean(workingDirectory))
	if string(frame) != want {
		t.Fatalf("environment frame = %q, want %q", frame, want)
	}
	if exit, err := process.Wait(context.Background()); err != nil || exit.ExitLike != 0 {
		t.Fatalf("wait = %#v, %v", exit, err)
	}
}

func TestSupervisorCleanupStopsChildAndForgetsIt(t *testing.T) {
	supervisor, err := NewSupervisor(Options{TerminationGrace: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	process, err := supervisor.StartProcess(context.Background(), productruntime.NativeCommand{
		Path: "/bin/sh",
		Args: []string{"-c", `while :; do sleep 1; done`},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.Cleanup(cleanupCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Wait(context.Background(), process.Ref()); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("wait after cleanup = %v, want ErrNotOwned", err)
	}
}

func TestSupervisorCloseRejectsNewChildren(t *testing.T) {
	supervisor, err := NewSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = supervisor.StartProcess(context.Background(), productruntime.NativeCommand{Path: "/bin/true"})
	if !errors.Is(err, ErrSupervisorClosed) {
		t.Fatalf("start after close = %v, want ErrSupervisorClosed", err)
	}
}
