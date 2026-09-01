package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/federation"
)

func main() {
	raw := []byte(`{"type":"lane_exec","version":3,"product":"opencode","capabilities":["opencode-lane"],"future_additive_field":{"ignored":true}}`)
	var message federation.Message
	if err := json.Unmarshal(raw, &message); err != nil {
		fmt.Fprintf(os.Stderr, "decode additive field: %v\n", err)
		os.Exit(1)
	}
	if federation.ProtocolVersion != 3 || message.Version != 3 || message.Product != "opencode" || len(message.Capabilities) != 1 || message.Capabilities[0] != "opencode-lane" {
		fmt.Fprintf(os.Stderr, "unexpected projection: %#v\n", message)
		os.Exit(1)
	}
	fmt.Printf("{\"additive_unknown_field\":\"accepted\",\"capability\":%q,\"product\":%q,\"protocol_version\":%d}\n", message.Capabilities[0], message.Product, message.Version)
}
