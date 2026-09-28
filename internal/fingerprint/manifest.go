package fingerprint

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// manifest records the checksum of each source file of a task, as of the last
// time the task's combined checksum was stored. Comparing it with the current
// files tells which files were added, modified or removed.
type manifest struct {
	Files map[string]string `json:"files"`
}

func writeManifest(path string, files map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(manifest{Files: files})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// readManifest returns nil, without an error, if there is no manifest.
func readManifest(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return m.Files, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// relativeTo returns path relative to dir, using forward slashes. It returns
// path unchanged if it cannot be made relative.
func relativeTo(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
