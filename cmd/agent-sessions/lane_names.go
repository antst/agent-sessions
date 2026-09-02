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
	if !active || c.laneNamesLoaded[parent.ID] {
		c.mu.Unlock()
		return nil
	}
	c.laneNamesLoaded[parent.ID] = true
	c.mu.Unlock()

	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	candidates, err := engine.Candidates(parent.ID, product)
	if err != nil {
		c.mu.Lock()
		delete(c.laneNamesLoaded, parent.ID)
		c.mu.Unlock()
		return err
	}
	confirmed := make([]laneNameEntry, 0, len(candidates))
	for _, candidate := range candidates {
		entry, ok := c.resolveCandidate(ctx, runtime, parent, candidate)
		if !ok {
			continue
		}
		entry.UUID = candidate.NativeSessionID
		entry.Product = candidate.Product
		entry.Parent = candidate.Parent
		entry.Groups = candidateLaneGroups(candidate)
		entry.SecondaryGroups = append([]string(nil), candidate.SecondaryGroups...)
		if entry.Name == "" {
			entry.Name = candidate.NativeSessionID
		}
		if entry.Cwd == "" {
			entry.Cwd = parent.Cwd
		}
		confirmed = append(confirmed, entry)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, stillActive := c.liveReports[parent.ID]; !stillActive {
		delete(c.laneNames, parent.ID)
		delete(c.laneNamesLoaded, parent.ID)
		return nil
	}
	if c.laneNames[parent.ID] == nil {
		c.laneNames[parent.ID] = map[string]laneNameEntry{}
	}
	for _, entry := range confirmed {
		c.laneNames[parent.ID][entry.UUID] = entry
	}
	return nil
}

func candidateLaneGroups(candidate daemonpkg.LaneCandidate) []string {
	return uniqueStrings(append(
		append([]string{candidate.PrimaryGroup}, candidate.SecondaryGroups...),
		candidate.PrimaryGroup+"/"+candidate.NativeSessionID,
	))
}
