package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

const (
	laneNoticeRetryInterval   = time.Second
	laneNoticeDeliveryTimeout = 10 * time.Second
	laneNoticeSettleDelay     = 150 * time.Millisecond
)

func laneTerminalNoticeID(laneID, turnID string) string {
	return "lane-terminal-" + sessionkey.FromID(laneID+"\x00"+turnID)
}

func laneTerminalNoticeBody(actor laneActor, destinationHost string, collectionRequired bool) string {
	body := fmt.Sprintf(
		"%s_LANE_TERMINAL notice=%s name=%s session=%s turn=%s status=%s outcome=%s exit=%v collection=%s",
		strings.ToUpper(actor.product), laneTerminalNoticeID(actor.id, actor.turnID), actor.name,
		actor.id, actor.turnID, actor.outcome, actor.outcome, laneOutcomeExit(actor.outcome),
		map[bool]string{true: "required", false: "not_required"}[collectionRequired],
	)
	if !collectionRequired {
		return body
	}
	hint := fmt.Sprintf("agent_sessions.lane product=%s command=wait arguments=[%q]", actor.product, actor.id)
	if destinationHost != "" {
		hint = fmt.Sprintf("agent_sessions.lane host=%s product=%s command=wait arguments=[%q]", destinationHost, actor.product, actor.id)
	}
	return body + "\nCollection hint: " + hint
}

func (c *hostCoordinator) queueLaneTerminalNotice(runtime *daemonpkg.Runtime, actor *laneActor) {
	if actor == nil {
		return
	}
	copyActor := *actor
	copyActor.groups = append([]string(nil), actor.groups...)
	go func() {
		timer := time.NewTimer(laneNoticeSettleDelay)
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			_ = c.publishLaneTerminalNotice(runtime, &copyActor)
		}
	}()
}

func (c *hostCoordinator) replayLaneTerminalNotices(runtime *daemonpkg.Runtime) {
	c.mu.Lock()
	actors := make([]laneActor, 0, len(c.lanes))
	for _, actor := range c.lanes {
		if actor.state == "terminal" && actor.parentID != "" {
			copyActor := *actor
			copyActor.groups = append([]string(nil), actor.groups...)
			actors = append(actors, copyActor)
		}
	}
	c.mu.Unlock()
	for index := range actors {
		_ = c.publishLaneTerminalNotice(runtime, &actors[index])
	}
}

func (c *hostCoordinator) publishLaneTerminalNotice(runtime *daemonpkg.Runtime, actor *laneActor) error {
	if actor == nil || actor.state != "terminal" || actor.parentID == "" || actor.turnID == "" {
		return nil
	}
	c.noticeMu.Lock()
	defer c.noticeMu.Unlock()
	noticeID := laneTerminalNoticeID(actor.id, actor.turnID)
	acknowledged, err := c.prepareLaneTerminalDelivery(runtime, *actor, noticeID)
	if err != nil || acknowledged {
		return err
	}
	deliveryCtx, cancel := context.WithTimeout(c.ctx, laneNoticeDeliveryTimeout)
	defer cancel()
	if err := c.presentLaneTerminalNotice(deliveryCtx, runtime, *actor, noticeID); err != nil {
		_ = c.setLaneTerminalDeliveryState(runtime, noticeID, "retryable", "destination-unavailable")
		return err
	}
	if err := c.setLaneTerminalDeliveryState(runtime, noticeID, "presented", ""); err != nil {
		return err
	}
	return c.setLaneTerminalDeliveryState(runtime, noticeID, "acknowledged", "")
}

func (c *hostCoordinator) prepareLaneTerminalDelivery(
	runtime *daemonpkg.Runtime,
	actor laneActor,
	noticeID string,
) (bool, error) {
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return false, err
	}
	return engine.PrepareTerminalNotice(daemonpkg.Delivery{
		ID: noticeID, CorrelationID: actor.turnID, Sender: actor.id,
		Destinations: []string{actor.parentID}, Groups: append([]string(nil), actor.groups...),
		SentAt: actor.completedAt, State: "accepted",
	})
}

func (c *hostCoordinator) setLaneTerminalDeliveryState(
	runtime *daemonpkg.Runtime,
	noticeID, state, retryCause string,
) error {
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	return engine.TransitionTerminalNotice(noticeID, state, retryCause)
}

func (c *hostCoordinator) presentLaneTerminalNotice(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	actor laneActor,
	noticeID string,
) error {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	// The durable Turn is the single terminal outcome authority. In particular,
	// a receipt/native-acceptance CAS ambiguity committed after the product
	// process exits must not be presented as a process-local completed/exit-0
	// notice.
	actor = durableLaneTerminalNoticeActor(snapshot, actor)
	hostID := strings.TrimSpace(snapshot.Catalog.Host.Host)
	remoteParent := strings.Contains(actor.parentID, "/")
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	collectionRequired, err := engine.HasCollectionDebt(actor.id)
	if err != nil {
		return err
	}
	body := laneTerminalNoticeBody(actor, map[bool]string{true: hostID}[remoteParent], collectionRequired)
	if !remoteParent {
		target, ok, targetErr := runtime.Attachments().ActiveAttachment(actor.parentID)
		if targetErr != nil {
			return targetErr
		}
		if !ok {
			return errors.New("lane parent is no longer an active local attachment")
		}
		mode := "prompting"
		if actor.permission == "bypassPermissions" {
			mode = "bypass"
		}
		message := bridge.WrapPeerMessage(
			actor.product, "session:"+actor.id, actor.nativeID, actor.name, mode,
			noticeID, time.Now().UTC().Format(time.RFC3339Nano), body,
		)
		return c.deliverPreparedMessage(ctx, target, noticeID, "session:"+actor.id, message)
	}
	c.mu.Lock()
	host := c.federation
	c.mu.Unlock()
	if host == nil {
		return errors.New("daemon federation component is unavailable")
	}
	var target federationpkg.Peer
	found := false
	for _, candidate := range host.RemotePeers() {
		if candidate.ID == actor.parentID {
			target, found = candidate, true
			break
		}
	}
	if !found {
		return errors.New("remote lane parent is not connected")
	}
	source, err := federationpkg.BuildPeer(
		hostID, daemonSetting("AGENT_SESSIONS_HOST_NAME"), actor.id, actor.name, "idle", actor.cwd,
		actor.product, actor.permission, actor.product+":"+actor.id, actor.parentID, actor.groups,
	)
	if err != nil {
		return err
	}
	return host.Send(ctx, source, target, noticeID, body, "")
}

func durableLaneTerminalNoticeActor(snapshot daemonpkg.StateSnapshot, actor laneActor) laneActor {
	if turn, ok := snapshot.Catalog.Turns[actor.turnID]; ok && turn.LaneID == actor.id &&
		(turn.State == "terminal" || turn.State == "collected") {
		actor.outcome, actor.result, actor.failure = turn.Outcome, turn.Result, turn.Diagnostic
		actor.startedAt, actor.deadlineAt, actor.completedAt = turn.StartedAt, turn.DeadlineAt, turn.CompletedAt
	}
	return actor
}
