package main

import (
	"context"
	"testing"

	"github.com/antst/sessionbus/wrappers/host"
)

func TestLaneModeRejectsArguments(t *testing.T) {
	t.Setenv(host.TokenEnv, "token")
	err := run(context.Background(), []string{"mcp"})
	if err == nil || err.Error() != "lane mode accepts no arguments" {
		t.Fatalf("run = %v", err)
	}
}
