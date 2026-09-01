package productruntime

import (
	"fmt"
	"strings"
)

// ValidateLaneOpenResult enforces the product-neutral Open authority contract.
// A fresh lane may be unbound only when its driver explicitly declares that
// the native product creates sessions on the first turn. Resume always returns
// the exact requested native identity, regardless of that capability.
func ValidateLaneOpenResult(capabilities LaneCapabilitySet, request LaneOpenRequest, result NativeSessionRef) error {
	if strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(result.LaneID) == "" {
		return fmt.Errorf("%w: lane open result is missing lane identity", ErrProtocol)
	}
	if result.LaneID != request.LaneID {
		return fmt.Errorf("%w: lane open returned lane %q, want %q", ErrAmbiguousSession, result.LaneID, request.LaneID)
	}
	if result.Generation == 0 {
		return fmt.Errorf("%w: lane open result is missing generation", ErrProtocol)
	}
	nativeID := strings.TrimSpace(result.NativeSessionID)
	if result.NativeSessionID != "" && nativeID == "" {
		return fmt.Errorf("%w: lane open returned a blank native session identity", ErrProtocol)
	}
	resumeID := strings.TrimSpace(request.ResumeNativeID)
	if request.ResumeNativeID != "" && resumeID == "" {
		return fmt.Errorf("%w: lane resume requested a blank native session identity", ErrProtocol)
	}
	if request.ResumeNativeID != "" {
		if result.NativeSessionID != request.ResumeNativeID {
			return fmt.Errorf("%w: lane resume returned native session %q, want %q", ErrAmbiguousSession, result.NativeSessionID, request.ResumeNativeID)
		}
		return nil
	}
	if capabilities.DeferredSessionBinding {
		if nativeID != "" {
			return fmt.Errorf("%w: deferred-session-binding driver created a native session during fresh Open", ErrProtocol)
		}
		return nil
	}
	if nativeID == "" {
		return fmt.Errorf("%w: lane driver returned an unbound session without deferred-session-binding capability", ErrProtocol)
	}
	return nil
}

// NativeSessionBindingFromOpen is the central commit guard after Open. A true
// result authorizes the exact-at-Open SetNativeSessionID path. A valid deferred
// result returns false and MUST skip that path; only the first-turn atomic
// acceptance boundary may bind it.
func NativeSessionBindingFromOpen(
	capabilities LaneCapabilitySet,
	request LaneOpenRequest,
	result NativeSessionRef,
) (nativeSessionID string, bindAtOpen bool, err error) {
	if err := ValidateLaneOpenResult(capabilities, request, result); err != nil {
		return "", false, err
	}
	if result.NativeSessionID == "" {
		return "", false, nil
	}
	return result.NativeSessionID, true, nil
}

// ValidateLaneStartTurnResult enforces the one-time binding boundary. An
// unbound input is legal only for a deferred-binding driver; successful first
// StartTurn must return the product-created native session and exact native
// turn identity while preserving LaneID and generation. Once bound, the native
// session may never be substituted. Bound products whose protocol genuinely
// has no distinct turn ID retain the base contract's documented exception.
func ValidateLaneStartTurnResult(capabilities LaneCapabilitySet, input NativeSessionRef, result NativeTurnRef) error {
	if strings.TrimSpace(input.LaneID) == "" || input.Generation == 0 {
		return fmt.Errorf("%w: lane start-turn input is missing lane identity or generation", ErrProtocol)
	}
	if input.NativeSessionID != "" && strings.TrimSpace(input.NativeSessionID) == "" {
		return fmt.Errorf("%w: lane start-turn input has a blank native session identity", ErrProtocol)
	}
	if input.NativeSessionID == "" && !capabilities.DeferredSessionBinding {
		return fmt.Errorf("%w: lane start-turn input is unbound without deferred-session-binding capability", ErrProtocol)
	}
	if result.LaneID != input.LaneID {
		return fmt.Errorf("%w: lane start-turn returned lane %q, want %q", ErrAmbiguousSession, result.LaneID, input.LaneID)
	}
	if result.Generation != input.Generation {
		return fmt.Errorf("%w: lane start-turn returned generation %d, want %d", ErrStale, result.Generation, input.Generation)
	}
	if strings.TrimSpace(result.NativeSessionID) == "" {
		return fmt.Errorf("%w: lane start-turn result lacks exact native session identity", ErrProtocol)
	}
	if input.NativeSessionID == "" && strings.TrimSpace(result.NativeTurnID) == "" {
		return fmt.Errorf("%w: deferred lane start-turn result lacks exact native turn identity", ErrProtocol)
	}
	if input.NativeSessionID != "" && result.NativeSessionID != input.NativeSessionID {
		return fmt.Errorf("%w: lane start-turn substituted native session %q for %q", ErrAmbiguousSession, result.NativeSessionID, input.NativeSessionID)
	}
	return nil
}
