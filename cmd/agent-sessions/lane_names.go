package main

import (
	"context"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func (c *hostCoordinator) ensureActiveLaneNames(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	product string,
) error {
	c.mu.Lock()
	_, active := c.liveReports[parent.ID]
	if !active {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	candidates, err := engine.Candidates(product)
	if err != nil {
		return err
	}
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return err
	}
	type confirmedLane struct {
		entry     laneNameEntry
		candidate daemonpkg.LaneCandidate
	}
	confirmed := make([]confirmedLane, 0, len(candidates))
	for _, candidate := range candidates {
		if !groupsIntersect(parentGroups, candidateLaneGroups(candidate)) {
			continue
		}
		entry, ok := c.resolveCandidate(ctx, runtime, parent, candidate)
		if !ok {
			continue
		}
		entry.UUID = candidate.NativeSessionID
		entry.Product = candidate.Product
		if entry.Name == "" {
			entry.Name = candidate.NativeSessionID
		}
		confirmed = append(confirmed, confirmedLane{entry: entry, candidate: candidate})
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, stillActive := c.liveReports[parent.ID]; !stillActive {
		delete(c.laneNames, parent.ID)
		return nil
	}
	if c.laneNames[parent.ID] == nil {
		c.laneNames[parent.ID] = map[string]laneNameEntry{}
	}
	for _, confirmed := range confirmed {
		entry := confirmed.entry
		c.laneNames[parent.ID][entry.UUID] = entry
		found := false
		for _, actor := range c.lanes {
			if actor.product == entry.Product && actor.nativeID == entry.UUID {
				found = true
				break
			}
		}
		if found {
			continue
		}
		c.lanes[entry.UUID] = &laneActor{
			id: entry.UUID, nativeID: entry.UUID, product: entry.Product,
			name: entry.Name, parentID: parent.ID,
			groups:         candidateLaneGroups(confirmed.candidate),
			explicitGroups: append([]string(nil), confirmed.candidate.SecondaryGroups...),
			state:          "archived", done: make(chan struct{}),
		}
	}
	return nil
}

func candidateLaneGroups(candidate daemonpkg.LaneCandidate) []string {
	return uniqueStrings(append(
		append([]string{candidate.PrimaryGroup}, candidate.SecondaryGroups...),
		candidate.PrimaryGroup+"/"+candidate.NativeSessionID,
	))
}
