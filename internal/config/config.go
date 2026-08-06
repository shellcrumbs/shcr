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
	Sync    Sync    `json:"sync"`
	Ranking Ranking `json:"ranking"`
}

// Ranking configures how the picker orders results, and whether it keeps the
// record needed to tell whether that ordering is any good.
type Ranking struct {
	// LogAcceptances records, locally, which candidate was taken for which
	// query and where it ranked. It is the only way to tell whether a change to
	// the ranking made things better or worse, rather than merely different.
	//
	// It stays on this machine: its own table, never synced, never exported,
	// and dropped by `shcr rank forget`. Opt out by setting it to false.
	// The zero value is off, so DefaultRanking supplies the default for a
	// config file that has never mentioned it.
	LogAcceptances bool `json:"log_acceptances"`
}

// DefaultRanking is used when the config file predates these settings.
func DefaultRanking() Ranking { return Ranking{LogAcceptances: true} }

type Sync struct {
	// Enabled gates the background sync loop. Sync is off until asked for.
	Enabled bool `json:"enabled"`
	// Backend selects the storage implementation: "file" for a directory acting
	// as the bucket — a NAS mount, a synced folder, an rclone mount — or "gcs"
	// for a Google Cloud Storage bucket.
	Backend string `json:"backend"`
	// Path is the bucket root for the file backend.
	Path string `json:"path"`
	// Bucket is the bucket name for the gcs backend.
	Bucket string `json:"bucket,omitempty"`
	// Prefix optionally nests everything under a folder inside that bucket, so
	// one bucket can hold more than shcr.
	Prefix string `json:"prefix,omitempty"`
	// ShareHostname puts a hostname hint in the manifest, which the storage
	// provider can read. Off by default.
	ShareHostname bool `json:"share_hostname"`
}

func Path() string { return filepath.Join(paths.DataDir(), "config.json") }

func Load() (Config, error) {
	c := Config{Ranking: DefaultRanking()}
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	// Defaults have to be applied after unmarshalling, not before: a field the
	// file does not mention would otherwise keep whatever was preset, and a
	// field it sets to false would be indistinguishable from one it omits.
	c.Ranking = DefaultRanking()
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
