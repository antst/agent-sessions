package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENT_SESSIONS_PLUGIN_ROOT") == "" {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			panic(err)
		}
		if err := os.Setenv("AGENT_SESSIONS_PLUGIN_ROOT", root); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
