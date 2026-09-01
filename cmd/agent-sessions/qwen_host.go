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
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

type qwenPending struct {
	request launcher.QwenDaemonPrepareRequest
	root    string
	input   federator.QwenArtifactAttestation
	events  federator.QwenArtifactAttestation
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
		selected, err := resolveQwenDaemonResume(runtime, request)
		if err != nil {
			return launcher.QwenDaemonPrepareResult{}, err
		}
		request = inheritQwenResumeRequest(request, selected)
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
	input, err := federator.QwenArtifactAttestationForPath(inputPath)
	if err != nil {
		_ = removeQwenRoot(root, nil)
		return launcher.QwenDaemonPrepareResult{}, err
	}
	events, err := federator.QwenArtifactAttestationForPath(eventsPath)
	if err != nil {
		_ = removeQwenRoot(root, []federator.QwenArtifactAttestation{input})
		return launcher.QwenDaemonPrepareResult{}, err
	}
	pending := &qwenPending{request: request, root: root, input: input, events: events}
	c.mu.Lock()
	if c.qwenPending[request.SessionID] != nil {
		c.mu.Unlock()
		_ = removeQwenRoot(root, []federator.QwenArtifactAttestation{input, events})
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
		_ = removeQwenRoot(root, []federator.QwenArtifactAttestation{input, events})
		return launcher.QwenDaemonPrepareResult{}, err
	}
	c.startQwenOwnerMonitor(runtime, prepared.ID, request.Owner)
	return launcher.QwenDaemonPrepareResult{
		SessionID: prepared.ID, Cwd: request.Cwd, InputPath: inputPath, EventsPath: eventsPath, Capability: capability,
		LaunchPreference: request.LaunchPreference, ExpectedInitialMode: request.ExpectedInitialMode,
	}, nil
}

func inheritQwenResumeRequest(
	request launcher.QwenDaemonPrepareRequest,
	selected daemonpkg.ManagedAttachment,
) launcher.QwenDaemonPrepareRequest {
	request.SessionID, request.Cwd = selected.NativeSessionID, selected.Cwd
	if !request.GroupsSpecified {
		request.Groups = append([]string(nil), selected.Groups...)
	}
	if !request.PermissionSpecified {
		request.LaunchPreference = selected.LaunchIntent
		if request.LaunchPreference == "" && selected.PermissionMode == "bypassPermissions" {
			request.LaunchPreference = "yolo"
		}
		request.ExpectedInitialMode = qwenInitialModeForPreference(request.LaunchPreference)
	}
	return request
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

func resolveQwenDaemonResume(runtime *daemonpkg.Runtime, request launcher.QwenDaemonPrepareRequest) (daemonpkg.ManagedAttachment, error) {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return daemonpkg.ManagedAttachment{}, err
	}
	selector := strings.TrimSpace(request.ResumeTarget)
	matches := make([]daemonpkg.ManagedAttachment, 0, 1)
	for _, attachment := range snapshot.Catalog.Attachments {
		if attachment.Product != "qwen" || attachment.ProfileIdentity != request.Profile.Fingerprint ||
			(attachment.NativeSessionID != selector && attachment.ID != selector) {
			continue
		}
		if attachment.State != "detached" {
			return daemonpkg.ManagedAttachment{}, errors.New("managed Qwen session is already live")
		}
		matches = append(matches, attachment)
	}
	if len(matches) == 0 {
		return daemonpkg.ManagedAttachment{}, fmt.Errorf("no managed Qwen session matches %q", selector)
	}
	if len(matches) != 1 {
		return daemonpkg.ManagedAttachment{}, fmt.Errorf("managed Qwen session %q is ambiguous; use an exact session UUID", selector)
	}
	return matches[0], nil
}

func qwenEvidence(pending *qwenPending) (daemonpkg.NativeEvidence, error) {
	input, err := federator.QwenArtifactAttestationForPath(pending.input.Path)
	if err != nil || input.Device != pending.input.Device || input.Inode != pending.input.Inode {
		return daemonpkg.NativeEvidence{}, errors.New("qwen input artifact changed while capturing evidence")
	}
	events, err := federator.QwenArtifactAttestationForPath(pending.events.Path)
	if err != nil || events.Device != pending.events.Device || events.Inode != pending.events.Inode {
		return daemonpkg.NativeEvidence{}, errors.New("qwen event artifact changed while capturing evidence")
	}
	return daemonpkg.NativeEvidence{
		Process: pending.request.Owner, Executable: pending.request.QwenBin,
		ThreadID: pending.request.SessionID, ArtifactPath: pending.input.Path,
		RegistryPath:     pending.events.Path,
		ArtifactRevision: input.Fingerprint + ":" + events.Fingerprint,
		ArtifactDevice:   pending.input.Device,
		ArtifactInode:    pending.input.Inode,
		ArtifactPrefix:   input.Fingerprint,
		ArtifactBytes:    input.Size,
		RegistryDevice:   pending.events.Device,
		RegistryInode:    pending.events.Inode,
		RegistryPrefix:   events.Fingerprint,
		RegistryBytes:    events.Size,
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
				if !qwenOwnerExecReady(pending) ||
					!federator.QwenArtifactIdentityMatches(pending.input) || !federator.QwenArtifactIdentityMatches(pending.events) {
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
	return strings.Contains(joined, "\x00"+pending.input.Path+"\x00") &&
		strings.Contains(joined, "\x00"+pending.events.Path+"\x00") &&
		strings.Contains(joined, "\x00"+pending.request.SessionID+"\x00")
}

func (c *hostCoordinator) refreshQwenAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
	c.mu.Lock()
	pending := c.qwenPending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return refreshDurableQwenAttachment(c.stateRoot, attachment)
	}
	if procinfo.ObserveIdentity(pending.request.Owner).Status != procinfo.IdentityMatches ||
		!qwenOwnerExecReady(pending) || !federator.QwenArtifactIdentityMatches(pending.input) ||
		!federator.QwenArtifactIdentityMatches(pending.events) {
		return daemonpkg.NativeEvidence{}, errors.New("qwen native owner or dual-output artifacts are not live")
	}
	return qwenEvidence(pending)
}

//nolint:gocyclo // Refresh revalidates each native process and exact dual-output artifact independently.
func refreshDurableQwenAttachment(stateRoot string, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
	evidence := attachment.Evidence
	root := filepath.Join(stateRoot, "native", "qwen", attachment.NativeSessionID)
	expectedInput := filepath.Join(root, "input.jsonl")
	expectedEvents := filepath.Join(root, "events.jsonl")
	if procinfo.ObserveIdentity(evidence.Process).Status != procinfo.IdentityMatches ||
		evidence.ThreadID != attachment.NativeSessionID || !filepath.IsAbs(evidence.ArtifactPath) ||
		!filepath.IsAbs(evidence.RegistryPath) || evidence.ArtifactPath != expectedInput ||
		evidence.RegistryPath != expectedEvents {
		return daemonpkg.NativeEvidence{}, errors.New("qwen native owner or dual-output artifacts are not live")
	}
	input, inputErr := federator.QwenArtifactIdentityForPath(evidence.ArtifactPath)
	events, eventsErr := federator.QwenArtifactIdentityForPath(evidence.RegistryPath)
	if inputErr != nil || eventsErr != nil ||
		(evidence.ArtifactDevice != 0 && (input.Device != evidence.ArtifactDevice || input.Inode != evidence.ArtifactInode)) ||
		(evidence.RegistryDevice != 0 && (events.Device != evidence.RegistryDevice || events.Inode != evidence.RegistryInode)) ||
		(evidence.ArtifactPrefix != "" && !federator.QwenArtifactPrefixMatches(
			evidence.ArtifactPath, evidence.ArtifactPrefix, evidence.ArtifactBytes,
		)) ||
		(evidence.RegistryPrefix != "" && !federator.QwenArtifactPrefixMatches(
			evidence.RegistryPath, evidence.RegistryPrefix, evidence.RegistryBytes,
		)) {
		return daemonpkg.NativeEvidence{}, errors.New("qwen dual-output artifact identity changed")
	}
	args, err := procinfo.Args(evidence.Process.PID)
	if err != nil {
		return daemonpkg.NativeEvidence{}, errors.New("qwen native owner arguments are unavailable")
	}
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	for _, value := range []string{evidence.ArtifactPath, evidence.RegistryPath, attachment.NativeSessionID} {
		if !strings.Contains(joined, "\x00"+value+"\x00") {
			return daemonpkg.NativeEvidence{}, errors.New("qwen native owner is not attached to its daemon artifacts")
		}
	}
	inputCheckpoint, err := federator.QwenArtifactAttestationForPath(evidence.ArtifactPath)
	if err != nil {
		return daemonpkg.NativeEvidence{}, errors.New("qwen input artifact changed while checkpointing")
	}
	eventsCheckpoint, err := federator.QwenArtifactAttestationForPath(evidence.RegistryPath)
	if err != nil {
		return daemonpkg.NativeEvidence{}, errors.New("qwen event artifact changed while checkpointing")
	}
	// Older v2 development records retained only the launch-time content
	// fingerprint. Upgrade them after exact owner, argv, path, and append-only
	// prefix corroboration so inode recycling cannot disguise replacement.
	evidence.ArtifactDevice, evidence.ArtifactInode = input.Device, input.Inode
	evidence.RegistryDevice, evidence.RegistryInode = events.Device, events.Inode
	evidence.ArtifactPrefix, evidence.ArtifactBytes = inputCheckpoint.Fingerprint, inputCheckpoint.Size
	evidence.RegistryPrefix, evidence.RegistryBytes = eventsCheckpoint.Fingerprint, eventsCheckpoint.Size
	evidence.ArtifactRevision = inputCheckpoint.Fingerprint + ":" + eventsCheckpoint.Fingerprint
	return evidence, nil
}

func (c *hostCoordinator) cleanupQwenAttachment(_ context.Context, attachment daemonpkg.ManagedAttachment) error {
	c.mu.Lock()
	pending := c.qwenPending[attachment.ID]
	c.mu.Unlock()
	if pending == nil {
		return cleanupDurableQwenAttachment(c.stateRoot, attachment)
	}
	if pending.writer != nil {
		_ = pending.writer.Close()
		pending.writer = nil
	}
	if procinfo.Read(pending.request.Owner.PID).Status != procinfo.Absent {
		return errors.New("native Qwen PID is not absent during cleanup")
	}
	if err := removeQwenRoot(pending.root, []federator.QwenArtifactAttestation{pending.input, pending.events}); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.qwenPending, attachment.ID)
	c.mu.Unlock()
	return nil
}

func cleanupDurableQwenAttachment(stateRoot string, attachment daemonpkg.ManagedAttachment) error {
	if procinfo.Read(attachment.Evidence.Process.PID).Status != procinfo.Absent {
		return errors.New("native Qwen PID is not absent during cleanup")
	}
	root := filepath.Join(stateRoot, "native", "qwen", attachment.NativeSessionID)
	input := federator.QwenArtifactAttestation{
		Path: attachment.Evidence.ArtifactPath, Device: attachment.Evidence.ArtifactDevice, Inode: attachment.Evidence.ArtifactInode,
		Fingerprint: attachment.Evidence.ArtifactPrefix, Size: attachment.Evidence.ArtifactBytes,
	}
	events := federator.QwenArtifactAttestation{
		Path: attachment.Evidence.RegistryPath, Device: attachment.Evidence.RegistryDevice, Inode: attachment.Evidence.RegistryInode,
		Fingerprint: attachment.Evidence.RegistryPrefix, Size: attachment.Evidence.RegistryBytes,
	}
	if input.Device == 0 || input.Inode == 0 || events.Device == 0 || events.Inode == 0 ||
		input.Path != filepath.Join(root, "input.jsonl") || events.Path != filepath.Join(root, "events.jsonl") {
		return errors.New("qwen durable cleanup identity is incomplete")
	}
	return removeQwenRoot(root, []federator.QwenArtifactAttestation{input, events})
}

func removeQwenRoot(root string, artifacts []federator.QwenArtifactAttestation) error {
	for _, artifact := range artifacts {
		if filepath.Dir(artifact.Path) != root || !federator.QwenArtifactIdentityMatches(artifact) {
			return errors.New("qwen protocol artifact identity changed before cleanup")
		}
		if err := os.Remove(artifact.Path); err != nil && !os.IsNotExist(err) {
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
