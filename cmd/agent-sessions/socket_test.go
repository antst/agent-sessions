package main

import (
	"context"
	"path/filepath"
	"testing"

	daemonpkg "github.com/antst/sessionbus/internal/daemon"
	"github.com/antst/sessionbus/internal/testutil"
)

// shortDaemonTestRoot avoids testing.T.TempDir's test-name component in paths
// that will contain a Unix-domain socket. Darwin permits only 103 pathname
// bytes in sockaddr_un.sun_path.
func shortDaemonTestRoot(t testing.TB) string {
	t.Helper()
	return testutil.ShortSocketRoot(t, "as-", filepath.Join("run", "c-00000000000000000000.sock"))
}

func activateTestAttachment(t testing.TB, runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) {
	t.Helper()
	if attachment.CapabilityHash == "" {
		attachment.CapabilityHash = "test-capability"
	}
	if attachment.ProfileIdentity == "" {
		attachment.ProfileIdentity = "test-profile"
	}
	runtime.Attachments().SetAdapter(attachment.Product, daemonpkg.AttachmentAdapter{
		Prepare: func(context.Context, daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
			return daemonpkg.NativeEvidence{}, nil
		},
		Adopt: func(_ context.Context, _ daemonpkg.ManagedAttachment, evidence daemonpkg.NativeEvidence) (daemonpkg.NativeEvidence, error) {
			return evidence, nil
		},
	})
	if _, err := runtime.Attachments().Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Attachments().Adopt(context.Background(), attachment.ID, daemonpkg.NativeEvidence{ThreadID: attachment.NativeSessionID}); err != nil {
		t.Fatal(err)
	}
	runtime.Attachments().ReportLive(attachment.ID, attachment.ID, attachment.Product, attachment.Groups, map[string]string{}, false)
}
