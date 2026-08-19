package claudeprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentSourcePreservesNativeCredentialNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	source, err := CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	if source.ConfigRoot != filepath.Join(home, ".claude") || source.StatePath != filepath.Join(home, ".claude.json") ||
		source.SecureConfig != "" || !source.ConfigEnvSet || source.ConfigEnvValue != "" || !source.SecureEnvSet {
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
	if source.ConfigRoot != explicitDefault || source.StatePath != filepath.Join(explicitDefault, ".claude.json") ||
		source.SecureConfig != explicitDefault || !source.ConfigEnvSet || source.ConfigEnvValue != explicitDefault || source.SecureEnvSet {
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
	if source.SecureConfig != secure || !source.SecureEnvSet {
		t.Fatalf("explicit secure storage spelling = %#v", source)
	}
}

func TestSharedSourceSelectsConfiguredRootWithoutCopyingState(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, "shared")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	_ = os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	source, err := SharedSource(shared)
	if err != nil {
		t.Fatal(err)
	}
	if source.ConfigRoot != shared || !source.ConfigEnvSet || source.ConfigEnvValue != shared || source.SecureEnvSet {
		t.Fatalf("shared Claude source = %#v", source)
	}
	if _, err := os.Stat(filepath.Join(shared, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("resolving shared source mutated native state: %v", err)
	}
}

func TestManagedClaudeProfileRejectsRelativeConfigOrSecureRoots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "relative/claude")
	if _, err := CurrentSource(); err == nil {
		t.Fatal("relative CLAUDE_CONFIG_DIR was accepted")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "relative/secure")
	if _, err := CurrentSource(); err == nil {
		t.Fatal("relative CLAUDE_SECURESTORAGE_CONFIG_DIR was accepted")
	}
	if _, err := SharedSource("relative/shared"); err == nil {
		t.Fatal("relative shared profile root was accepted")
	}
}
