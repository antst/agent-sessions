package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/socketpath"
)

type grokPending struct {
	request        launcher.GrokDaemonPrepareRequest
	root           string
	leaderSocket   string
	leader         *exec.Cmd
	leaderIdentity procinfo.Identity
	diagnostics    *os.File
	observer       *bridge.GrokNativeObserver
	adopted        bool
}

func (c *hostCoordinator) grokAdapter() daemonpkg.AttachmentAdapter {
	return daemonpkg.NewGrokAttachmentAdapter(daemonpkg.GrokAdapterConfig{
		Prepare: func(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
			return pendingAttachmentEvidence(&c.mu, c.grokPending, attachment.ID, "grok", grokEvidence)
		},
		Refresh:  c.refreshGrokAttachment,
		Detach:   c.cleanupGrokAttachment,
		Rollback: c.cleanupGrokAttachment,
	})
}

//nolint:gocyclo // Native owner, leader, observer, resume, and rollback gates form one transaction.
func (c *hostCoordinator) prepareGrok(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request launcher.GrokDaemonPrepareRequest,
) (launcher.GrokDaemonPrepareResult, error) {
	if procinfo.ObserveIdentity(request.Owner).Status != procinfo.IdentityMatches ||
		strings.TrimSpace(request.SessionID) == "" || !filepath.IsAbs(request.Cwd) ||
		!filepath.IsAbs(request.GrokBin) || strings.TrimSpace(request.LaunchToken) == "" {
		return launcher.GrokDaemonPrepareResult{}, errors.New("grok launch identity is incomplete")
	}
	if info, err := os.Stat(request.GrokBin); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return launcher.GrokDaemonPrepareResult{}, errors.New("grok native executable is unavailable")
	}
	digest := sha256.Sum256([]byte(request.LaunchToken))
	root := filepath.Join(c.stateRoot, "run", "g-"+hex.EncodeToString(digest[:10]))
	leaderSocket := filepath.Join(root, "leader.sock")
	if err := socketpath.Validate(leaderSocket); err != nil {
		return launcher.GrokDaemonPrepareResult{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return launcher.GrokDaemonPrepareResult{}, err
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // private daemon-owned leader root.
		return launcher.GrokDaemonPrepareResult{}, err
	}
	diagnostics, err := os.OpenFile(filepath.Join(root, "diagnostics.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // exact daemon-owned path.
	if err != nil {
		return launcher.GrokDaemonPrepareResult{}, err
	}
	// The private leader belongs to the live Grok attachment, not to one daemon
	// generation. A user-service restart must leave it running so the successor
	// can re-attest the same owner, leader, and socket.
	command := exec.Command(request.GrokBin, //nolint:gosec // exact executable validated above.
		"--permission-mode", "default", "agent", "leader", "--leader-socket", leaderSocket,
		"--no-exit-on-disconnect", "--relay-on-demand", "--no-auto-update",
	)
	command.Dir, command.Stdout, command.Stderr = request.Cwd, diagnostics, diagnostics
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Env = append(os.Environ(),
		"AGENT_SESSIONS_GROK_LAUNCH_TOKEN="+request.LaunchToken,
		"AGENT_SESSIONS_GROK_SESSION_ID="+request.SessionID,
	)
	if err := command.Start(); err != nil {
		_ = diagnostics.Close()
		return launcher.GrokDaemonPrepareResult{}, fmt.Errorf("start private Grok leader: %w", err)
	}
	leaderIdentity, err := procinfo.CaptureIdentity(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = diagnostics.Close()
		return launcher.GrokDaemonPrepareResult{}, err
	}
	pending := &grokPending{
		request: request, root: root, leaderSocket: leaderSocket,
		leader: command, leaderIdentity: leaderIdentity, diagnostics: diagnostics,
	}
	if err := waitForUnixSocket(ctx, leaderSocket, 10*time.Second); err != nil {
		_ = stopGrokLeader(pending)
		return launcher.GrokDaemonPrepareResult{}, fmt.Errorf("private Grok leader did not become ready: %w", err)
	}
	c.mu.Lock()
	if c.grokPending[request.SessionID] != nil {
		c.mu.Unlock()
		_ = stopGrokLeader(pending)
		return launcher.GrokDaemonPrepareResult{}, errors.New("grok session is already being prepared")
	}
	c.grokPending[request.SessionID] = pending
	c.mu.Unlock()
	capability, err := randomCapability()
	if err != nil {
		_ = stopGrokLeader(pending)
		return launcher.GrokDaemonPrepareResult{}, err
	}
	prepared, err := runtime.Attachments().Prepare(ctx, daemonpkg.ManagedAttachment{
		ID: request.SessionID, CapabilityHash: daemonpkg.CapabilityDigest(capability), Product: "grok",
		ProfileIdentity: grokProfileRoot(), NativeSessionID: request.SessionID,
		Cwd: request.Cwd, Groups: append([]string(nil), request.Groups...), PermissionMode: request.PermissionMode,
	})
	if err != nil {
		c.mu.Lock()
		delete(c.grokPending, request.SessionID)
		c.mu.Unlock()
		_ = stopGrokLeader(pending)
		return launcher.GrokDaemonPrepareResult{}, err
	}
	c.startGrokOwnerMonitor(runtime, prepared.ID, request.Owner)
	return launcher.GrokDaemonPrepareResult{SessionID: prepared.ID, Cwd: request.Cwd, LeaderSocket: leaderSocket}, nil
}

func grokEvidence(pending *grokPending) daemonpkg.NativeEvidence {
	return daemonpkg.NativeEvidence{
		Process: pending.request.Owner, Ancestry: []procinfo.Identity{pending.leaderIdentity},
		Executable: pending.request.GrokBin, SocketPath: pending.leaderSocket,
		ThreadID: pending.request.SessionID,
	}
}

func grokProfileRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".grok")
}

func waitForUnixSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("socket readiness timed out")
		case <-ticker.C:
		}
	}
}

func (c *hostCoordinator) startGrokOwnerMonitor(runtime *daemonpkg.Runtime, id string, owner procinfo.Identity) {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		lastNamePoll := time.Time{}
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if procinfo.Read(owner.PID).Status == procinfo.Absent {
					if _, err := runtime.Attachments().Detach(context.Background(), id, "native-owner-exited"); err == nil {
						c.archiveIdleLanesForParent(runtime, id)
					}
					return
				}
				c.mu.Lock()
				pending := c.grokPending[id]
				c.mu.Unlock()
				if pending == nil {
					if time.Since(lastNamePoll) >= time.Second {
						if attachment, ok, _ := runtime.Attachments().ActiveAttachment(id); ok {
							c.observeDurableGrokNativeName(runtime, attachment)
						}
						lastNamePoll = time.Now()
					}
					continue
				}
				if !grokOwnerExecReady(pending) {
					continue
				}
				if !pending.adopted {
					if _, err := runtime.Attachments().Adopt(context.Background(), id, grokEvidence(pending)); err != nil {
						return
					}
					c.mu.Lock()
					if current := c.grokPending[id]; current != nil {
						current.adopted = true
					}
					c.mu.Unlock()
				}
				if time.Since(lastNamePoll) >= time.Second {
					c.observeGrokNativeName(runtime, id, pending)
					lastNamePoll = time.Now()
				}
			}
		}
	}()
}

func (c *hostCoordinator) observeDurableGrokNativeName(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) {
	c.mu.Lock()
	observer := c.grokObservers[attachment.ID]
	c.mu.Unlock()
	if observer == nil {
		evidence, err := refreshDurableGrokAttachment(attachment)
		if err != nil {
			return
		}
		environment := envutil.Set(os.Environ(), "AGENT_SESSIONS_PRODUCT", "grok")
		environment = envutil.Set(environment, "AGENT_SESSIONS_SESSION_ID", attachment.NativeSessionID)
		created, err := bridge.OpenGrokNativeObserver(
			c.ctx, evidence.Executable, attachment.Cwd, evidence.SocketPath,
			attachment.NativeSessionID, environment, nil,
		)
		if err != nil {
			return
		}
		c.mu.Lock()
		if existing := c.grokObservers[attachment.ID]; existing == nil {
			c.grokObservers[attachment.ID] = created
			observer = created
		} else {
			created.Close()
			observer = existing
		}
		c.mu.Unlock()
	}
	name, err := observer.SessionName(c.ctx)
	if err == nil {
		_ = runtime.Attachments().ObserveNativeTitle(
			attachment.ID, attachment.NativeSessionID, bridge.NormalizePeerName(name),
		)
	}
}

func (c *hostCoordinator) observeGrokNativeName(runtime *daemonpkg.Runtime, id string, pending *grokPending) {
	if pending.observer == nil {
		environment := envutil.Set(os.Environ(), "AGENT_SESSIONS_PRODUCT", "grok")
		environment = envutil.Set(environment, "AGENT_SESSIONS_SESSION_ID", pending.request.SessionID)
		observer, err := bridge.OpenGrokNativeObserver(
			c.ctx, pending.request.GrokBin, pending.request.Cwd, pending.leaderSocket,
			pending.request.SessionID, environment, pending.diagnostics,
		)
		if err != nil {
			return
		}
		c.mu.Lock()
		if current := c.grokPending[id]; current != nil && current.observer == nil {
			current.observer = observer
			pending = current
		} else {
			observer.Close()
		}
		c.mu.Unlock()
	}
	if pending.observer == nil {
		return
	}
	name, err := pending.observer.SessionName(c.ctx)
	if err == nil {
		_ = runtime.Attachments().ObserveNativeTitle(
			id, pending.request.SessionID, bridge.NormalizePeerName(name),
		)
	}
}

func grokOwnerExecReady(pending *grokPending) bool {
	args, err := procinfo.Args(pending.request.Owner.PID)
	if err != nil || len(args) == 0 {
		return false
	}
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	return strings.Contains(joined, "\x00"+pending.leaderSocket+"\x00") &&
		(strings.Contains(joined, "\x00"+pending.request.SessionID+"\x00") || pending.request.LateBoundResume)
}

func (c *hostCoordinator) refreshGrokAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
	c.mu.Lock()
	pending := c.grokPending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return refreshDurableGrokAttachment(attachment)
	}
	if procinfo.ObserveIdentity(pending.request.Owner).Status != procinfo.IdentityMatches ||
		procinfo.ObserveIdentity(pending.leaderIdentity).Status != procinfo.IdentityMatches || !grokOwnerExecReady(pending) {
		return daemonpkg.NativeEvidence{}, errors.New("grok native owner or private leader is not live")
	}
	return grokEvidence(pending), nil
}

func refreshDurableGrokAttachment(attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
	evidence := attachment.Evidence
	if procinfo.ObserveIdentity(evidence.Process).Status != procinfo.IdentityMatches || len(evidence.Ancestry) != 1 ||
		procinfo.ObserveIdentity(evidence.Ancestry[0]).Status != procinfo.IdentityMatches ||
		strings.TrimSpace(evidence.ThreadID) == "" || evidence.ThreadID != attachment.NativeSessionID {
		return daemonpkg.NativeEvidence{}, errors.New("grok native owner or private leader is not live")
	}
	info, err := os.Lstat(evidence.SocketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return daemonpkg.NativeEvidence{}, errors.New("grok private leader socket is not live")
	}
	args, err := procinfo.Args(evidence.Process.PID)
	if err != nil {
		return daemonpkg.NativeEvidence{}, errors.New("grok native owner arguments are unavailable")
	}
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	if !strings.Contains(joined, "\x00"+evidence.SocketPath+"\x00") {
		return daemonpkg.NativeEvidence{}, errors.New("grok native owner is not attached to its private leader")
	}
	return evidence, nil
}

func (c *hostCoordinator) cleanupGrokAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) error {
	c.mu.Lock()
	pending := c.grokPending[attachment.ID]
	durableObserver := c.grokObservers[attachment.ID]
	delete(c.grokObservers, attachment.ID)
	c.mu.Unlock()
	if durableObserver != nil {
		durableObserver.Close()
	}
	if pending == nil {
		return cleanupDurableGrokAttachment(c.stateRoot, attachment)
	}
	if pending.observer != nil {
		pending.observer.Close()
		pending.observer = nil
	}
	if procinfo.Read(pending.request.Owner.PID).Status != procinfo.Absent {
		return errors.New("native Grok PID is not absent during cleanup")
	}
	if err := stopGrokLeader(pending); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.grokPending, attachment.ID)
	c.mu.Unlock()
	return nil
}

//nolint:gocyclo // Cleanup revalidates every identity, owned path, and bounded signal transition.
func cleanupDurableGrokAttachment(stateRoot string, attachment daemonpkg.ManagedAttachment) error {
	evidence := attachment.Evidence
	if procinfo.Read(evidence.Process.PID).Status != procinfo.Absent {
		return errors.New("native Grok PID is not absent during cleanup")
	}
	if len(evidence.Ancestry) != 1 || !filepath.IsAbs(evidence.SocketPath) {
		return errors.New("grok durable cleanup identity is incomplete")
	}
	root := filepath.Dir(evidence.SocketPath)
	name := filepath.Base(root)
	if evidence.SocketPath != filepath.Join(root, "leader.sock") || filepath.Dir(root) != filepath.Join(stateRoot, "run") ||
		len(name) != 22 || !strings.HasPrefix(name, "g-") || !isLowerHex(name[2:]) {
		return errors.New("grok durable cleanup root is outside daemon ownership")
	}
	leader := evidence.Ancestry[0]
	switch procinfo.ObserveIdentity(leader).Status {
	case procinfo.IdentityMatches:
		if err := syscall.Kill(-leader.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && procinfo.ObserveIdentity(leader).Status == procinfo.IdentityMatches {
			time.Sleep(25 * time.Millisecond)
		}
		if procinfo.ObserveIdentity(leader).Status == procinfo.IdentityMatches {
			if err := syscall.Kill(-leader.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	case procinfo.IdentityStale:
		// Already absent or reused; never signal the current occupant.
	case procinfo.IdentityUnknown:
		return errors.New("grok private leader identity cannot be corroborated for cleanup")
	}
	for _, path := range []string{evidence.SocketPath, filepath.Join(root, "leader.lock"), filepath.Join(root, "diagnostics.log")} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func stopGrokLeader(pending *grokPending) error {
	if pending == nil {
		return nil
	}
	if procinfo.ObserveIdentity(pending.leaderIdentity).Status == procinfo.IdentityMatches {
		_ = syscall.Kill(-pending.leaderIdentity.PID, syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- pending.leader.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pending.leaderIdentity.PID, syscall.SIGKILL)
			<-done
		}
	}
	if pending.diagnostics != nil {
		_ = pending.diagnostics.Close()
	}
	for _, path := range []string{pending.leaderSocket, filepath.Join(pending.root, "leader.lock"), filepath.Join(pending.root, "diagnostics.log")} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(pending.root); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func requestGrokPreparation(ctx context.Context, request launcher.GrokDaemonPrepareRequest) (launcher.GrokDaemonPrepareResult, error) {
	return requestPreparation[launcher.GrokDaemonPrepareRequest, launcher.GrokDaemonPrepareResult](
		ctx, "attachment.grok.prepare", request,
	)
}
