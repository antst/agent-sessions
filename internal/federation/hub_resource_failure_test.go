package federation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

func TestHubResourceFailuresRejectBeforeDurableRegistrationOrWorkAcceptance(t *testing.T) {
	failures := []struct {
		resource string
		cause    error
	}{
		{resource: "disk", cause: syscall.ENOSPC},
		{resource: "memory", cause: syscall.ENOMEM},
		{resource: "file_descriptor", cause: syscall.EMFILE},
		{resource: "process", cause: syscall.EAGAIN},
	}
	for _, failure := range failures {
		t.Run(failure.resource, func(t *testing.T) {
			committed := []string{"already-accepted"}
			preflightCalls, commitCalls := 0, 0
			result, err := AdmitHubWork(context.Background(), HubAdmissionRequest{
				Operation: "host.register", HostID: "host-new", RequestID: "registration-1",
			}, HubAdmissionHooks{
				Preflight: func(context.Context, HubAdmissionRequest) error {
					preflightCalls++
					return fmt.Errorf("%s resource unavailable: %w", failure.resource, failure.cause)
				},
				Commit: func(_ context.Context, request HubAdmissionRequest) (uint64, error) {
					commitCalls++
					committed = append(committed, request.RequestID)
					return uint64(len(committed)), nil
				},
			})
			if err == nil || result.Accepted || result.Revision != 0 {
				t.Fatalf("%s failure = result %+v error %v, want refusal before acceptance", failure.resource, result, err)
			}
			if preflightCalls != 1 || commitCalls != 0 {
				t.Fatalf("%s ordering = preflight %d commit %d", failure.resource, preflightCalls, commitCalls)
			}
			if !errors.Is(err, failure.cause) || !strings.Contains(err.Error(), failure.resource) {
				t.Fatalf("%s failure lost real resource cause: %v", failure.resource, err)
			}
			var refusal interface {
				Accepted() bool
				Retryable() bool
			}
			if !errors.As(err, &refusal) || refusal.Accepted() || !refusal.Retryable() {
				t.Fatalf("%s failure is not an explicit retryable pre-acceptance refusal: %T %v", failure.resource, err, err)
			}
			if !reflectStringsEqual(committed, []string{"already-accepted"}) {
				t.Fatalf("%s failure discarded or replaced prior accepted work: %q", failure.resource, committed)
			}
		})
	}
}

func TestHubAdmissionReportsSuccessOnlyAfterDurableCommit(t *testing.T) {
	commitFailure := syscall.ENOSPC
	result, err := AdmitHubWork(context.Background(), HubAdmissionRequest{
		Operation: "route.accept", HostID: "host-a", RequestID: "route-1",
	}, HubAdmissionHooks{
		Preflight: func(context.Context, HubAdmissionRequest) error { return nil },
		Commit: func(context.Context, HubAdmissionRequest) (uint64, error) {
			return 0, fmt.Errorf("commit hub route journal: %w", commitFailure)
		},
	})
	if err == nil || result.Accepted || result.Revision != 0 || !errors.Is(err, commitFailure) {
		t.Fatalf("failed durable commit = result %+v error %v", result, err)
	}

	result, err = AdmitHubWork(context.Background(), HubAdmissionRequest{
		Operation: "route.accept", HostID: "host-a", RequestID: "route-2",
	}, HubAdmissionHooks{
		Preflight: func(context.Context, HubAdmissionRequest) error { return nil },
		Commit:    func(context.Context, HubAdmissionRequest) (uint64, error) { return 44, nil },
	})
	if err != nil || !result.Accepted || result.Revision != 44 {
		t.Fatalf("durable hub commit = result %+v error %v", result, err)
	}
}

func TestHubAdmissionHasNoAgentSessionsQuota(t *testing.T) {
	const operations = 2049
	committed := 0
	hooks := HubAdmissionHooks{
		Preflight: func(context.Context, HubAdmissionRequest) error { return nil },
		Commit: func(context.Context, HubAdmissionRequest) (uint64, error) {
			committed++
			return uint64(committed), nil
		},
	}
	for index := 0; index < operations; index++ {
		result, err := AdmitHubWork(context.Background(), HubAdmissionRequest{
			Operation: "route.accept", HostID: "host-a", RequestID: fmt.Sprintf("route-%d", index),
		}, hooks)
		if err != nil || !result.Accepted || result.Revision != uint64(index+1) {
			t.Fatalf("operation %d was subject to an Agent Sessions quota: result %+v error %v", index, result, err)
		}
	}
	if committed != operations {
		t.Fatalf("durably committed operations = %d, want %d", committed, operations)
	}
}

func reflectStringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
