package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/antst/sessionbus/internal/clihelp"
	daemonpkg "github.com/antst/sessionbus/internal/daemon"
	federationpkg "github.com/antst/sessionbus/internal/federation"
)

const operatorRosterSchema = "agent-sessions.roster.v1"

type operatorRosterHost struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Release             string `json:"release,omitempty"`
	State               string `json:"state"`
	Generation          uint64 `json:"generation"`
	HubConfigured       bool   `json:"hub_configured"`
	FederationConnected bool   `json:"federation_connected"`
}

type operatorRosterEntry struct {
	Kind            string   `json:"kind"`
	Scope           string   `json:"scope"`
	ID              string   `json:"id"`
	LocalID         string   `json:"local_id"`
	NativeSessionID string   `json:"native_session_id,omitempty"`
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name,omitempty"`
	HostID          string   `json:"host_id"`
	HostName        string   `json:"host_name"`
	Product         string   `json:"product"`
	State           string   `json:"state"`
	Live            bool     `json:"live"`
	Cwd             string   `json:"cwd,omitempty"`
	Groups          []string `json:"groups"`
	PermissionMode  string   `json:"permission_mode,omitempty"`
	OwnerSessionID  string   `json:"owner_session_id,omitempty"`
	Persistent      bool     `json:"persistent"`
}

type operatorRosterSummary struct {
	LocalPeers     int `json:"local_peers"`
	LocalLanes     int `json:"local_lanes"`
	RemotePeers    int `json:"remote_peers"`
	RemoteLanes    int `json:"remote_lanes"`
	FederatedHosts int `json:"federated_hosts"`
}

type operatorRosterReport struct {
	Schema         string                `json:"schema"`
	Host           operatorRosterHost    `json:"host"`
	Summary        operatorRosterSummary `json:"summary"`
	Local          []operatorRosterEntry `json:"local"`
	FederatedHosts []federationpkg.Host  `json:"federated_hosts"`
	Remote         []operatorRosterEntry `json:"remote"`
}

func runRoster(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
	set := flag.NewFlagSet("agent-sessions roster", flag.ContinueOnError)
	stateRoot := set.String("state-root", defaultStateRoot(), "durable Agent Sessions state root")
	jsonOutput := set.Bool("json", false, "emit the stable JSON roster contract")
	if err := set.Parse(invocation.Arguments); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("agent-sessions roster received unexpected arguments")
	}
	response, err := callExistingDaemon(ctx, *stateRoot, daemonpkg.ControlRequest{
		ID: commandRequestID(), Role: daemonpkg.RoleAdmin, Operation: "roster",
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New(response.Error.Message)
	}
	if *jsonOutput {
		return writeJSONLine(output, response.Payload)
	}
	var report operatorRosterReport
	if err := json.Unmarshal(response.Payload, &report); err != nil || report.Schema != operatorRosterSchema {
		return errors.New("daemon returned an incompatible operator roster")
	}
	return writeOperatorRoster(output, report)
}

func (c *hostCoordinator) operatorRoster(runtime *daemonpkg.Runtime) (json.RawMessage, error) {
	attachments, err := runtime.Attachments().ListActive()
	if err != nil {
		return nil, err
	}
	displayNames := c.attachmentDisplayNames(runtime, attachments)
	c.mu.Lock()
	federationHost := c.federation
	c.mu.Unlock()
	var remoteHosts []federationpkg.Host
	var remotePeers []federationpkg.Peer
	connected := false
	if federationHost != nil {
		remoteHosts = federationHost.RemoteHosts()
		remotePeers = federationHost.RemotePeers()
		connected = federationHost.Connected()
	}
	hostName := daemonSetting("AGENT_SESSIONS_HOST_NAME")
	hostID := runtime.HostID()
	if hostName == "" {
		hostName = hostID
	}
	report := buildOperatorRoster(
		attachments, runtime.Generation(), hostID, runtime.Release(), hostName, daemonSetting("AGENT_SESSIONS_HUB") != "", connected,
		remoteHosts, remotePeers,
	)
	c.mu.Lock()
	for _, lane := range c.lanes {
		if lane == nil || lane.state == "archived" || lane.state == "retiring" {
			continue
		}
		report.Local = append(report.Local, operatorRosterEntry{
			Kind: "lane", Scope: "local", ID: lane.id, LocalID: lane.id,
			NativeSessionID: lane.nativeID, Name: operatorDefaultString(lane.name, lane.id),
			HostID: hostID, HostName: hostName, Product: lane.product, State: lane.state,
			Live: true, Cwd: lane.cwd, Groups: append([]string(nil), lane.groups...),
			PermissionMode: lane.permission, OwnerSessionID: lane.parentID, Persistent: lane.persistent,
		})
		report.Summary.LocalLanes++
	}
	c.mu.Unlock()
	for index := range report.Local {
		entry := &report.Local[index]
		if entry.Kind != "peer" || !entry.Live {
			continue
		}
		if name := displayNames[entry.ID]; name != "" {
			entry.Name = name
		}
	}
	return json.Marshal(report)
}

func buildOperatorRoster(
	attachments []daemonpkg.ManagedAttachment,
	generation uint64,
	hostID, release string,
	hostName string,
	hubConfigured, federationConnected bool,
	remoteHosts []federationpkg.Host,
	remotePeers []federationpkg.Peer,
) operatorRosterReport {
	if strings.TrimSpace(hostName) == "" {
		hostName = hostID
	}
	report := operatorRosterReport{
		Schema: operatorRosterSchema,
		Host: operatorRosterHost{
			ID: hostID, Name: hostName, Release: release, State: "running",
			Generation: generation, HubConfigured: hubConfigured, FederationConnected: federationConnected,
		},
		Local: make([]operatorRosterEntry, 0), Remote: make([]operatorRosterEntry, 0),
		FederatedHosts: append([]federationpkg.Host(nil), remoteHosts...),
	}
	for _, attachment := range attachments {
		report.Local = append(report.Local, operatorRosterEntry{
			Kind: "peer", Scope: "local", ID: attachment.ID, LocalID: attachment.ID,
			NativeSessionID: attachment.NativeSessionID, Name: attachment.ID,
			HostID: hostID, HostName: hostName, Product: attachment.Product, State: attachment.State,
			Live: true,
			Cwd:  attachment.Cwd, Groups: operatorLocalGroups(hostID, attachment.ID, attachment.Groups),
			PermissionMode: attachment.PermissionMode,
		})
		report.Summary.LocalPeers++
	}
	for _, peer := range remotePeers {
		kind := "peer"
		if strings.TrimSpace(peer.ParentSessionID) != "" {
			kind = "lane"
		}
		product := peer.Product
		if product == "" {
			product = peer.Entrypoint
		}
		report.Remote = append(report.Remote, operatorRosterEntry{
			Kind: kind, Scope: "remote", ID: peer.GlobalID, LocalID: peer.SessionID,
			Name: peer.Name, DisplayName: peer.DisplayName, HostID: peer.HostID, HostName: peer.HostName,
			Product: product, State: peer.Status, Live: true, Cwd: peer.Cwd,
			Groups: append([]string(nil), peer.Groups...), PermissionMode: peer.PermissionMode,
			OwnerSessionID: peer.ParentSessionID,
		})
		if kind == "lane" {
			report.Summary.RemoteLanes++
		} else {
			report.Summary.RemotePeers++
		}
	}
	report.Summary.FederatedHosts = len(report.FederatedHosts)
	sort.Slice(report.Local, func(i, j int) bool { return operatorRosterEntryLess(report.Local[i], report.Local[j]) })
	sort.Slice(report.Remote, func(i, j int) bool { return operatorRosterEntryLess(report.Remote[i], report.Remote[j]) })
	sort.Slice(report.FederatedHosts, func(i, j int) bool { return report.FederatedHosts[i].ID < report.FederatedHosts[j].ID })
	return report
}

func operatorLocalGroups(hostID, sessionID string, groups []string) []string {
	anchor := federationpkg.PrivateGroup(hostID, sessionID)
	return uniqueStrings(append(append([]string(nil), groups...), anchor))
}

func operatorRosterEntryLess(left, right operatorRosterEntry) bool {
	for _, pair := range [][2]string{
		{left.HostID, right.HostID}, {left.Kind, right.Kind}, {left.Product, right.Product},
		{left.Name, right.Name}, {left.LocalID, right.LocalID},
	} {
		if pair[0] != pair[1] {
			return pair[0] < pair[1]
		}
	}
	return false
}

func writeOperatorRoster(output io.Writer, report operatorRosterReport) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(writer,
		"HOST\tNAME\tSTATE\tGENERATION\tRELEASE\tHUB\tCONNECTED\n%s\t%s\t%s\t%d\t%s\t%t\t%t\n\n",
		report.Host.ID, report.Host.Name, report.Host.State, report.Host.Generation,
		operatorDefaultString(report.Host.Release, "-"), report.Host.HubConfigured, report.Host.FederationConnected,
	); err != nil {
		return err
	}
	if err := writeOperatorRosterEntries(writer, "LOCAL REGISTRATIONS", report.Local); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "\nFEDERATED HOSTS (%d)\nHOST\tNAME\tBUILD\tCAPABILITIES\n", len(report.FederatedHosts)); err != nil {
		return err
	}
	for _, host := range report.FederatedHosts {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", host.ID, host.Name,
			operatorDefaultString(host.Build, "-"), operatorDefaultString(strings.Join(host.Capabilities, ","), "-")); err != nil {
			return err
		}
	}
	if err := writeOperatorRosterEntries(writer, "\nREMOTE REGISTRATIONS", report.Remote); err != nil {
		return err
	}
	return writer.Flush()
}

func writeOperatorRosterEntries(writer io.Writer, title string, entries []operatorRosterEntry) error {
	if _, err := fmt.Fprintf(writer, "%s (%d)\nKIND\tPRODUCT\tSTATE\tLIVE\tNAME\tHOST\tSESSION\tNATIVE SESSION\tPERMISSION\tOWNER\tPERSISTENT\tGROUPS\n", title, len(entries)); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\t%s\t%t\t%s\n",
			entry.Kind, entry.Product, entry.State, entry.Live, entry.Name, entry.HostID, entry.LocalID,
			operatorDefaultString(entry.NativeSessionID, "-"), operatorDefaultString(entry.PermissionMode, "-"),
			operatorDefaultString(entry.OwnerSessionID, "-"), entry.Persistent, strings.Join(entry.Groups, ",")); err != nil {
			return err
		}
	}
	return nil
}

func operatorDefaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
