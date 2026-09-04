package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestScanBoundaries(t *testing.T) {
	for _, suffix := range []string{"\n", ""} {
		calls := 0
		err := scan(strings.NewReader(strings.Repeat("x", federation.MaxAgentContent)+suffix), io.Discard, func(_ int, body string) error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatalf("exact max suffix %q: calls=%d err=%v", suffix, calls, err)
		}
	}
	calls := 0
	if err := scan(strings.NewReader(strings.Repeat("x", federation.MaxAgentContent+1)), io.Discard, func(_ int, _ string) error { calls++; return nil }); err == nil || calls != 0 {
		t.Fatalf("over max: calls=%d err=%v", calls, err)
	}
}

func TestScanDisciplineAndParams(t *testing.T) {
	var stderr bytes.Buffer
	var got []string
	err := scan(bytes.NewReader([]byte("\r\n\xff\nx\r\na\rb\nz\r")), &stderr, func(_ int, body string) error {
		got = append(got, body)
		return nil
	})
	if err != nil || strings.Join(got, "|") != "x|a\rb|z" || !strings.Contains(stderr.String(), "line 2 rejected") {
		t.Fatalf("got=%q stderr=%q err=%v", got, stderr.String(), err)
	}
	calls := 0
	if err := scan(strings.NewReader("one\ntwo\n"), io.Discard, func(_ int, _ string) error { calls++; return errors.New("stop") }); err == nil || calls != 1 {
		t.Fatalf("send stop calls=%d err=%v", calls, err)
	}
	group := messageParams(options{groups: []string{"g"}}, "b")
	target := messageParams(options{groups: []string{"g"}, target: "t"}, "b")
	if group["group"] != "g" || target["target"] != "t" || target["group"] != nil {
		t.Fatalf("group=%v target=%v", group, target)
	}
}
