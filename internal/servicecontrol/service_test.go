package servicecontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type recordedCommand struct {
	name      string
	arguments []string
}

type recordingRunner struct {
	commands []recordedCommand
	failAt   int
}

func (runner *recordingRunner) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	runner.commands = append(runner.commands, recordedCommand{name: name, arguments: append([]string(nil), arguments...)})
	if runner.failAt > 0 && len(runner.commands) == runner.failAt {
		return nil, errors.New("injected service-manager failure")
	}
	return nil, nil
}

func readRepositoryAsset(t *testing.T, relative string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	body, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
