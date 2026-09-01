// Package diagnostics builds bounded, metadata-only daemon status and doctor
// reports. It deliberately accepts only fixed enums, booleans, and counts so
// message, result, credential, and transcript bytes cannot reach its output.
package diagnostics

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const (
	// Schema is the stable admin-report schema identifier.
	Schema = "agent-sessions.admin.v1"
	// MaxReportBytes is a defensive upper bound well below the control frame.
	MaxReportBytes = 32 << 10
)

// Input is the closed aggregate input accepted by the diagnostic projector.
type Input struct {
	Operation       string
	RuntimeReady    bool
	Generation      uint64
	CatalogRevision uint64
	ServiceState    string
	ReleasePresent  bool
	EndpointPresent bool
	Revisions       Revisions
	Records         Records
	ProductStates   map[string]string
}

// Revisions exposes monotonic catalog family revisions only.
type Revisions struct {
	Attachments uint64 `json:"attachments"`
	Lanes       uint64 `json:"lanes"`
	CleanupDebt uint64 `json:"cleanup_debt"`
	Federation  uint64 `json:"federation"`
}

// Records exposes aggregate durable record counts only.
type Records struct {
	Attachments       int `json:"attachments"`
	ActiveAttachments int `json:"active_attachments"`
	Lanes             int `json:"lanes"`
	ActiveLanes       int `json:"active_lanes"`
	Turns             int `json:"turns"`
	UncollectedTurns  int `json:"uncollected_turns"`
	CleanupDebts      int `json:"cleanup_debts"`
}

// Report is one fixed, metadata-only admin response.
type Report struct {
	Schema    string    `json:"schema"`
	Operation string    `json:"operation"`
	Ready     bool      `json:"ready"`
	Daemon    Daemon    `json:"daemon"`
	Revisions Revisions `json:"revisions"`
	Records   Records   `json:"records"`
	Products  []Product `json:"products"`
	Checks    []Check   `json:"checks,omitempty"`
}

// Daemon contains fixed service metadata, never raw host/path values.
type Daemon struct {
	Generation      uint64 `json:"generation"`
	CatalogRevision uint64 `json:"catalog_revision"`
	ServiceState    string `json:"service_state"`
	ReleasePresent  bool   `json:"release_present"`
	EndpointPresent bool   `json:"endpoint_present"`
}

// Product contains one authoritative product ID and normalized readiness.
type Product struct {
	ID        string `json:"id"`
	Readiness string `json:"readiness"`
}

// Check is one fixed diagnostic assertion code.
type Check struct {
	Code   string `json:"code"`
	Status string `json:"status"`
}

// Build creates one truthful fixed-schema status or doctor report.
func Build(input Input) (Report, error) {
	if input.Operation != "status" && input.Operation != "doctor" {
		return Report{}, errors.New("diagnostic operation must be status or doctor")
	}
	serviceState := normalizedServiceState(input.ServiceState)
	report := Report{
		Schema: Schema, Operation: input.Operation,
		Ready: input.RuntimeReady && serviceState == "running" && input.EndpointPresent,
		Daemon: Daemon{
			Generation: input.Generation, CatalogRevision: input.CatalogRevision,
			ServiceState: serviceState, ReleasePresent: input.ReleasePresent,
			EndpointPresent: input.EndpointPresent,
		},
		Revisions: input.Revisions,
		Records:   nonnegativeRecords(input.Records),
		Products:  products(input.ProductStates),
	}
	if input.Operation == "doctor" {
		report.Checks = doctorChecks(report)
	}
	return report, nil
}

// Marshal builds a report and enforces the defensive output bound.
func Marshal(input Input) (json.RawMessage, error) {
	report, err := Build(input)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxReportBytes {
		return nil, errors.New("diagnostic report exceeds fixed output bound")
	}
	return body, nil
}

func products(states map[string]string) []Product {
	result := make([]Product, 0, len(productcatalog.All()))
	for _, product := range productcatalog.All() {
		result = append(result, Product{ID: product.ID, Readiness: normalizedReadiness(states[product.ID])})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func normalizedReadiness(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ready", "available", "ok", "true":
		return "available"
	case "missing", "unavailable", "failed", "error", "false":
		return "unavailable"
	default:
		return "unknown"
	}
}

func normalizedServiceState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "running", "stopped", "failed":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func nonnegativeRecords(records Records) Records {
	values := []*int{
		&records.Attachments, &records.ActiveAttachments,
		&records.Lanes, &records.ActiveLanes, &records.Turns,
		&records.UncollectedTurns, &records.CleanupDebts,
	}
	for _, value := range values {
		if *value < 0 {
			*value = 0
		}
	}
	return records
}

func doctorChecks(report Report) []Check {
	status := func(value bool) string {
		if value {
			return "pass"
		}
		return "fail"
	}
	checks := make([]Check, 0, 3+len(report.Products))
	checks = append(checks,
		Check{Code: "daemon.ready", Status: status(report.Ready)},
		Check{Code: "daemon.endpoint", Status: status(report.Daemon.EndpointPresent)},
		Check{Code: "catalog.cleanup_debt_clear", Status: status(report.Records.CleanupDebts == 0)},
	)
	for _, product := range report.Products {
		checkStatus := "unknown"
		switch product.Readiness {
		case "available":
			checkStatus = "pass"
		case "unavailable":
			checkStatus = "unavailable"
		}
		checks = append(checks, Check{Code: "product." + product.ID, Status: checkStatus})
	}
	return checks
}
