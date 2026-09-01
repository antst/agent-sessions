package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/socketpath"
)

type claudePending struct {
	request       launcher.ClaudeDaemonPrepareRequest
	lifecycleRoot string
	managedSocket string
	keyBaseline   []federator.ClaudeKeyBaselineEntry
	observedKeys  []federator.ClaudeKeyBaselineEntry
	row           launcher.ClaudeNativePeerRecord
	adopted       bool
}

func (c *hostCoordinator) claudeAdapter() daemonpkg.AttachmentAdapter {
	return daemonpkg.NewClaudeAttachmentAdapter(daemonpkg.ClaudeAdapterConfig{
		Prepare: func(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
			return pendingAttachmentEvidence(&c.mu, c.claudePending, attachment.ID, "claude", claudeExpectedEvidence)
		},
		Refresh:  c.refreshClaudeAttachment,
		Detach:   c.cleanupClaudeAttachment,
		Rollback: c.cleanupClaudeAttachment,
	})
}

//nolint:gocyclo // Native profile, socket, row, ownership, and rollback gates stay in one transaction.
func (c *hostCoordinator) prepareClaude(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request launcher.ClaudeDaemonPrepareRequest,
) (launcher.ClaudeDaemonPrepareResult, error) {
	if procinfo.ObserveIdentity(request.Owner).Status != procinfo.IdentityMatches {
		return launcher.ClaudeDaemonPrepareResult{}, errors.New("claude launcher identity is not live")
	}
	if strings.TrimSpace(request.AttachmentID) == "" || !filepath.IsAbs(request.ConfigRoot) {
		return launcher.ClaudeDaemonPrepareResult{}, errors.New("claude launch identity is incomplete")
	}
	if request.ConfigEnvSet && request.ConfigEnvValue != "" && request.ConfigEnvValue != request.ConfigRoot {
		return launcher.ClaudeDaemonPrepareResult{}, errors.New("claude profile identity is inconsistent")
	}
	registry := filepath.Join(request.ConfigRoot, "sessions")
	info, err := os.Lstat(registry)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return launcher.ClaudeDaemonPrepareResult{}, errors.New("claude native session registry is unavailable")
	}
	lifecycleRoot := filepath.Join(c.stateRoot, "native", "claude", request.AttachmentID)
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		return launcher.ClaudeDaemonPrepareResult{}, fmt.Errorf("create Claude lifecycle root: %w", err)
	}
	if err := os.Chmod(lifecycleRoot, 0o700); err != nil { //nolint:gosec // private daemon-owned launch root.
		return launcher.ClaudeDaemonPrepareResult{}, err
	}
	managedSocket := claudeManagedSocket(c.stateRoot, request.AttachmentID)
	if err := socketpath.Validate(managedSocket); err != nil {
		return launcher.ClaudeDaemonPrepareResult{}, err
	}
	if _, err := os.Lstat(managedSocket); err == nil || !os.IsNotExist(err) {
		return launcher.ClaudeDaemonPrepareResult{}, errors.New("managed Claude messaging socket path already exists")
	}
	keyBaseline, err := federator.ClaudePeerKeySidecars(request.ConfigRoot, request.Owner.PID)
	if err != nil {
		return launcher.ClaudeDaemonPrepareResult{}, fmt.Errorf("snapshot Claude native key sidecars: %w", err)
	}
	pending := &claudePending{
		request: request, lifecycleRoot: lifecycleRoot, managedSocket: managedSocket,
		keyBaseline: keyBaseline,
	}
	c.mu.Lock()
	if c.claudePending[request.AttachmentID] != nil {
		c.mu.Unlock()
		return launcher.ClaudeDaemonPrepareResult{}, errors.New("claude attachment is already being prepared")
	}
	c.claudePending[request.AttachmentID] = pending
	c.mu.Unlock()
	capability, err := randomCapability()
	if err != nil {
		return launcher.ClaudeDaemonPrepareResult{}, err
	}
	permission := "default"
	if request.AlwaysApprove {
		permission = "bypassPermissions"
	}
	prepared, err := runtime.Attachments().Prepare(ctx, daemonpkg.ManagedAttachment{
		ID: request.AttachmentID, CapabilityHash: daemonpkg.CapabilityDigest(capability), Product: "claude",
		ProfileIdentity: request.ConfigRoot, NativeSessionID: request.SessionID,
		Cwd: request.Cwd, Groups: append([]string(nil), request.Groups...), PermissionMode: permission,
	})
	if err != nil {
		c.mu.Lock()
		delete(c.claudePending, request.AttachmentID)
		c.mu.Unlock()
		return launcher.ClaudeDaemonPrepareResult{}, err
	}
	c.startClaudeOwnerMonitor(runtime, prepared.ID, request.Owner)
	return launcher.ClaudeDaemonPrepareResult{
		AttachmentID: prepared.ID, LifecycleRoot: lifecycleRoot,
		ManagedSocket: managedSocket, AlwaysApprove: request.AlwaysApprove,
	}, nil
}

func claudeManagedSocket(stateRoot, attachmentID string) string {
	digest := sha256.Sum256([]byte(attachmentID))
	return filepath.Join(stateRoot, "run", "c-"+hex.EncodeToString(digest[:10])+".sock")
}

func claudeExpectedEvidence(pending *claudePending) daemonpkg.NativeEvidence {
	return daemonpkg.NativeEvidence{
		Process:      pending.request.Owner,
		RegistryPath: filepath.Join(pending.request.ConfigRoot, "sessions", strconv.Itoa(pending.request.Owner.PID)+".json"),
		SocketPath:   pending.managedSocket, ThreadID: pending.request.SessionID,
		ArtifactPath: filepath.Join(pending.lifecycleRoot, "launch-settings.json"),
	}
}

//nolint:gocyclo // Owner monitoring keeps exact identity and terminal cleanup transitions together.
func (c *hostCoordinator) startClaudeOwnerMonitor(runtime *daemonpkg.Runtime, id string, owner procinfo.Identity) {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		lastNamePoll := time.Time{}
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				// Native Claude can briefly remain a zombie after the TUI exits.
				// The established cleanup deliberately requires PID absence (not
				// merely a terminal identity), so wait for the parent to reap it
				// before withdrawing and cleaning the attachment.
				if procinfo.Read(owner.PID).Status == procinfo.Absent {
					if _, err := runtime.Attachments().Detach(context.Background(), id, "native-owner-exited"); err == nil {
						c.archiveIdleLanesForParent(runtime, id)
					}
					return
				}
				c.mu.Lock()
				pending := c.claudePending[id]
				c.mu.Unlock()
				if pending == nil || pending.adopted {
					if time.Since(lastNamePoll) >= time.Second {
						attachment, ok, _ := runtime.Attachments().ActiveAttachment(id)
						if ok {
							c.observeClaudeNativeName(runtime, attachment)
						}
						lastNamePoll = time.Now()
					}
					continue
				}
				keys, keyErr := federator.ObserveClaudePeerNewKeySidecars(
					pending.request.ConfigRoot, owner.PID, owner.Start, owner.StrongStart, pending.keyBaseline,
				)
				if keyErr == nil {
					c.mu.Lock()
					if current := c.claudePending[id]; current != nil {
						current.observedKeys = keys
					}
					c.mu.Unlock()
				}
				row, err := launcher.ObserveClaudeNativePeer(
					pending.request.ConfigRoot, owner.PID, owner.Start, pending.managedSocket,
				)
				if err != nil || !c.claudeSelectionMatches(pending, row) {
					continue
				}
				permission, permissionErr := claudeEffectivePermissionMode(row.PermissionMode, pending.request.AlwaysApprove)
				if permissionErr != nil {
					_, _ = runtime.Attachments().Rollback(context.Background(), id, "native-permission-mismatch")
					return
				}
				if _, err := runtime.Attachments().SelectNative(id, row.SessionID, row.Cwd, permission); err != nil {
					_, _ = runtime.Attachments().Rollback(context.Background(), id, "native-selection-failed")
					return
				}
				observed := claudeExpectedEvidence(pending)
				observed.ThreadID = row.SessionID
				if _, err := runtime.Attachments().Adopt(context.Background(), id, observed); err != nil {
					return
				}
				c.mu.Lock()
				if current := c.claudePending[id]; current != nil {
					current.row, current.adopted = row, true
				}
				c.mu.Unlock()
				if attachment, ok, _ := runtime.Attachments().ActiveAttachment(id); ok {
					c.observeClaudeNativeName(runtime, attachment)
					lastNamePoll = time.Now()
				}
			}
		}
	}()
}

func (c *hostCoordinator) observeClaudeNativeName(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) {
	title, ok := federator.ClaudeNativeSessionTitle(attachment.ProfileIdentity, attachment.NativeSessionID)
	if !ok {
		return
	}
	_ = runtime.Attachments().ObserveNativeTitle(
		attachment.ID, attachment.NativeSessionID, bridge.NormalizePeerName(title),
	)
}

func (c *hostCoordinator) claudeSelectionMatches(pending *claudePending, row launcher.ClaudeNativePeerRecord) bool {
	if pending.request.SessionID != "" {
		return row.SessionID == pending.request.SessionID
	}
	if !pending.request.Resume || strings.TrimSpace(pending.request.ResumeTarget) == "" {
		return true
	}
	title, ok := federator.ClaudeNativeSessionTitle(pending.request.ConfigRoot, row.SessionID)
	return ok && strings.EqualFold(strings.TrimSpace(title), strings.TrimSpace(pending.request.ResumeTarget))
}

func (c *hostCoordinator) refreshClaudeAttachment(
	_ context.Context,
	attachment daemonpkg.ManagedAttachment,
) (daemonpkg.NativeEvidence, error) {
	c.mu.Lock()
	pending := c.claudePending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return refreshDurableClaudeAttachment(attachment)
	}
	if procinfo.ObserveIdentity(pending.request.Owner).Status != procinfo.IdentityMatches {
		return daemonpkg.NativeEvidence{}, errors.New("claude TUI owner is not live")
	}
	row, err := launcher.ObserveClaudeNativePeer(
		pending.request.ConfigRoot, pending.request.Owner.PID, pending.request.Owner.Start, pending.managedSocket,
	)
	if err != nil || row.SessionID != attachment.NativeSessionID {
		return daemonpkg.NativeEvidence{}, errors.New("claude native publication is not live")
	}
	if _, err := claudeEffectivePermissionMode(row.PermissionMode, pending.request.AlwaysApprove); err != nil {
		return daemonpkg.NativeEvidence{}, err
	}
	evidence := claudeExpectedEvidence(pending)
	evidence.ThreadID = row.SessionID
	return evidence, nil
}

func refreshDurableClaudeAttachment(attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
	evidence := attachment.Evidence
	if procinfo.ObserveIdentity(evidence.Process).Status != procinfo.IdentityMatches ||
		!filepath.IsAbs(attachment.ProfileIdentity) || !filepath.IsAbs(evidence.SocketPath) {
		return daemonpkg.NativeEvidence{}, errors.New("claude TUI owner is not live")
	}
	row, err := launcher.ObserveClaudeNativePeer(
		attachment.ProfileIdentity, evidence.Process.PID, evidence.Process.Start, evidence.SocketPath,
	)
	if err != nil || row.SessionID != attachment.NativeSessionID {
		return daemonpkg.NativeEvidence{}, errors.New("claude native publication is not live")
	}
	if _, err := claudeEffectivePermissionMode(
		row.PermissionMode, attachment.PermissionMode == "bypassPermissions",
	); err != nil {
		return daemonpkg.NativeEvidence{}, err
	}
	evidence.ThreadID = row.SessionID
	return evidence, nil
}

// claudeEffectivePermissionMode preserves the established Claude adapter rule:
// current native rows may omit permissionMode, so absence falls back to the
// immutable managed launch decision. An explicit native mode may corroborate
// that decision but never silently upgrade or downgrade it.
func claudeEffectivePermissionMode(nativeMode string, durableBypass bool) (string, error) {
	nativeMode = strings.TrimSpace(nativeMode)
	if nativeMode != "" && (nativeMode == "bypassPermissions") != durableBypass {
		return "", errors.New("claude native permission mode disagrees with durable launch policy")
	}
	if durableBypass {
		return "bypassPermissions", nil
	}
	return "default", nil
}

func (c *hostCoordinator) cleanupClaudeAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) error {
	c.mu.Lock()
	pending := c.claudePending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return cleanupDurableClaudeAttachment(c.stateRoot, attachment)
	}
	if procinfo.ObserveIdentity(pending.request.Owner).Status != procinfo.IdentityStale {
		return errors.New("native Claude PID is not absent during cleanup")
	}
	row := pending.row
	if row.PID == 0 {
		row.PID = pending.request.Owner.PID
		row.SessionID = attachment.NativeSessionID
		row.ProcStart = pending.request.Owner.Start
		row.MessagingSocketPath = pending.managedSocket
	}
	if row.SessionID != "" {
		if err := launcher.CleanupClaudeNativePeer(
			pending.request.ConfigRoot, row, pending.request.Owner.Start, pending.request.Owner.StrongStart,
			row.SessionID, pending.keyBaseline, pending.observedKeys,
		); err != nil {
			return err
		}
	}
	if err := removeClaudeLifecycleRoot(pending.lifecycleRoot); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.claudePending, attachment.ID)
	c.mu.Unlock()
	return nil
}

func cleanupDurableClaudeAttachment(stateRoot string, attachment daemonpkg.ManagedAttachment) error {
	evidence := attachment.Evidence
	if procinfo.Read(evidence.Process.PID).Status != procinfo.Absent {
		return errors.New("native Claude PID is not absent during cleanup")
	}
	paths, err := durableClaudeCleanupPaths(stateRoot, attachment)
	if err != nil {
		return err
	}
	row := launcher.ClaudeNativePeerRecord{
		PID: evidence.Process.PID, SessionID: attachment.NativeSessionID,
		ProcStart: evidence.Process.Start, MessagingSocketPath: paths.managedSocket,
	}
	if err := launcher.CleanupClaudeNativePeer(
		attachment.ProfileIdentity, row, evidence.Process.Start, evidence.Process.StrongStart,
		attachment.NativeSessionID, nil, nil,
	); err != nil {
		return err
	}
	return removeClaudeLifecycleRoot(paths.lifecycleRoot)
}

type claudeDurableCleanupIdentity struct {
	lifecycleRoot string
	managedSocket string
}

func durableClaudeCleanupPaths(stateRoot string, attachment daemonpkg.ManagedAttachment) (claudeDurableCleanupIdentity, error) {
	evidence := attachment.Evidence
	nativeRoot := filepath.Join(stateRoot, "native", "claude")
	if attachment.ID == "" || filepath.Base(attachment.ID) != attachment.ID || attachment.ID == "." || attachment.ID == ".." ||
		!filepath.IsAbs(attachment.ProfileIdentity) || attachment.NativeSessionID == "" || evidence.Process.PID <= 1 ||
		evidence.Process.Start == "" || evidence.Process.StrongStart == "" {
		return claudeDurableCleanupIdentity{}, errors.New("claude durable cleanup identity is incomplete")
	}
	lifecycleRoot := filepath.Join(nativeRoot, attachment.ID)
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	managedSocket := claudeManagedSocket(stateRoot, attachment.ID)
	registryPath := filepath.Join(
		attachment.ProfileIdentity, "sessions", strconv.Itoa(evidence.Process.PID)+".json",
	)
	if evidence.ThreadID != attachment.NativeSessionID || evidence.RegistryPath != registryPath ||
		evidence.SocketPath != managedSocket || evidence.ArtifactPath != settingsPath {
		return claudeDurableCleanupIdentity{}, errors.New("claude durable cleanup paths are outside exact daemon ownership")
	}
	return claudeDurableCleanupIdentity{lifecycleRoot: lifecycleRoot, managedSocket: managedSocket}, nil
}

func removeClaudeLifecycleRoot(lifecycleRoot string) error {
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	if info, err := os.Lstat(settingsPath); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("managed Claude launch settings changed type")
		}
		if err := os.Remove(settingsPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(lifecycleRoot); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func requestClaudePreparation(
	ctx context.Context,
	request launcher.ClaudeDaemonPrepareRequest,
) (launcher.ClaudeDaemonPrepareResult, error) {
	return requestPreparation[launcher.ClaudeDaemonPrepareRequest, launcher.ClaudeDaemonPrepareResult](
		ctx, "attachment.claude.prepare", request,
	)
}
