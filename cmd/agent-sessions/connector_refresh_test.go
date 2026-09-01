package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConnectorImageRefresherReexecsReplacedInstalledImageWithoutChangingStreams(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	alias := filepath.Join(root, "agent-sessions")
	if err := os.WriteFile(first, []byte("first"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	firstHash := sha256.Sum256([]byte("first"))
	firstIdentity := hex.EncodeToString(firstHash[:])
	secondHash := sha256.Sum256([]byte("second"))
	secondIdentity := hex.EncodeToString(secondHash[:])
	refresher, err := newConnectorImageRefresher(
		[]string{alias, "connector", "codex", "--release-identity", firstIdentity},
		[]string{"HOME=/test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	type invocation struct {
		path    string
		args    []string
		environ []string
	}
	var invoked []invocation
	refresher.exec = func(path string, args, environ []string) error {
		invoked = append(invoked, invocation{path: path, args: args, environ: environ})
		return nil
	}
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	if len(invoked) != 0 {
		t.Fatalf("unchanged image caused exec: %+v", invoked)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	if len(invoked) != 1 {
		t.Fatalf("replacement exec calls = %d, want 1", len(invoked))
	}
	call := invoked[0]
	if call.path != alias {
		t.Fatalf("exec path = %q, want %q", call.path, alias)
	}
	wantArgs := []string{alias, "connector", "codex", "--release-identity", secondIdentity}
	if !reflect.DeepEqual(call.args, wantArgs) {
		t.Fatalf("exec args = %#v, want %#v", call.args, wantArgs)
	}
	if !reflect.DeepEqual(call.environ, []string{"HOME=/test"}) {
		t.Fatalf("exec environment = %#v", call.environ)
	}
}

func TestConnectorImageRefresherNormalizesSourcePlaceholderBeforeServing(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "agent-sessions")
	if err := os.WriteFile(binary, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte("current"))
	wantIdentity := hex.EncodeToString(wantHash[:])
	refresher, err := newConnectorImageRefresher(
		[]string{binary, "connector", "auto", "--release-identity", "@AGENT_SESSIONS_RELEASE_ID@"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	refresher.exec = func(_ string, arguments, _ []string) error {
		got = append([]string(nil), arguments...)
		return nil
	}
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	want := []string{binary, "connector", "auto", "--release-identity", wantIdentity}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized args = %#v, want %#v", got, want)
	}
}

func TestConnectorImageRefresherAddsIdentityToLegacyConnectorArgs(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "agent-sessions")
	if err := os.WriteFile(binary, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte("current"))
	wantIdentity := hex.EncodeToString(wantHash[:])
	refresher, err := newConnectorImageRefresher([]string{binary, "connector", "claude", "mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	refresher.exec = func(_ string, arguments, _ []string) error {
		got = append([]string(nil), arguments...)
		return nil
	}
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	want := []string{binary, "connector", "claude", "mcp", "--release-identity", wantIdentity}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized legacy args = %#v, want %#v", got, want)
	}
}
