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

const (
	defaultTerminationGrace = 2 * time.Second
	processExitPollInterval = 10 * time.Millisecond
)

var (
	ErrNotOwned         = fmt.Errorf("%w: structured process reference is not exactly owned", productruntime.ErrCleanupDebt)
	ErrNotRunning       = errors.New("structured process is not running")
	ErrSupervisorClosed = errors.New("structured process supervisor is closed")
)

// Options bounds framed I/O and graceful process-group termination.
type Options struct {
	MaxFrameBytes    int
	TerminationGrace time.Duration
}

// ExitEvidence is the immutable result of reaping one exact owned child. A
// nonzero native status is evidence, not a Go transport error.
type ExitEvidence struct {
	Ref       productruntime.OwnedProcessRef
	Exit      productruntime.ProcessExit
	StartedAt time.Time
	ExitedAt  time.Time
}

// Supervisor creates private process groups and retains the exact identity
// needed to signal only those groups. Cleanup can also consume an exact ref
// after a supervisor restart; it re-attests the live leader and group before
// the first destructive signal.
type Supervisor struct {
	maxFrameBytes    int
	terminationGrace time.Duration

	mu        sync.Mutex
	closed    bool
	processes map[int]*processState
}

type processState struct {
	ref       productruntime.OwnedProcessRef
	command   *exec.Cmd
	framer    *Framer
	startedAt time.Time

	frames    chan frameResult
	frameStop chan struct{}
	readDone  chan struct{}
	stopOnce  sync.Once
	readErr   error
	done      chan struct{}
	evidence  ExitEvidence
	waitErr   error
}

type frameResult struct {
	frame []byte
	err   error
}

// Process is a framed I/O handle for one child owned by a Supervisor.
type Process struct {
	supervisor *Supervisor
	ref        productruntime.OwnedProcessRef
}

// NewSupervisor constructs a supervisor with bounded defaults. The optional
// form exists so hosts that need only the frozen OwnedProcessSupervisor
// contract do not need to manufacture an Options value.
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
		processes:        make(map[int]*processState),
	}, nil
}

// Start implements productruntime.OwnedProcessSupervisor. The context owns the
// child lifetime: cancellation closes framed I/O and performs bounded group
// termination.
func (supervisor *Supervisor) Start(ctx context.Context, native productruntime.NativeCommand) (productruntime.OwnedProcessRef, error) {
	process, err := supervisor.StartProcess(ctx, native)
	if err != nil {
		return productruntime.OwnedProcessRef{}, err
	}
	return process.Ref(), nil
}

// StartProcess starts a child and returns its framed handle.
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
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open structured process stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("open structured process stderr: %w", err)
	}
	configureOwnedProcessGroup(command)

	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, ErrSupervisorClosed
	}
	if err := command.Start(); err != nil {
		supervisor.mu.Unlock()
		command.Env = nil
		return nil, fmt.Errorf("start structured process %q: %w", native.Path, err)
	}
	command.Env = nil
	identity, captureErr := captureStartedIdentity(command.Process.Pid)
	if captureErr != nil {
		supervisor.mu.Unlock()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("capture structured process identity: %w", captureErr)
	}
	processGroup, groupErr := currentProcessGroup(command.Process.Pid)
	if groupErr != nil || processGroup != command.Process.Pid {
		supervisor.mu.Unlock()
		_ = command.Process.Kill()
		_ = command.Wait()
		if groupErr != nil {
			return nil, fmt.Errorf("capture structured process group: %w", groupErr)
		}
		return nil, errors.New("structured process did not enter a private process group")
	}
	framer, err := NewFramer(stdout, stdin, supervisor.maxFrameBytes)
	if err != nil {
		supervisor.mu.Unlock()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	ref := productruntime.OwnedProcessRef{Process: identity, ProcessGroup: processGroup}
	state := &processState{
		ref:       ref,
		command:   command,
		framer:    framer,
		startedAt: time.Now().UTC(),
		frames:    make(chan frameResult, 1),
		frameStop: make(chan struct{}),
		readDone:  make(chan struct{}),
		done:      make(chan struct{}),
	}
	supervisor.processes[identity.PID] = state
	supervisor.mu.Unlock()

	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	readerStarted := make(chan struct{})
	go supervisor.readFrames(state, readerStarted)
	<-readerStarted
	go supervisor.reap(state)
	if ctx.Done() != nil {
		go supervisor.cancelOnContext(ctx, state)
	}
	return &Process{supervisor: supervisor, ref: ref}, nil
}

// Open returns the in-memory framed handle for an exact process ref. Protocol
// streams cannot be reconstructed after restart; Cleanup remains available for
// a durable owner that replays the ref.
func (supervisor *Supervisor) Open(ref productruntime.OwnedProcessRef) (*Process, error) {
	if _, err := supervisor.ownedState(ref); err != nil {
		return nil, err
	}
	return &Process{supervisor: supervisor, ref: ref}, nil
}

// Signal implements productruntime.OwnedProcessSupervisor. Unknown local refs
// are accepted only after exact live identity and private-group re-attestation,
// which is the restart cleanup boundary.
func (supervisor *Supervisor) Signal(ctx context.Context, ref productruntime.OwnedProcessRef, signal productruntime.ProcessSignal) error {
	if ctx == nil {
		return errors.New("structured process signal requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := nativeSignal(signal); err != nil {
		return err
	}
	state, known := supervisor.findState(ref)
	if !known {
		status, err := attestRestartedRef(ref)
		if err != nil {
			return err
		}
		if status == procinfo.IdentityStale {
			return ErrNotRunning
		}
	} else {
		select {
		case <-state.done:
			return ErrNotRunning
		default:
		}
	}
	return sendProcessGroupSignal(ref.ProcessGroup, signal)
}

// Wait implements productruntime.OwnedProcessSupervisor. A canceled wait does
// not discard evidence and can be retried.
func (supervisor *Supervisor) Wait(ctx context.Context, ref productruntime.OwnedProcessRef) (productruntime.ProcessExit, error) {
	evidence, err := supervisor.WaitEvidence(ctx, ref)
	return evidence.Exit, err
}

// WaitEvidence returns exact identity, timing, status, and signal evidence.
func (supervisor *Supervisor) WaitEvidence(ctx context.Context, ref productruntime.OwnedProcessRef) (ExitEvidence, error) {
	if ctx == nil {
		return ExitEvidence{}, errors.New("structured process wait requires context")
	}
	state, err := supervisor.ownedState(ref)
	if err != nil {
		return ExitEvidence{}, err
	}
	select {
	case <-ctx.Done():
		return ExitEvidence{}, ctx.Err()
	case <-state.done:
		return state.evidence, state.waitErr
	}
}

// Cleanup performs idempotent TERM-then-KILL group cleanup. For a restarted
// supervisor, the first signal is authorized only while the exact leader is
// live in its recorded private group.
func (supervisor *Supervisor) Cleanup(ctx context.Context, ref productruntime.OwnedProcessRef) error {
	if ctx == nil {
		return errors.New("structured process cleanup requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state, known := supervisor.findState(ref)
	if known {
		state.stopFraming()
		select {
		case <-state.done:
			return state.waitErr
		default:
		}
	}
	if err := supervisor.Signal(ctx, ref, productruntime.ProcessTerminate); err != nil {
		if errors.Is(err, ErrNotRunning) {
			return nil
		}
		return err
	}
	if exited, err := supervisor.waitCleanupInterval(ctx, ref.ProcessGroup, supervisor.terminationGrace); err != nil {
		return err
	} else if exited {
		return nil
	}
	if err := sendProcessGroupSignal(ref.ProcessGroup, productruntime.ProcessKill); err != nil {
		return err
	}
	return supervisor.waitForCleanup(ctx, ref.ProcessGroup)
}

// Close rejects new children and independently cleans every child that this
// supervisor started. It is safe to call repeatedly.
func (supervisor *Supervisor) Close() error {
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.closed = true
	states := make([]*processState, 0, len(supervisor.processes))
	for _, state := range supervisor.processes {
		states = append(states, state)
	}
	supervisor.mu.Unlock()

	var cleanupErrors []error
	for _, state := range states {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), supervisor.terminationGrace+2*time.Second)
		if err := supervisor.Cleanup(cleanupCtx, state.ref); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		cancel()
	}
	return errors.Join(cleanupErrors...)
}

// Ref returns the exact credential-free process reference.
func (process *Process) Ref() productruntime.OwnedProcessRef { return process.ref }

func (process *Process) ReadFrame(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("structured process read requires context")
	}
	state, err := process.supervisor.ownedState(process.ref)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result, ok := <-state.frames:
		if !ok {
			if state.readErr != nil {
				return nil, state.readErr
			}
			return nil, io.EOF
		}
		return result.frame, result.err
	}
}

func (process *Process) WriteFrame(ctx context.Context, frame []byte) error {
	state, err := process.supervisor.ownedState(process.ref)
	if err != nil {
		return err
	}
	return state.framer.WriteFrame(ctx, frame)
}

func (process *Process) Signal(ctx context.Context, signal productruntime.ProcessSignal) error {
	return process.supervisor.Signal(ctx, process.ref, signal)
}

func (process *Process) Wait(ctx context.Context) (productruntime.ProcessExit, error) {
	return process.supervisor.Wait(ctx, process.ref)
}

func (process *Process) WaitEvidence(ctx context.Context) (ExitEvidence, error) {
	return process.supervisor.WaitEvidence(ctx, process.ref)
}

func (process *Process) Cleanup(ctx context.Context) error {
	return process.supervisor.Cleanup(ctx, process.ref)
}

func (supervisor *Supervisor) reap(state *processState) {
	waitErr := state.command.Wait()
	exitedAt := time.Now().UTC()
	<-state.readDone
	state.stopFraming()
	exit, evidenceErr := classifyProcessExit(state.command.ProcessState, waitErr)
	cleanupErr := supervisor.cleanupExitedGroup(state.ref.ProcessGroup)
	state.evidence = ExitEvidence{
		Ref:       state.ref,
		Exit:      exit,
		StartedAt: state.startedAt,
		ExitedAt:  exitedAt,
	}
	state.waitErr = errors.Join(evidenceErr, cleanupErr)
	close(state.done)
}

func (supervisor *Supervisor) readFrames(state *processState, started chan<- struct{}) {
	close(started)
	defer close(state.readDone)
	defer close(state.frames)
	for {
		frame, err := state.framer.ReadFrame(context.Background())
		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return
		}
		if err != nil && !errors.Is(err, ErrFrameTooLarge) {
			state.readErr = err
			return
		}
		result := frameResult{frame: frame, err: err}
		select {
		case <-state.frameStop:
			return
		default:
		}
		select {
		case <-state.frameStop:
			return
		case state.frames <- result:
		}
	}
}

func (state *processState) stopFraming() {
	state.stopOnce.Do(func() {
		close(state.frameStop)
		_ = state.framer.Close()
	})
}

func (supervisor *Supervisor) cleanupExitedGroup(processGroup int) error {
	exists, err := processGroupExists(processGroup)
	if err != nil {
		return fmt.Errorf("observe residual structured process group: %w", err)
	}
	if !exists {
		return nil
	}
	if err := sendProcessGroupSignal(processGroup, productruntime.ProcessTerminate); err != nil {
		return fmt.Errorf("terminate residual structured process group: %w", err)
	}
	if exited, err := waitForProcessGroup(processGroup, supervisor.terminationGrace); err != nil {
		return err
	} else if exited {
		return nil
	}
	if err := sendProcessGroupSignal(processGroup, productruntime.ProcessKill); err != nil {
		return fmt.Errorf("kill residual structured process group: %w", err)
	}
	if exited, err := waitForProcessGroup(processGroup, supervisor.terminationGrace); err != nil {
		return err
	} else if !exited {
		return fmt.Errorf("%w: residual structured process group %d did not exit", productruntime.ErrCleanupDebt, processGroup)
	}
	return nil
}

func waitForProcessGroup(processGroup int, duration time.Duration) (bool, error) {
	deadline := time.Now().Add(duration)
	for {
		exists, err := processGroupExists(processGroup)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		if remaining > processExitPollInterval {
			remaining = processExitPollInterval
		}
		time.Sleep(remaining)
	}
}

func (supervisor *Supervisor) cancelOnContext(ctx context.Context, state *processState) {
	select {
	case <-state.done:
		return
	case <-ctx.Done():
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), supervisor.terminationGrace+2*time.Second)
	defer cancel()
	_ = supervisor.Cleanup(cleanupCtx, state.ref)
}

func (supervisor *Supervisor) waitCleanupInterval(
	ctx context.Context,
	processGroup int,
	duration time.Duration,
) (bool, error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	exists, err := processGroupExists(processGroup)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	ticker := time.NewTicker(processExitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case <-ticker.C:
			exists, err = processGroupExists(processGroup)
			if err != nil {
				return false, err
			}
			if !exists {
				return true, nil
			}
		}
	}
}

func (supervisor *Supervisor) waitForCleanup(ctx context.Context, processGroup int) error {
	ticker := time.NewTicker(processExitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			exists, err := processGroupExists(processGroup)
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
		}
	}
}

func (supervisor *Supervisor) ownedState(ref productruntime.OwnedProcessRef) (*processState, error) {
	state, ok := supervisor.findState(ref)
	if !ok {
		return nil, ErrNotOwned
	}
	return state, nil
}

func (supervisor *Supervisor) findState(ref productruntime.OwnedProcessRef) (*processState, bool) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	state, ok := supervisor.processes[ref.Process.PID]
	return state, ok && state.ref == ref
}

func attestRestartedRef(ref productruntime.OwnedProcessRef) (procinfo.IdentityStatus, error) {
	if ref.Process.PID <= 1 || ref.Process.Start == "" || ref.Process.StrongStart == "" || ref.ProcessGroup != ref.Process.PID {
		return procinfo.IdentityUnknown, ErrNotOwned
	}
	observation := procinfo.ObserveIdentity(ref.Process)
	switch observation.Status {
	case procinfo.IdentityStale:
		if observation.Current.Start != "" &&
			(observation.Current.Start != ref.Process.Start || observation.Current.StrongStart != ref.Process.StrongStart) {
			return observation.Status, ErrNotOwned
		}
		return observation.Status, nil
	case procinfo.IdentityUnknown:
		return observation.Status, ErrNotOwned
	case procinfo.IdentityMatches:
	}
	processGroup, err := currentProcessGroup(ref.Process.PID)
	if err != nil {
		return procinfo.IdentityUnknown, fmt.Errorf("attest structured process group: %w", err)
	}
	if processGroup != ref.ProcessGroup {
		return procinfo.IdentityUnknown, ErrNotOwned
	}
	return observation.Status, nil
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
	command := exec.Command(native.Path, native.Args...) //nolint:gosec // executable and argv are supplied by the typed product driver.
	command.Dir = native.Cwd
	command.Env = envutil.Replace(os.Environ(), replacements)
	return command, nil
}

func captureStartedIdentity(pid int) (procinfo.Identity, error) {
	identity, err := procinfo.CaptureIdentity(pid)
	if err == nil {
		return identity, nil
	}
	// A very short-lived child can already be a zombie by the first host
	// snapshot. It remains our unreaped child, and its kernel start fields still
	// provide the exact, reuse-safe exit reference.
	info := procinfo.Read(pid)
	if info.Status == procinfo.Known && info.Start != "" && info.StrongStart != "" {
		return procinfo.Identity{PID: pid, Start: info.Start, StrongStart: info.StrongStart}, nil
	}
	return procinfo.Identity{}, err
}

func validateEnvironmentVariable(name, value string) error {
	if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
		return errors.New("structured process environment variable is invalid")
	}
	return nil
}

var _ productruntime.OwnedProcessSupervisor = (*Supervisor)(nil)
