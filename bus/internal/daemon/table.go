package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

var errDuplicateRow = errors.New("session row already exists")

type row struct {
	SessionID string               `json:"session_id"`
	Product   string               `json:"product"`
	Name      string               `json:"name"`
	Groups    []string             `json:"groups"`
	Open      protocol.OpenOptions `json:"open"`
	CreatedAt time.Time            `json:"created_at"`
}

type table struct {
	mu   sync.Mutex
	path string
	rows map[string]row
}

func openTable(path string) (*table, error) {
	t := &table{path: path, rows: map[string]row{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for id, raw := range rows {
		var fields map[string]json.RawMessage
		var value row
		_ = json.Unmarshal(raw, &fields)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if len(fields) != 6 || decoder.Decode(&value) != nil {
			return nil, errors.New("invalid durable session table")
		}
		if value.SessionID != id || value.Name == "" || len(value.Groups) < 2 {
			return nil, errors.New("invalid durable session table")
		}
		if names[value.Name] {
			return nil, errors.New("duplicate durable session name")
		}
		names[value.Name] = true
		t.rows[id] = cloneRow(value)
	}
	return t, nil
}

func (t *table) list() []row {
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := make([]row, 0, len(t.rows))
	for _, value := range t.rows {
		rows = append(rows, cloneRow(value))
	}
	return rows
}

func (t *table) get(id string) (row, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.rows[id]
	return cloneRow(value), ok
}

func (t *table) byName(name string) (row, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, value := range t.rows {
		if value.Name == name {
			return cloneRow(value), true
		}
	}
	return row{}, false
}

func (t *table) insert(value row) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.rows[value.SessionID]; exists {
		return errDuplicateRow
	}
	for _, existing := range t.rows {
		if existing.Name == value.Name {
			return errDuplicateRow
		}
	}
	t.rows[value.SessionID] = cloneRow(value)
	if err := t.persist(); err != nil {
		delete(t.rows, value.SessionID)
		return err
	}
	return nil
}

func (t *table) delete(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.rows[id]
	if !ok {
		return nil
	}
	delete(t.rows, id)
	if err := t.persist(); err != nil {
		t.rows[id] = value
		return err
	}
	return nil
}

func (t *table) persist() error {
	raw, err := json.Marshal(t.rows)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(t.path), ".sessions-*")
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
		err = os.Rename(name, t.path)
	}
	return err
}

func cloneRow(value row) row {
	value.Groups = append([]string(nil), value.Groups...)
	value.Open.Arguments = append([]string(nil), value.Open.Arguments...)
	return value
}
