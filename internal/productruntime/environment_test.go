package productruntime

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseNativeEnvironmentPreservesExactEntries(t *testing.T) {
	got, err := ParseNativeEnvironment([]string{"ONE=first", "EMPTY=", "ONE=last=part"})
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvVar{{Name: "ONE", Value: "first"}, {Name: "EMPTY", Value: ""}, {Name: "ONE", Value: "last=part"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native environment = %#v, want %#v", got, want)
	}
	for _, input := range [][]string{{"MISSING"}, {"=value"}} {
		if _, err := ParseNativeEnvironment(input); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid native environment %q error = %v", input, err)
		}
	}
}
