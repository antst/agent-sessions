//go:build linux

package codebuddy

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
)

func TestLinuxSocketOwnerCorrelatesExactListeningPIDAndRejectsPortRecycle(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	verifier := NewPlatformSocketOwnerVerifier()
	observation, err := verifier.VerifyOwner(context.Background(), endpoint, os.Getpid())
	if err != nil || observation.PID != os.Getpid() || observation.Identity == "" {
		_ = listener.Close()
		t.Fatalf("socket observation = %#v, %v", observation, err)
	}
	if _, err := verifier.VerifyOwner(context.Background(), endpoint, os.Getpid()+1000000); !errors.Is(err, ErrSocketOwner) {
		_ = listener.Close()
		t.Fatalf("wrong-pid error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyOwner(context.Background(), endpoint, os.Getpid()); !errors.Is(err, ErrSocketOwner) {
		t.Fatalf("closed/recycled error = %v", err)
	}
}
