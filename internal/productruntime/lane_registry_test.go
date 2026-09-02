package productruntime

import "testing"

func TestLaneRegistryIsExplicitAndImmutable(t *testing.T) {
	driver := fakeLaneDriver{}
	input := map[string]LaneDriver{"test": driver}
	registry, err := NewLaneRegistry(input)
	if err != nil {
		t.Fatal(err)
	}
	delete(input, "test")
	if got, ok := registry.ByProduct("test"); !ok || got == nil {
		t.Fatal("registry changed with its input map")
	}
	if _, err := NewLaneRegistry(map[string]LaneDriver{"": driver}); err == nil {
		t.Fatal("empty product unexpectedly entered registry")
	}
	if _, err := NewLaneRegistry(map[string]LaneDriver{"test": nil}); err == nil {
		t.Fatal("nil driver unexpectedly entered registry")
	}
}
