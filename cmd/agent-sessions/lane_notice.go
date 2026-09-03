package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

const (
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

func (c *hostCoordinator) publishLaneTerminalNotice(runtime *daemonpkg.Runtime, actor *laneActor) error {
	if actor == nil || actor.state != "terminal" || actor.parentID == "" || actor.turnID == "" {
		return nil
	}
	c.noticeMu.Lock()
	defer c.noticeMu.Unlock()
	noticeID := laneTerminalNoticeID(actor.id, actor.turnID)
	deliveryCtx, cancel := context.WithTimeout(c.ctx, laneNoticeDeliveryTimeout)
	defer cancel()
	return c.presentLaneTerminalNotice(deliveryCtx, runtime, *actor, noticeID)
}

func (c *hostCoordinator) presentLaneTerminalNotice(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	actor laneActor,
	noticeID string,
) error {
	hostID := strings.TrimSpace(runtime.HostID())
	remoteParent := strings.Contains(actor.parentID, "/")
	body := laneTerminalNoticeBody(actor, map[bool]string{true: hostID}[remoteParent], false)
	if !remoteParent {
		target, ok, targetErr := runtime.Attachments().ActiveAttachment(actor.parentID)
		if targetErr != nil {
			return targetErr
		}
		if !ok {
			return errors.New("lane parent is no longer an active local attachment")
		}
		message := productruntime.NativeMessage{
			ID: noticeID, Body: body,
			From: productruntime.NativeMessageSource{
				UUID: actor.nativeID, Name: actor.name, Product: actor.product,
				Groups: append([]string(nil), actor.groups...),
			},
		}
		return c.deliverPreparedMessage(ctx, target, message)
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
