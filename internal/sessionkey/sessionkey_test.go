package sessionkey

import "testing"

func TestFromIDPreservesEstablishedKey(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000001"
	const expected = "7ac1b8d7010bb6cd3a3e"
	if got := FromID(id); got != expected {
		t.Fatalf("FromID(%q) = %q, want %q", id, got, expected)
	}
}
