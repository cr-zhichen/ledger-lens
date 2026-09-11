package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"ledger-lens/internal/version"
)

const (
	checkInterval   = 24 * time.Hour
	failureCooldown = time.Hour
)

var errCacheBusy = errors.New("update cache is locked")

type cachedRelease struct {
	Tag       string    `json:"tag"`
	CheckedAt time.Time `json:"checked_at"`
	RetryAt   time.Time `json:"retry_at,omitzero"`
}

func (c *Client) Cached(current string) Result {
	return c.loadCache().result(current, "cache")
}

func (cache cachedRelease) result(current, source string) Result {
	latest, valid := version.FromTag(cache.Tag)
	if !valid || cache.CheckedAt.IsZero() {
		return Result{CurrentVersion: current, ReleaseURL: ReleasesURL}
	}
	return releaseResult(current, latest, cache.CheckedAt, source)
}

func (cache cachedRelease) due(now time.Time) bool {
	if now.Before(cache.RetryAt) {
		return false
	}
	return cache.CheckedAt.IsZero() || now.Before(cache.CheckedAt) || !now.Before(cache.CheckedAt.Add(checkInterval))
}

func (c *Client) loadCache() cachedRelease {
	if c.CachePath == "" {
		return cachedRelease{}
	}
	f, err := os.Open(c.CachePath)
	if err != nil {
		return cachedRelease{}
	}
	defer f.Close()
	var cache cachedRelease
	if err := json.NewDecoder(io.LimitReader(f, 4096)).Decode(&cache); err != nil {
		return cachedRelease{}
	}
	if _, valid := version.FromTag(cache.Tag); !valid {
		cache.Tag = ""
		cache.CheckedAt = time.Time{}
	}
	return cache
}

// Atomic replacement keeps readers from seeing partially written JSON.
func (c *Client) saveCache(cache cachedRelease) error {
	if c.CachePath == "" {
		return os.ErrNotExist
	}
	dir := filepath.Dir(c.CachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".update-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	writeErr := json.NewEncoder(f).Encode(cache)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), c.CachePath)
}

// Keep the lock file in place: unlinking it would let processes lock different inodes.
// The OS releases the lock on Close or process exit, including crashes.
func (c *Client) lockCache(ctx context.Context, wait bool) (*os.File, error) {
	if c.CachePath == "" {
		return nil, os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(c.CachePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		locked, err := tryLock(f)
		if locked {
			return f, nil
		}
		if err != nil || !wait {
			f.Close()
			if err != nil {
				return nil, err
			}
			return nil, errCacheBusy
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
