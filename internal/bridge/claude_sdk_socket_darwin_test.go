//go:build darwin

package bridge

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDarwinClaudeSDKSocketPathRequiresPrivateOwnedDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "cc-socks")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	const pid = 4242
	socket := filepath.Join(directory, strconv.Itoa(pid)+".sock")
	if !ownedClaudeSDKSocketPathUnder(socket, pid, directory) {
		t.Fatal("private current-user SDK directory was rejected")
	}
	if ownedClaudeSDKSocketPathUnder(socket, pid+1, directory) {
		t.Fatal("worker socket with the wrong PID was accepted")
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if ownedClaudeSDKSocketPathUnder(socket, pid, directory) {
		t.Fatal("non-private SDK directory was accepted")
	}
}

func TestDarwinClaudeSDKSocketPathRejectsSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "cc-socks")
	if err := os.Symlink(realDirectory, directory); err != nil {
		t.Fatal(err)
	}
	const pid = 4242
	if ownedClaudeSDKSocketPathUnder(filepath.Join(directory, strconv.Itoa(pid)+".sock"), pid, directory) {
		t.Fatal("symlinked SDK directory was accepted")
	}
}

func TestDarwinClaudeSDKSocketPathAcceptsResolvedAncestorAlias(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real-parent")
	directory := filepath.Join(realParent, "cc-socks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	const pid = 4242
	resolved := filepath.Join(directory, strconv.Itoa(pid)+".sock")
	aliasedDirectory := filepath.Join(aliasParent, "cc-socks")
	if !ownedClaudeSDKSocketPathUnder(resolved, pid, aliasedDirectory) {
		t.Fatal("resolved SDK socket path was rejected through a safe ancestor alias")
	}
}
