package claudeprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSeedCopiesAccountBindingAndPreservesPresentationChoices(t *testing.T) {
	root := t.TempDir()
	privateRoot := filepath.Join(root, "private")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(root, "public.json")
	public := map[string]any{
		"hasCompletedOnboarding": true,
		"theme":                  "light",
		"oauthAccount": map[string]any{
			"accountUuid": "current", "organizationUuid": "org", "emailAddress": "current@example.invalid",
		},
		"userID":        "do-not-copy",
		"machineID":     "do-not-copy",
		"primaryApiKey": "do-not-copy",
		"projects":      map[string]any{"secret": true},
	}
	private := map[string]any{
		"theme":        "dark",
		"oauthAccount": map[string]any{"accountUuid": "stale"},
	}
	writeTestJSON(t, publicPath, public)
	writeTestJSON(t, filepath.Join(privateRoot, ".claude.json"), private)
	if err := Seed(privateRoot, publicPath); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(privateRoot, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	account, _ := got["oauthAccount"].(map[string]any)
	completedOnboarding, _ := got["hasCompletedOnboarding"].(bool)
	if got["theme"] != "dark" || !completedOnboarding || account["accountUuid"] != "current" {
		t.Fatalf("seeded profile = %#v", got)
	}
	for _, key := range []string{"projects", "userID", "machineID", "primaryApiKey"} {
		if _, copied := got[key]; copied {
			t.Fatalf("private profile copied unrelated public key %q: %#v", key, got)
		}
	}
	if !reflect.DeepEqual(account, public["oauthAccount"]) {
		t.Fatalf("private profile copied unrelated public state: %#v", got)
	}
	info, err := os.Stat(filepath.Join(privateRoot, ".claude.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private profile mode = %v, %v", info, err)
	}
}

func TestSeedRejectsMalformedPublicProfileAndAccount(t *testing.T) {
	root := t.TempDir()
	privateRoot := filepath.Join(root, "private")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"null-profile":   "null\n",
		"null-account":   `{"oauthAccount":null}`,
		"scalar-account": `{"oauthAccount":"wrong"}`,
		"array-account":  `{"oauthAccount":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			publicPath := filepath.Join(root, name+".json")
			if err := os.WriteFile(publicPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Seed(privateRoot, publicPath); err == nil {
				t.Fatalf("malformed public profile %q was accepted", body)
			}
		})
	}
}

func TestCurrentSourcePreservesNativeCredentialNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	source, err := CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	if source.ConfigRoot != filepath.Join(home, ".claude") || source.StatePath != filepath.Join(home, ".claude.json") || source.SecureConfig != "" {
		t.Fatalf("default Claude source = %#v", source)
	}

	if err := os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); err != nil {
		t.Fatal(err)
	}
	explicitDefault := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", explicitDefault)
	source, err = CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	if source.ConfigRoot != explicitDefault || source.StatePath != filepath.Join(explicitDefault, ".claude.json") || source.SecureConfig != explicitDefault {
		t.Fatalf("explicit default-spelled Claude source = %#v", source)
	}

	custom := filepath.Join(home, "custom", "..", "custom")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	source, err = CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	if source.ConfigRoot != custom || source.StatePath != filepath.Join(custom, ".claude.json") || source.SecureConfig != custom {
		t.Fatalf("custom Claude source = %#v", source)
	}

	secure := filepath.Join(home, "credentials", "..", "credentials")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", secure)
	source, err = CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	if source.SecureConfig != secure {
		t.Fatalf("explicit secure storage spelling = %q", source.SecureConfig)
	}
}

func TestSeedRemovesStaleAccountBindingOnLogout(t *testing.T) {
	root := t.TempDir()
	privateRoot := filepath.Join(root, "private")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(root, "public.json")
	writeTestJSON(t, publicPath, map[string]any{"theme": "light"})
	writeTestJSON(t, filepath.Join(privateRoot, ".claude.json"), map[string]any{
		"theme": "dark", "oauthAccount": map[string]any{"accountUuid": "stale"},
	})
	if err := Seed(privateRoot, publicPath); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(privateRoot, ".claude.json"))
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if _, exists := got["oauthAccount"]; exists {
		t.Fatalf("stale account binding survived logout: %#v", got)
	}
}

func TestSeedRejectsNullPrivateProfile(t *testing.T) {
	root := t.TempDir()
	privateRoot := filepath.Join(root, "private")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(root, "public.json")
	writeTestJSON(t, publicPath, map[string]any{"hasCompletedOnboarding": true})
	if err := os.WriteFile(filepath.Join(privateRoot, ".claude.json"), []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Seed(privateRoot, publicPath); err == nil {
		t.Fatal("null private profile was accepted")
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
