package sessionidentity

import "testing"

func TestSessionGroupGrammarValidatesEveryNativeIDSegment(t *testing.T) {
	for _, group := range []string{"session:host/parent", "session:host/parent/lane", "session:host/parent/lane/child"} {
		if !ValidGroup(group) {
			t.Errorf("ValidGroup(%q) = false", group)
		}
	}
	for _, group := range []string{
		"session:host", "session:/parent", "session:host/", "session:host//lane",
		"session:host/parent/", "session:host//", "session:host/parent/bad lane",
		"operator/child",
	} {
		if ValidGroup(group) {
			t.Errorf("ValidGroup(%q) = true", group)
		}
	}
}
