//go:build !darwin

package bridge

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestClaudeSDKSocketPathRequiresExactPrivateDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "cc-socks")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	expected := filepath.Join(directory, strconv.Itoa(pid)+".sock")
	if !ownedClaudeSDKSocketPathUnder(expected, pid, directory) {
		t.Fatal("exact SDK socket under a private owned directory was rejected")
	}
	if ownedClaudeSDKSocketPathUnder(filepath.Join(directory, "other.sock"), pid, directory) {
		t.Fatal("non-PID SDK socket was accepted")
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if ownedClaudeSDKSocketPathUnder(expected, pid, directory) {
		t.Fatal("SDK socket under a non-private directory was accepted")
	}
}

func TestClaudeSDKSocketPathRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if ownedClaudeSDKSocketPathUnder(filepath.Join(alias, strconv.Itoa(pid)+".sock"), pid, alias) {
		t.Fatal("SDK socket under a symlinked directory was accepted")
	}
}
