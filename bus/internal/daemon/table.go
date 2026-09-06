package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/antst/sessionbus/bus/internal/protocol"
)

type row struct {
	SessionID string               `json:"session_id"`
	Product   string               `json:"product"`
	Name      string               `json:"name"`
	Groups    []string             `json:"groups"`
	Open      protocol.OpenOptions `json:"open"`
	CreatedAt time.Time            `json:"created_at"`
}

type table struct{ path string }

func openTable(path string) (*table, []row, error) {
	t := &table{path: path}
	files, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var rows []row
	ids, names := map[string]bool{}, map[string]bool{}
	for _, file := range files {
		if filepath.Ext(file.Name()) != ".json" {
			continue
		}
		if file.IsDir() {
			return nil, nil, errors.New("invalid durable session table")
		}
		raw, readErr := os.ReadFile(filepath.Join(path, file.Name()))
		var fields map[string]json.RawMessage
		var value row
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if readErr != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 6 || decoder.Decode(&value) != nil || file.Name() != rowFile(value.SessionID) || value.Name == "" || len(value.Groups) < 2 || ids[value.SessionID] || names[value.Name] {
			return nil, nil, errors.New("invalid durable session table")
		}
		ids[value.SessionID], names[value.Name] = true, true
		rows = append(rows, cloneRow(value))
	}
	return t, rows, nil
}

func (t *table) write(value row) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(t.path, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(t.path, ".row-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(t.path, rowFile(value.SessionID)))
	}
	if err == nil {
		directory, openErr := os.Open(t.path)
		if openErr == nil {
			err = directory.Sync()
			_ = directory.Close()
		} else {
			err = openErr
		}
	}
	return err
}

func (t *table) delete(id string) error {
	err := os.Remove(filepath.Join(t.path, rowFile(id)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func rowFile(id string) string {
	digest := sha256.Sum256([]byte(id))
	return hex.EncodeToString(digest[:]) + ".json"
}

func cloneRow(value row) row {
	value.Groups = append([]string(nil), value.Groups...)
	value.Open.Arguments = append([]string(nil), value.Open.Arguments...)
	return value
}
