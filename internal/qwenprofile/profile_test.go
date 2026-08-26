package qwenprofile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func TestResolveDefaultProfilePreservesEnvironmentAbsence(t *testing.T) {
	home := t.TempDir()
	identity := resolveTestIdentity(t, map[string]string{"HOME": home})
	if identity.QwenHomeSet || identity.QwenHome != "" {
		t.Fatalf("unset QWEN_HOME identity = %#v", identity)
	}
	if identity.QwenRuntimeSet || identity.QwenRuntimeDir != "" {
		t.Fatalf("unset QWEN_RUNTIME_DIR identity = %#v", identity)
	}
	assertProfileFingerprint(t, identity.Fingerprint)
}

func TestResolveExplicitProfilePreservesValueAndPresence(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen")
	runtimeDir := filepath.Join(root, "runtime")
	for _, path := range []string{qwenHome, runtimeDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	identity := resolveTestIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": qwenHome, "QWEN_RUNTIME_DIR": runtimeDir,
	})
	canonicalHome := canonicalExistingTestPath(t, qwenHome)
	canonicalRuntime := canonicalExistingTestPath(t, runtimeDir)
	if !identity.QwenHomeSet || identity.QwenHome != canonicalHome {
		t.Fatalf("explicit QWEN_HOME identity = %#v", identity)
	}
	if !identity.QwenRuntimeSet || identity.QwenRuntimeDir != canonicalRuntime {
		t.Fatalf("explicit QWEN_RUNTIME_DIR identity = %#v", identity)
	}
	assertProfileFingerprint(t, identity.Fingerprint)
}

func TestResolveRejectsNonAbsoluteProfilePaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		field  string
	}{
		{name: "home", values: map[string]string{"HOME": t.TempDir(), "QWEN_HOME": "relative/qwen"}, field: "QWEN_HOME"},
		{name: "empty home", values: map[string]string{"HOME": t.TempDir(), "QWEN_HOME": ""}, field: "QWEN_HOME"},
		{name: "runtime", values: map[string]string{"HOME": t.TempDir(), "QWEN_RUNTIME_DIR": "relative/runtime"}, field: "QWEN_RUNTIME_DIR"},
		{name: "empty runtime", values: map[string]string{"HOME": t.TempDir(), "QWEN_RUNTIME_DIR": ""}, field: "QWEN_RUNTIME_DIR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveEnvironment(testLookup(test.values))
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("invalid %s error = %v", test.field, err)
			}
		})
	}
}

func TestResolveCanonicalizesAbsoluteProfilePaths(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "profiles", "qwen")
	runtimeDir := filepath.Join(root, "runtime")
	for _, path := range []string{qwenHome, runtimeDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	identity := resolveTestIdentity(t, map[string]string{
		"HOME":             root,
		"QWEN_HOME":        filepath.Join(root, "profiles", "discard", "..", "qwen"),
		"QWEN_RUNTIME_DIR": filepath.Join(root, "unused", "..", "runtime"),
	})
	canonicalHome := canonicalExistingTestPath(t, qwenHome)
	canonicalRuntime := canonicalExistingTestPath(t, runtimeDir)
	if identity.QwenHome != canonicalHome || identity.QwenRuntimeDir != canonicalRuntime {
		t.Fatalf("canonical profile identity = %#v, want home=%q runtime=%q", identity, canonicalHome, canonicalRuntime)
	}
	if !filepath.IsAbs(identity.QwenHome) || !filepath.IsAbs(identity.QwenRuntimeDir) {
		t.Fatalf("profile identity is not absolute: %#v", identity)
	}
}

func TestResolveRejectsSymlinkProfileAmbiguity(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	realHome := filepath.Join(realRoot, "qwen")
	realRuntime := filepath.Join(realRoot, "runtime")
	for _, path := range []string{realHome, realRuntime} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkedRoot := filepath.Join(root, "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkedHome := filepath.Join(root, "linked-qwen")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	linkedRuntime := filepath.Join(root, "linked-runtime")
	if err := os.Symlink(realRuntime, linkedRuntime); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		field string
		path  string
	}{
		{name: "home leaf", field: "QWEN_HOME", path: linkedHome},
		{name: "home ancestor", field: "QWEN_HOME", path: filepath.Join(linkedRoot, "qwen")},
		{name: "runtime leaf", field: "QWEN_RUNTIME_DIR", path: linkedRuntime},
		{name: "runtime ancestor", field: "QWEN_RUNTIME_DIR", path: filepath.Join(linkedRoot, "runtime")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveEnvironment(testLookup(map[string]string{"HOME": root, test.field: test.path}))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
				t.Fatalf("symlink-selected %s error = %v", test.field, err)
			}
		})
	}
}

func TestProfileFingerprintIsStableAndDoesNotReadSecrets(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen")
	if err := os.Mkdir(qwenHome, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(qwenHome, "credentials.json")
	const firstSecret = "credential-sentinel-one"
	if err := os.WriteFile(credentialPath, []byte(firstSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"HOME": root, "QWEN_HOME": qwenHome}
	first := resolveTestIdentity(t, values)
	assertProfileFingerprint(t, first.Fingerprint)
	if strings.Contains(first.Fingerprint, firstSecret) || strings.Contains(first.Fingerprint, qwenHome) {
		t.Fatalf("profile fingerprint exposes Qwen-owned data: %q", first.Fingerprint)
	}
	repeated := resolveTestIdentity(t, values)
	if repeated.Fingerprint != first.Fingerprint {
		t.Fatalf("identical profile identities have different fingerprints: %q != %q", repeated.Fingerprint, first.Fingerprint)
	}

	if err := os.WriteFile(credentialPath, []byte("credential-sentinel-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := resolveTestIdentity(t, values)
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("credential contents changed profile identity: %q != %q", second.Fingerprint, first.Fingerprint)
	}

	spellingVariant := resolveTestIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": filepath.Join(root, "ignored", "..", "qwen"),
	})
	if spellingVariant.Fingerprint != first.Fingerprint {
		t.Fatalf("canonical-equivalent paths have different fingerprints: %q != %q", spellingVariant.Fingerprint, first.Fingerprint)
	}

	nativeDefault := resolveTestIdentity(t, map[string]string{"HOME": root})
	if nativeDefault.Fingerprint == first.Fingerprint {
		t.Fatal("explicit QWEN_HOME and absent QWEN_HOME have the same fingerprint")
	}

	explicitRuntime := resolveTestIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": qwenHome, "QWEN_RUNTIME_DIR": qwenHome,
	})
	if explicitRuntime.Fingerprint == first.Fingerprint {
		t.Fatal("explicit QWEN_RUNTIME_DIR and absent QWEN_RUNTIME_DIR have the same fingerprint")
	}
}

func TestResumeRequiresExactProfileIdentity(t *testing.T) {
	root := t.TempDir()
	firstHome := filepath.Join(root, "first")
	secondHome := filepath.Join(root, "second")
	firstRuntime := filepath.Join(root, "runtime-first")
	secondRuntime := filepath.Join(root, "runtime-second")
	defaultHome := filepath.Join(root, ".qwen")
	for _, path := range []string{firstHome, secondHome, firstRuntime, secondRuntime, defaultHome} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stored := resolveTestIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": firstHome, "QWEN_RUNTIME_DIR": firstRuntime,
	})
	equivalent := resolveTestIdentity(t, map[string]string{
		"HOME":             root,
		"QWEN_HOME":        filepath.Join(root, "unused", "..", "first"),
		"QWEN_RUNTIME_DIR": filepath.Join(root, "discard", "..", "runtime-first"),
	})
	if err := MatchResume(stored, equivalent); err != nil {
		t.Fatalf("canonical-equivalent resume rejected: %v", err)
	}

	cases := []struct {
		name      string
		requested Identity
		field     string
	}{
		{name: "different home", requested: resolveTestIdentity(t, map[string]string{"HOME": root, "QWEN_HOME": secondHome, "QWEN_RUNTIME_DIR": firstRuntime}), field: "QWEN_HOME"},
		{name: "home absence differs from explicit value", requested: resolveTestIdentity(t, map[string]string{"HOME": root, "QWEN_RUNTIME_DIR": firstRuntime}), field: "QWEN_HOME"},
		{name: "different runtime", requested: resolveTestIdentity(t, map[string]string{"HOME": root, "QWEN_HOME": firstHome, "QWEN_RUNTIME_DIR": secondRuntime}), field: "QWEN_RUNTIME_DIR"},
		{name: "runtime absence differs from explicit value", requested: resolveTestIdentity(t, map[string]string{"HOME": root, "QWEN_HOME": firstHome}), field: "QWEN_RUNTIME_DIR"},
		{name: "fingerprint mismatch", requested: withTestFingerprint(equivalent, strings.Repeat("0", 64)), field: "fingerprint"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := MatchResume(stored, test.requested)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.field)) {
				t.Fatalf("profile mismatch error = %v, want field %s", err, test.field)
			}
		})
	}

	unset := resolveTestIdentity(t, map[string]string{"HOME": root})
	explicitNativeDefault := resolveTestIdentity(t, map[string]string{"HOME": root, "QWEN_HOME": defaultHome})
	if err := MatchResume(unset, explicitNativeDefault); err == nil {
		t.Fatal("unset QWEN_HOME matched an explicitly selected native-default path")
	}
}

func resolveTestIdentity(t *testing.T, values map[string]string) Identity {
	t.Helper()
	identity, err := ResolveEnvironment(testLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func testLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func assertProfileFingerprint(t *testing.T, fingerprint string) {
	t.Helper()
	if !fingerprintPattern.MatchString(fingerprint) {
		t.Fatalf("profile fingerprint = %q, want lowercase SHA-256", fingerprint)
	}
}

func canonicalExistingTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func withTestFingerprint(identity Identity, fingerprint string) Identity {
	identity.Fingerprint = fingerprint
	return identity
}
