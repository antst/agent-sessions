package federator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/procinfo"
)

// RemoteLaneOptions selects one destination host and native lane product.
type RemoteLaneOptions struct {
	RuntimeDir    string
	Host          string
	Product       string
	SourceSession string
	Args          []string
}

type laneRoute struct {
	source      *hubClient
	destination *hubClient
	sourceID    string
	targetHost  string
	responses   chan Message
	done        chan struct{}
	stopOnce    sync.Once
}

type pendingLane struct {
	responses  chan Message
	failed     chan string
	cancelOnce sync.Once
}

type laneRun struct {
	mu        sync.Mutex
	process   *os.Process
	cancel    context.CancelFunc
	cancelled bool
	done      chan struct{}
	stopOnce  sync.Once
}

const (
	maxRemoteLaneRuns           = 32
	maxRemoteLaneArgs           = 256
	maxRemoteLaneArgBytes       = 512 * 1024
	maxRemoteAutoArchiveSeconds = 24 * 60 * 60
	maxHubLaneResponses         = 256
)

func newLaneRoute(source, destination *hubClient, sourceID, targetHost string) *laneRoute {
	return &laneRoute{
		source: source, destination: destination, sourceID: sourceID, targetHost: targetHost,
		responses: make(chan Message, maxHubLaneResponses), done: make(chan struct{}),
	}
}

func (r *laneRoute) stop() {
	r.stopOnce.Do(func() { close(r.done) })
}

func normalizeCapabilities(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if _, ok := ProductByCapability(value); !ok || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func resolveLaneExecutable(configured, fallback string) string {
	if configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path
		}
		return ""
	}
	if path, err := exec.LookPath(fallback); err == nil {
		return path
	}
	home, _ := os.UserHomeDir()
	candidate := filepath.Join(home, ".local", "bin", fallback)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
		return candidate
	}
	return ""
}

func (a *agent) laneCapabilities() []string {
	if a.embedded != nil {
		return append([]string(nil), a.embedded.capabilities...)
	}
	if !a.options.EnableRemoteLanes {
		return nil
	}
	descriptors := ProductDescriptors()
	capabilities := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if a.laneExecutable(descriptor.ID) != "" {
			capabilities = append(capabilities, descriptor.FederationCapability)
		}
	}
	return capabilities
}

func capabilityForProduct(product string) string {
	descriptor, ok := ProductByID(product)
	if !ok {
		return ""
	}
	return descriptor.FederationCapability
}

func (a *agent) laneExecutable(product string) string {
	switch product {
	case "codex":
		return a.options.CodexLaneExecutable
	case "claude":
		return a.options.ClaudeLaneExecutable
	case "grok":
		return a.options.GrokLaneExecutable
	case "qwen":
		return a.options.QwenLaneExecutable
	default:
		return ""
	}
}

func hostHasCapability(host Host, capability string) bool {
	for _, current := range host.Capabilities {
		if current == capability {
			return true
		}
	}
	return false
}

func (a *agent) remoteHostSnapshot() ([]Host, bool) {
	a.mu.RLock()
	if a.network == nil {
		a.mu.RUnlock()
		return nil, false
	}
	hosts := make([]Host, 0, len(a.remoteHosts))
	for _, host := range a.remoteHosts {
		host.Capabilities = append([]string(nil), host.Capabilities...)
		hosts = append(hosts, host)
	}
	a.mu.RUnlock()
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return hosts, true
}

func (a *agent) resolveRemoteHost(target, capability string) (Host, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.network == nil {
		return Host{}, errors.New("hub is disconnected")
	}
	if host, exists := a.remoteHosts[target]; exists {
		if !hostHasCapability(host, capability) {
			return Host{}, fmt.Errorf("remote host %s does not advertise %s", host.ID, capability)
		}
		return host, nil
	}
	matches := []Host{}
	for _, host := range a.remoteHosts {
		if strings.EqualFold(host.Name, target) {
			matches = append(matches, host)
		}
	}
	if len(matches) == 0 {
		return Host{}, fmt.Errorf("remote host %q is not connected to the hub", target)
	}
	if len(matches) > 1 {
		return Host{}, fmt.Errorf("remote host name %q is ambiguous; use a host id", target)
	}
	if !hostHasCapability(matches[0], capability) {
		return Host{}, fmt.Errorf("remote host %s does not advertise %s", matches[0].ID, capability)
	}
	return matches[0], nil
}

func (a *agent) sourcePeerForSession(sessionID string) (localPeer, error) {
	if err := a.refreshLocal(); err != nil {
		return localPeer{}, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, peer := range a.local {
		if peer.SessionID == sessionID {
			return peer, nil
		}
	}
	return localPeer{}, fmt.Errorf("source session %q is not a live local peer", sessionID)
}

func randomLaneRequestID(hostID string) (string, error) {
	body := make([]byte, 12)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return cleanID(hostID) + "-" + hex.EncodeToString(body), nil
}

//nolint:gocyclo // Validation and the bounded stream relay are clearer as one request lifecycle.
func (a *agent) handleLaneControl(conn net.Conn, request Message) {
	capability := capabilityForProduct(request.Product)
	if capability == "" || len(request.Args) == 0 {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", Error: "lane request requires a supported --product and native lane arguments"})
		return
	}
	if len(request.Input) > maxLaneInputBytes {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", Error: fmt.Sprintf("lane stdin exceeds %d bytes", maxLaneInputBytes)})
		return
	}
	source, err := a.sourcePeerForSession(request.SourceSessionID)
	if err != nil {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", Error: err.Error()})
		return
	}
	parent, err := a.parentContext(source.SessionID)
	if err != nil {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", Error: err.Error()})
		return
	}
	host, err := a.resolveRemoteHost(request.TargetHostID, capability)
	if err != nil {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", Error: err.Error()})
		return
	}
	requestID, err := randomLaneRequestID(a.options.HostID)
	if err != nil {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", Error: "generate lane request id: " + err.Error()})
		return
	}
	request.RequestID = requestID
	request.SourceID = source.ID
	request.ParentContext = &parent
	request.TargetHostID = host.ID
	pending := &pendingLane{responses: make(chan Message, 256), failed: make(chan string, 1)}
	a.laneMu.Lock()
	a.pendingLanes[requestID] = pending
	a.laneMu.Unlock()
	finished := false
	defer func() {
		a.laneMu.Lock()
		delete(a.pendingLanes, requestID)
		a.laneMu.Unlock()
		if !finished {
			a.sendLaneCancel(request)
		}
	}()
	a.mu.RLock()
	wire := a.network
	a.mu.RUnlock()
	if wire == nil || wire.Send(request) != nil {
		_ = newWireConn(conn).Send(Message{Type: "lane_error", RequestID: requestID, Error: "hub is disconnected"})
		return
	}
	clientWire := newWireConn(conn)
	heartbeat := time.NewTicker(2 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case response := <-pending.responses:
			if err := clientWire.Send(response); err != nil {
				return
			}
			if response.Type == "lane_exit" || response.Type == "lane_error" {
				finished = true
				return
			}
		case reason := <-pending.failed:
			_ = clientWire.Send(Message{Type: "lane_error", RequestID: requestID, Error: reason})
			finished = true
			return
		case <-heartbeat.C:
			if err := clientWire.Send(Message{Type: "heartbeat", RequestID: requestID}); err != nil {
				return
			}
		}
	}
}

func (a *agent) sendLaneCancel(request Message) {
	a.mu.RLock()
	wire := a.network
	a.mu.RUnlock()
	if wire != nil {
		_ = wire.Send(Message{Type: "lane_cancel", RequestID: request.RequestID, SourceID: request.SourceID, TargetHostID: request.TargetHostID})
	}
}

func (a *agent) deliverLaneResponse(message Message) {
	a.laneMu.Lock()
	pending := a.pendingLanes[message.RequestID]
	a.laneMu.Unlock()
	if pending == nil {
		return
	}
	select {
	case pending.responses <- message:
	default:
		select {
		case pending.failed <- "remote lane output exceeded the local proxy buffer":
			pending.cancelOnce.Do(func() { a.sendLaneCancel(message) })
		default:
		}
	}
}

func (a *agent) failPendingLanes(reason string) {
	a.laneMu.Lock()
	for _, pending := range a.pendingLanes {
		select {
		case pending.failed <- reason:
		default:
		}
	}
	a.laneMu.Unlock()
}

func (a *agent) startRemoteLane(request Message) {
	a.laneMu.Lock()
	if _, exists := a.laneRuns[request.RequestID]; exists {
		a.laneMu.Unlock()
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "duplicate lane request"})
		return
	}
	if len(a.laneRuns) >= maxRemoteLaneRuns {
		a.laneMu.Unlock()
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "remote lane concurrency limit reached"})
		return
	}
	run := &laneRun{done: make(chan struct{})}
	a.laneRuns[request.RequestID] = run
	a.laneMu.Unlock()
	go a.runRemoteLane(request, run)
}

//nolint:gocyclo // Product selection and reserved lifecycle flag validation are intentionally linear.
func (a *agent) prepareRemoteLane(request Message) (string, []string, error) {
	if !a.options.EnableRemoteLanes {
		return "", nil, errors.New("remote lane execution is disabled on this host")
	}
	if _, ok := ProductByID(request.Product); !ok {
		return "", nil, fmt.Errorf("unsupported lane product %q", request.Product)
	}
	executable := a.laneExecutable(request.Product)
	if executable == "" {
		return "", nil, fmt.Errorf("%s lane launcher is unavailable", request.Product)
	}
	if len(request.Args) == 0 {
		return "", nil, errors.New("remote lane command is missing")
	}
	if err := validateRemoteLaneArgBounds(request.Args); err != nil {
		return "", nil, err
	}
	args := append([]string(nil), request.Args...)
	command := args[0]
	a.mu.RLock()
	_, sourceExists := a.remote[request.SourceID]
	hubConnected := a.network != nil
	a.mu.RUnlock()
	if !hubConnected {
		return "", nil, errors.New("hub is disconnected")
	}
	if !sourceExists {
		return "", nil, errors.New("originating peer is no longer connected through the hub")
	}
	if command != "run" && command != "start" && command != "resume" {
		return executable, args, nil
	}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		name := strings.SplitN(argument, "=", 2)[0]
		if name == "--persistent" || name == "--notify" || name == "--no-notify" || name == "--no-auto-archive" {
			return "", nil, fmt.Errorf("remote %s owns %s; omit --persistent/--notify/--no-notify/--no-auto-archive", command, name)
		}
		if name == "--auto-archive-after" {
			value := ""
			if parts := strings.SplitN(argument, "=", 2); len(parts) == 2 {
				value = parts[1]
			} else if index+1 < len(args) {
				index++
				value = args[index]
			}
			seconds, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0.001 || seconds > maxRemoteAutoArchiveSeconds {
				return "", nil, fmt.Errorf("remote --auto-archive-after must be between 0.001 and %d seconds", maxRemoteAutoArchiveSeconds)
			}
		}
	}
	if request.ParentContext == nil || request.ParentContext.SessionID == "" {
		return "", nil, errors.New("remote lane request has no attested parent context")
	}
	args = append(args, "--persistent", "--notify", request.SourceID)
	return executable, args, nil
}

func validateRemoteLaneArgBounds(args []string) error {
	if len(args) > maxRemoteLaneArgs {
		return fmt.Errorf("remote lane argv exceeds %d arguments", maxRemoteLaneArgs)
	}
	total := 0
	for _, argument := range args {
		total += len(argument)
		if total > maxRemoteLaneArgBytes {
			return fmt.Errorf("remote lane argv exceeds %d bytes", maxRemoteLaneArgBytes)
		}
	}
	return nil
}

func (a *agent) runRemoteLane(request Message, run *laneRun) {
	defer func() {
		a.laneMu.Lock()
		delete(a.laneRuns, request.RequestID)
		a.laneMu.Unlock()
		close(run.done)
	}()
	if a.embedded != nil {
		a.runEmbeddedRemoteLane(request, run)
		return
	}
	executable, args, err := a.prepareRemoteLane(request)
	if err != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	watchArgs := make([]string, 0, len(args)+5)
	watchArgs = append(watchArgs, "lane-watch", "--liveness-fd", "3", "--", executable)
	watchArgs = append(watchArgs, args...)
	// #nosec G204 -- the executable is this agent binary and native argv remains a vector without a shell.
	command := exec.Command(a.options.Executable, watchArgs...)
	parentBody, marshalErr := json.Marshal(request.ParentContext)
	if marshalErr != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: marshalErr.Error()})
		return
	}
	claudeProfile, profileErr := a.resolvedClaudeProfile()
	if profileErr != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: profileErr.Error()})
		return
	}
	environmentUpdates := remoteLaneEnvironmentUpdates(a.options, request.Product, string(parentBody))
	command.Env = claudeProfileEnvironment(envutil.Replace(os.Environ(), environmentUpdates), claudeProfile)
	command.Stdin = bytes.NewReader(request.Input)
	livenessReader, livenessWriter, err := os.Pipe()
	if err != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	defer func() {
		_ = livenessReader.Close()
		_ = livenessWriter.Close()
	}()
	command.ExtraFiles = []*os.File{livenessReader}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	run.mu.Lock()
	if run.cancelled {
		run.mu.Unlock()
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "lane request was cancelled"})
		return
	}
	if err := command.Start(); err != nil {
		run.mu.Unlock()
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	_ = livenessReader.Close()
	run.process = command.Process
	run.mu.Unlock()
	var streams sync.WaitGroup
	streams.Add(2)
	go func() {
		defer streams.Done()
		a.streamLaneOutput(request.RequestID, "lane_stdout", stdout, run)
	}()
	go func() {
		defer streams.Done()
		a.streamLaneOutput(request.RequestID, "lane_stderr", stderr, run)
	}()
	streams.Wait()
	waitErr := command.Wait()
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = 1
		}
	}
	_ = a.sendLaneMessage(Message{Type: "lane_exit", RequestID: request.RequestID, ExitCode: exitCode})
}

func remoteLaneEnvironmentUpdates(options AgentOptions, product, parentContext string) map[string]string {
	updates := map[string]string{
		"AGENT_SESSIONS_AGENT_RUNTIME_DIR":     options.RuntimeDir,
		"AGENT_SESSIONS_REMOTE_PARENT_CONTEXT": parentContext,
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR":        options.ClaudeConfigDir,
	}
	if product == "qwen" && options.QwenExecutable != "" {
		updates["QWEN_PEER_QWEN_BIN"] = options.QwenExecutable
	}
	return updates
}

func claudeProfileEnvironment(environment []string, profile claudeprofile.Source) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if name != "CLAUDE_CONFIG_DIR" && name != "CLAUDE_SECURESTORAGE_CONFIG_DIR" {
			result = append(result, entry)
		}
	}
	if profile.ConfigEnvSet {
		result = append(result, "CLAUDE_CONFIG_DIR="+profile.ConfigEnvValue)
	}
	if profile.SecureEnvSet {
		result = append(result, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+profile.SecureConfig)
	}
	return result
}

func (a *agent) streamLaneOutput(requestID, kind string, reader io.Reader, run *laneRun) {
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			data := append([]byte(nil), buffer[:count]...)
			if sendErr := a.sendLaneMessage(Message{Type: kind, RequestID: requestID, Data: data}); sendErr != nil {
				run.stop()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (a *agent) sendLaneMessage(message Message) error {
	a.mu.RLock()
	wire := a.network
	a.mu.RUnlock()
	if wire == nil {
		return errors.New("hub is disconnected")
	}
	return wire.Send(message)
}

func (r *laneRun) stop() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.cancelled = true
		process := r.process
		cancel := r.cancel
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if process == nil {
			return
		}
		_ = process.Signal(os.Interrupt)
		go func() {
			select {
			case <-r.done:
			case <-time.After(laneWatchKillGrace + 2*time.Second):
				_ = process.Kill()
			}
		}()
	})
}

func (a *agent) cancelRemoteLane(requestID string) {
	a.laneMu.Lock()
	run := a.laneRuns[requestID]
	a.laneMu.Unlock()
	if run != nil {
		run.stop()
	}
}

func (a *agent) cancelAllRemoteLanes() {
	a.laneMu.Lock()
	runs := make([]*laneRun, 0, len(a.laneRuns))
	for _, run := range a.laneRuns {
		runs = append(runs, run)
	}
	a.laneMu.Unlock()
	for _, run := range runs {
		run.stop()
	}
}

//nolint:gocyclo // Each route-rejection reason is kept explicit for actionable client errors.
func (h *hub) routeLaneExec(source *hubClient, message Message) error {
	capability := capabilityForProduct(message.Product)
	if message.RequestID == "" || message.SourceID == "" || message.TargetHostID == "" || capability == "" || len(message.Args) == 0 {
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "invalid remote lane request"})
	}
	h.mu.Lock()
	if h.laneRoutes == nil {
		h.laneRoutes = map[string]*laneRoute{}
	}
	sourcePeer, sourceExists := source.peers[message.SourceID]
	parentValid := sourceExists && message.ParentContext != nil &&
		message.ParentContext.HostID == sourcePeer.HostID && message.ParentContext.SessionID == sourcePeer.SessionID &&
		message.ParentContext.Product == sourcePeer.Entrypoint && message.ParentContext.InstanceID == sourcePeer.InstanceID &&
		reflect.DeepEqual(sortedUnique(message.ParentContext.Groups), sortedUnique(sourcePeer.Groups))
	destination := h.clients[message.TargetHostID]
	_, duplicate := h.laneRoutes[message.RequestID]
	var route *laneRoute
	if sourceExists && parentValid && destination != nil && !duplicate && containsString(destination.capabilities, capability) {
		route = newLaneRoute(source, destination, message.SourceID, message.TargetHostID)
		h.laneRoutes[message.RequestID] = route
	}
	h.mu.Unlock()
	switch {
	case !sourceExists:
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "source peer is not advertised by this host"})
	case destination == nil:
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "target host is not connected to the hub"})
	case duplicate:
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "duplicate lane request id"})
	case !containsString(destination.capabilities, capability):
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "target host lacks the requested lane capability"})
	case !parentValid:
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "remote lane parent context does not match its source peer"})
	}
	go h.forwardLaneRoute(message.RequestID, route)
	if err := destination.wire.Send(message); err != nil {
		h.removeLaneRoute(message.RequestID, route)
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "forward remote lane request: " + err.Error()})
	}
	return nil
}

func (h *hub) routeLaneCancel(source *hubClient, message Message) error {
	h.mu.Lock()
	route, exists := h.laneRoutes[message.RequestID]
	h.mu.Unlock()
	if !exists || route.source != source {
		return nil
	}
	if err := route.destination.wire.Send(Message{Type: "lane_cancel", RequestID: message.RequestID}); err != nil {
		h.removeLaneRoute(message.RequestID, route)
		return source.wire.Send(Message{
			Type: "lane_error", RequestID: message.RequestID,
			Error: "target host disconnected while cancelling remote lane",
		})
	}
	return nil
}

func (h *hub) routeLaneResponse(destination *hubClient, message Message) error {
	h.mu.Lock()
	route, exists := h.laneRoutes[message.RequestID]
	h.mu.Unlock()
	if !exists || route.destination != destination {
		return nil
	}
	select {
	case route.responses <- message:
		return nil
	default:
		if h.removeLaneRoute(message.RequestID, route) {
			go h.failLaneRoute(message.RequestID, route, "remote lane output exceeded the hub route buffer")
		}
		return nil
	}
}

func (h *hub) forwardLaneRoute(requestID string, route *laneRoute) {
	for {
		select {
		case <-route.done:
			return
		default:
		}
		select {
		case <-route.done:
			return
		case message := <-route.responses:
			if err := route.source.wire.Send(message); err != nil {
				if h.removeLaneRoute(requestID, route) {
					_ = route.destination.wire.Send(Message{Type: "lane_cancel", RequestID: requestID})
					h.logger.Printf("remote lane %s source stopped accepting output; route cancelled", requestID)
				}
				return
			}
			if message.Type == "lane_exit" || message.Type == "lane_error" {
				h.removeLaneRoute(requestID, route)
				return
			}
		}
	}
}

func (h *hub) removeLaneRoute(requestID string, route *laneRoute) bool {
	h.mu.Lock()
	if h.laneRoutes[requestID] != route {
		h.mu.Unlock()
		return false
	}
	delete(h.laneRoutes, requestID)
	h.mu.Unlock()
	route.stop()
	return true
}

func (h *hub) failLaneRoute(requestID string, route *laneRoute, reason string) {
	_ = route.destination.wire.Send(Message{Type: "lane_cancel", RequestID: requestID})
	_ = route.source.wire.Send(Message{Type: "lane_error", RequestID: requestID, Error: reason})
	h.logger.Printf("remote lane %s cancelled: %s", requestID, reason)
}

func (h *hub) dropLaneRoutes(client *hubClient) {
	type notification struct {
		wire    *wireConn
		message Message
	}
	notifications := []notification{}
	h.mu.Lock()
	for requestID, route := range h.laneRoutes {
		switch client {
		case route.source:
			notifications = append(notifications, notification{wire: route.destination.wire, message: Message{Type: "lane_cancel", RequestID: requestID}})
		case route.destination:
			notifications = append(notifications, notification{wire: route.source.wire, message: Message{Type: "lane_error", RequestID: requestID, Error: "target host disconnected from hub"}})
		default:
			continue
		}
		delete(h.laneRoutes, requestID)
		route.stop()
	}
	h.mu.Unlock()
	for _, current := range notifications {
		go func(notification notification) {
			_ = notification.wire.Send(notification.message)
		}(current)
	}
}

// ReadRemoteHosts returns connected remote agents from the local hub-bound agent.
func ReadRemoteHosts(runtimeDir string) ([]Host, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir, "agent.sock"), time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := newWireConn(conn).Send(Message{Type: "hosts"}); err != nil {
		return nil, err
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, err
	}
	if response.Type != "hosts" {
		return nil, fmt.Errorf("agent host listing failed: %s", response.Error)
	}
	return response.Hosts, nil
}

// RunRemoteLane proxies one native lane command through the connected hub.
//
//nolint:gocyclo // The public proxy owns validation, bounded stdin, cancellation, and frame dispatch.
func RunRemoteLane(ctx context.Context, options RemoteLaneOptions, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if options.RuntimeDir == "" {
		options.RuntimeDir = DefaultRuntimeDir()
	}
	if options.Host == "" || capabilityForProduct(options.Product) == "" || len(options.Args) == 0 {
		return 1, errors.New("remote lane requires --host, a supported --product, and native lane arguments after --")
	}
	if options.SourceSession == "" {
		options.SourceSession = inferRemoteLaneSourceSession(os.Getpid())
	}
	if options.SourceSession == "" {
		return 1, errors.New("cannot identify the originating product session; pass --source-session")
	}
	input := []byte(nil)
	if remoteLaneReadsStdin(options.Product, options.Args) {
		body, err := io.ReadAll(io.LimitReader(stdin, maxLaneInputBytes+1))
		if err != nil {
			return 1, err
		}
		if len(body) > maxLaneInputBytes {
			return 1, fmt.Errorf("lane stdin exceeds %d bytes", maxLaneInputBytes)
		}
		input = body
	}
	conn, err := net.DialTimeout("unix", filepath.Join(options.RuntimeDir, "agent.sock"), 2*time.Second)
	if err != nil {
		return 1, fmt.Errorf("local federation agent is unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	request := Message{
		Type: "lane_exec", TargetHostID: options.Host, Product: options.Product,
		SourceSessionID: options.SourceSession, Args: append([]string(nil), options.Args...), Input: input,
	}
	if err := newWireConn(conn).Send(request); err != nil {
		return 1, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	decoder := json.NewDecoder(conn)
	for {
		var response Message
		if err := decoder.Decode(&response); err != nil {
			if ctx.Err() != nil {
				return 130, ctx.Err()
			}
			return 1, fmt.Errorf("remote lane stream ended: %w", err)
		}
		switch response.Type {
		case "heartbeat":
			continue
		case "lane_stdout":
			if _, err := stdout.Write(response.Data); err != nil {
				return 1, err
			}
		case "lane_stderr":
			if _, err := stderr.Write(response.Data); err != nil {
				return 1, err
			}
		case "lane_exit":
			return response.ExitCode, nil
		case "lane_error":
			return 1, errors.New(response.Error)
		case "error":
			return 1, errors.New(response.Error)
		default:
			return 1, fmt.Errorf("unsupported remote lane frame %q", response.Type)
		}
	}
}

func inferRemoteLaneSourceSession(startPID int) string {
	if sessionID := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_SESSION_ID")); sessionID != "" {
		return sessionID
	}
	codexSession := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
	claudeSession := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	claudePID, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("CLAUDE_PID")))
	visited := map[int]bool{}
	for pid := startPID; pid > 1 && !visited[pid]; {
		visited[pid] = true
		if claudeSession != "" && pid == claudePID {
			return claudeSession
		}
		if codexSession != "" {
			if args, err := procinfo.Args(pid); err == nil && procinfo.LooksLikeCodexHost(args) {
				return codexSession
			}
		}
		parent, ok := parentProcessID(pid)
		if !ok {
			return ""
		}
		pid = parent
	}
	return ""
}

func remoteLaneReadsStdin(product string, args []string) bool {
	if len(args) == 0 || (args[0] != "run" && args[0] != "start" && args[0] != "resume") {
		return false
	}
	valueOptions := map[string]bool{
		"-n": true, "--name": true, "--peer-name": true, "-C": true, "--cd": true,
		"-m": true, "--model": true, "--effort": true, "--timeout": true,
		"--notify": true, "--auto-archive-after": true, "--schema": true,
	}
	if product == "codex" {
		valueOptions["--reasoning-effort"] = true
		valueOptions["--sandbox"] = true
		valueOptions["--approval-policy"] = true
		valueOptions["-c"] = true
		valueOptions["--config"] = true
	} else {
		valueOptions["--permission-mode"] = true
		valueOptions["--max-budget-usd"] = true
		valueOptions["--tools"] = true
		valueOptions["--allowed-tools"] = true
		valueOptions["--disallowed-tools"] = true
	}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		if argument == "--prompt-file" {
			return false
		}
		if valueOptions[argument] && index+1 < len(args) {
			index++
		}
	}
	return true
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
