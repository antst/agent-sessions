package bridge

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGrokNativeLeaderBootstrapOwnsTheOneLeaderArgv(t *testing.T) {
	root := t.TempDir()
	bootstrap, err := NewGrokNativeLeaderBootstrap(
		"/usr/bin/grok", root, filepath.Join(root, "leader.sock"), []string{"PATH=/bin"}, io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	command := bootstrap.Command()
	want := []string{
		"/usr/bin/grok", "--permission-mode", "default", "agent", "leader", "--leader-socket",
		filepath.Join(root, "leader.sock"), "--relay-on-demand", "--no-auto-update",
	}
	if !reflect.DeepEqual(command.Args, want) || command.Dir != root || !reflect.DeepEqual(command.Env, []string{"PATH=/bin"}) {
		t.Fatalf("command args=%q dir=%q env=%q", command.Args, command.Dir, command.Env)
	}
	for _, argument := range command.Args {
		if argument == "--no-exit-on-disconnect" {
			t.Fatal("leader bootstrap restored orphan-preserving flag")
		}
	}
	if command.Stdout != io.Discard || command.Stderr != io.Discard {
		t.Fatal("leader diagnostics were not shared")
	}
	_ = os.Remove(filepath.Join(root, "leader.sock"))
}
