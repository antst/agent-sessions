package host

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
)

type PrivateEndpoint struct {
	net.Listener
	Path string
}

func ListenPrivate(socket, sessionID string) (*PrivateEndpoint, error) {
	if invalidPart(sessionID) {
		return nil, errors.New("private endpoint session id is invalid")
	}
	directory := filepath.Join(filepath.Dir(socket), "lanes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, sessionID+".sock")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &PrivateEndpoint{Listener: listener, Path: path}, nil
}

func (e *PrivateEndpoint) Close() error {
	err := e.Listener.Close()
	if removeErr := os.Remove(e.Path); !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

func BridgeStdio(ctx context.Context, path string, input io.ReadCloser, output io.Writer) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = input.Close() })
	defer stop()
	written := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(connection, input)
		if unix, ok := connection.(*net.UnixConn); ok {
			_ = unix.CloseWrite()
		}
		written <- copyErr
	}()
	_, readErr := io.Copy(output, connection)
	_ = input.Close()
	writeErr := <-written
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(readErr, writeErr)
}
