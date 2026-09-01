package codebuddy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestFileWorkerRegistryParsesBoundedCredentialFreeRowsAndDetectsMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "123.json")
	body := `{"sessionId":"session-1","pid":123,"kind":"interactive","cwd":"/work","url":"http://127.0.0.1:8080","name":"alpha"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewFileWorkerRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := registry.FindInteractive(context.Background(), "session-1")
	if err != nil || claim.PID != 123 || claim.Endpoint != "http://127.0.0.1:8080" || claim.Registry.Inode == 0 {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	if err := registry.VerifyUnchanged(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+" "), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registry.VerifyUnchanged(context.Background(), claim); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("mutation error = %v", err)
	}
}

func TestFileWorkerRegistryRejectsCredentialsSymlinksAndAmbiguity(t *testing.T) {
	for _, field := range []string{"password", "auth", "bearerToken"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			body := fmt.Sprintf(`{"sessionId":"session-1","pid":123,"kind":"interactive","cwd":"/work","url":"http://127.0.0.1:8080",%q:"x"}`, field)
			if err := os.WriteFile(filepath.Join(root, "123.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			registry, _ := NewFileWorkerRegistry(root)
			if _, err := registry.FindInteractive(context.Background(), "session-1"); !errors.Is(err, ErrInvalidRegistry) {
				t.Fatalf("credential row error = %v", err)
			}
		})
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	_ = os.WriteFile(target, []byte(`{}`), 0o600)
	_ = os.Symlink(target, filepath.Join(root, "123.json"))
	registry, _ := NewFileWorkerRegistry(root)
	if _, err := registry.FindInteractive(context.Background(), "session-1"); !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("symlink row error = %v", err)
	}

	root = t.TempDir()
	for _, name := range []string{"1.json", "2.json"} {
		_ = os.WriteFile(filepath.Join(root, name), []byte(`{"sessionId":"session-1","pid":123,"kind":"interactive","cwd":"/work","url":"http://127.0.0.1:8080"}`), 0o600)
	}
	registry, _ = NewFileWorkerRegistry(root)
	if _, err := registry.FindInteractive(context.Background(), "session-1"); !errors.Is(err, ErrWorkerAmbiguous) {
		t.Fatalf("ambiguous row error = %v", err)
	}
}

func TestFileWorkerRegistryIgnoresUnrelatedWorkerShapeAndSecrets(t *testing.T) {
	root := t.TempDir()
	selected := `{"sessionId":"selected","pid":123,"kind":"interactive","cwd":"/work","url":"http://127.0.0.1:8080"}`
	unrelated := `{"sessionId":"worker","kind":"daemon","endpoint":"http://203.0.113.8:9000","auth":{"bearerToken":"must-not-be-selected"},"futureShape":[1,2,3]}`
	if err := os.WriteFile(filepath.Join(root, "selected.json"), []byte(selected), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worker.json"), []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewFileWorkerRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := registry.FindInteractive(context.Background(), "selected")
	if err != nil || claim.SessionID != "selected" {
		t.Fatalf("selected claim = %#v, %v", claim, err)
	}
}
