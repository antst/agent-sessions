//go:build darwin

package qwenprofile

import (
	"path/filepath"
	"testing"
)

func TestResolveCanonicalizesDarwinSystemPathAliases(t *testing.T) {
	for _, test := range []struct {
		name  string
		alias string
		want  string
	}{
		{name: "tmp", alias: "/tmp", want: "/private/tmp"},
		{name: "var", alias: "/var", want: "/private/var"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(test.alias, "agent-sessions-qwen-profile-alias-test")
			qwenHome := filepath.Join(root, "qwen")
			runtimeDir := filepath.Join(root, "runtime")
			identity := resolveTestIdentity(t, map[string]string{
				"HOME": root, "QWEN_HOME": qwenHome, "QWEN_RUNTIME_DIR": runtimeDir,
			})
			relative, err := filepath.Rel(test.alias, root)
			if err != nil {
				t.Fatal(err)
			}
			wantRoot := filepath.Join(test.want, relative)
			if identity.QwenHome != filepath.Join(wantRoot, "qwen") {
				t.Fatalf("QWEN_HOME = %q, want canonical Darwin path under %q", identity.QwenHome, wantRoot)
			}
			if identity.QwenRuntimeDir != filepath.Join(wantRoot, "runtime") {
				t.Fatalf("QWEN_RUNTIME_DIR = %q, want canonical Darwin path under %q", identity.QwenRuntimeDir, wantRoot)
			}
		})
	}
}
