package bridge

import (
	"strings"
	"testing"
)

func TestParseGrokLaneArgsCommandContract(t *testing.T) {
	t.Parallel()

	if options, err := parseGrokLaneArgs(nil); err != nil || !options.help {
		t.Fatalf("empty argv = %#v, %v; want help", options, err)
	}
	if options, err := parseGrokLaneArgs([]string{"--help"}); err != nil || !options.help {
		t.Fatalf("help argv = %#v, %v; want help", options, err)
	}
	for _, command := range []string{"resume", "wait", "status", "interrupt", "archive"} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			options, err := parseGrokLaneArgs([]string{command, "lane-a"})
			if err != nil {
				t.Fatalf("parse %s: %v", command, err)
			}
			if options.command != command || options.target != "lane-a" {
				t.Fatalf("parse %s = %#v", command, options)
			}
		})
	}
}

func TestParseGrokLaneArgsRejectsUnknownOrMissingTarget(t *testing.T) {
	t.Parallel()

	if _, err := parseGrokLaneArgs([]string{"invent"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
	for _, command := range []string{"resume", "wait", "status", "interrupt", "archive"} {
		if _, err := parseGrokLaneArgs([]string{command}); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("missing %s target error = %v", command, err)
		}
	}
}
