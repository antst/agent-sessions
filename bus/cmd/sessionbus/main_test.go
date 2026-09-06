package main

import (
	"path/filepath"
	"testing"
)

func TestParseLocalConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	socket := filepath.Join(t.TempDir(), "sessionbus.sock")
	table := filepath.Join(t.TempDir(), "sessions.json")
	configuration, err := parse([]string{"-socket", socket, "-table", table, "-host", "test-host", "-products", "one-peer,two-peer"})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.SocketPath != socket || configuration.TablePath != table || configuration.Host != "test-host" || len(configuration.Products) != 2 {
		t.Fatalf("configuration = %#v", configuration)
	}
}
