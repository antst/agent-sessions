package daemon

import (
	"encoding/json"
	"strings"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

func (r *Runtime) runtimeStatus(operation string) (json.RawMessage, error) {
	snapshot, err := r.state.Read()
	if err != nil {
		return nil, err
	}
	activeAttachments, err := r.attachments.ListActive()
	if err != nil {
		return nil, err
	}
	catalog := snapshot.Catalog
	records := diagnostics.Records{
		Attachments: len(catalog.Attachments), ActiveAttachments: len(activeAttachments),
		Deliveries: len(catalog.Deliveries), Lanes: len(catalog.Lanes),
		Turns: len(catalog.Turns), CleanupDebts: len(catalog.CleanupDebts),
	}
	for _, lane := range catalog.Lanes {
		if lane.State != "archived" {
			records.ActiveLanes++
		}
	}
	for _, turn := range catalog.Turns {
		if turn.State == "terminal" && turn.CollectionRevision == 0 {
			records.UncollectedTurns++
		}
	}
	host := catalog.Host
	return diagnostics.Marshal(diagnostics.Input{
		Operation: operation, RuntimeReady: r.ready.Load(), Generation: r.generation,
		CatalogRevision: snapshot.Revision, ServiceState: host.ServiceState,
		ReleasePresent:  strings.TrimSpace(host.Release) != "",
		EndpointPresent: strings.TrimSpace(host.Endpoint) != "",
		Revisions: diagnostics.Revisions{
			Attachments: host.AttachmentRevision, Deliveries: host.DeliveryRevision,
			Lanes: host.LaneRevision, CleanupDebt: host.CleanupDebtRevision,
			Federation: host.FederationRevision,
		},
		Records: records, ProductStates: host.ProductReadiness,
	})
}
