package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/antst/sessionbus/internal/diagnostics"
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
	records := diagnostics.Records{
		Attachments: len(activeAttachments), ActiveAttachments: len(activeAttachments),
		// Candidate rows are private lookup inputs, never live lane records.
		Lanes: 0,
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
	return diagnostics.Marshal(diagnostics.Input{
		Operation: operation, RuntimeReady: r.ready.Load(), Generation: r.generation,
		CatalogRevision: snapshot.Revision, ServiceState: "running",
		ReleasePresent: strings.TrimSpace(r.release) != "", EndpointPresent: r.control != nil,
		Records: records, ProductStates: productStates,
	})
}
