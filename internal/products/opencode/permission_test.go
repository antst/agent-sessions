package opencode

import (
	"errors"
	"testing"

	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/productruntime"
)

func TestOpenCodePermissionMappingIsExactAndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		mode, action string
	}{{string(permissionmode.Default), "ask"}, {string(permissionmode.BypassPermissions), "allow"}} {
		rules, err := MapPermission(permissionmode.Mode(test.mode))
		if err != nil || len(rules) != 1 || rules[0].Action != test.action || rules[0].Permission != "*" || rules[0].Pattern != "*" {
			t.Fatalf("%s = %#v, %v", test.mode, rules, err)
		}
	}
	if _, err := MapPermission(permissionmode.Mode("accept-edits")); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unknown mode = %v", err)
	}
}
