package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	federator "github.com/antst/agent-sessions/internal/attachmentcontrol"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/pathidentity"
)

var exactLaunchThreadIDRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const (
	preparedPublicationTimeout = 60 * time.Second
	preparedAbortTimeout       = 45 * time.Second
)

//nolint:gocyclo // Resume binding keeps its resolution, publication, and rollback transaction linear.
func bindPreparedResumeNative(argv []string) (threadID, effectiveCwd string, resultErr error) {
	options, err := parseLaunchOptions(argv)
	if err != nil {
		return "", "", err
	}
	for option := range options {
		if option != "target" && option != "cwd" && option != "owner-pid" && option != "owner-proc-start" &&
			option != "cwd-explicit" && option != "approval-policy" && option != "sandbox" && !agentLaunchOption(option) {
			return "", "", fmt.Errorf("unknown bind option --%s", option)
		}
	}
	requestedCwd, err := canonicalLaunchDirectory(options["cwd"])
	if err != nil {
		return "", "", err
	}
	if value := options["cwd-explicit"]; value != "" && value != "true" && value != "false" {
		return "", "", errors.New("bind --cwd-explicit must be true or false")
	}
	ownerPID, ownerStart, err := validatedLaunchOwner(options)
	if err != nil {
		return "", "", err
	}
	target := strings.TrimSpace(options["target"])
	if target == "" {
		return "", "", errors.New("bind requires --target")
	}
	paths := resolveNativePaths()
	client, err := dialLaunchAppServer(paths)
	if err != nil {
		return "", "", err
	}
	defer client.close()
	thread, err := resolvePreparedLaunchTarget(client, paths, target)
	if err != nil {
		return "", "", err
	}
	if err := validatePreparedRootThread(thread); err != nil {
		return "", "", err
	}
	resolved, managed, err := resolvePreparedPeerPreferences(thread.ID, options)
	if err != nil {
		return "", "", err
	}
	if managed {
		applyResolvedApproval(options, resolved.Preference.AlwaysApprove)
	}
	// Match native `codex resume`: every invocation uses the launcher's
	// effective cwd (the shell cwd unless -C overrides it). Codex retains the
	// original cwd in thread metadata, so that persisted value is not a resume
	// default and may legitimately name a directory that has since moved.
	detachLoadedForCwd := resumeCwdChanged(thread.Cwd, requestedCwd)
	staleLoadedOwner, err := releaseStaleLoadedInteractiveOwner(client, paths, thread.ID, detachLoadedForCwd)
	if err != nil {
		return "", "", err
	}
	record, err := bindInteractiveOwner(client, paths, thread, ownerPID, ownerStart, requestedCwd, staleLoadedOwner)
	if err != nil {
		return "", "", err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		_, cleanupErr := requestControl(paths.supervisorSock, map[string]any{
			"action": "abort_prepared", "sessionId": thread.ID, "requestId": record.RequestID,
			"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
		}, preparedAbortTimeout)
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unpublish failed Codex peer resume %s: %w", thread.ID, cleanupErr))
		}
	}()
	publication := map[string]any{
		"action": "register_prepared", "sessionId": thread.ID, "requestId": record.RequestID,
		"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
		"cwd": record.Cwd, "name": record.Name, "nameSource": record.NameSource,
		"status": "idle",
	}
	putIf(publication, "agentRuntimeDir", options["agent-runtime-dir"])
	putIf(publication, "approvalPolicy", options["approval-policy"])
	putIf(publication, "sandbox", options["sandbox"])
	published, err := requestControl(paths.supervisorSock, publication, preparedPublicationTimeout)
	if err != nil {
		return "", "", fmt.Errorf("publish resumed Codex peer: %w", err)
	}
	state, _ := published["state"].(map[string]any)
	if stringValue(state["sessionId"]) != thread.ID {
		return "", "", errors.New("resumed Codex peer publication returned mismatched state")
	}
	return thread.ID, record.Cwd, nil
}

// releaseStaleLoadedInteractiveOwner closes the deterministic gap between a
// normal TUI exit and the supervisor's next reconciliation tick. An attached
// stale owner is released through the supervisor. A stale zero-turn prepared
// owner is returned as a one-use takeover proof: Codex supports resuming that
// still-loaded thread, while archive/unarchive races with the replacement TUI
// and moves its rollout out of the live session tree.
func resumeCwdChanged(savedCwd, requestedCwd string) bool {
	canonicalSaved, err := canonicalLaunchDirectory(savedCwd)
	return err != nil || canonicalSaved != requestedCwd
}

func releaseStaleLoadedInteractiveOwner(client *appServerClient, paths nativePaths, threadID string, detachForCwdOverride bool) (*interactiveOwnerRecord, error) {
	loaded, err := loadedPreparedThreads(client)
	if err != nil {
		return nil, err
	}
	if !loaded[threadID] {
		return nil, nil
	}
	owner := readInteractiveOwner(paths, threadID)
	if owner == nil {
		return nil, fmt.Errorf("codex thread %s is already loaded; close its existing client before resuming it as a peer", threadID)
	}
	switch exactProcessIdentityStatus(owner.OwnerPID, owner.OwnerProcStart).Status {
	case processIdentityMatches:
		return nil, fmt.Errorf("codex thread %s is already loaded by its live peer owner", threadID)
	case processIdentityUnknown:
		return nil, fmt.Errorf("cannot currently corroborate the loaded owner of Codex thread %s", threadID)
	case processIdentityStale:
	}
	if owner.Pending {
		if !owner.Prepared || !owner.ParkOnAbort {
			return nil, fmt.Errorf("codex thread %s has an incomplete stale launch owner", threadID)
		}
		if detachForCwdOverride {
			if _, err := requestControl(paths.supervisorSock, map[string]any{
				"action": "detach_stale_prepared", "sessionId": threadID, "requestId": owner.RequestID,
				"ownerPid": owner.OwnerPID, "ownerProcStart": owner.OwnerProcStart,
			}, preparedAbortTimeout); err != nil {
				return nil, fmt.Errorf("detach exited Codex peer %s for cwd override: %w", threadID, err)
			}
		}
		return owner, nil
	}
	if _, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "release_stale_interactive", "sessionId": threadID,
	}, preparedAbortTimeout); err != nil {
		return nil, fmt.Errorf("release exited Codex peer %s: %w", threadID, err)
	}
	loaded, err = loadedPreparedThreads(client)
	if err != nil {
		return nil, err
	}
	if loaded[threadID] {
		return nil, fmt.Errorf("exited Codex peer %s remained loaded after release", threadID)
	}
	return nil, nil
}

//nolint:gocyclo // Binding fails closed across archived, loaded, cwd, lane, and owner conflicts.
func bindInteractiveOwner(client *appServerClient, paths nativePaths, thread appThread, ownerPID int, ownerStart, effectiveCwd string, staleLoadedOwner *interactiveOwnerRecord) (*interactiveOwnerRecord, error) {
	if !validSessionID(thread.ID) {
		return nil, errors.New("cannot bind an invalid Codex thread id")
	}
	if err := validatePreparedRootThread(thread); err != nil {
		return nil, err
	}
	lifecycleLock, err := lockLaneLifecycle(paths, thread.ID)
	if err != nil {
		return nil, err
	}
	defer unlockLaneLifecycle(lifecycleLock)
	archived, err := listThreadMembership(client, true)
	if err != nil {
		return nil, err
	}
	if archived[thread.ID] || isRetiredThreadNative(paths, thread.ID) {
		return nil, fmt.Errorf("codex thread %s is archived; unarchive it before resuming", thread.ID)
	}
	loaded, err := loadedPreparedThreads(client)
	if err != nil {
		return nil, err
	}
	if loaded[thread.ID] && staleLoadedOwner == nil {
		return nil, fmt.Errorf("codex thread %s is already loaded; close its existing client before resuming it as a peer", thread.ID)
	}
	if lane, laneErr := readLaneStateFile(paths, thread.ID); laneErr == nil && lane.Type == "codex-peer-lane" {
		return nil, fmt.Errorf("codex thread %s is governed by codex-peer-lane", thread.ID)
	}
	if current := readInteractiveOwner(paths, thread.ID); current != nil {
		switch exactProcessIdentityStatus(current.OwnerPID, current.OwnerProcStart).Status {
		case processIdentityMatches:
			return nil, fmt.Errorf("codex thread %s already has a live interactive owner", thread.ID)
		case processIdentityUnknown:
			return nil, fmt.Errorf("cannot currently corroborate the existing owner of Codex thread %s", thread.ID)
		case processIdentityStale:
			if loaded[thread.ID] && (!sameInteractiveOwner(current, staleLoadedOwner) || !current.Pending || !current.Prepared || !current.ParkOnAbort) {
				return nil, fmt.Errorf("codex thread %s loaded-owner takeover proof changed before binding", thread.ID)
			}
			removeInteractiveOwnerIfMatching(paths, thread.ID, current)
		}
	} else if loaded[thread.ID] {
		return nil, fmt.Errorf("codex thread %s loaded-owner takeover proof disappeared before binding", thread.ID)
	}
	record := &interactiveOwnerRecord{
		ThreadID: thread.ID, RequestID: randomID(), OwnerPID: ownerPID, OwnerProcStart: ownerStart,
		Pending: true, Prepared: true, ResumeLoaded: loaded[thread.ID], Cwd: effectiveCwd,
		Name: thread.Name, NameSource: map[bool]string{true: "codex", false: "generated"}[thread.Name != ""],
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeInteractiveOwnerRecord(paths, *record); err != nil {
		return nil, err
	}
	return record, nil
}

type launchIndexEntry struct {
	ID        string
	Name      string
	UpdatedAt time.Time
}

type indexedLaunchCandidate struct {
	thread  appThread
	recency time.Time
	order   int
}

func resolvePreparedLaunchTarget(client *appServerClient, paths nativePaths, target string) (appThread, error) {
	if exactLaunchThreadIDRE.MatchString(target) {
		thread, err := readExactPreparedThread(client, target)
		if err != nil {
			return appThread{}, err
		}
		if err := validatePreparedRootThread(thread); err != nil {
			return appThread{}, err
		}
		return thread, nil
	}
	archived, err := listThreadMembership(client, true)
	if err != nil {
		return appThread{}, err
	}
	retired := readRetiredThreads(paths)
	listed, found, err := firstListedPreparedLaunchTarget(client, target, archived, retired)
	if err != nil {
		return appThread{}, err
	}
	// Codex resolves an exact name to the first usable App Server result in
	// recency order. Preserve that ordering instead of treating duplicates as
	// an ambiguity that the native CLI never presents.
	if found {
		return listed, nil
	}
	// The local session index is only a fallback for zero-turn or otherwise
	// unlisted sessions. A corrupt historical line must not block an exact name
	// the live App Server can already resolve.
	index, err := readLaunchSessionIndex(paths)
	if err != nil {
		return appThread{}, err
	}
	return resolveIndexedPreparedLaunchTarget(client, index, target, archived, retired)
}

func firstListedPreparedLaunchTarget(
	client *appServerClient,
	target string,
	archived, retired map[string]bool,
) (appThread, bool, error) {
	var match appThread
	found := false
	seen := map[string]bool{}
	err := visitPreparedThreads(client, false, func(thread appThread) {
		if found || thread.Name != target || archived[thread.ID] || retired[thread.ID] || seen[thread.ID] ||
			validatePreparedRootThread(thread) != nil {
			return
		}
		seen[thread.ID] = true
		match, found = thread, true
	})
	return match, found, err
}

func resolveIndexedPreparedLaunchTarget(
	client *appServerClient,
	index []launchIndexEntry,
	target string,
	archived, retired map[string]bool,
) (appThread, error) {
	indexed := []indexedLaunchCandidate{}
	seen := map[string]bool{}
	for position := len(index) - 1; position >= 0; position-- {
		entry := index[position]
		// Name records are append-only. The first row seen from the end is
		// the thread's current name; an older alias must not remain resumable.
		if seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		if entry.Name != target || archived[entry.ID] || retired[entry.ID] {
			continue
		}
		thread, readErr := readExactPreparedThread(client, entry.ID)
		if isPreparedThreadNotFound(readErr) {
			continue
		}
		if readErr != nil {
			return appThread{}, readErr
		}
		if validatePreparedRootThread(thread) != nil {
			continue
		}
		// Codex replaces a used thread's display title with its first prompt,
		// while session_index.jsonl retains the explicit session name. The
		// newest index row is therefore the durable naming authority; thread/read
		// above corroborates the exact UUID and current root-thread state, not the
		// mutable title. Preserve the indexed name for owner publication too.
		thread.Name = entry.Name
		indexed = append(indexed, indexedLaunchCandidate{
			thread: thread, recency: preparedThreadRecency(thread, entry.UpdatedAt), order: position,
		})
	}
	if len(indexed) == 0 {
		return appThread{}, fmt.Errorf("codex thread %q was not found", target)
	}
	sort.SliceStable(indexed, func(left, right int) bool {
		if indexed[left].recency.Equal(indexed[right].recency) {
			return indexed[left].order > indexed[right].order
		}
		return indexed[left].recency.After(indexed[right].recency)
	})
	return indexed[0].thread, nil
}

func preparedThreadRecency(thread appThread, fallback time.Time) time.Time {
	if thread.UpdatedAt > 0 {
		fallback = time.UnixMilli(thread.UpdatedAt)
	}
	if thread.Path != "" {
		if info, err := os.Stat(thread.Path); err == nil {
			fallback = info.ModTime()
		}
	}
	return fallback
}

func readExactPreparedThread(client *appServerClient, threadID string) (appThread, error) {
	var read struct {
		Thread appThread `json:"thread"`
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/read", map[string]any{
		"threadId": threadID, "includeTurns": false,
	}, &read); err != nil {
		return appThread{}, err
	}
	if read.Thread.ID != threadID {
		return appThread{}, fmt.Errorf("thread/read returned %q for %s", read.Thread.ID, threadID)
	}
	return read.Thread, nil
}

func validatePreparedRootThread(thread appThread) error {
	if thread.ParentThreadID != "" || !rootThreadSource(thread.Source) {
		return fmt.Errorf("codex thread %s is not an interactive root and cannot be resumed as a peer", thread.ID)
	}
	return nil
}

func visitPreparedThreads(client *appServerClient, archived bool, visit func(appThread)) error {
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{"archived": archived, "limit": 100, "sortDirection": "desc", "sortKey": "updated_at"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appThread `json:"data"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := requestWithTimeout(client, 30*time.Second, "thread/list", params, &page); err != nil {
			return err
		}
		for _, thread := range page.Data {
			visit(thread)
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return nil
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

func loadedPreparedThreads(client *appServerClient) (map[string]bool, error) {
	var loaded struct {
		Data []string `json:"data"`
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/loaded/list", map[string]any{}, &loaded); err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(loaded.Data))
	for _, threadID := range loaded.Data {
		if validSessionID(threadID) {
			result[threadID] = true
		}
	}
	return result, nil
}

func readLaunchSessionIndex(paths nativePaths) ([]launchIndexEntry, error) {
	path := filepath.Join(paths.codexHome, "session_index.jsonl")
	file, err := os.Open(path) //nolint:gosec // fixed index beneath configured Codex home.
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Codex session index %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	entries := []launchIndexEntry{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		entry := launchIndexEntry{ID: stringValue(row["id"]), Name: stringValue(row["thread_name"])}
		entry.UpdatedAt, _ = time.Parse(time.RFC3339Nano, stringValue(row["updated_at"]))
		if validSessionID(entry.ID) {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Codex session index %s: %w", path, err)
	}
	return entries, nil
}

func isPreparedThreadNotFound(err error) bool {
	if err == nil {
		return false
	}
	if isRolloutMissingRPC(err) {
		return true
	}
	var rpcErr *rpcError
	return errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "not found")
}

func isRolloutMissingRPC(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.Message)
	return strings.Contains(message, "rollout") &&
		(strings.Contains(message, "no rollout") || strings.Contains(message, "not found"))
}

// startPreparedLaunchNative creates only a fresh interactive root. Creation-time
// policy is supplied explicitly; unsupported per-invocation config is rejected
// by the wrapper instead of being applied too late by a remote attachment.
//
//nolint:gocyclo // The linear preparation transaction keeps rollback boundaries explicit.
func startPreparedLaunchNative(argv []string) (threadID string, resultErr error) {
	options, err := parseLaunchOptions(argv)
	if err != nil {
		return "", err
	}
	for option := range options {
		if option != "cwd" && option != "name" && option != "name-source" && option != "owner-pid" && option != "owner-proc-start" &&
			option != "approval-policy" && option != "sandbox" && !agentLaunchOption(option) {
			return "", fmt.Errorf("unknown start option --%s", option)
		}
	}
	cwd, err := canonicalLaunchDirectory(options["cwd"])
	if err != nil {
		return "", err
	}
	ownerPID, ownerStart, err := validatedLaunchOwner(options)
	if err != nil {
		return "", err
	}

	paths := resolveNativePaths()
	client, err := dialLaunchAppServer(paths)
	if err != nil {
		return "", err
	}
	defer client.close()

	created := false
	ownerRecorded := false
	var record interactiveOwnerRecord
	defer func() {
		if resultErr == nil || !created || threadID == "" {
			return
		}
		var cleanupErr error
		if ownerRecorded {
			_, cleanupErr = requestControl(paths.supervisorSock, map[string]any{
				"action": "abort_prepared", "sessionId": threadID, "requestId": record.RequestID,
				"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
			}, preparedAbortTimeout)
		} else {
			cleanupErr = deletePreparedThread(client, threadID)
		}
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove partially prepared thread %s: %w", threadID, cleanupErr))
		}
	}()

	params := map[string]any{"cwd": cwd, "ephemeral": false, "serviceName": "codex-peer"}
	putIf(params, "approvalPolicy", options["approval-policy"])
	putIf(params, "sandbox", options["sandbox"])
	var started struct {
		Thread         appThread `json:"thread"`
		ApprovalPolicy any       `json:"approvalPolicy"`
	}
	if err := requestWithTimeout(client, 60*time.Second, "thread/start", params, &started); err != nil {
		return "", err
	}
	threadID = started.Thread.ID
	created = true
	if !validSessionID(threadID) {
		return threadID, errors.New("invalid thread id returned by App Server")
	}
	resolved, managed, err := resolvePreparedPeerPreferences(threadID, options)
	if err != nil {
		return threadID, err
	}
	if managed && resolved.Preference.AlwaysApprove != (strings.TrimSpace(options["approval-policy"]) == "never") {
		return threadID, errors.New("resolved yolo preference does not match the fresh Codex launch policy")
	}
	approvalPolicy := stringValue(started.ApprovalPolicy)
	if approvalPolicy == "" {
		return threadID, errors.New("thread/start did not report its effective approval policy")
	}
	if expected := strings.TrimSpace(options["approval-policy"]); expected != "" && approvalPolicy != expected {
		return threadID, fmt.Errorf("thread/start applied approval policy %q, expected %q", approvalPolicy, expected)
	}

	name := strings.TrimSpace(options["name"])
	nameSource := strings.TrimSpace(options["name-source"])
	if name == "" {
		if nameSource != "" {
			return threadID, errors.New("start --name-source requires --name")
		}
		name = defaultPeerName(cwd, threadID)
		nameSource = "generated"
	} else {
		if nameSource == "" {
			nameSource = "launch"
		}
		if nameSource != "launch" && nameSource != "explicit" {
			return threadID, fmt.Errorf("unsupported start name source %q", nameSource)
		}
		name = sanitizeName(name)
	}
	// Codex 0.147 materializes a zero-turn rollout only after naming it. This
	// must happen before publishing any owner record or resuming the exact UUID.
	if err := requestWithTimeout(client, 30*time.Second, "thread/name/set", map[string]any{
		"threadId": threadID, "name": name,
	}, nil); err != nil {
		return threadID, err
	}
	record = interactiveOwnerRecord{
		ThreadID: threadID, RequestID: randomID(), OwnerPID: ownerPID,
		OwnerProcStart: ownerStart, Pending: true, Prepared: true, DeleteOnAbort: true, Cwd: cwd, Name: name,
		NameSource: nameSource, UpdatedAt: time.Now().UnixMilli(),
	}
	if err := writeInteractiveOwnerRecord(paths, record); err != nil {
		return threadID, err
	}
	ownerRecorded = true
	permissionMode := permissionModeForApprovalPolicy(approvalPolicy)
	// Remote Codex 0.147 does not emit SessionStart until the first user turn.
	// Publish the exact prepared owner now so a fresh peer is reachable while
	// idle, but leave it pending: if the exec/attachment never materializes,
	// the stale-owner reaper still deletes the zero-turn thread.
	published, err := requestControl(paths.supervisorSock, map[string]any{
		"action": "register_prepared", "sessionId": threadID, "requestId": record.RequestID,
		"ownerPid": record.OwnerPID, "ownerProcStart": record.OwnerProcStart,
		"cwd": cwd, "name": name, "nameSource": nameSource,
		"approvalPolicy": approvalPolicy, "permissionMode": permissionMode, "status": "idle",
		"agentRuntimeDir": options["agent-runtime-dir"],
	}, preparedPublicationTimeout)
	if err != nil {
		return threadID, fmt.Errorf("publish prepared Codex peer: %w", err)
	}
	state, _ := published["state"].(map[string]any)
	if stringValue(state["sessionId"]) != threadID {
		return threadID, errors.New("prepared Codex peer publication returned mismatched state")
	}
	return threadID, nil
}

func dialLaunchAppServer(paths nativePaths) (*appServerClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, err := dialAppServer(ctx, paths.appServerSock)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("connect to shared App Server: %w", err)
	}
	return client, nil
}

func deletePreparedThread(client *appServerClient, threadID string) error {
	if err := requestWithTimeout(client, 15*time.Second, "thread/delete", map[string]any{
		"threadId": threadID,
	}, nil); err != nil {
		if isPreparedThreadNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func validatedLaunchOwner(options map[string]string) (int, string, error) {
	pid, err := strconv.Atoi(options["owner-pid"])
	if err != nil || pid <= 1 {
		return 0, "", errors.New("invalid launch owner pid")
	}
	started := options["owner-proc-start"]
	if !exactProcessIdentityMatch(pid, started) {
		return 0, "", errors.New("cannot corroborate the launch owner process identity")
	}
	return pid, started, nil
}

func canonicalLaunchDirectory(value string) (string, error) {
	if value == "" {
		return "", errors.New("launch requires --cwd")
	}
	canonical, err := pathidentity.ExistingDirectory(value)
	if err != nil {
		return "", fmt.Errorf("resolve launch cwd: %w", err)
	}
	if strings.ContainsAny(canonical, "\r\n") {
		return "", errors.New("launch cwd cannot contain a line break")
	}
	return canonical, nil
}

func agentLaunchOption(option string) bool {
	switch option {
	case "agent-runtime-dir", "groups-json", "groups-specified", "parent-session", "parent-specified",
		"inherit-parent-groups", "inherit-groups-specified", "always-approve", "always-approve-specified":
		return true
	default:
		return false
	}
}

func resolvePreparedPeerPreferences(sessionID string, options map[string]string) (federator.ResolvedPreferences, bool, error) {
	runtimeDir := strings.TrimSpace(options["agent-runtime-dir"])
	if runtimeDir == "" {
		return federator.ResolvedPreferences{}, false, nil
	}
	groups := []string{}
	if err := json.Unmarshal([]byte(defaultString(options["groups-json"], "[]")), &groups); err != nil {
		return federator.ResolvedPreferences{}, true, errors.New("decode peer groups")
	}
	groupsSpecified, err := strictLaunchBool(options, "groups-specified")
	if err != nil {
		return federator.ResolvedPreferences{}, true, err
	}
	parentSpecified, err := strictLaunchBool(options, "parent-specified")
	if err != nil {
		return federator.ResolvedPreferences{}, true, err
	}
	inherit, err := strictLaunchBool(options, "inherit-parent-groups")
	if err != nil {
		return federator.ResolvedPreferences{}, true, err
	}
	inheritSpecified, err := strictLaunchBool(options, "inherit-groups-specified")
	if err != nil {
		return federator.ResolvedPreferences{}, true, err
	}
	alwaysApprove, err := strictLaunchBool(options, "always-approve")
	if err != nil {
		return federator.ResolvedPreferences{}, true, err
	}
	alwaysApproveSpecified, err := strictLaunchBool(options, "always-approve-specified")
	if err != nil {
		return federator.ResolvedPreferences{}, true, err
	}
	resolved, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: sessionID, Product: "codex", Kind: federation.SessionKindInteractive, Groups: groups, GroupsSpecified: groupsSpecified,
		ParentSessionID: options["parent-session"], ParentSpecified: parentSpecified,
		InheritParentGroups: inherit, InheritGroupsSpecified: inheritSpecified,
		AlwaysApprove: alwaysApprove, AlwaysApproveSpecified: alwaysApproveSpecified,
	})
	if err != nil {
		return federator.ResolvedPreferences{}, true, fmt.Errorf("resolve Agent Sessions peer preferences: %w", err)
	}
	return resolved, true, nil
}

func strictLaunchBool(options map[string]string, name string) (bool, error) {
	value := options[name]
	if value == "true" {
		return true, nil
	}
	if value == "false" {
		return false, nil
	}
	return false, fmt.Errorf("--%s must be true or false", name)
}

func applyResolvedApproval(options map[string]string, alwaysApprove bool) {
	if alwaysApprove {
		options["approval-policy"] = "never"
		options["sandbox"] = "danger-full-access"
		return
	}
	if options["always-approve-specified"] == "true" {
		delete(options, "approval-policy")
		delete(options, "sandbox")
	}
}

func parseLaunchOptions(argv []string) (map[string]string, error) {
	options := map[string]string{}
	for len(argv) > 0 {
		if len(argv) < 2 {
			return nil, fmt.Errorf("launch argument %q has no value", argv[0])
		}
		option, value := argv[0], argv[1]
		argv = argv[2:]
		if !strings.HasPrefix(option, "--") || len(option) < 3 {
			return nil, fmt.Errorf("invalid launch argument %q", option)
		}
		options[strings.TrimPrefix(option, "--")] = value
	}
	return options, nil
}
