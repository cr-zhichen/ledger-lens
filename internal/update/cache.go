package update

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"ledger-lens/internal/version"
)

type cachedRelease struct {
	Tag       string    `json:"tag"`
	CheckedAt time.Time `json:"checked_at"`
}

func (c *Client) Cached(current string) Result {
	empty := Result{CurrentVersion: current, ReleaseURL: ReleasesURL}
	if c.CachePath == "" {
		return empty
	}
	f, err := os.Open(c.CachePath)
	if err != nil {
		return empty
	}
	defer f.Close()
	var cache cachedRelease
	if err := json.NewDecoder(io.LimitReader(f, 4096)).Decode(&cache); err != nil || cache.CheckedAt.IsZero() {
		return empty
	}
	latest, valid := version.FromTag(cache.Tag)
	if !valid {
		return empty
	}
	return releaseResult(current, latest, cache.CheckedAt, "cache")
}

// Cache failures never fail a command. Atomic replacement avoids partially written JSON.
func (c *Client) saveCache(cache cachedRelease) {
	if c.CachePath == "" {
		return
	}
	dir := filepath.Dir(c.CachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".update-*.json")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	writeErr := json.NewEncoder(f).Encode(cache)
	closeErr := f.Close()
	if writeErr == nil && closeErr == nil {
		_ = os.Rename(f.Name(), c.CachePath)
	}
}
