package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestLegacyQuiescenceRefusesEveryExactBlockerBeforeMutation(t *testing.T) {
	root := t.TempDir()
	canary := filepath.Join(root, "legacy-state-canary.json")
	wantCanary := []byte(`{"revision":17,"state":"legacy-authority"}`)
	if err := os.WriteFile(canary, wantCanary, 0o600); err != nil {
		t.Fatal(err)
	}

	shim := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("shim", 501), []string{"codex-peer-1"}, nil)
	shim.SourcePath = canary
	claudeLane := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("claude_lane_manager", 502), nil, []string{"claude-lane-1"})
	grokHost := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("grok_host", 503), []string{"grok-peer-1"}, nil)
	quiescent := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("supervisor", 504), nil, nil)
	candidates := []LegacyRuntimeCandidate{shim, claudeLane, grokHost, quiescent}
	before := cloneLegacyRuntimeCandidatesForTest(candidates)

	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{Candidates: candidates})
	if !errors.Is(err, ErrLegacyQuiescenceBlocked) {
		t.Fatalf("quiescence error = %v, want ErrLegacyQuiescenceBlocked", err)
	}
	if report.LegacyAbsenceVerified || report.Strategy != legacyQuiescenceStrategy {
		t.Fatalf("blocked report = %#v", report)
	}
	wantBlockers := []legacyBlockerExpectation{
		{candidateID: shim.CandidateID, kind: "shim", resourceType: "peer", resourceID: "codex-peer-1"},
		{candidateID: claudeLane.CandidateID, kind: "claude_lane_manager", resourceType: "lane", resourceID: "claude-lane-1"},
		{candidateID: grokHost.CandidateID, kind: "grok_host", resourceType: "peer", resourceID: "grok-peer-1"},
		{candidateID: quiescent.CandidateID, kind: "supervisor", resourceType: "authority", resourceID: quiescent.CandidateID},
	}
	assertLegacyBlockers(t, report.Blockers, wantBlockers)
	for _, blocker := range report.Blockers {
		if blocker.RequiredAction != "close_before_retry" ||
			!strings.Contains(report.RetryInstruction, blocker.ResourceID) {
			t.Fatalf("blocker is not actionable: blocker=%#v instruction=%q", blocker, report.RetryInstruction)
		}
	}
	if len(report.Debt) != 0 {
		t.Fatalf("exact live blockers unexpectedly became debt: %#v", report.Debt)
	}
	if !reflect.DeepEqual(candidates, before) {
		t.Fatalf("quiescence inspection mutated candidate input:\nafter=%#v\nbefore=%#v", candidates, before)
	}
	gotCanary, readErr := os.ReadFile(canary)
	if readErr != nil || !reflect.DeepEqual(gotCanary, wantCanary) {
		t.Fatalf("blocked inspection mutated legacy state: body=%q err=%v", gotCanary, readErr)
	}
}

func TestLegacyQuiescenceTreatsDisprovenStaleScalarCountsAsNonBlocking(t *testing.T) {
	staleEvidence := exactLegacyCandidateEvidence("supervisor", 601)
	staleEvidence.Process.Status = "absent"
	staleEvidence.Endpoint.Status = "absent"
	staleEvidence.ReportedActiveCount = 37
	stale := classifiedLegacyCandidate(t, staleEvidence, nil, nil)
	stoppedAgentEvidence := exactLegacyCandidateEvidence("host_federation_agent", 602)
	stoppedAgentEvidence.Process.Status = "absent"
	stoppedAgentEvidence.Endpoint.Status = "absent"
	stoppedAgent := classifiedLegacyCandidate(t, stoppedAgentEvidence, nil, nil)

	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{
		Candidates: []LegacyRuntimeCandidate{stale, stoppedAgent},
	})
	if err != nil {
		t.Fatalf("stale scalar count blocked migration: %v; report=%#v", err, report)
	}
	if !report.LegacyAbsenceVerified || len(report.Blockers) != 0 || len(report.Debt) != 0 ||
		!reflect.DeepEqual(report.StaleCandidateIDs, []string{stoppedAgent.CandidateID, stale.CandidateID}) {
		t.Fatalf("stale-count report = %#v", report)
	}
	if report.Strategy != legacyQuiescenceStrategy {
		t.Fatalf("migration strategy = %q", report.Strategy)
	}
}

func TestLegacyQuiescenceRecordsEveryUnknownIdentityAsRetryableDebt(t *testing.T) {
	unknownProcessEvidence := exactLegacyCandidateEvidence("qwen_lane_manager", 701)
	unknownProcessEvidence.Process.Status = "unknown"
	unknownProcess := classifiedLegacyCandidate(t, unknownProcessEvidence, nil, nil)

	changedEndpointEvidence := exactLegacyCandidateEvidence("grok_host", 702)
	changedEndpointEvidence.Endpoint.OwnerPID++
	changedEndpoint := classifiedLegacyCandidate(t, changedEndpointEvidence, nil, nil)

	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{
		Candidates: []LegacyRuntimeCandidate{unknownProcess, changedEndpoint},
	})
	if !errors.Is(err, ErrLegacyQuiescenceBlocked) {
		t.Fatalf("identity debt error = %v, want ErrLegacyQuiescenceBlocked", err)
	}
	if report.LegacyAbsenceVerified || len(report.Blockers) != 0 || report.Strategy != legacyQuiescenceStrategy {
		t.Fatalf("identity-debt report = %#v", report)
	}
	want := map[string]string{
		unknownProcess.CandidateID:  "unknown_identity",
		changedEndpoint.CandidateID: "conflicting_identity",
	}
	if len(report.Debt) != len(want) {
		t.Fatalf("debt = %#v, want %d entries", report.Debt, len(want))
	}
	for _, debt := range report.Debt {
		if want[debt.CandidateID] != debt.Code || !debt.Retryable || debt.DebtID == "" {
			t.Fatalf("migration debt = %#v, want code %q and retryable", debt, want[debt.CandidateID])
		}
		delete(want, debt.CandidateID)
	}
	if len(want) != 0 {
		t.Fatalf("missing identity debt for %v", want)
	}
}

func TestLegacyQuiescenceNeverPlansLiveHandoffOrAutomaticNativeTermination(t *testing.T) {
	claudePeer := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("claude_host", 801), []string{"claude-live"}, nil)
	qwenLane := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("qwen_lane_manager", 802), nil, []string{"qwen-live"})
	nativeVendor := classifiedLegacyCandidate(t, exactLegacyCandidateEvidence("native_qwen", 803), nil, nil)

	report, err := EvaluateLegacyQuiescence(context.Background(), LegacyQuiescenceRequest{
		Candidates: []LegacyRuntimeCandidate{claudePeer, qwenLane, nativeVendor},
	})
	if !errors.Is(err, ErrLegacyQuiescenceBlocked) {
		t.Fatalf("live work error = %v, want blocked", err)
	}
	if report.Strategy != legacyQuiescenceStrategy || report.LegacyAbsenceVerified {
		t.Fatalf("migration attempted unsupported live transfer/termination: %#v", report)
	}
	if len(report.Blockers) != 2 {
		t.Fatalf("live blocker count = %d, want two: %#v", len(report.Blockers), report.Blockers)
	}
	for _, blocker := range report.Blockers {
		if blocker.RequiredAction != "close_before_retry" {
			t.Fatalf("live blocker action = %#v", blocker)
		}
	}
	for _, debt := range report.Debt {
		if debt.CandidateID == nativeVendor.CandidateID {
			t.Fatalf("excluded native vendor became migration debt: %#v", debt)
		}
	}
}

type legacyBlockerExpectation struct {
	candidateID, kind, resourceType, resourceID string
}

func assertLegacyBlockers(t *testing.T, blockers []LegacyMigrationBlocker, want []legacyBlockerExpectation) {
	t.Helper()
	got := make([]legacyBlockerExpectation, 0, len(blockers))
	for _, blocker := range blockers {
		got = append(got, legacyBlockerExpectation{
			candidateID: blocker.CandidateID, kind: blocker.Kind,
			resourceType: blocker.ResourceType, resourceID: blocker.ResourceID,
		})
	}
	sort.Slice(got, func(i, j int) bool {
		return got[i].candidateID+got[i].resourceID < got[j].candidateID+got[j].resourceID
	})
	sort.Slice(want, func(i, j int) bool {
		return want[i].candidateID+want[i].resourceID < want[j].candidateID+want[j].resourceID
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers = %#v, want %#v", got, want)
	}
}

func classifiedLegacyCandidate(
	t *testing.T,
	evidence LegacyCandidateEvidence,
	sessionIDs, laneIDs []string,
) LegacyRuntimeCandidate {
	t.Helper()
	evidence.RelatedSessionIDs = append([]string(nil), sessionIDs...)
	evidence.RelatedLaneIDs = append([]string(nil), laneIDs...)
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func cloneLegacyRuntimeCandidatesForTest(candidates []LegacyRuntimeCandidate) []LegacyRuntimeCandidate {
	result := append([]LegacyRuntimeCandidate(nil), candidates...)
	for index := range result {
		result[index].RelatedSessionIDs = append([]string(nil), result[index].RelatedSessionIDs...)
		result[index].RelatedLaneIDs = append([]string(nil), result[index].RelatedLaneIDs...)
	}
	return result
}
