package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type SessionLock struct {
	file *os.File
	path string
}

func AcquireSessionLock(socket, product, sessionID string) (*SessionLock, error) {
	if invalidPart(product) || invalidPart(sessionID) {
		return nil, errors.New("session lock path is invalid")
	}
	directory := filepath.Join(filepath.Dir(socket), "locks", product)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, sessionID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("session busy")
		}
		return nil, err
	}
	return &SessionLock{file: file, path: file.Name()}, nil
}

func (l *SessionLock) File() *os.File { return l.file }
func (l *SessionLock) Close() error   { return l.file.Close() }
func (l *SessionLock) Rename(name string) error {
	if invalidPart(name) {
		return errors.New("session lock path is invalid")
	}
	path := filepath.Join(filepath.Dir(l.path), name)
	if err := os.Rename(l.path, path); err != nil {
		return err
	}
	l.path = path
	return nil
}

func invalidPart(value string) bool {
	return strings.TrimSpace(value) == "" || value == "." || value == ".." || filepath.Base(value) != value
}
