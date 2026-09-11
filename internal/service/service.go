package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
)

type Service struct {
	Store     *store.Store
	Account   store.Account
	Timeout   time.Duration
	refreshed bool
}

func New(ctx context.Context, db *store.Store, timeout time.Duration) (*Service, error) {
	a, err := db.Account(ctx)
	if err != nil {
		return nil, err
	}
	return &Service{Store: db, Account: a, Timeout: timeout}, nil
}

func (s *Service) client(ctx context.Context, base string) (*qianji.Client, error) {
	return qianji.New(qianji.Options{BaseURL: base, Timeout: s.Timeout, NextTimestamp: func(route string) (int64, error) { return s.Store.NextTimestamp(ctx, route) }})
}

func (s *Service) Login(ctx context.Context, account, password, digest, base string) error {
	if account == "" {
		account = s.Account.Identifier
	}
	if strings.TrimSpace(account) == "" || (password == "" && digest == "") || (password != "" && digest != "") {
		return fault.Invalid("请提供账号，以及 password 或 password_md5 中的一项")
	}
	if password != "" {
		digest = qianji.PasswordMD5(password)
	}
	if !qianji.ValidMD5(digest) {
		return fault.Invalid("password_md5 必须为 32 位十六进制摘要")
	}
	if base == "" {
		base = s.Account.BaseURL
	}
	client, err := s.client(ctx, base)
	if err != nil {
		return err
	}
	device := s.Account.Device
	if device == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fault.New("INTERNAL_ERROR", "无法生成设备标识", 1)
		}
		device = hex.EncodeToString(random[:])
	}
	session, err := client.Login(ctx, account, digest, device)
	if err != nil {
		return err
	}
	record := store.Account{Revision: s.Account.Revision, BaseURL: client.BaseURL(), Identifier: account, PasswordMD5: strings.ToLower(digest), AutoLogin: true, Session: session}
	return s.saveAccount(ctx, record)
}

func (s *Service) saveAccount(ctx context.Context, record store.Account) error {
	updated, err := s.Store.SaveAccount(ctx, record)
	if err != nil {
		return err
	}
	s.Account = updated
	return nil
}

func (s *Service) Read(ctx context.Context, read func(*qianji.Client, qianji.Session) (any, error)) (any, error) {
	client, err := s.client(ctx, s.Account.BaseURL)
	if err != nil {
		return nil, err
	}
	result, err := read(client, s.Account.Session)
	if !fault.Is(err, "TOKEN_EXPIRED") || s.refreshed || !s.Account.AutoLogin || s.Account.Identifier == "" || s.Account.PasswordMD5 == "" {
		return result, err
	}
	s.refreshed = true
	session, err := client.Login(ctx, s.Account.Identifier, s.Account.PasswordMD5, s.Account.Device)
	if err != nil {
		return nil, err
	}
	if session.UID != s.Account.UID {
		return nil, fault.New("ACCOUNT_MISMATCH", "重新登录返回了不同用户，已停止读取", 3)
	}
	record := s.Account
	record.Session = session
	if err := s.saveAccount(ctx, record); err != nil {
		return nil, err
	}
	return read(client, s.Account.Session)
}

type SyncResult struct {
	Mode  string `json:"mode"`
	Pages int    `json:"pages"`
	store.CacheState
}

func (s *Service) Sync(ctx context.Context, full bool) (SyncResult, error) {
	if s.Account.Token == "" {
		return SyncResult{}, fault.New("AUTH_REQUIRED", "请先登录", 3)
	}
	before, err := s.Store.CacheState(ctx, s.Account.Owner())
	if err != nil {
		return SyncResult{}, err
	}
	full = full || !before.Initialized
	cursor := qianji.Cursor{BookID: "-1"}
	if !full {
		cursor.LastTimes = before.LastTimes
	}
	baseline := cursor.LastTimes
	seen := map[string]bool{cursorKey(cursor): true}
	pages := []qianji.PullPage{}
	var bytes int
	for {
		result, err := s.Read(ctx, func(c *qianji.Client, a qianji.Session) (any, error) { return c.PullBills(ctx, a, cursor) })
		if err != nil {
			return SyncResult{}, err
		}
		page := result.(qianji.PullPage)
		for _, raw := range page.Changes {
			bytes += len(raw)
		}
		for _, raw := range page.Deletes {
			bytes += len(raw)
		}
		for _, raw := range page.Categories {
			bytes += len(raw)
		}
		bytes += len(page.LastTimes) + len(page.NextCursor.LastTimes) + len(page.NextCursor.PageSign)
		if bytes > 256<<20 {
			return SyncResult{}, fault.New("SYNC_LIMIT", "同步数据超过 256 MiB 限制，缓存未改变", 4)
		}
		pages = append(pages, page)
		if !page.HasMore {
			break
		}
		if len(pages) >= 200 {
			return SyncResult{}, fault.New("SYNC_LIMIT", "同步超过 200 页限制，缓存未改变", 4)
		}
		cursor = page.NextCursor
		cursor.LastTimes = baseline
		key := cursorKey(cursor)
		if seen[key] {
			return SyncResult{}, fault.New("SYNC_CURSOR_INVALID", "账单分页游标发生循环，缓存未改变", 4)
		}
		seen[key] = true
	}
	last := pages[len(pages)-1].LastTimes
	if !json.Valid(last) || string(last) == "null" {
		return SyncResult{}, fault.New("SYNC_CURSOR_INVALID", "账单同步缺少最终 lasttimes，缓存未改变", 4)
	}
	state, err := s.Store.ApplySync(ctx, s.Account, before, pages, full)
	if err != nil {
		return SyncResult{}, err
	}
	mode := "incremental"
	if full {
		mode = "full"
	}
	return SyncResult{Mode: mode, Pages: len(pages), CacheState: state}, nil
}

func cursorKey(c qianji.Cursor) string {
	return fmt.Sprintf("%s:%d:%s", c.BookID, c.PageOffset, c.PageSign)
}
