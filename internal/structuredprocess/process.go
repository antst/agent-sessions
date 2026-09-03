package structuredprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const defaultTerminationGrace = 2 * time.Second

var (
	ErrNotOwned         = errors.New("structured process is not owned by this supervisor")
	ErrNotRunning       = errors.New("structured process is not running")
	ErrSupervisorClosed = errors.New("structured process supervisor is closed")
)

// Options configures direct child I/O and shutdown.
type Options struct {
	MaxFrameBytes    int
	TerminationGrace time.Duration
}

// Supervisor owns only the child processes it started in this daemon process.
// There is no restart recovery or completed-process cache.
type Supervisor struct {
	maxFrameBytes    int
	terminationGrace time.Duration

	mu        sync.Mutex
	closed    bool
	processes map[int]*Process
}

// Process is a direct framed-pipe handle for one child.
type Process struct {
	supervisor *Supervisor
	ref        productruntime.OwnedProcessRef
	command    *exec.Cmd
	framer     *Framer

	cleanupMu sync.Mutex
	done      chan struct{}
	exit      productruntime.ProcessExit
	waitErr   error
}

func NewSupervisor(options ...Options) (*Supervisor, error) {
	if len(options) > 1 {
		return nil, errors.New("structured process supervisor accepts at most one options value")
	}
	configuration := Options{}
	if len(options) == 1 {
		configuration = options[0]
	}
	if configuration.MaxFrameBytes == 0 {
		configuration.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if configuration.MaxFrameBytes < 1 || configuration.MaxFrameBytes > MaximumFrameBytes {
		return nil, errors.New("structured process frame limit is outside the bounded range")
	}
	if configuration.TerminationGrace == 0 {
		configuration.TerminationGrace = defaultTerminationGrace
	}
	if configuration.TerminationGrace < 0 {
		return nil, errors.New("structured process termination grace must not be negative")
	}
	return &Supervisor{
		maxFrameBytes:    configuration.MaxFrameBytes,
		terminationGrace: configuration.TerminationGrace,
		processes:        make(map[int]*Process),
	}, nil
}

func (supervisor *Supervisor) Start(ctx context.Context, native productruntime.NativeCommand) (productruntime.OwnedProcessRef, error) {
	process, err := supervisor.StartProcess(ctx, native)
	if err != nil {
		return productruntime.OwnedProcessRef{}, err
	}
	return process.Ref(), nil
}

// StartProcess starts one child with stdin/stdout JSONL pipes.
func (supervisor *Supervisor) StartProcess(ctx context.Context, native productruntime.NativeCommand) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("structured process start requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command, err := prepareCommand(native)
	if err != nil {
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open structured process stdin: %w", err)
	}
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open structured process stdout: %w", err)
	}
	command.Stdout = childStdout
	command.Stderr = io.Discard
	configureOwnedProcessGroup(command)

	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = childStdout.Close()
		return nil, ErrSupervisorClosed
	}
	if err := command.Start(); err != nil {
		supervisor.mu.Unlock()
		command.Env = nil
		_ = stdin.Close()
		_ = stdout.Close()
		_ = childStdout.Close()
		return nil, fmt.Errorf("start structured process %q: %w", native.Path, err)
	}
	command.Env = nil
	_ = childStdout.Close()

	identity := procinfo.Identity{PID: command.Process.Pid}
	if captured, captureErr := procinfo.CaptureIdentity(command.Process.Pid); captureErr == nil {
		identity = captured
	}
	framer, err := NewFramer(stdout, stdin, supervisor.maxFrameBytes)
	if err != nil {
		supervisor.mu.Unlock()
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = stdout.Close()
		_ = stdin.Close()
		return nil, err
	}
	process := &Process{
		supervisor: supervisor,
		ref: productruntime.OwnedProcessRef{
			Process:      identity,
			ProcessGroup: command.Process.Pid,
		},
		command: command,
		framer:  framer,
		done:    make(chan struct{}),
	}
	supervisor.processes[command.Process.Pid] = process
	supervisor.mu.Unlock()

	go process.reap()
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				cleanupCtx, cancel := context.WithTimeout(context.Background(), supervisor.terminationGrace+time.Second)
				defer cancel()
				_ = process.Cleanup(cleanupCtx)
			case <-process.done:
			}
		}()
	}
	return process, nil
}

func (supervisor *Supervisor) Signal(ctx context.Context, ref productruntime.OwnedProcessRef, signal productruntime.ProcessSignal) error {
	process, err := supervisor.lookup(ctx, ref)
	if err != nil {
		return err
	}
	select {
	case <-process.done:
		return ErrNotRunning
	default:
		return sendProcessGroupSignal(process.ref.ProcessGroup, signal)
	}
}

func (supervisor *Supervisor) Wait(ctx context.Context, ref productruntime.OwnedProcessRef) (productruntime.ProcessExit, error) {
	process, err := supervisor.lookup(ctx, ref)
	if err != nil {
		return productruntime.ProcessExit{}, err
	}
	return process.Wait(ctx)
}

// Close stops every child still owned by the supervisor.
func (supervisor *Supervisor) Close() error {
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.closed = true
	processes := make([]*Process, 0, len(supervisor.processes))
	for _, process := range supervisor.processes {
		processes = append(processes, process)
	}
	supervisor.mu.Unlock()

	var failures []error
	for _, process := range processes {
		ctx, cancel := context.WithTimeout(context.Background(), supervisor.terminationGrace+time.Second)
		failures = append(failures, process.Cleanup(ctx))
		cancel()
	}
	return errors.Join(failures...)
}

func (process *Process) Ref() productruntime.OwnedProcessRef { return process.ref }

func (process *Process) ReadFrame(ctx context.Context) ([]byte, error) {
	return process.framer.ReadFrame(ctx)
}

func (process *Process) WriteFrame(ctx context.Context, frame []byte) error {
	return process.framer.WriteFrame(ctx, frame)
}

func (process *Process) Signal(ctx context.Context, signal productruntime.ProcessSignal) error {
	return process.supervisor.Signal(ctx, process.ref, signal)
}

func (process *Process) Wait(ctx context.Context) (productruntime.ProcessExit, error) {
	if ctx == nil {
		return productruntime.ProcessExit{}, errors.New("structured process wait requires context")
	}
	select {
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	case <-process.done:
		return process.exit, process.waitErr
	}
}

// Cleanup terminates the child if needed and releases its pipes.
func (process *Process) Cleanup(ctx context.Context) error {
	if ctx == nil {
		return errors.New("structured process cleanup requires context")
	}
	process.cleanupMu.Lock()
	defer process.cleanupMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-process.done:
	default:
		_ = sendProcessGroupSignal(process.ref.ProcessGroup, productruntime.ProcessTerminate)
		timer := time.NewTimer(process.supervisor.terminationGrace)
		select {
		case <-process.done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			_ = sendProcessGroupSignal(process.ref.ProcessGroup, productruntime.ProcessKill)
			select {
			case <-process.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		}
	}

	_ = process.framer.Close()
	process.supervisor.forget(process)
	return process.waitErr
}

func (process *Process) reap() {
	waitErr := process.command.Wait()
	process.exit, process.waitErr = classifyProcessExit(process.command.ProcessState, waitErr)
	close(process.done)
}

func (supervisor *Supervisor) lookup(ctx context.Context, ref productruntime.OwnedProcessRef) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("structured process operation requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	process := supervisor.processes[ref.Process.PID]
	if process == nil || process.ref != ref {
		return nil, ErrNotOwned
	}
	return process, nil
}

func (supervisor *Supervisor) forget(process *Process) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.processes[process.ref.Process.PID] == process {
		delete(supervisor.processes, process.ref.Process.PID)
	}
}

func prepareCommand(native productruntime.NativeCommand) (*exec.Cmd, error) {
	if native.Path == "" || strings.IndexByte(native.Path, 0) >= 0 {
		return nil, errors.New("structured process executable path is invalid")
	}
	replacements := make(map[string]string, len(native.Env)+len(native.SensitiveEnv))
	for _, variable := range native.Env {
		if err := validateEnvironmentVariable(variable.Name, variable.Value); err != nil {
			return nil, err
		}
		replacements[variable.Name] = variable.Value
	}
	for _, variable := range native.SensitiveEnv {
		value := variable.Value.Reveal()
		if err := validateEnvironmentVariable(variable.Name, value); err != nil {
			return nil, err
		}
		replacements[variable.Name] = value
	}
	for _, argument := range native.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return nil, errors.New("structured process argument contains NUL")
		}
	}
	if strings.IndexByte(native.Cwd, 0) >= 0 {
		return nil, errors.New("structured process working directory contains NUL")
	}
	command := exec.Command(native.Path, native.Args...) //nolint:gosec // executable and argv are supplied by the product driver.
	command.Dir = native.Cwd
	command.Env = envutil.Replace(os.Environ(), replacements)
	return command, nil
}

func validateEnvironmentVariable(name, value string) error {
	if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
		return errors.New("structured process environment variable is invalid")
	}
	return nil
}

var _ productruntime.OwnedProcessSupervisor = (*Supervisor)(nil)
