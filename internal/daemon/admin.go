package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

func (r *Runtime) runtimeStatus(ctx context.Context, operation string) (json.RawMessage, error) {
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
		Lanes:        len(catalog.Lanes),
		CleanupDebts: len(catalog.CleanupDebts),
	}
	for _, lane := range catalog.Lanes {
		if lane.State != "archived" {
			records.ActiveLanes++
		}
	}
	var productStates map[string]string
	if r.productDiagnosticsProvider != nil {
		productStates, err = r.productDiagnosticsProvider(ctx, operation)
		if err != nil {
			// Product probes can return vendor diagnostics. Keep the admin
			// failure surface fixed and metadata-only.
			return nil, errors.New("live product diagnostics unavailable")
		}
	}
	host := catalog.Host
	return diagnostics.Marshal(diagnostics.Input{
		Operation: operation, RuntimeReady: r.ready.Load(), Generation: r.generation,
		CatalogRevision: snapshot.Revision, ServiceState: host.ServiceState,
		ReleasePresent:  strings.TrimSpace(host.Release) != "",
		EndpointPresent: strings.TrimSpace(host.Endpoint) != "",
		Revisions: diagnostics.Revisions{
			Attachments: host.AttachmentRevision,
			Lanes:       host.LaneRevision, CleanupDebt: host.CleanupDebtRevision,
			Federation: host.FederationRevision,
		},
		Records: records, ProductStates: productStates,
	})
}
