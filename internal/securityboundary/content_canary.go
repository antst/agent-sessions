// Package securityboundary supplies content-canary and metadata-only helpers
// used by acceptance tests. Production code must not treat these fixture values
// as credentials or inspect vendor content to produce metadata.
package securityboundary

import (
	"bytes"
	"errors"
	"os"
	"sort"
	"time"
)

// ContentClass names one forbidden diagnostic, state, or evidence content class.
type ContentClass string

const (
	// CredentialContent marks a test-only credential canary.
	CredentialContent ContentClass = "credential"
	// PromptContent marks a test-only prompt canary.
	PromptContent ContentClass = "prompt"
	// ResultContent marks a test-only result canary.
	ResultContent ContentClass = "result"
	// TranscriptContent marks a test-only transcript canary.
	TranscriptContent ContentClass = "transcript"
	// LogContent marks a test-only log canary.
	LogContent ContentClass = "log"
)

// FixtureCanaries returns deterministic, unmistakable test-only content bytes.
func FixtureCanaries() map[ContentClass][]byte {
	return map[ContentClass][]byte{
		CredentialContent: []byte("AS_CANARY_CREDENTIAL_VALUE_0fe743a7"),
		PromptContent:     []byte("AS_CANARY_PROMPT_VALUE_a741398e"),
		ResultContent:     []byte("AS_CANARY_RESULT_VALUE_18f4e09c"),
		TranscriptContent: []byte("AS_CANARY_TRANSCRIPT_VALUE_974cd6c2"),
		LogContent:        []byte("AS_CANARY_LOG_VALUE_2d49ac5f"),
	}
}

// Detect reports every fixture content class present in data.
func Detect(data []byte, canaries map[ContentClass][]byte) []ContentClass {
	found := make([]ContentClass, 0, len(canaries))
	for class, canary := range canaries {
		if len(canary) != 0 && bytes.Contains(data, canary) {
			found = append(found, class)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i] < found[j] })
	return found
}

// FileMetadata is a non-content filesystem observation.
type FileMetadata struct {
	Exists  bool        `json:"exists"`
	Mode    os.FileMode `json:"mode,omitempty"`
	Size    int64       `json:"size,omitempty"`
	ModTime time.Time   `json:"mod_time,omitempty"`
}

// ObserveFileMetadata uses lstat only and never opens or hashes the file.
func ObserveFileMetadata(path string) (FileMetadata, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileMetadata{}, nil
	}
	if err != nil {
		return FileMetadata{}, err
	}
	return FileMetadata{Exists: true, Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}, nil
}
