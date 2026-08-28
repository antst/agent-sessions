package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const legacyQuiescenceStrategy = "operator_stopped_maintenance_window"

// EvaluateLegacyQuiescence evaluates the complete, already-classified legacy
// inventory without mutating it or any durable source. A successful result
// proves the operator-held maintenance window for later adoption and exact
// artifact retirement; it never stops, transfers, or retires work itself.
func EvaluateLegacyQuiescence(
	ctx context.Context,
	request LegacyQuiescenceRequest,
) (LegacyQuiescenceReport, error) {
	report := LegacyQuiescenceReport{Strategy: legacyQuiescenceStrategy}
	seenCandidates := make(map[string]struct{}, len(request.Candidates))

	for index := range request.Candidates {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		candidate := request.Candidates[index]
		if err := candidate.Validate(); err != nil {
			return report, fmt.Errorf("validate legacy candidate %q: %w", candidate.CandidateID, err)
		}
		if _, duplicate := seenCandidates[candidate.CandidateID]; duplicate {
			return report, fmt.Errorf("legacy quiescence repeats candidate %q", candidate.CandidateID)
		}
		seenCandidates[candidate.CandidateID] = struct{}{}

		if legacyQuiescenceExcluded(candidate) {
			continue
		}
		switch candidate.Classification {
		case LegacyClassificationActiveManagedBlocker:
			report.Blockers = append(report.Blockers, blockersForLegacyCandidate(candidate)...)
		case LegacyClassificationLiveLegacyAuthority:
			report.Blockers = append(report.Blockers, blockersForLegacyCandidate(candidate)...)
		case LegacyClassificationStale:
			report.StaleCandidateIDs = append(report.StaleCandidateIDs, candidate.CandidateID)
		case LegacyClassificationUnknown:
			report.Debt = append(report.Debt, quiescenceDebt(candidate, "unknown_identity"))
		case LegacyClassificationConflicting:
			report.Debt = append(report.Debt, quiescenceDebt(candidate, "conflicting_identity"))
		case LegacyClassificationRetired, LegacyClassificationExcluded:
			// These candidates confer no authority and require no action.
		default:
			// Validate rejects this branch; keep the switch fail-closed if its
			// accepted set and this evaluator ever drift.
			return report, fmt.Errorf("legacy candidate %q has unsupported classification %q",
				candidate.CandidateID, candidate.Classification)
		}
	}

	sortLegacyQuiescenceReport(&report)
	report.LegacyAbsenceVerified = len(report.Blockers) == 0 && len(report.Debt) == 0
	if report.LegacyAbsenceVerified {
		return report, nil
	}
	report.RetryInstruction = legacyQuiescenceRetryInstruction(report)
	return report, fmt.Errorf("%w: %d live legacy blocker(s), %d identity debt item(s)",
		ErrLegacyQuiescenceBlocked, len(report.Blockers), len(report.Debt))
}

func blockersForLegacyCandidate(candidate LegacyRuntimeCandidate) []LegacyMigrationBlocker {
	blockers := make([]LegacyMigrationBlocker, 0, len(candidate.RelatedSessionIDs)+len(candidate.RelatedLaneIDs))
	for _, sessionID := range candidate.RelatedSessionIDs {
		blockers = append(blockers, legacyMigrationBlocker(candidate, "peer", sessionID))
	}
	for _, laneID := range candidate.RelatedLaneIDs {
		blockers = append(blockers, legacyMigrationBlocker(candidate, "lane", laneID))
	}
	if len(blockers) == 0 {
		blockers = append(blockers, legacyMigrationBlocker(candidate, "authority", candidate.CandidateID))
	}
	return blockers
}

func legacyMigrationBlocker(
	candidate LegacyRuntimeCandidate,
	resourceType, resourceID string,
) LegacyMigrationBlocker {
	return LegacyMigrationBlocker{
		SchemaVersion: MigrationSchemaVersion,
		Revision:      1,
		BlockerID: quiescenceRecordID(
			"legacy-blocker", candidate.CandidateID, resourceType, resourceID,
		),
		CandidateID: candidate.CandidateID, Kind: candidate.Kind,
		ResourceType: resourceType, ResourceID: resourceID, RequiredAction: "close_before_retry",
		EvidenceRevision: candidate.EvidenceRevision, LastObservedAt: candidate.LastObservedAt,
	}
}

func quiescenceDebt(candidate LegacyRuntimeCandidate, code string) LegacyMigrationDebt {
	return LegacyMigrationDebt{
		SchemaVersion: MigrationSchemaVersion,
		Revision:      1,
		DebtID:        quiescenceRecordID("legacy-debt", candidate.CandidateID, code),
		CandidateID:   candidate.CandidateID,
		Code:          code,
		Retryable:     true,
		ExpectedIdentity: strings.Join([]string{
			candidate.RuntimeIdentity, candidate.ProcStart, candidate.StrongStart, candidate.EndpointPath,
		}, "|"),
		ObservedIdentity: candidate.Classification,
		RetryPredicate:   "reinventory_exact_candidate",
		ProhibitedScope:  "stop_or_retire:" + candidate.CandidateID,
		EvidenceRevision: candidate.EvidenceRevision,
		CreatedAt:        candidate.LastObservedAt,
		UpdatedAt:        candidate.LastObservedAt,
	}
}

func legacyQuiescenceExcluded(candidate LegacyRuntimeCandidate) bool {
	if candidate.Classification == LegacyClassificationExcluded {
		return true
	}
	switch candidate.Kind {
	case "central_hub", "native_codex", "native_claude", "native_grok", "native_qwen":
		return true
	default:
		return false
	}
}

func sortLegacyQuiescenceReport(report *LegacyQuiescenceReport) {
	sort.Slice(report.Blockers, func(i, j int) bool {
		left, right := report.Blockers[i], report.Blockers[j]
		return left.CandidateID+"\x00"+left.ResourceType+"\x00"+left.ResourceID <
			right.CandidateID+"\x00"+right.ResourceType+"\x00"+right.ResourceID
	})
	sort.Slice(report.Debt, func(i, j int) bool {
		left, right := report.Debt[i], report.Debt[j]
		return left.CandidateID+"\x00"+left.Code < right.CandidateID+"\x00"+right.Code
	})
	sort.Strings(report.StaleCandidateIDs)
}

func legacyQuiescenceRetryInstruction(report LegacyQuiescenceReport) string {
	parts := make([]string, 0, len(report.Blockers)+len(report.Debt))
	for _, blocker := range report.Blockers {
		parts = append(parts, fmt.Sprintf("close %s %s", blocker.ResourceType, blocker.ResourceID))
	}
	for _, debt := range report.Debt {
		parts = append(parts, fmt.Sprintf("reinspect candidate %s (%s)", debt.CandidateID, debt.Code))
	}
	return "migration remains read-only; " + strings.Join(parts, "; ") + "; then retry"
}

func quiescenceRecordID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}
