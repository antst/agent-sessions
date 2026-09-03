package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConnectorImageRefresherIgnoresFileReplacementUntilDaemonIdentityChanges(t *testing.T) {
	root := t.TempDir()
	first := writeConnectorTestImage(t, root, "first", "first")
	second := writeConnectorTestImage(t, root, "second", "second")
	alias := filepath.Join(root, "agent-sessions")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	firstIdentity := connectorTestIdentity("first")
	refresher, err := newConnectorImageRefresher(
		[]string{alias, "connector", "codex", "--release-identity", firstIdentity}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	refresher.exec = func(string, []string, []string) error { calls++; return nil }
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	refresher.observeDaemon(firstIdentity)
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("file-only replacement exec calls = %d, want 0", calls)
	}
}

func TestConnectorImageRefresherExecsOnceWhenDaemonAndFileIdentityMatch(t *testing.T) {
	root := t.TempDir()
	first := writeConnectorTestImage(t, root, "first", "first")
	second := writeConnectorTestImage(t, root, "second", "second")
	alias := filepath.Join(root, "agent-sessions")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	firstIdentity := connectorTestIdentity("first")
	secondIdentity := connectorTestIdentity("second")
	refresher, err := newConnectorImageRefresher(
		[]string{alias, "connector", "claude", "mcp", "--release-identity", firstIdentity},
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
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	refresher.observeDaemon(secondIdentity)
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	if len(invoked) != 1 {
		t.Fatalf("replacement exec calls = %d, want 1", len(invoked))
	}
	want := invocation{
		path:    alias,
		args:    []string{alias, "connector", "claude", "mcp", "--release-identity", secondIdentity},
		environ: []string{"HOME=/test"},
	}
	if !reflect.DeepEqual(invoked[0], want) {
		t.Fatalf("exec invocation = %#v, want %#v", invoked[0], want)
	}
}

func TestConnectorImageRefresherIgnoresDaemonIdentityUntilFileMatches(t *testing.T) {
	root := t.TempDir()
	first := writeConnectorTestImage(t, root, "first", "first")
	second := writeConnectorTestImage(t, root, "second", "second")
	alias := filepath.Join(root, "agent-sessions")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	refresher, err := newConnectorImageRefresher([]string{alias, "connector", "grok"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	refresher.exec = func(string, []string, []string) error { calls++; return nil }
	refresher.observeDaemon(connectorTestIdentity("different-ready-daemon"))
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if err := refresher.refresh(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("mid-transaction identity mismatch exec calls = %d, want 0", calls)
	}
}

func writeConnectorTestImage(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func connectorTestIdentity(body string) string {
	hash := sha256.Sum256([]byte(body))
	return hex.EncodeToString(hash[:])
}
