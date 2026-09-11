package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Account struct {
	Revision    int64
	BaseURL     string
	Identifier  string
	PasswordMD5 string
	AutoLogin   bool
	qianji.Session
}

func (a Account) Owner() string { return a.BaseURL + "\n" + a.UID }

func DefaultPath() (string, error) {
	if path := os.Getenv("LEDGERLENS_DB"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", databaseError()
	}
	return filepath.Join(dir, "ledger-lens", "ledgerlens.sqlite"), nil
}

func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, databaseError()
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, databaseError()
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, databaseError()
	}
	err = f.Chmod(0o600)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, databaseError()
	}
	// SQLite file URIs require forward slashes and /C:/... for Windows drive paths.
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	u.RawQuery = "_pragma=busy_timeout%285000%29&_pragma=secure_delete%28ON%29&_txlock=immediate"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, databaseError()
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS account (
 id INTEGER PRIMARY KEY CHECK (id = 1), revision INTEGER NOT NULL DEFAULT 0,
 base_url TEXT NOT NULL DEFAULT '', identifier TEXT NOT NULL DEFAULT '',
 password_md5 TEXT NOT NULL DEFAULT '', uid TEXT NOT NULL DEFAULT '',
 token TEXT NOT NULL DEFAULT '', device TEXT NOT NULL DEFAULT '',
 auto_login INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO account (id) VALUES (1);
CREATE TABLE IF NOT EXISTS request_clock (route TEXT PRIMARY KEY, epoch_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sync_state (
 owner TEXT PRIMARY KEY, revision INTEGER NOT NULL, lasttimes TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS bills (
 owner TEXT NOT NULL, id TEXT NOT NULL, book_id TEXT NOT NULL,
 occurred_at INTEGER NOT NULL, raw TEXT NOT NULL,
 PRIMARY KEY (owner, id)
);
CREATE INDEX IF NOT EXISTS bills_by_time ON bills (owner, occurred_at DESC, id);
CREATE INDEX IF NOT EXISTS bills_by_book ON bills (owner, book_id, occurred_at DESC);
`)
	if err != nil {
		_ = db.Close()
		return nil, databaseError()
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Account(ctx context.Context) (Account, error) {
	var a Account
	err := s.db.QueryRowContext(ctx, `SELECT revision, base_url, identifier, password_md5, uid, token, device, auto_login FROM account WHERE id=1`).Scan(
		&a.Revision, &a.BaseURL, &a.Identifier, &a.PasswordMD5, &a.UID, &a.Token, &a.Device, &a.AutoLogin,
	)
	if err != nil {
		return Account{}, databaseError()
	}
	return a, nil
}

func (s *Store) SaveAccount(ctx context.Context, a Account) (Account, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE account SET revision=revision+1, base_url=?, identifier=?, password_md5=?, uid=?, token=?, device=?, auto_login=? WHERE id=1 AND revision=?`,
		a.BaseURL, a.Identifier, a.PasswordMD5, a.UID, a.Token, a.Device, a.AutoLogin, a.Revision)
	if err != nil {
		return Account{}, databaseError()
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return Account{}, conflict()
	}
	a.Revision++
	return a, nil
}

func (s *Store) Logout(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account SET revision=revision+1, password_md5='', token='', auto_login=0 WHERE id=1`)
	if err != nil {
		return databaseError()
	}
	return nil
}

// Reserve timestamps atomically across CLI processes sharing this database.
func (s *Store) NextTimestamp(ctx context.Context, route string) (int64, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO request_clock (route, epoch_ms) VALUES (?, ?)
ON CONFLICT(route) DO UPDATE SET epoch_ms=MAX(excluded.epoch_ms, request_clock.epoch_ms+1)
RETURNING epoch_ms`, route, time.Now().UnixMilli()).Scan(&ms)
	if err != nil {
		return 0, databaseError()
	}
	return ms, nil
}

func databaseError() error {
	return fault.New("DATABASE_ERROR", "无法访问本地 SQLite 数据库", 1)
}
func conflict() error {
	return fault.New("STATE_CHANGED", "本地会话或缓存已被另一进程更新，请重新执行", 1)
}

func noRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
