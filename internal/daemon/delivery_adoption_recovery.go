package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/statestore"
)

const deliveryDestinationDispatching = "dispatching"

func (runtime *Runtime) reconcileAdoptedDeliveries(ctx context.Context) error {
	engine := runtime.deliveryEngine()
	if engine == nil {
		return nil
	}
	return engine.ReconcileAdoptedDeliveries(ctx)
}

// ReconcileAdoptedDeliveries resumes only migration-tagged accepted work.
// A durable dispatching claim is written before the native adapter is called;
// after a crash that claim becomes exact ambiguity debt and is never blindly
// redispatched. Ordinary pre-existing delivery records retain their existing
// recovery policy.
//
//nolint:gocyclo // Durable claim, ambiguity, adapter, and terminal transitions fail independently.
func (engine *DeliveryEngine) ReconcileAdoptedDeliveries(ctx context.Context) error {
	if engine == nil || engine.state == nil || engine.attachments == nil {
		return errors.New("adopted delivery reconciliation requires delivery and attachment authorities")
	}
	attached, ambiguous := adoptedDeliveryAttachments(engine.attachments.attachedRecords())
	engine.mu.Lock()
	defer engine.mu.Unlock()

	messageIDs := make([]string, 0, len(engine.records))
	for messageID := range engine.records {
		messageIDs = append(messageIDs, messageID)
	}
	sort.Strings(messageIDs)
	for _, messageID := range messageIDs {
		record := engine.records[messageID]
		if record.AdoptedSourceRevision == "" || record.State != DeliveryStateAccepted {
			continue
		}
		destinations := append([]string(nil), record.ResolvedDestinations...)
		sort.Strings(destinations)
		for _, destination := range destinations {
			switch record.DestinationResults[destination] {
			case deliveryDestinationDispatching:
				if err := engine.recordAdoptedDeliveryDebt(ctx, record, destination, "delivery_dispatch_outcome_ambiguous"); err != nil {
					return err
				}
				continue
			case DeliveryDestinationPending:
			default:
				continue
			}
			if _, collision := ambiguous[destination]; collision {
				if err := engine.recordAdoptedDeliveryDebt(ctx, record, destination, "delivery_destination_ambiguous"); err != nil {
					return err
				}
				continue
			}
			target, ready := attached[destination]
			if !ready {
				continue // Dormant accepted work remains durable until its exact session attaches.
			}
			adapter := engine.adapters[target.Product]
			if adapter == nil {
				if err := engine.recordAdoptedDeliveryDebt(ctx, record, destination, "delivery_adapter_unavailable"); err != nil {
					return err
				}
				continue
			}
			claimed := cloneDeliveryRecord(record)
			claimed.DestinationResults[destination] = deliveryDestinationDispatching
			engine.advanceAdoptedDelivery(&claimed)
			if err := engine.commitDeliveryCatalog(ctx, func(records map[string]DeliveryRecord) {
				records[claimed.MessageID] = claimed
			}); err != nil {
				return fmt.Errorf("claim adopted delivery %s/%s: %w", claimed.MessageID, destination, err)
			}

			frame := federation.AgentFrame{
				Version: federation.AgentFrameVersion, Type: claimed.Operation, MessageID: claimed.MessageID,
				SourceSessionID: claimed.SourceSessionID, Targets: []string{destination}, Group: claimed.Group,
				Content: claimed.Content, Summary: claimed.Summary, SentAt: claimed.SentAt,
			}
			deliveryErr := adapter.Deliver(ctx, cloneAttachmentRecord(target), frame)
			terminal := cloneDeliveryRecord(claimed)
			if deliveryErr == nil {
				terminal.DestinationResults[destination] = DeliveryDestinationDelivered
			} else {
				terminal.DestinationResults[destination] = DeliveryDestinationFailed
			}
			terminal.State = adoptedDeliveryAggregateState(terminal.DestinationResults)
			engine.advanceAdoptedDelivery(&terminal)
			if err := engine.commitDeliveryCatalog(ctx, func(records map[string]DeliveryRecord) {
				records[terminal.MessageID] = terminal
			}); err != nil {
				return fmt.Errorf("commit adopted delivery %s/%s outcome: %w", terminal.MessageID, destination, err)
			}
			engine.emit(
				DeliveryRequest{MessageID: terminal.MessageID, Operation: terminal.Operation},
				terminal.State, 1, deliveryErrorCode(deliveryErr),
			)
			record = terminal
		}
	}
	return nil
}

func adoptedDeliveryAttachments(records []AttachmentRecord) (map[string]AttachmentRecord, map[string]struct{}) {
	result := make(map[string]AttachmentRecord, len(records))
	ambiguous := make(map[string]struct{})
	for _, record := range records {
		address := attachmentAddress(record)
		if prior, exists := result[address]; exists && prior.AttachmentID != record.AttachmentID {
			delete(result, address)
			ambiguous[address] = struct{}{}
			continue
		}
		if _, collision := ambiguous[address]; !collision {
			result[address] = record
		}
	}
	return result, ambiguous
}

func adoptedDeliveryAggregateState(results map[string]string) string {
	failed := false
	for _, result := range results {
		switch result {
		case DeliveryDestinationPending, deliveryDestinationDispatching:
			return DeliveryStateAccepted
		case DeliveryDestinationFailed:
			failed = true
		}
	}
	if failed {
		return DeliveryStateFailed
	}
	return DeliveryStateDelivered
}

func (engine *DeliveryEngine) advanceAdoptedDelivery(record *DeliveryRecord) {
	record.Revision++
	now := engine.now().UnixMilli()
	if now < record.UpdatedAt {
		now = record.UpdatedAt
	}
	if now <= 0 {
		now = 1
	}
	record.UpdatedAt = now
}

func (engine *DeliveryEngine) recordAdoptedDeliveryDebt(
	ctx context.Context,
	record DeliveryRecord,
	destination string,
	cause string,
) error {
	debtID := quiescenceRecordID("adopted-delivery-debt", record.MessageID, destination, cause)
	if _, _, err := engine.state.ReadDebt(ctx, debtID); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	now := engine.now().UnixMilli()
	if now <= 0 {
		now = record.UpdatedAt
	}
	debt := DebtRecord{
		RecordHeader: productionLegacyMigrationHeader(now), DebtID: debtID,
		Operation: "delivery.reconcile", ResourceKind: "adopted_delivery",
		ResourceIdentity: record.MessageID + "/" + destination,
		ExpectedRevision: record.AdoptedSourceRevision, ObservedRevision: record.RequestDigest,
		CauseCode:       cause,
		RetryPredicate:  "reconcile the exact destination acceptance without redispatching an uncertain native call",
		ProhibitedScope: "do not read vendor transcripts, result/error bodies, profiles, or content logs",
	}
	if _, err := engine.state.CompareAndSwapDebt(ctx, 0, debt); err != nil {
		if !errors.Is(err, statestore.ErrRevisionConflict) {
			return err
		}
		if _, _, readErr := engine.state.ReadDebt(ctx, debtID); readErr != nil {
			return errors.Join(err, readErr)
		}
	}
	return nil
}
