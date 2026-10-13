package store

import (
	"context"
	"encoding/json"
	"time"

	"ledger-lens/internal/fault"
)

type Catalog struct {
	Data     json.RawMessage `json:"data"`
	SyncedAt string          `json:"synced_at"`
	Source   string          `json:"source"`
}

func (s *Store) Catalog(ctx context.Context, owner, name string) (Catalog, error) {
	var raw string
	r := Catalog{Source: "cache"}
	err := s.db.QueryRowContext(ctx, `SELECT raw,updated_at FROM catalogs WHERE owner=? AND name=?`, owner, name).Scan(&raw, &r.SyncedAt)
	if noRows(err) {
		return r, fault.New("CACHE_EMPTY", "资料尚未缓存，请联网查询或执行 cache refresh", 5)
	}
	if err != nil || !json.Valid([]byte(raw)) {
		return r, databaseError()
	}
	r.Data = json.RawMessage(raw)
	return r, nil
}

func (s *Store) SaveCatalogs(ctx context.Context, a Account, entries map[string]json.RawMessage) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", databaseError()
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM account WHERE id=1`).Scan(&revision); err != nil {
		return "", databaseError()
	}
	if revision != a.Revision {
		return "", conflict()
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for name, raw := range entries {
		if !json.Valid(raw) {
			return "", databaseError()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalogs(owner,name,raw,updated_at) VALUES(?,?,?,?) ON CONFLICT(owner,name) DO UPDATE SET raw=excluded.raw,updated_at=excluded.updated_at`, a.Owner(), name, string(raw), at); err != nil {
			return "", databaseError()
		}
	}
	if err := tx.Commit(); err != nil {
		return "", databaseError()
	}
	return at, nil
}

func (s *Store) InvalidateAssets(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM catalogs WHERE owner=? AND name IN ('assets:0','assets:2')`, owner)
	if err != nil {
		return databaseError()
	}
	return nil
}

// Snapshot reads every bill and its sync revision in one transaction. Reports
// must not combine separate pages from different sync generations.
func (s *Store) Snapshot(ctx context.Context, owner string) ([]json.RawMessage, CacheState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, CacheState{}, databaseError()
	}
	defer tx.Rollback()
	state, err := readCacheState(ctx, tx, owner)
	if err != nil {
		return nil, state, err
	}
	if !state.Initialized {
		return nil, state, fault.New("CACHE_EMPTY", "账单尚未缓存，请先执行 sync", 5)
	}
	rows, err := tx.QueryContext(ctx, `SELECT raw FROM bills WHERE owner=?`, owner)
	if err != nil {
		return nil, state, databaseError()
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			return nil, state, databaseError()
		}
		items = append(items, json.RawMessage(raw))
	}
	if rows.Err() != nil {
		return nil, state, databaseError()
	}
	if err := rows.Close(); err != nil {
		return nil, state, databaseError()
	}
	if err := tx.Commit(); err != nil {
		return nil, state, databaseError()
	}
	return items, state, nil
}
