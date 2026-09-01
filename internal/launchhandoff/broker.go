//go:build linux || darwin

package launchhandoff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const endpointName = "launch.sock"

type entryState uint8

const (
	entryPending entryState = iota + 1
	entryClaimed
	entryWritingGO
	entryResolving
)

type handoffResolution uint8

const (
	handoffConsumed handoffResolution = iota + 1
	handoffRollback
	handoffAmbiguous
)

type resolutionAction interface {
	finish(context.Context)
}

type consumedResolution struct{}
type rollbackResolution struct{ capability rollbackCapability }
type ambiguousResolution struct {
	capability ambiguousCapability
	fact       AmbiguousHandoff
}

func (consumedResolution) finish(context.Context) {}
func (r rollbackResolution) finish(ctx context.Context) {
	_ = r.capability.run(ctx)
}
func (r ambiguousResolution) finish(ctx context.Context) {
	_ = r.capability.run(ctx, r.fact)
}

func (p *FinalizationPlan) take(resolution handoffResolution, fact AmbiguousHandoff) resolutionAction {
	if p == nil {
		return consumedResolution{}
	}
	var action resolutionAction
	switch resolution {
	case handoffConsumed:
		action = consumedResolution{}
	case handoffRollback:
		action = rollbackResolution{capability: p.zeroWrite}
	default:
		// Unknown/possible classifications fail toward reconciliation, never
		// toward destructive rollback or optimistic consumption.
		action = ambiguousResolution{capability: p.possibleWrite, fact: fact}
	}
	*p = FinalizationPlan{}
	return action
}

type brokerConnection interface {
	ReadFrame() ([]byte, error)
	WriteFrame([]byte) error
	WriteFrameOutcome([]byte) (localtransport.FrameWriteOutcome, error)
	SetDeadline(time.Time) error
	Close() error
}

type stagedEntry struct {
	ticket       Ticket
	attachmentID string
	wrapper      WrapperIdentity
	command      productruntime.NativeCommand
	finalizers   FinalizationPlan
	created      time.Time
	deadline     time.Time
	bytes        int
	state        entryState
	cancelled    bool
	connection   brokerConnection
}

// Broker is one generation-local launch authority. It holds no durable fact;
// every entry is bounded and single-consumer. Proven pre-exec failures roll
// back, while a possible GO write can only hand off live reconciliation/debt.
type Broker struct {
	listener *localtransport.ByteListener
	endpoint string
	limits   Limits
	random   io.Reader
	now      func() time.Time
	capture  ProcessCapture

	mu        sync.Mutex
	settled   *sync.Cond
	entries   map[string]*stagedEntry
	total     int
	closed    bool
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func Endpoint(stateRoot string) string { return filepath.Join(stateRoot, "run", endpointName) }

func NewBroker(config Config) (*Broker, error) {
	if config.StateRoot == "" {
		return nil, fmt.Errorf("%w: state root is empty", ErrInvalid)
	}
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	} else if !config.Limits.valid() {
		return nil, fmt.Errorf("%w: launch handoff limits", ErrInvalid)
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.CaptureProcess == nil {
		config.CaptureProcess = procinfo.CaptureIdentity
	}
	endpoint := Endpoint(config.StateRoot)
	listener, err := localtransport.ListenBytes(endpoint, uint32(config.Limits.MaxCommandBytes)) //nolint:gosec // validated positive and bounded by int.
	if err != nil {
		return nil, fmt.Errorf("bind private launch handoff endpoint: %w", err)
	}
	broker := &Broker{
		listener: listener, endpoint: endpoint, limits: config.Limits, random: config.Random,
		now: config.Now, capture: config.CaptureProcess, entries: map[string]*stagedEntry{},
	}
	broker.settled = sync.NewCond(&broker.mu)
	return broker, nil
}

func (b *Broker) Endpoint() string {
	if b == nil {
		return ""
	}
	return b.endpoint
}

func (b *Broker) Stage(request StageRequest) (Ticket, error) {
	if b == nil || b.listener == nil || !request.Finalizers.valid() ||
		invalidRequiredField(request.AttachmentID, b.limits.MaxFieldBytes) ||
		request.Wrapper.UID != os.Geteuid() || request.Wrapper.Process.PID <= 1 ||
		invalidRequiredField(request.Wrapper.Process.Start, b.limits.MaxFieldBytes) ||
		invalidRequiredField(request.Wrapper.Process.StrongStart, b.limits.MaxFieldBytes) {
		return Ticket{}, ErrInvalid
	}
	encoded, err := encodeCommand(request.Command, b.limits)
	if err != nil {
		return Ticket{}, err
	}
	size := len(encoded)
	zero(encoded)
	ticket, err := b.newTicket()
	if err != nil {
		return Ticket{}, ErrUnavailable
	}
	now := b.now()
	entry := &stagedEntry{
		ticket: ticket, attachmentID: request.AttachmentID, wrapper: request.Wrapper,
		command: cloneCommand(request.Command), finalizers: request.Finalizers,
		created: now, deadline: now.Add(b.limits.PendingTTL), bytes: size, state: entryPending,
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		clearEntry(entry)
		return Ticket{}, ErrUnavailable
	}
	if len(b.entries) >= b.limits.MaxPending || b.total+size > b.limits.MaxAggregate {
		clearEntry(entry)
		return Ticket{}, ErrCapacity
	}
	for _, current := range b.entries {
		if current.attachmentID == request.AttachmentID {
			clearEntry(entry)
			return Ticket{}, ErrClaimed
		}
	}
	if _, collision := b.entries[ticket.ID]; collision {
		clearEntry(entry)
		return Ticket{}, ErrUnavailable
	}
	b.entries[ticket.ID] = entry
	b.total += size
	return ticket, nil
}

// Run accepts exact wrappers until cancellation. Context cancellation closes
// the endpoint, retires every unconsumed record, and waits for active claims.
func (b *Broker) Run(ctx context.Context) error {
	if b == nil || b.listener == nil || ctx == nil {
		return ErrUnavailable
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = b.Close()
		case <-stop:
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				b.expire()
			}
		}
	}()
	for {
		connection, peer, err := b.listener.Accept()
		if err != nil {
			b.wg.Wait()
			if ctx.Err() != nil || b.isClosed() {
				return nil
			}
			return fmt.Errorf("accept launch handoff: %w", err)
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = connection.Close()
			continue
		}
		b.wg.Add(1)
		b.mu.Unlock()
		go func() {
			defer b.wg.Done()
			b.serve(ctx, connection, peer)
		}()
	}
}

func (b *Broker) Close() error {
	if b == nil {
		return nil
	}
	var pending []*stagedEntry
	var active []brokerConnection
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		for _, entry := range b.entries {
			switch entry.state {
			case entryPending:
				pending = append(pending, entry)
			case entryClaimed, entryWritingGO:
				if !entry.cancelled {
					entry.cancelled = true
					active = append(active, entry.connection)
				}
			}
		}
		b.mu.Unlock()
		if b.listener != nil {
			b.closeErr = b.listener.Close()
		}
		for _, connection := range active {
			if connection != nil {
				_ = connection.Close()
			}
		}
	})
	for _, entry := range pending {
		b.resolve(entry, handoffRollback)
	}
	b.wg.Wait()
	b.mu.Lock()
	for len(b.entries) != 0 {
		b.settled.Wait()
	}
	b.mu.Unlock()
	return b.closeErr
}

func (b *Broker) serve(ctx context.Context, connection brokerConnection, peer localtransport.PeerIdentity) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(b.now().Add(b.limits.HandshakeTimeout))
	requestBody, err := connection.ReadFrame()
	if err != nil {
		return
	}
	ticket, err := decodeClaim(requestBody)
	zero(requestBody)
	if err != nil {
		_ = connection.WriteFrame(encodeError(err))
		return
	}
	observed, err := b.capture(peer.PID)
	if err != nil || peer.UID < 0 || observed.PID != peer.PID || observed.Start == "" || observed.StrongStart == "" {
		_ = connection.WriteFrame(encodeError(ErrUnauthorized))
		return
	}
	entry, err := b.claim(ticket, peer, observed, connection)
	if err != nil {
		_ = connection.WriteFrame(encodeError(err))
		return
	}
	resolution := handoffRollback
	resolved := false
	defer func() {
		if !resolved {
			b.resolve(entry, resolution)
		}
	}()
	commandBody, err := b.commandFrame(entry)
	if err != nil {
		_ = connection.WriteFrame(encodeError(ErrInvalid))
		return
	}
	digest := protocolDigest(commandBody)
	if err := connection.WriteFrame(commandBody); err != nil {
		zero(commandBody)
		return
	}
	zero(commandBody)
	ack, err := connection.ReadFrame()
	if err != nil || decodeAck(ack, digest) != nil {
		zero(ack)
		return
	}
	zero(ack)
	if !b.beginGO(entry) {
		_ = connection.WriteFrame(encodeError(ErrStale))
		return
	}
	writeOutcome, _ := connection.WriteFrameOutcome(simpleFrame(frameGo))
	switch {
	case writeOutcome.Complete():
		resolution = handoffConsumed
	case writeOutcome.Zero():
		resolution = handoffRollback
	default:
		resolution = handoffAmbiguous
	}
	b.resolve(entry, resolution)
	resolved = true
	_ = ctx // the connection deadline and Broker.Close bound this one-shot operation.
}

func (b *Broker) claim(ticket Ticket, peer localtransport.PeerIdentity, observed procinfo.Identity, connection brokerConnection) (*stagedEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[ticket.ID]
	if b.closed || entry == nil || entry.ticket != ticket || !b.now().Before(entry.deadline) {
		return nil, ErrStale
	}
	if entry.state != entryPending {
		return nil, ErrClaimed
	}
	if peer.UID != entry.wrapper.UID || peer.PID != observed.PID || entry.wrapper.Process != observed {
		return nil, ErrUnauthorized
	}
	entry.state = entryClaimed
	entry.connection = connection
	return entry, nil
}

func (b *Broker) commandFrame(entry *stagedEntry) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.entries[entry.ticket.ID]
	if b.closed || current != entry || entry.state != entryClaimed || entry.cancelled || !b.now().Before(entry.deadline) {
		return nil, ErrStale
	}
	return encodeCommand(entry.command, b.limits)
}

func (b *Broker) beginGO(entry *stagedEntry) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.entries[entry.ticket.ID] != entry || entry.state != entryClaimed || entry.cancelled || !b.now().Before(entry.deadline) {
		return false
	}
	entry.state = entryWritingGO
	return true
}

// resolve holds the attachment reservation and aggregate accounting through
// the callback handoff. A replacement Stage cannot race an old rollback or
// ambiguity finalizer and inherit cross-contaminating cleanup.
func (b *Broker) resolve(entry *stagedEntry, resolution handoffResolution) {
	if entry == nil {
		return
	}
	b.mu.Lock()
	if b.entries[entry.ticket.ID] != entry || entry.state == entryResolving {
		b.mu.Unlock()
		return
	}
	entry.state = entryResolving
	entry.connection = nil
	ambiguousFact := AmbiguousHandoff{AttachmentID: entry.attachmentID, Ticket: entry.ticket}
	action := entry.finalizers.take(resolution, ambiguousFact)
	clearCommand(&entry.command)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), b.limits.RollbackTimeout)
	action.finish(ctx)
	cancel()

	b.mu.Lock()
	if b.entries[entry.ticket.ID] == entry {
		delete(b.entries, entry.ticket.ID)
		b.total -= entry.bytes
		clearEntry(entry)
		b.settled.Broadcast()
	}
	b.mu.Unlock()
}

func (b *Broker) expire() {
	now := b.now()
	b.mu.Lock()
	pending := make([]*stagedEntry, 0)
	active := make([]brokerConnection, 0)
	for _, entry := range b.entries {
		if !now.Before(entry.deadline) {
			switch entry.state {
			case entryPending:
				pending = append(pending, entry)
			case entryClaimed, entryWritingGO:
				if !entry.cancelled {
					entry.cancelled = true
					active = append(active, entry.connection)
				}
			}
		}
	}
	b.mu.Unlock()
	for _, connection := range active {
		if connection != nil {
			_ = connection.Close()
		}
	}
	for _, entry := range pending {
		b.resolve(entry, handoffRollback)
	}
}

func (b *Broker) newTicket() (Ticket, error) {
	var body [16]byte
	if _, err := io.ReadFull(b.random, body[:]); err != nil {
		return Ticket{}, err
	}
	return Ticket{ID: hex.EncodeToString(body[:]), Contract: ContractVersion}, nil
}

func (b *Broker) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func cloneCommand(command productruntime.NativeCommand) productruntime.NativeCommand {
	command.Args = append([]string(nil), command.Args...)
	command.Env = append([]productruntime.EnvVar(nil), command.Env...)
	command.SensitiveEnv = append([]productruntime.SensitiveEnvVar(nil), command.SensitiveEnv...)
	return command
}

func clearEntry(entry *stagedEntry) {
	if entry == nil {
		return
	}
	clearCommand(&entry.command)
	entry.wrapper = WrapperIdentity{}
	entry.attachmentID = ""
	entry.finalizers = FinalizationPlan{}
	entry.connection = nil
	entry.cancelled = false
	entry.bytes = 0
}

func clearCommand(command *productruntime.NativeCommand) {
	if command == nil {
		return
	}
	for index := range command.Args {
		command.Args[index] = ""
	}
	for index := range command.Env {
		command.Env[index] = productruntime.EnvVar{}
	}
	for index := range command.SensitiveEnv {
		command.SensitiveEnv[index] = productruntime.SensitiveEnvVar{}
	}
	*command = productruntime.NativeCommand{}
}
