package envutil

import (
	"reflect"
	"testing"
)

func TestLookupUsesLastValueAndPreservesPresence(t *testing.T) {
	lookup := Lookup([]string{"EMPTY=", "VALUE=old", "MALFORMED", "VALUE=new"})
	if value, ok := lookup("VALUE"); !ok || value != "new" {
		t.Fatalf("VALUE = %q, %v; want new, true", value, ok)
	}
	if value, ok := lookup("EMPTY"); !ok || value != "" {
		t.Fatalf("EMPTY = %q, %v; want empty, true", value, ok)
	}
	if _, ok := lookup("MISSING"); ok {
		t.Fatal("missing value reported present")
	}
}

func TestReplaceRemovesDuplicatesAndSortsReplacements(t *testing.T) {
	got := Replace([]string{"KEEP=1", "B=old", "B=older", "A=old"}, map[string]string{
		"B": "new",
		"A": "new",
	})
	want := []string{"KEEP=1", "A=new", "B=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Replace() = %#v; want %#v", got, want)
	}
}
