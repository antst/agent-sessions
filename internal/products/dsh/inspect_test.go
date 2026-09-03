package dsh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectSessionUsesColdProfileListResult(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "dsh")
	script := `#!/bin/sh
set -eu
[ "$1 $2" = "--profile agent-sessions" ]
[ "$AGENT_SESSIONS_DSH_INSPECT_SESSION_ID" = "session-native" ]
printf '%s\n' '{"found":true,"session_id":"session-native","name":"native title","cwd":"/product/work","updated_at":1720000000000}'
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	got, found, err := InspectSession(context.Background(), executable, "session-native", []string{"PATH=/usr/bin:/bin"})
	if err != nil || !found {
		t.Fatalf("InspectSession() = %#v, %v, %v", got, found, err)
	}
	if got.SessionID != "session-native" || got.Name != "native title" || got.Cwd != "/product/work" || got.UpdatedAt != 1720000000000 {
		t.Fatalf("candidate = %#v", got)
	}
}

func TestInspectSessionRejectsChangedIdentityAndAcceptsAbsentCandidate(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    string
		wantFound bool
		wantError bool
	}{
		{name: "absent", result: `{"found":false}`, wantFound: false},
		{name: "changed identity", result: `{"found":true,"session_id":"other","name":"","cwd":"","updated_at":1}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "dsh")
			script := "#!/bin/sh\nprintf '%s\\n' '" + test.result + "'\n"
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			_, found, err := InspectSession(context.Background(), executable, "session-native", []string{"PATH=/usr/bin:/bin"})
			if found != test.wantFound || (err != nil) != test.wantError {
				t.Fatalf("InspectSession() = found %v, err %v", found, err)
			}
		})
	}
}
