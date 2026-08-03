// Package config holds the small amount of on-disk settings shcr needs.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shellcrumbs/shcr/internal/paths"
)

type Config struct {
	Sync Sync `json:"sync"`
}

type Sync struct {
	// Enabled gates the background sync loop. Sync is off until asked for.
	Enabled bool `json:"enabled"`
	// Backend selects the storage implementation. Only "file" exists so far —
	// a directory acting as the bucket, which covers a NAS mount, a synced
	// folder or an rclone mount.
	Backend string `json:"backend"`
	// Path is the bucket root for the file backend.
	Path string `json:"path"`
	// ShareHostname puts a hostname hint in the manifest, which the storage
	// provider can read. Off by default.
	ShareHostname bool `json:"share_hostname"`
}

func Path() string { return filepath.Join(paths.DataDir(), "config.json") }

func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", Path(), err)
	}
	return c, nil
}

func Save(c Config) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), append(b, '\n'), 0o600)
}

// RedactConfigPath is the user's extra redaction rules.
func RedactConfigPath() string { return filepath.Join(paths.DataDir(), "redact.conf") }
