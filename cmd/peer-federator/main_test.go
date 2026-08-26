package main

import (
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestLaneHelpProjectsAuthoritativeProductInventory(t *testing.T) {
	descriptors := federator.ProductDescriptors()
	productIDs := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		productIDs = append(productIDs, descriptor.ID)
	}

	pipeChoices := strings.Join(productIDs, "|")
	if got := laneProductChoices("|"); got != pipeChoices {
		t.Fatalf("lane product choices = %q, want %q", got, pipeChoices)
	}
	if got := usage(); !strings.Contains(got, "--product "+pipeChoices+" --") {
		t.Fatalf("top-level usage omits authoritative lane products: %q", got)
	}

	commaChoices := strings.Join(productIDs, ", ")
	if got := laneProductChoices(", "); got != commaChoices {
		t.Fatalf("lane flag choices = %q, want %q", got, commaChoices)
	}
}
