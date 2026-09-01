package releaseevidence

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

func loadBoundedYAML[T any](path, label string, maxBytes int64) (*T, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("%s is not a bounded regular file", label)
	}
	// #nosec G304 -- callers provide repository-owned public contract paths; size is bounded above.
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var decoded T
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s contains multiple YAML documents", label)
		}
		return nil, fmt.Errorf("decode trailing %s data: %w", label, err)
	}
	return &decoded, nil
}
