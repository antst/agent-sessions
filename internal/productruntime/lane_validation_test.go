package productruntime

import (
	"errors"
	"testing"
)

func TestValidateLaneOpenResultDefersOnlyForFlaggedFreshLane(t *testing.T) {
	fresh := LaneOpenRequest{ProductID: "synthetic", LaneID: "lane"}
	resume := fresh
	resume.ResumeNativeID = "native-existing"
	bound := NativeSessionRef{LaneID: fresh.LaneID, NativeSessionID: "native-new", Generation: 7}
	unbound := NativeSessionRef{LaneID: fresh.LaneID, Generation: 7}

	tests := []struct {
		name         string
		capabilities LaneCapabilitySet
		request      LaneOpenRequest
		result       NativeSessionRef
		wantCategory error
	}{
		{name: "ordinary fresh bound", request: fresh, result: bound},
		{name: "flagged fresh bound", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: fresh, result: bound, wantCategory: ErrProtocol},
		{name: "flagged fresh unbound", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: fresh, result: unbound},
		{name: "ordinary fresh unbound", request: fresh, result: unbound, wantCategory: ErrProtocol},
		{name: "flagged resume exact", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: resume, result: NativeSessionRef{LaneID: fresh.LaneID, NativeSessionID: resume.ResumeNativeID, Generation: 7}},
		{name: "flagged resume unbound", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: resume, result: unbound, wantCategory: ErrAmbiguousSession},
		{name: "flagged resume substituted", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: resume, result: bound, wantCategory: ErrAmbiguousSession},
		{name: "wrong lane", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: fresh, result: NativeSessionRef{LaneID: "other", Generation: 7}, wantCategory: ErrAmbiguousSession},
		{name: "missing generation", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: fresh, result: NativeSessionRef{LaneID: fresh.LaneID}, wantCategory: ErrProtocol},
		{name: "whitespace native identity", capabilities: LaneCapabilitySet{DeferredSessionBinding: true}, request: fresh, result: NativeSessionRef{LaneID: fresh.LaneID, NativeSessionID: "  ", Generation: 7}, wantCategory: ErrProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateLaneOpenResult(test.capabilities, test.request, test.result)
			if test.wantCategory == nil && err != nil {
				t.Fatalf("valid open result rejected: %v", err)
			}
			if test.wantCategory != nil && !errors.Is(err, test.wantCategory) {
				t.Fatalf("open result category = %v, want %v", err, test.wantCategory)
			}
		})
	}
}

func TestNativeSessionBindingFromOpenGuardsDeferredLaneFromOpenTimeBinding(t *testing.T) {
	fresh := LaneOpenRequest{ProductID: "synthetic", LaneID: "lane"}
	unbound := NativeSessionRef{LaneID: fresh.LaneID, Generation: 7}
	nativeID, bindAtOpen, err := NativeSessionBindingFromOpen(
		LaneCapabilitySet{DeferredSessionBinding: true}, fresh, unbound,
	)
	if err != nil || bindAtOpen || nativeID != "" {
		t.Fatalf("deferred Open authorized SetNativeSessionID: id=%q bind=%v err=%v", nativeID, bindAtOpen, err)
	}
	if nativeID, bindAtOpen, err = NativeSessionBindingFromOpen(
		LaneCapabilitySet{DeferredSessionBinding: true}, fresh,
		NativeSessionRef{LaneID: fresh.LaneID, NativeSessionID: "invented-at-open", Generation: 7},
	); !errors.Is(err, ErrProtocol) || bindAtOpen || nativeID != "" {
		t.Fatalf("deferred bound Open escaped the commit guard: id=%q bind=%v err=%v", nativeID, bindAtOpen, err)
	}

	bound := NativeSessionRef{LaneID: fresh.LaneID, NativeSessionID: "native-at-open", Generation: 7}
	nativeID, bindAtOpen, err = NativeSessionBindingFromOpen(LaneCapabilitySet{}, fresh, bound)
	if err != nil || !bindAtOpen || nativeID != bound.NativeSessionID {
		t.Fatalf("exact Open lost native binding authority: id=%q bind=%v err=%v", nativeID, bindAtOpen, err)
	}
}

func TestValidateLaneStartTurnResultBindsOnceAndPreservesExactAuthority(t *testing.T) {
	capabilities := LaneCapabilitySet{DeferredSessionBinding: true}
	unbound := NativeSessionRef{LaneID: "lane", Generation: 7}
	bound := NativeSessionRef{LaneID: "lane", NativeSessionID: "native", Generation: 7}

	tests := []struct {
		name         string
		capabilities LaneCapabilitySet
		input        NativeSessionRef
		result       NativeTurnRef
		wantCategory error
	}{
		{name: "first turn binds", capabilities: capabilities, input: unbound, result: NativeTurnRef{NativeSessionRef: bound, NativeTurnID: "native-turn"}},
		{name: "ordinary bound turn remains exact", input: bound, result: NativeTurnRef{NativeSessionRef: bound, NativeTurnID: "native-turn"}},
		{name: "documented bound protocol may omit distinct turn id", input: bound, result: NativeTurnRef{NativeSessionRef: bound}},
		{name: "unflagged input cannot be unbound", input: unbound, result: NativeTurnRef{NativeSessionRef: bound, NativeTurnID: "native-turn"}, wantCategory: ErrProtocol},
		{name: "first turn must bind", capabilities: capabilities, input: unbound, result: NativeTurnRef{NativeSessionRef: unbound, NativeTurnID: "native-turn"}, wantCategory: ErrProtocol},
		{name: "first turn requires exact native turn", capabilities: capabilities, input: unbound, result: NativeTurnRef{NativeSessionRef: bound}, wantCategory: ErrProtocol},
		{name: "bound turn substitutes session", input: bound, result: NativeTurnRef{NativeSessionRef: NativeSessionRef{LaneID: "lane", NativeSessionID: "other-native", Generation: 7}, NativeTurnID: "native-turn"}, wantCategory: ErrAmbiguousSession},
		{name: "turn substitutes lane", capabilities: capabilities, input: unbound, result: NativeTurnRef{NativeSessionRef: NativeSessionRef{LaneID: "other", NativeSessionID: "native", Generation: 7}, NativeTurnID: "native-turn"}, wantCategory: ErrAmbiguousSession},
		{name: "turn changes generation", capabilities: capabilities, input: unbound, result: NativeTurnRef{NativeSessionRef: NativeSessionRef{LaneID: "lane", NativeSessionID: "native", Generation: 8}, NativeTurnID: "native-turn"}, wantCategory: ErrStale},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateLaneStartTurnResult(test.capabilities, test.input, test.result)
			if test.wantCategory == nil && err != nil {
				t.Fatalf("valid start-turn result rejected: %v", err)
			}
			if test.wantCategory != nil && !errors.Is(err, test.wantCategory) {
				t.Fatalf("start-turn result category = %v, want %v", err, test.wantCategory)
			}
		})
	}
}
