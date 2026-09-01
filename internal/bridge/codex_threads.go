package bridge

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/pathidentity"
)

var exactLaunchThreadIDRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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

func firstListedPreparedLaunchTarget(client *appServerClient, target string, archived map[string]bool) (appThread, bool, error) {
	var match appThread
	found := false
	seen := map[string]bool{}
	err := visitPreparedThreads(client, false, func(thread appThread) {
		if found || thread.Name != target || archived[thread.ID] || seen[thread.ID] || validatePreparedRootThread(thread) != nil {
			return
		}
		seen[thread.ID] = true
		match, found = thread, true
	})
	return match, found, err
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

func validatePreparedRootThread(thread appThread) error {
	if thread.ParentThreadID != "" || !rootThreadSource(thread.Source) {
		return fmt.Errorf("codex thread %s is not an interactive root and cannot be resumed as a peer", thread.ID)
	}
	return nil
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
