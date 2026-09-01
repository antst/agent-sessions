package component

import (
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

// BindingView is the immutable, secret-free evidence supplied to daemon-owned
// frame handlers.
type BindingView struct {
	BindingID         string
	AttachmentID      string
	ProductID         string
	ProcessIdentity   procinfo.Identity
	PeerIdentity      localtransport.PeerIdentity
	Generation        uint64
	BootstrapRevision uint64
	LastInboundSeq    uint64
	LastOutboundSeq   uint64
}

type binding struct {
	mu                sync.Mutex
	view              BindingView
	connection        *localtransport.Conn
	replayWindow      uint64
	maxOutstanding    int
	inboundDigests    map[uint64][sha256.Size]byte
	pendingToolCalls  map[string][sha256.Size]byte
	deliveries        *deliveryTracker
	lastHeartbeat     time.Time
	heartbeatInterval time.Duration
	heartbeatGrace    int
	closed            bool
}

// deliveryTracker is shared only by successive, re-attested bindings for the
// same attachment in one daemon generation. It preserves bounded delivery
// admission across a transient stream loss without becoming reconnect
// authority.
type deliveryTracker struct {
	mu             sync.Mutex
	maxOutstanding int
	replayWindow   uint64
	pending        map[string]struct{}
	completed      map[string][sha256.Size]byte
	completedOrder []string
}

func newDeliveryTracker(maxOutstanding int, replayWindow uint64) *deliveryTracker {
	return &deliveryTracker{
		maxOutstanding: maxOutstanding,
		replayWindow:   replayWindow,
		pending:        make(map[string]struct{}),
		completed:      make(map[string][sha256.Size]byte),
	}
}

func (b *binding) snapshot() BindingView {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.view
}

func (b *binding) acceptInbound(frame Frame, encoded []byte) (duplicate bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	digest := sha256.Sum256(encoded)
	if frame.Seq <= b.view.LastInboundSeq {
		prior, ok := b.inboundDigests[frame.Seq]
		if ok && prior == digest {
			return true, nil
		}
		return false, &ProtocolError{Category: CategoryReplay, Detail: "frame sequence conflicts with bounded replay history"}
	}
	if frame.Seq != b.view.LastInboundSeq+1 {
		return false, &ProtocolError{Category: CategorySequenceGap, Detail: "component frame sequence has a gap"}
	}
	b.view.LastInboundSeq = frame.Seq
	b.inboundDigests[frame.Seq] = digest
	if frame.Seq > b.replayWindow {
		delete(b.inboundDigests, frame.Seq-b.replayWindow)
	}
	return false, nil
}

func (b *binding) trackOperation(frame Frame) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch frame.Type {
	case TypeToolCall:
		var call ToolCall
		if err := frame.PayloadInto(&call); err != nil {
			return false, err
		}
		digest := sha256.Sum256(frame.Payload)
		if prior, exists := b.pendingToolCalls[call.CallID]; exists {
			if prior == digest {
				return true, nil
			}
			return false, &ProtocolError{Category: CategoryReplay, Detail: "tool call id conflicts with an outstanding operation"}
		}
		if len(b.pendingToolCalls) >= b.maxOutstanding {
			return false, &ProtocolError{Category: CategoryTooManyOutstanding, Detail: "too many tool calls are outstanding"}
		}
		b.pendingToolCalls[call.CallID] = digest
	case TypeToolCancel:
		var cancel ToolCancel
		if err := frame.PayloadInto(&cancel); err != nil {
			return false, err
		}
		if _, exists := b.pendingToolCalls[cancel.CallID]; !exists {
			return false, &ProtocolError{Category: CategoryProtocol, Detail: "tool cancel does not name an outstanding call"}
		}
	case TypeDeliveryAccept, TypeDeliveryReject:
		return b.deliveries.track(frame)
	}
	return false, nil
}

// rollbackOperation restores only the binding-local pre-admission mutation
// made by trackOperation. Durable handler state is authoritative and is not
// modified here.
func (b *binding) rollbackOperation(frame Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch frame.Type {
	case TypeToolCall:
		var call ToolCall
		if err := frame.PayloadInto(&call); err != nil {
			return
		}
		digest := sha256.Sum256(frame.Payload)
		if current, exists := b.pendingToolCalls[call.CallID]; exists && current == digest {
			delete(b.pendingToolCalls, call.CallID)
		}
	case TypeDeliveryAccept, TypeDeliveryReject:
		b.deliveries.rollback(frame)
	}
}

func (d *deliveryTracker) track(frame Frame) (bool, error) {
	deliveryID, err := deliveryOperationID(frame)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(frame.Payload)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.pending[deliveryID]; !exists {
		if completed, ok := d.completed[deliveryID]; ok {
			if completed == digest {
				return true, nil
			}
			return false, &ProtocolError{Category: CategoryReplay, Detail: "delivery result conflicts with prior native evidence"}
		}
		return false, &ProtocolError{Category: CategoryProtocol, Detail: "delivery result does not name an outstanding delivery"}
	}
	delete(d.pending, deliveryID)
	d.completed[deliveryID] = digest
	d.completedOrder = append(d.completedOrder, deliveryID)
	if len(d.completedOrder) > int(d.replayWindow) {
		delete(d.completed, d.completedOrder[0])
		d.completedOrder = d.completedOrder[1:]
	}
	return false, nil
}

func (d *deliveryTracker) rollback(frame Frame) {
	deliveryID, err := deliveryOperationID(frame)
	if err != nil {
		return
	}
	digest := sha256.Sum256(frame.Payload)
	d.mu.Lock()
	defer d.mu.Unlock()
	if completed, exists := d.completed[deliveryID]; !exists || completed != digest {
		return
	}
	delete(d.completed, deliveryID)
	d.pending[deliveryID] = struct{}{}
	for index := len(d.completedOrder) - 1; index >= 0; index-- {
		if d.completedOrder[index] == deliveryID {
			d.completedOrder = append(d.completedOrder[:index], d.completedOrder[index+1:]...)
			break
		}
	}
}

func (d *deliveryTracker) reserve(deliveryID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.pending[deliveryID]; exists {
		return false, nil
	}
	if len(d.pending) >= d.maxOutstanding {
		return false, &ProtocolError{Category: CategoryTooManyOutstanding, Detail: "too many deliveries are outstanding"}
	}
	d.pending[deliveryID] = struct{}{}
	return true, nil
}

func (d *deliveryTracker) release(deliveryID string) {
	d.mu.Lock()
	delete(d.pending, deliveryID)
	d.mu.Unlock()
}

func (d *deliveryTracker) hasState() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending) > 0 || len(d.completed) > 0
}

func deliveryOperationID(frame Frame) (string, error) {
	if frame.Type == TypeDeliveryAccept {
		var value DeliveryAccept
		if err := frame.PayloadInto(&value); err != nil {
			return "", err
		}
		return value.DeliveryID, nil
	}
	var value DeliveryReject
	if err := frame.PayloadInto(&value); err != nil {
		return "", err
	}
	return value.DeliveryID, nil
}

func (b *binding) send(frameType FrameType, id string, payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("component binding is closed")
	}
	deliveryID := ""
	deliveryReserved := false
	toolResultID := ""
	var err error
	if frameType == TypeDeliveryPresent {
		value, ok := payload.(DeliveryPresent)
		if !ok || !validIdentifier(value.DeliveryID) {
			return errors.New("delivery.present requires a typed delivery payload")
		}
		deliveryReserved, err = b.deliveries.reserve(value.DeliveryID)
		if err != nil {
			return err
		}
		deliveryID = value.DeliveryID
	}
	if frameType == TypeToolResult {
		value, ok := payload.(ToolResult)
		if !ok || !validIdentifier(value.CallID) {
			return errors.New("tool.result requires a typed tool payload")
		}
		if _, exists := b.pendingToolCalls[value.CallID]; !exists {
			return &ProtocolError{Category: CategoryProtocol, Detail: "tool result does not name an outstanding call"}
		}
		toolResultID = value.CallID
	}
	b.view.LastOutboundSeq++
	frame, err := NewFrame(frameType, id, b.view.LastOutboundSeq, payload)
	if err != nil {
		b.view.LastOutboundSeq--
		if deliveryReserved {
			b.deliveries.release(deliveryID)
		}
		return err
	}
	body, err := EncodeFrame(frame)
	if err != nil {
		b.view.LastOutboundSeq--
		if deliveryReserved {
			b.deliveries.release(deliveryID)
		}
		return err
	}
	if err := b.connection.WriteFrame(body); err != nil {
		if deliveryReserved {
			b.deliveries.release(deliveryID)
		}
		return err
	}
	if toolResultID != "" {
		delete(b.pendingToolCalls, toolResultID)
	}
	return nil
}

func (b *binding) markHeartbeat() {
	b.mu.Lock()
	b.lastHeartbeat = time.Now()
	b.mu.Unlock()
}

func (b *binding) heartbeatDeadline() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastHeartbeat.Add(time.Duration(b.heartbeatGrace) * b.heartbeatInterval)
}

func (b *binding) close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	return b.connection.Close()
}
