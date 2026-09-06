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
	return l.rename(name, os.Link, os.SameFile)
}

func (l *SessionLock) rename(name string, link func(string, string) error, same func(os.FileInfo, os.FileInfo) bool) error {
	if invalidPart(name) {
		return errors.New("session lock path is invalid")
	}
	path := filepath.Join(filepath.Dir(l.path), name)
	for {
		err := link(l.path, path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		stale, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = syscall.Flock(int(stale.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = stale.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				_ = os.Remove(l.path)
				return errors.New("session busy")
			}
			return err
		}
		opened, openErr := stale.Stat()
		current, pathErr := os.Stat(path)
		if errors.Is(pathErr, os.ErrNotExist) || openErr == nil && pathErr == nil && !same(opened, current) {
			_ = stale.Close()
			continue
		}
		if openErr != nil || pathErr != nil {
			_ = stale.Close()
			return errors.Join(openErr, pathErr)
		}
		if err = os.Remove(path); err != nil {
			_ = stale.Close()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		err = link(l.path, path)
		_ = stale.Close()
		if err == nil {
			break
		}
		if errors.Is(err, os.ErrExist) {
			_ = os.Remove(l.path)
			return errors.New("session busy")
		}
		return err
	}
	if err := os.Remove(l.path); err != nil {
		_ = os.Remove(path)
		return err
	}
	l.path = path
	return nil
}

func invalidPart(value string) bool {
	return strings.TrimSpace(value) == "" || value == "." || value == ".." || filepath.Base(value) != value
}
