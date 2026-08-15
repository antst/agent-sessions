package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/antst/agent-sessions/internal/fileutil"
)

const agyPluginName = "agent-sessions"

type agyPluginImport struct {
	Name       string   `json:"name"`
	Source     string   `json:"source"`
	ImportedAt string   `json:"importedAt"`
	Components []string `json:"components"`
}

// RegisterAgyPluginManifest records the directly staged plugin using the same
// import shape as `agy plugin install`, without invoking the desktop-delegating
// command. Unknown top-level fields and other plugin entries are preserved.
func RegisterAgyPluginManifest(path string) error {
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Agy config directory: %w", err)
	}
	manifest := make(map[string]json.RawMessage)
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read Agy import manifest: %w", err)
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &manifest); err != nil {
			return fmt.Errorf("parse Agy import manifest: %w", err)
		}
		if manifest == nil {
			return fmt.Errorf("parse Agy import manifest: expected an object")
		}
	}
	var imports []json.RawMessage
	if raw := manifest["imports"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &imports); err != nil {
			return fmt.Errorf("parse Agy plugin imports: %w", err)
		}
	}
	retained := make([]json.RawMessage, 0, len(imports)+1)
	for _, raw := range imports {
		var identity struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			return fmt.Errorf("parse Agy plugin import: %w", err)
		}
		if identity.Name != agyPluginName {
			retained = append(retained, raw)
		}
	}
	entry, err := json.Marshal(agyPluginImport{
		Name:       agyPluginName,
		Source:     "antigravity",
		ImportedAt: time.Now().UTC().Format(time.RFC3339),
		Components: []string{"skills", "mcpServers", "hooks"},
	})
	if err != nil {
		return fmt.Errorf("encode Agy plugin import: %w", err)
	}
	retained = append(retained, entry)
	encodedImports, err := json.Marshal(retained)
	if err != nil {
		return fmt.Errorf("encode Agy plugin imports: %w", err)
	}
	manifest["imports"] = encodedImports
	if err := fileutil.WriteJSONAtomic(path, manifest); err != nil {
		return fmt.Errorf("write Agy import manifest: %w", err)
	}
	return nil
}
