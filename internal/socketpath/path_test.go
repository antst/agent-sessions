package socketpath

import (
	"strings"
	"testing"
)

func TestValidateRequiresAbsoluteCleanPathWithinPlatformLimit(t *testing.T) {
	if err := Validate("relative.sock"); err == nil {
		t.Fatal("relative socket path was accepted")
	}
	if err := Validate("/tmp/nested/../peer.sock"); err == nil {
		t.Fatal("unclean socket path was accepted")
	}
	tooLong := "/" + strings.Repeat("x", Limit())
	if err := Validate(tooLong); err == nil || !strings.Contains(err.Error(), "platform limit") {
		t.Fatalf("overlong socket path error = %v", err)
	}
	maximum := "/" + strings.Repeat("x", Limit()-1)
	if err := Validate(maximum); err != nil {
		t.Fatalf("maximum socket path: %v", err)
	}
}

func TestPreferRootUsesFallbackWhenLongestPathDoesNotFit(t *testing.T) {
	preferred := "/" + strings.Repeat("p", Limit())
	fallback := "/tmp/f"
	if got := PreferRoot(preferred, fallback, "peer.sock"); got != fallback {
		t.Fatalf("PreferRoot() = %q, want %q", got, fallback)
	}
	if got := PreferRoot("/tmp/preferred", fallback, "peer.sock"); got != "/tmp/preferred" {
		t.Fatalf("PreferRoot() = %q, want preferred root", got)
	}
}
