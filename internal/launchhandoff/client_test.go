//go:build linux || darwin

package launchhandoff

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestWrapperRejectsTruncatedGOWithoutEnteringExecSeam(t *testing.T) {
	root := testStateRoot(t)
	path := filepath.Join(root, "run", endpointName)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		if _, err := readRawFrame(connection); err != nil {
			serverDone <- err
			return
		}
		command, err := encodeCommand(testCommand(), DefaultLimits())
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeRawFrame(connection, command); err != nil {
			zero(command)
			serverDone <- err
			return
		}
		zero(command)
		if _, err := readRawFrame(connection); err != nil {
			serverDone <- err
			return
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(simpleFrame(frameGo))))
		if _, err := connection.Write(header[:]); err != nil {
			serverDone <- err
			return
		}
		_, err = connection.Write(simpleFrame(frameGo)[:3])
		serverDone <- err
	}()

	ticket := Ticket{ID: "0123456789abcdef0123456789abcdef", Contract: ContractVersion}
	var execCalls atomic.Int32
	err = consumeAndExec(context.Background(), path, ticket, DefaultLimits(), func(string, []string, []string, string) error {
		execCalls.Add(1)
		return nil
	})
	if !errors.Is(err, ErrUnavailable) || execCalls.Load() != 0 {
		t.Fatalf("truncated GO result = %v, exec calls=%d", err, execCalls.Load())
	}
	if serverErr := <-serverDone; serverErr != nil {
		t.Fatal(serverErr)
	}
}

func readRawFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	body := make([]byte, int(binary.BigEndian.Uint32(header[:])))
	_, err := io.ReadFull(reader, body)
	return body, err
}

func writeRawFrame(writer io.Writer, body []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body))) //nolint:gosec // bounded test command.
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(body)
	return err
}
