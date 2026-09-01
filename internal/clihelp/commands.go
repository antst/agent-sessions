// Package clihelp owns the authoritative multi-call command and help surface.
package clihelp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// Invocation is one parsed multi-call operation with untouched trailing bytes.
type Invocation struct {
	Command   string
	Product   string
	Arguments []string
}

// Parse resolves either the agent-sessions command surface or one installed
// peer/lane alias without interpreting vendor arguments.
func Parse(argv0 string, args []string) (Invocation, error) {
	name := filepath.Base(argv0)
	if product, ok := productcatalog.ByCommand(name); ok {
		command := "peer"
		if name == product.LaneAlias {
			command = "lane"
		}
		return Invocation{Command: command, Product: product.ID, Arguments: append([]string(nil), args...)}, nil
	}
	if name != "agent-sessions" {
		return Invocation{}, fmt.Errorf("unknown Agent Sessions command alias %q", name)
	}
	if len(args) == 0 {
		return Invocation{}, errors.New("agent sessions command is required")
	}
	command := args[0]
	switch command {
	case "help", "--help", "-h":
		return Invocation{Command: "help"}, nil
	case "version", "--version", "-version":
		return Invocation{Command: "version"}, nil
	case "daemon", "status", "doctor", "roster", "catalog":
		return Invocation{Command: command, Arguments: append([]string(nil), args[1:]...)}, nil
	case "peer", "lane", "hook", "connector":
		if len(args) < 2 {
			return Invocation{}, fmt.Errorf("%s requires a product", command)
		}
		if command == "connector" && args[1] == "auto" {
			return Invocation{Command: command, Product: "auto", Arguments: append([]string(nil), args[2:]...)}, nil
		}
		product, ok := productcatalog.ByID(args[1])
		if !ok {
			return Invocation{}, fmt.Errorf("%s product %q is unsupported", command, args[1])
		}
		return Invocation{Command: command, Product: product.ID, Arguments: append([]string(nil), args[2:]...)}, nil
	default:
		return Invocation{}, fmt.Errorf("unknown Agent Sessions command %q", command)
	}
}

// Usage renders the closed command, product, and alias inventory.
func Usage() string {
	products := productcatalog.All()
	ids := make([]string, 0, len(products))
	aliases := make([]string, 0, 2*len(products))
	for _, product := range products {
		ids = append(ids, product.ID)
		aliases = append(aliases, product.PeerAlias+" → peer "+product.ID, product.LaneAlias+" → lane "+product.ID)
	}
	return fmt.Sprintf(`agent-sessions — one user-host Agent Sessions daemon and short-lived clients

Usage:
  agent-sessions daemon [run [--state-root PATH]|start|stop|restart]
  agent-sessions status [--state-root PATH]
  agent-sessions doctor [--state-root PATH]
  agent-sessions roster [--json] [--state-root PATH]
  agent-sessions catalog --json
  agent-sessions peer PRODUCT [ARGS...]
  agent-sessions lane PRODUCT [ARGS...]
  agent-sessions hook PRODUCT [ARGS...]
  agent-sessions connector PRODUCT|auto [ARGS...]
  agent-sessions version

Products: %s
Aliases:
  %s

Peer, lane, hook, and connector commands require an already running daemon and
never start, stop, restart, or supervise it.
`, strings.Join(ids, "|"), strings.Join(aliases, "\n  "))
}
