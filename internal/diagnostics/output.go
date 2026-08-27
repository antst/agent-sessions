// Package diagnostics renders bounded metadata-only operational output shared
// by host and hub roles.
package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// EnvelopeSchemaVersion is the stable operational-output schema.
	EnvelopeSchemaVersion = 1
	// MaxCauseDetailBytes bounds non-secret operator error context.
	MaxCauseDetailBytes   = 512
	maxMetadataString     = 1024
	maxMetadataCollection = 256
)

// OutputKind identifies one shared operational-output boundary.
type OutputKind string

const (
	// OutputNormal is ordinary metadata-only operational output.
	OutputNormal OutputKind = "normal"
	// OutputDebug is metadata-only debug output.
	OutputDebug OutputKind = "debug"
	// OutputError is metadata-only failure output.
	OutputError OutputKind = "error"
	// OutputCrashReport is a bounded metadata-only crash report.
	OutputCrashReport OutputKind = "crash_report"
	// OutputMetric is a metadata-only metric export.
	OutputMetric OutputKind = "metric"
	// OutputTrace is a metadata-only trace export.
	OutputTrace OutputKind = "trace"
)

var outputKinds = map[OutputKind]struct{}{
	OutputNormal: {}, OutputDebug: {}, OutputError: {}, OutputCrashReport: {}, OutputMetric: {}, OutputTrace: {},
}

// Metadata fields are deliberately closed. New operational fields require a
// contract change and canary coverage rather than flowing through implicitly.
var metadataFields = map[string]struct{}{
	"request_id": {}, "operation": {}, "role": {}, "product": {}, "identity": {},
	"state": {}, "revision": {}, "duration_ms": {}, "error_code": {}, "cause_detail": {},
	"runtime_version": {}, "runtime_identity": {}, "generation": {}, "pid": {}, "proc_start": {},
	"endpoint": {}, "service": {}, "host_id": {}, "host_name": {}, "protocol_version": {},
	"products": {}, "attachments": {}, "lanes": {}, "federation": {}, "migration": {},
	"debt": {}, "healthy": {}, "checks": {}, "listener": {}, "connected_hosts": {}, "routing": {},
	"admission": {}, "recovery_stage": {}, "retryable": {}, "next_action": {},
}

var forbiddenContentFields = map[string]struct{}{
	"payload": {}, "content": {}, "message": {}, "prompt": {}, "lane_input": {}, "lane_result": {},
	"tool_arguments": {}, "tool_result": {}, "raw_launch_capability": {}, "capability": {},
	"credential": {}, "credentials": {}, "secret": {}, "token": {}, "vendor_transcript": {}, "transcript": {},
}

// Envelope is the common bounded metadata-only diagnostic record.
type Envelope struct {
	SchemaVersion int            `json:"schema_version"`
	Kind          OutputKind     `json:"kind"`
	Event         string         `json:"event"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// Render filters first and serializes second, so debug, crash, metric, and
// trace paths cannot bypass the same content policy as normal logs.
func Render(kind OutputKind, event string, fields map[string]any) ([]byte, error) {
	if _, ok := outputKinds[kind]; !ok {
		return nil, fmt.Errorf("unsupported diagnostic output kind %q", kind)
	}
	if strings.TrimSpace(event) == "" {
		return nil, errors.New("diagnostic event is empty")
	}
	metadata := make(map[string]any)
	for key, value := range fields {
		if _, forbidden := forbiddenContentFields[key]; forbidden {
			continue
		}
		if _, allowed := metadataFields[key]; !allowed {
			continue
		}
		if key == "cause_detail" {
			metadata[key] = BoundedCauseDetail(fmt.Sprint(value))
			continue
		}
		if safe, ok := sanitizeMetadata(value, 0); ok {
			metadata[key] = safe
		}
	}
	return json.Marshal(Envelope{SchemaVersion: EnvelopeSchemaVersion, Kind: kind, Event: event, Metadata: metadata})
}

// BoundedCauseDetail removes control characters and returns at most the
// documented number of UTF-8 bytes of non-secret operator context.
func BoundedCauseDetail(detail string) string {
	var builder strings.Builder
	builder.Grow(min(len(detail), MaxCauseDetailBytes))
	for _, value := range detail {
		if value < 0x20 || value == 0x7f {
			value = ' '
		}
		width := utf8.RuneLen(value)
		if width < 0 || builder.Len()+width > MaxCauseDetailBytes {
			break
		}
		builder.WriteRune(value)
	}
	return strings.TrimSpace(builder.String())
}

func sanitizeMetadata(value any, depth int) (any, bool) {
	if depth > 4 {
		return nil, false
	}
	switch typed := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return typed, true
	case string:
		return boundMetadataString(typed), true
	case []string:
		limit := min(len(typed), maxMetadataCollection)
		result := make([]string, 0, limit)
		for _, item := range typed[:limit] {
			result = append(result, boundMetadataString(item))
		}
		return result, true
	case []any:
		limit := min(len(typed), maxMetadataCollection)
		result := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			if safe, ok := sanitizeMetadata(item, depth+1); ok {
				result = append(result, safe)
			}
		}
		return result, true
	case map[string]any:
		result := make(map[string]any)
		count := 0
		for key, item := range typed {
			if count >= maxMetadataCollection {
				break
			}
			if _, forbidden := forbiddenContentFields[key]; forbidden {
				continue
			}
			if safe, ok := sanitizeMetadata(item, depth+1); ok {
				result[boundMetadataString(key)] = safe
				count++
			}
		}
		return result, true
	default:
		return nil, false
	}
}

func boundMetadataString(value string) string {
	return boundedDetailWithLimit(value, maxMetadataString)
}

func boundedDetailWithLimit(detail string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var builder strings.Builder
	builder.Grow(min(len(detail), limit))
	for _, value := range detail {
		if value < 0x20 || value == 0x7f {
			value = ' '
		}
		width := utf8.RuneLen(value)
		if width < 0 || builder.Len()+width > limit {
			break
		}
		builder.WriteRune(value)
	}
	return strings.TrimSpace(builder.String())
}
