package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/statestore"
)

const (
	// DeliveryOperationSend is a direct one-target delivery.
	DeliveryOperationSend = "send"
	// DeliveryOperationMulticast is an explicit atomic multi-target admission.
	DeliveryOperationMulticast = "multicast"
	// DeliveryOperationBroadcast resolves every visible member of one existing global group.
	DeliveryOperationBroadcast = "broadcast"

	// DeliveryStateAccepted means the exact destination set is durably committed.
	DeliveryStateAccepted = "accepted"
	// DeliveryStateDelivered means every destination has a terminal delivered outcome.
	DeliveryStateDelivered = "delivered"
	// DeliveryStateFailed means at least one exact destination has a terminal failed outcome.
	DeliveryStateFailed = "failed"

	// DeliveryDestinationPending has not yet reached its vendor adapter.
	DeliveryDestinationPending = "pending"
	// DeliveryDestinationDelivered is one terminal successful destination result.
	DeliveryDestinationDelivered = "delivered"
	// DeliveryDestinationFailed is one terminal failed destination result.
	DeliveryDestinationFailed = "failed"
)

var (
	// ErrDeliveryUnauthorized rejects a recipient set or broadcast group before acceptance.
	ErrDeliveryUnauthorized = errors.New("delivery is not authorized by existing global groups")
	// ErrDeliveryNotFound identifies an absent durable message operation.
	ErrDeliveryNotFound = errors.New("delivery was not found")
	// ErrDeliveryIdempotencyConflict rejects reuse of a message ID for different work.
	ErrDeliveryIdempotencyConflict = errors.New("delivery idempotency key conflicts with accepted work")
)

// DeliveryResourceError reports one bounded local resource gate before durable acceptance.
type DeliveryResourceError struct{ Resource string }

func (failure *DeliveryResourceError) Error() string {
	return fmt.Sprintf("delivery %s capacity is unavailable before acceptance", failure.Resource)
}

// DeliveryAdapter emits an already admitted AgentFrame through one vendor-owned native channel.
type DeliveryAdapter interface {
	// Deliver returns only after the exact native destination has accepted or rejected the frame.
	Deliver(context.Context, AttachmentRecord, federation.AgentFrame) error
}

// DeliveryRequest is one product-neutral local delivery admission request.
type DeliveryRequest struct {
	MessageID          string
	SourceAttachmentID string
	Operation          string
	Targets            []string
	Group              string
	Content            string
	Summary            string
	SentAt             string
}

// DeliveryObservation is metadata-only diagnostic state; it intentionally has no content fields.
type DeliveryObservation struct {
	MessageID        string `json:"message_id"`
	Operation        string `json:"operation"`
	State            string `json:"state"`
	DestinationCount int    `json:"destination_count"`
	ErrorCode        string `json:"error_code,omitempty"`
}

// DeliveryEngineOptions composes durable routing with attachment and adapter authorities.
type DeliveryEngineOptions struct {
	State       *StateStore
	Attachments *AttachmentRegistry
	Adapters    map[string]DeliveryAdapter
	Now         func() time.Time
	Preflight   func(context.Context, DeliveryRequest) error
	Observe     func(DeliveryObservation)
}

type deliveryCatalog struct {
	Records []DeliveryRecord `json:"records"`
}

// DeliveryEngine owns local message admission, durable outcomes, retry, and discovery.
type DeliveryEngine struct {
	mu            sync.Mutex
	state         *StateStore
	storeRevision statestore.Revision
	attachments   *AttachmentRegistry
	adapters      map[string]DeliveryAdapter
	now           func() time.Time
	preflight     func(context.Context, DeliveryRequest) error
	observe       func(DeliveryObservation)
	records       map[string]DeliveryRecord
}

// NewDeliveryEngine loads accepted operations before admitting new messages.
func NewDeliveryEngine(options DeliveryEngineOptions) (*DeliveryEngine, error) {
	if options.State == nil || options.Attachments == nil {
		return nil, errors.New("delivery engine requires state and attachment registry")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	engine := &DeliveryEngine{
		state: options.State, attachments: options.Attachments, adapters: make(map[string]DeliveryAdapter),
		now: options.Now, preflight: options.Preflight, observe: options.Observe,
		records: make(map[string]DeliveryRecord),
	}
	for product, adapter := range options.Adapters {
		if adapter != nil {
			engine.adapters[product] = adapter
		}
	}
	catalog, revision, err := options.State.readDeliveryCatalog(context.Background())
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load delivery catalog: %w", err)
	}
	if err == nil {
		engine.storeRevision = revision
		for _, record := range catalog.Records {
			engine.records[record.MessageID] = cloneDeliveryRecord(record)
		}
	}
	return engine, nil
}

// Discover returns every other attached peer sharing at least one existing global group.
func (engine *DeliveryEngine) Discover(_ context.Context, sourceAttachmentID string) ([]federation.Peer, error) {
	source, ok := engine.attachments.attachedByID(sourceAttachmentID)
	if !ok {
		return nil, ErrAttachmentNotAttested
	}
	peers := make([]federation.Peer, 0)
	for _, destination := range engine.attachments.attachedRecords() {
		if destination.AttachmentID == source.AttachmentID || !attachmentsShareGroup(source, destination) {
			continue
		}
		peers = append(peers, attachmentPeer(destination))
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers, nil
}

// Accept validates the complete recipient set, commits it, and advances only pending destination outcomes.
func (engine *DeliveryEngine) Accept(ctx context.Context, request DeliveryRequest) (DeliveryRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.preflight != nil {
		if err := engine.preflight(ctx, request); err != nil {
			engine.emit(request, "rejected", 0, "resource_capacity")
			return DeliveryRecord{}, err
		}
	}
	digest, err := deliveryRequestDigest(request)
	if err != nil {
		return DeliveryRecord{}, err
	}
	if accepted, ok := engine.records[request.MessageID]; ok {
		if accepted.RequestDigest != digest {
			return DeliveryRecord{}, ErrDeliveryIdempotencyConflict
		}
		return cloneDeliveryRecord(accepted), nil
	}
	source, ok := engine.attachments.attachedByID(request.SourceAttachmentID)
	if !ok {
		return DeliveryRecord{}, ErrAttachmentNotAttested
	}
	destinations, err := engine.resolveDestinations(source, request)
	if err != nil {
		engine.emit(request, "rejected", 0, "unauthorized")
		return DeliveryRecord{}, err
	}
	accepted := engine.newAcceptedRecord(source, request, digest, destinations)
	if err := engine.commitDeliveryCatalog(ctx, func(records map[string]DeliveryRecord) {
		records[accepted.MessageID] = accepted
	}); err != nil {
		return DeliveryRecord{}, err
	}
	engine.emit(request, DeliveryStateAccepted, len(destinations), "")
	return engine.deliverAccepted(ctx, accepted, destinations)
}

// Read returns one exact durable message operation.
func (engine *DeliveryEngine) Read(_ context.Context, messageID string) (DeliveryRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	record, ok := engine.records[messageID]
	if !ok {
		return DeliveryRecord{}, ErrDeliveryNotFound
	}
	return cloneDeliveryRecord(record), nil
}

func (engine *DeliveryEngine) resolveDestinations(source AttachmentRecord, request DeliveryRequest) ([]AttachmentRecord, error) {
	if strings.TrimSpace(request.MessageID) == "" || len(request.Content) > federation.MaxAgentFrameBytes {
		return nil, ErrDeliveryUnauthorized
	}
	attached := engine.attachments.attachedRecords()
	var (
		destinations []AttachmentRecord
		err          error
	)
	switch request.Operation {
	case DeliveryOperationSend, DeliveryOperationMulticast:
		destinations, err = resolveExplicitDestinations(source, request, attached)
	case DeliveryOperationBroadcast:
		destinations, err = resolveBroadcastDestinations(source, request, attached)
	default:
		return nil, ErrDeliveryUnauthorized
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(destinations, func(i, j int) bool { return attachmentAddress(destinations[i]) < attachmentAddress(destinations[j]) })
	return destinations, nil
}

func resolveExplicitDestinations(
	source AttachmentRecord,
	request DeliveryRequest,
	attached []AttachmentRecord,
) ([]AttachmentRecord, error) {
	if len(request.Targets) == 0 || (request.Operation == DeliveryOperationSend && len(request.Targets) != 1) {
		return nil, ErrDeliveryUnauthorized
	}
	byAddress := make(map[string]AttachmentRecord, len(attached))
	for _, record := range attached {
		byAddress[attachmentAddress(record)] = record
	}
	seen := make(map[string]struct{}, len(request.Targets))
	destinations := make([]AttachmentRecord, 0, len(request.Targets))
	for _, target := range request.Targets {
		destination, ok := byAddress[target]
		if !ok || destination.AttachmentID == source.AttachmentID || !attachmentsShareGroup(source, destination) {
			return nil, ErrDeliveryUnauthorized
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		seen[target] = struct{}{}
		destinations = append(destinations, destination)
	}
	return destinations, nil
}

func resolveBroadcastDestinations(
	source AttachmentRecord,
	request DeliveryRequest,
	attached []AttachmentRecord,
) ([]AttachmentRecord, error) {
	if len(request.Targets) != 0 || !attachmentHasGroup(source, request.Group) {
		return nil, ErrDeliveryUnauthorized
	}
	destinations := make([]AttachmentRecord, 0, len(attached))
	for _, destination := range attached {
		if destination.AttachmentID != source.AttachmentID && attachmentHasGroup(destination, request.Group) {
			destinations = append(destinations, destination)
		}
	}
	if len(destinations) == 0 {
		return nil, ErrDeliveryUnauthorized
	}
	return destinations, nil
}

func (engine *DeliveryEngine) newAcceptedRecord(
	source AttachmentRecord,
	request DeliveryRequest,
	digest string,
	destinations []AttachmentRecord,
) DeliveryRecord {
	now := engine.now().UnixMilli()
	addresses := make([]string, 0, len(destinations))
	results := make(map[string]string, len(destinations))
	for _, destination := range destinations {
		address := attachmentAddress(destination)
		addresses = append(addresses, address)
		results[address] = DeliveryDestinationPending
	}
	return DeliveryRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: source.Generation, CreatedAt: now, UpdatedAt: now},
		MessageID:    request.MessageID, SourceAttachmentID: source.AttachmentID,
		SourceHostID: source.HostID, SourceSessionID: source.SessionID, SourceAttachmentRevision: source.Revision,
		Operation: request.Operation, RequestedTargets: append([]string(nil), request.Targets...), Group: request.Group,
		ResolvedDestinations: addresses, Content: request.Content, Summary: request.Summary, SentAt: request.SentAt,
		State: DeliveryStateAccepted, DestinationResults: results, AcceptedRevision: 1, AcceptedAt: now,
		RequestDigest: digest,
	}
}

func (engine *DeliveryEngine) deliverAccepted(
	ctx context.Context,
	record DeliveryRecord,
	destinations []AttachmentRecord,
) (DeliveryRecord, error) {
	var deliveryErr error
	for _, destination := range destinations {
		address := attachmentAddress(destination)
		if record.DestinationResults[address] != DeliveryDestinationPending {
			continue
		}
		adapter := engine.adapters[destination.Product]
		frame := federation.AgentFrame{
			Version: federation.AgentFrameVersion, Type: record.Operation, MessageID: record.MessageID,
			SourceSessionID: record.SourceSessionID, Targets: []string{address}, Group: record.Group,
			Content: record.Content, Summary: record.Summary, SentAt: record.SentAt,
		}
		if adapter == nil {
			record.DestinationResults[address] = DeliveryDestinationFailed
			deliveryErr = errors.Join(deliveryErr, fmt.Errorf("delivery adapter %s is unavailable", destination.Product))
		} else if err := adapter.Deliver(ctx, destination, frame); err != nil {
			record.DestinationResults[address] = DeliveryDestinationFailed
			deliveryErr = errors.Join(deliveryErr, fmt.Errorf("deliver %s: %w", address, err))
		} else {
			record.DestinationResults[address] = DeliveryDestinationDelivered
		}
	}
	record.State = DeliveryStateDelivered
	if deliveryErr != nil {
		record.State = DeliveryStateFailed
	}
	record.Revision++
	record.UpdatedAt = engine.now().UnixMilli()
	if err := engine.commitDeliveryCatalog(ctx, func(records map[string]DeliveryRecord) {
		records[record.MessageID] = record
	}); err != nil {
		return DeliveryRecord{}, errors.Join(deliveryErr, err)
	}
	engine.emit(DeliveryRequest{MessageID: record.MessageID, Operation: record.Operation}, record.State, len(record.ResolvedDestinations), deliveryErrorCode(deliveryErr))
	return cloneDeliveryRecord(record), deliveryErr
}

func (engine *DeliveryEngine) commitDeliveryCatalog(ctx context.Context, mutate func(map[string]DeliveryRecord)) error {
	records := cloneDeliveryMap(engine.records)
	mutate(records)
	catalog := deliveryCatalog{Records: make([]DeliveryRecord, 0, len(records))}
	for _, record := range records {
		catalog.Records = append(catalog.Records, record)
	}
	sort.Slice(catalog.Records, func(i, j int) bool { return catalog.Records[i].MessageID < catalog.Records[j].MessageID })
	next, err := engine.state.compareAndSwapDeliveryCatalog(ctx, engine.storeRevision, catalog)
	if err != nil {
		return err
	}
	engine.storeRevision = next
	engine.records = records
	return nil
}

func (engine *DeliveryEngine) emit(request DeliveryRequest, state string, destinationCount int, errorCode string) {
	if engine.observe != nil {
		engine.observe(DeliveryObservation{
			MessageID: request.MessageID, Operation: request.Operation, State: state,
			DestinationCount: destinationCount, ErrorCode: errorCode,
		})
	}
}

func deliveryRequestDigest(request DeliveryRequest) (string, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode delivery request identity: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func attachmentPeer(record AttachmentRecord) federation.Peer {
	address := attachmentAddress(record)
	return federation.Peer{
		ID: address, HostID: record.HostID, HostName: record.HostID, SessionID: record.SessionID,
		GlobalID: address, Name: record.Name, DisplayName: record.Name + "--" + record.HostID,
		Status: "idle", Cwd: record.Cwd, Entrypoint: record.Product,
		PermissionMode: record.PermissionMode, Groups: append([]string(nil), record.Groups...),
	}
}

func attachmentAddress(record AttachmentRecord) string { return record.HostID + "/" + record.SessionID }

func attachmentsShareGroup(left, right AttachmentRecord) bool {
	for _, group := range left.Groups {
		if attachmentHasGroup(right, group) {
			return true
		}
	}
	return false
}

func attachmentHasGroup(record AttachmentRecord, group string) bool {
	index := sort.SearchStrings(record.Groups, group)
	return group != "" && index < len(record.Groups) && record.Groups[index] == group
}

func cloneDeliveryRecord(record DeliveryRecord) DeliveryRecord {
	record.RequestedTargets = append([]string(nil), record.RequestedTargets...)
	record.ResolvedDestinations = append([]string(nil), record.ResolvedDestinations...)
	if record.DestinationResults != nil {
		results := make(map[string]string, len(record.DestinationResults))
		for key, value := range record.DestinationResults {
			results[key] = value
		}
		record.DestinationResults = results
	}
	return record
}

func cloneDeliveryMap(source map[string]DeliveryRecord) map[string]DeliveryRecord {
	result := make(map[string]DeliveryRecord, len(source))
	for key, record := range source {
		result[key] = cloneDeliveryRecord(record)
	}
	return result
}

func deliveryErrorCode(err error) string {
	if err != nil {
		return "native_delivery_failed"
	}
	return ""
}
