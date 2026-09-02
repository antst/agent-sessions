package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

type qwenPending struct {
	request launcher.QwenDaemonPrepareRequest
	root    string
	input   string
	events  string
	writer  *bridge.QwenNativeInput
	adopted bool
}

func (c *hostCoordinator) qwenAdapter() daemonpkg.AttachmentAdapter {
	return daemonpkg.NewQwenAttachmentAdapter(daemonpkg.QwenAdapterConfig{
		Prepare: func(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
			return pendingAttachmentEvidenceChecked(&c.mu, c.qwenPending, attachment.ID, "qwen", qwenEvidence)
		},
		Refresh:  c.refreshQwenAttachment,
		Detach:   c.cleanupQwenAttachment,
		Rollback: c.cleanupQwenAttachment,
	})
}

//nolint:gocyclo // Native owner, dual-output, resume, attachment, and rollback gates form one transaction.
func (c *hostCoordinator) prepareQwen(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request launcher.QwenDaemonPrepareRequest,
) (launcher.QwenDaemonPrepareResult, error) {
	if procinfo.ObserveIdentity(request.Owner).Status != procinfo.IdentityMatches ||
		!filepath.IsAbs(request.Cwd) || !filepath.IsAbs(request.QwenBin) || request.Profile.Fingerprint == "" {
		return launcher.QwenDaemonPrepareResult{}, errors.New("qwen launch identity is incomplete")
	}
	if info, err := os.Stat(request.QwenBin); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return launcher.QwenDaemonPrepareResult{}, errors.New("qwen native executable is unavailable")
	}
	if request.Resume {
		request.SessionID = strings.TrimSpace(request.ResumeTarget)
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return launcher.QwenDaemonPrepareResult{}, errors.New("qwen session identity is empty")
	}
	qwenHome, err := qwenprofile.EffectiveHome(request.Profile, os.LookupEnv)
	if err != nil {
		return launcher.QwenDaemonPrepareResult{}, err
	}
	root := filepath.Join(c.stateRoot, "native", "qwen", request.SessionID)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return launcher.QwenDaemonPrepareResult{}, err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return launcher.QwenDaemonPrepareResult{}, fmt.Errorf("create Qwen lifecycle root: %w", err)
	}
	inputPath, eventsPath := filepath.Join(root, "input.jsonl"), filepath.Join(root, "events.jsonl")
	for _, path := range []string{inputPath, eventsPath} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // exact daemon-owned protocol artifact.
		if err != nil {
			_ = removeQwenRoot(root, nil)
			return launcher.QwenDaemonPrepareResult{}, err
		}
		_ = file.Close()
	}
	pending := &qwenPending{request: request, root: root, input: inputPath, events: eventsPath}
	c.mu.Lock()
	if c.qwenPending[request.SessionID] != nil {
		c.mu.Unlock()
		_ = removeQwenRoot(root, []string{inputPath, eventsPath})
		return launcher.QwenDaemonPrepareResult{}, errors.New("qwen session is already being prepared")
	}
	c.qwenPending[request.SessionID] = pending
	c.mu.Unlock()
	capability, err := randomCapability()
	if err != nil {
		return launcher.QwenDaemonPrepareResult{}, err
	}
	prepared, err := runtime.Attachments().Prepare(ctx, daemonpkg.ManagedAttachment{
		ID: request.SessionID, CapabilityHash: daemonpkg.CapabilityDigest(capability), Product: "qwen",
		LaunchIntent:    request.LaunchPreference,
		ProfileIdentity: request.Profile.Fingerprint, NativeSessionID: request.SessionID,
		NativeProfileRoot: qwenHome,
		Cwd:               request.Cwd, Groups: append([]string(nil), request.Groups...),
		PermissionMode: qwenPermissionMode(request.LaunchPreference),
	})
	if err != nil {
		c.mu.Lock()
		delete(c.qwenPending, request.SessionID)
		c.mu.Unlock()
		_ = removeQwenRoot(root, []string{inputPath, eventsPath})
		return launcher.QwenDaemonPrepareResult{}, err
	}
	c.startQwenOwnerMonitor(runtime, prepared.ID, request.Owner)
	return launcher.QwenDaemonPrepareResult{
		SessionID: prepared.ID, Cwd: request.Cwd, InputPath: inputPath, EventsPath: eventsPath, Capability: capability,
		LaunchPreference: request.LaunchPreference, ExpectedInitialMode: request.ExpectedInitialMode,
	}, nil
}

func qwenInitialModeForPreference(preference string) string {
	switch {
	case preference == "yolo":
		return "yolo"
	case preference == "non_yolo":
		return "default"
	case strings.HasPrefix(preference, "native:"):
		return strings.TrimPrefix(preference, "native:")
	default:
		return ""
	}
}

func qwenEvidence(pending *qwenPending) (daemonpkg.NativeEvidence, error) {
	for _, path := range []string{pending.input, pending.events} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return daemonpkg.NativeEvidence{}, errors.New("qwen protocol stream is unavailable")
		}
	}
	return daemonpkg.NativeEvidence{
		Process: pending.request.Owner, Executable: pending.request.QwenBin,
		ThreadID: pending.request.SessionID, ArtifactPath: pending.input,
		RegistryPath: pending.events, ArtifactRevision: "live",
	}, nil
}

func qwenPermissionMode(preference string) string {
	if preference == "yolo" {
		return "bypassPermissions"
	}
	return "default"
}

//nolint:gocyclo // Owner, artifact, adoption, detach, and native-name gates remain explicit.
func (c *hostCoordinator) startQwenOwnerMonitor(runtime *daemonpkg.Runtime, id string, owner procinfo.Identity) {
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
				pending := c.qwenPending[id]
				c.mu.Unlock()
				if pending == nil || pending.adopted {
					if time.Since(lastNamePoll) >= time.Second {
						attachment, ok, _ := runtime.Attachments().ActiveAttachment(id)
						if ok {
							c.observeQwenNativeName(runtime, attachment)
						}
						lastNamePoll = time.Now()
					}
					continue
				}
				if !qwenOwnerExecReady(pending) {
					continue
				}
				evidence, evidenceErr := qwenEvidence(pending)
				if evidenceErr != nil {
					continue
				}
				if _, err := runtime.Attachments().Adopt(context.Background(), id, evidence); err != nil {
					return
				}
				c.mu.Lock()
				if current := c.qwenPending[id]; current != nil {
					current.adopted = true
				}
				c.mu.Unlock()
				if attachment, ok, _ := runtime.Attachments().ActiveAttachment(id); ok {
					c.observeQwenNativeName(runtime, attachment)
					lastNamePoll = time.Now()
				}
			}
		}
	}()
}

func (c *hostCoordinator) observeQwenNativeName(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) {
	home := attachment.NativeProfileRoot
	if home == "" {
		profile, err := qwenprofile.Current()
		if err != nil || profile.Fingerprint != attachment.ProfileIdentity {
			return
		}
		home, err = qwenprofile.EffectiveHome(profile, os.LookupEnv)
		if err != nil {
			return
		}
	}
	title, ok := bridge.QwenNativeSessionTitle(
		home, attachment.NativeSessionID, attachment.Cwd,
	)
	if !ok {
		return
	}
	_ = runtime.Attachments().ObserveNativeTitle(
		attachment.ID, attachment.NativeSessionID, bridge.NormalizePeerName(title),
	)
}

func qwenOwnerExecReady(pending *qwenPending) bool {
	args, err := procinfo.Args(pending.request.Owner.PID)
	if err != nil || len(args) == 0 {
		return false
	}
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	return strings.Contains(joined, "\x00"+pending.input+"\x00") &&
		strings.Contains(joined, "\x00"+pending.events+"\x00") &&
		strings.Contains(joined, "\x00"+pending.request.SessionID+"\x00")
}

func (c *hostCoordinator) refreshQwenAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
	c.mu.Lock()
	pending := c.qwenPending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return daemonpkg.NativeEvidence{}, errors.New("qwen session is not connected to this daemon")
	}
	if procinfo.ObserveIdentity(pending.request.Owner).Status != procinfo.IdentityMatches ||
		!qwenOwnerExecReady(pending) {
		return daemonpkg.NativeEvidence{}, errors.New("qwen native owner is not live")
	}
	return qwenEvidence(pending)
}

func (c *hostCoordinator) cleanupQwenAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) error {
	c.mu.Lock()
	pending := c.qwenPending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return nil
	}
	if pending.writer != nil {
		_ = pending.writer.Close()
		pending.writer = nil
	}
	if procinfo.Read(pending.request.Owner.PID).Status != procinfo.Absent {
		return errors.New("native Qwen PID is not absent during cleanup")
	}
	if err := removeQwenRoot(pending.root, []string{pending.input, pending.events}); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.qwenPending, attachment.ID)
	c.mu.Unlock()
	return nil
}

func removeQwenRoot(root string, artifacts []string) error {
	for _, artifact := range artifacts {
		if filepath.Dir(artifact) != root {
			return errors.New("qwen protocol artifact is outside its session root")
		}
		if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if len(artifacts) == 0 {
		for _, name := range []string{"input.jsonl", "events.jsonl"} {
			_ = os.Remove(filepath.Join(root, name))
		}
	}
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(root))
	return nil
}

func requestQwenPreparation(ctx context.Context, request launcher.QwenDaemonPrepareRequest) (launcher.QwenDaemonPrepareResult, error) {
	return requestPreparation[launcher.QwenDaemonPrepareRequest, launcher.QwenDaemonPrepareResult](
		ctx, "attachment.qwen.prepare", request,
	)
}
