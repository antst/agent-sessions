package daemon

import (
	"context"
	"testing"
)

func requireAuthorizationRejected(
	t *testing.T,
	authorize func(context.Context, ManagedAttachment, NativeEvidence) error,
	attachment ManagedAttachment,
	cases map[string]NativeEvidence,
) {
	t.Helper()
	for name, observed := range cases {
		t.Run(name, func(t *testing.T) {
			if err := authorize(context.Background(), attachment, observed); err == nil {
				t.Fatal("inexact native actor was authorized")
			}
		})
	}
}
