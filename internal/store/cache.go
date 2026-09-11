package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type CacheState struct {
	Revision    int64           `json:"-"`
	LastTimes   json.RawMessage `json:"-"`
	SyncedAt    string          `json:"synced_at"`
	Initialized bool            `json:"initialized"`
}

func (s *Store) CacheState(ctx context.Context, owner string) (CacheState, error) {
	return readCacheState(ctx, s.db, owner)
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readCacheState(ctx context.Context, db rowReader, owner string) (CacheState, error) {
	var state CacheState
	var last string
	err := db.QueryRowContext(ctx, `SELECT revision, lasttimes, updated_at FROM sync_state WHERE owner=?`, owner).Scan(&state.Revision, &last, &state.SyncedAt)
	if noRows(err) {
		return state, nil
	}
	if err != nil {
		return state, databaseError()
	}
	state.Initialized = true
	state.LastTimes = json.RawMessage(last)
	if !json.Valid(state.LastTimes) {
		return CacheState{}, databaseError()
	}
	return state, nil
}

// ApplySync replaces or updates a snapshot only after every remote page has succeeded.
// Session revision and cache revision prevent stale concurrent work from committing.
func (s *Store) ApplySync(ctx context.Context, account Account, before CacheState, pages []qianji.PullPage, full bool) (CacheState, error) {
	if len(pages) == 0 {
		return CacheState{}, fault.New("SYNC_INVALID", "同步未返回数据页", 4)
	}
	last := pages[len(pages)-1].LastTimes
	if !json.Valid(last) || string(last) == "null" {
		return CacheState{}, fault.New("SYNC_INVALID", "同步缺少最终 lasttimes", 4)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CacheState{}, databaseError()
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM account WHERE id=1`).Scan(&revision); err != nil {
		return CacheState{}, databaseError()
	}
	if revision != account.Revision {
		return CacheState{}, conflict()
	}
	var cachedRevision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM sync_state WHERE owner=?`, account.Owner()).Scan(&cachedRevision)
	if err != nil && !noRows(err) {
		return CacheState{}, databaseError()
	}
	if cachedRevision != before.Revision {
		return CacheState{}, conflict()
	}
	if full {
		if _, err = tx.ExecContext(ctx, `DELETE FROM bills WHERE owner=?`, account.Owner()); err != nil {
			return CacheState{}, databaseError()
		}
	}
	for _, page := range pages {
		for _, raw := range page.Changes {
			var bill struct {
				ID   json.RawMessage `json:"id"`
				Book json.RawMessage `json:"bookid"`
				Time json.Number     `json:"time"`
			}
			if json.Unmarshal(raw, &bill) != nil {
				return CacheState{}, invalidBill()
			}
			id, err := qianji.ID(bill.ID, false)
			if err != nil {
				return CacheState{}, invalidBill()
			}
			book := "-1"
			if len(bill.Book) > 0 && string(bill.Book) != "null" {
				book, err = qianji.ID(bill.Book, true)
				if err != nil {
					return CacheState{}, invalidBill()
				}
			}
			occurredAt, err := bill.Time.Int64()
			if err != nil || occurredAt < 0 {
				return CacheState{}, invalidBill()
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO bills (owner,id,book_id,occurred_at,raw) VALUES (?,?,?,?,?)
ON CONFLICT(owner,id) DO UPDATE SET book_id=excluded.book_id, occurred_at=excluded.occurred_at, raw=excluded.raw`, account.Owner(), id, book, occurredAt, string(raw))
			if err != nil {
				return CacheState{}, databaseError()
			}
		}
	}
	// Tombstones take precedence over changes in the same complete sync batch.
	for _, page := range pages {
		for _, raw := range page.Deletes {
			id, err := qianji.ID(raw, false)
			if err != nil {
				return CacheState{}, invalidBill()
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM bills WHERE owner=? AND id=?`, account.Owner(), id); err != nil {
				return CacheState{}, databaseError()
			}
		}
	}
	state := CacheState{Revision: before.Revision + 1, Initialized: true, LastTimes: last, SyncedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_state (owner,revision,lasttimes,updated_at) VALUES (?,?,?,?)
ON CONFLICT(owner) DO UPDATE SET revision=excluded.revision,lasttimes=excluded.lasttimes,updated_at=excluded.updated_at`, account.Owner(), state.Revision, string(last), state.SyncedAt)
	if err != nil {
		return CacheState{}, databaseError()
	}
	if err := tx.Commit(); err != nil {
		return CacheState{}, databaseError()
	}
	return state, nil
}

type Filter struct {
	Book   string
	Since  int64
	Until  int64
	Limit  int
	Offset int
}

type BillList struct {
	Items  []json.RawMessage `json:"items"`
	Total  int64             `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	Source string            `json:"source"`
	CacheState
}

func (f Filter) Validate() error {
	if (f.Book != "" && !qianji.ValidID(f.Book, true)) || f.Limit < 1 || f.Limit > 10000 || f.Offset < 0 || f.Since < 0 || f.Until < 0 || (f.Until > 0 && f.Since >= f.Until) {
		return fault.Invalid("账单筛选无效；limit 范围为 1–10000，日期范围必须递增")
	}
	return nil
}

func (s *Store) ListBills(ctx context.Context, owner string, filter Filter) (BillList, error) {
	if err := filter.Validate(); err != nil {
		return BillList{}, err
	}
	conditions := []string{"owner=?"}
	args := []any{owner}
	if filter.Book != "" {
		conditions = append(conditions, "book_id=?")
		args = append(args, filter.Book)
	}
	if filter.Since > 0 {
		conditions = append(conditions, "occurred_at>=?")
		args = append(args, filter.Since)
	}
	if filter.Until > 0 {
		conditions = append(conditions, "occurred_at<?")
		args = append(args, filter.Until)
	}
	where := strings.Join(conditions, " AND ")
	result := BillList{Items: []json.RawMessage{}, Limit: filter.Limit, Offset: filter.Offset, Source: "cache"}
	// The count and page share a read transaction, so concurrent sync cannot split them.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BillList{}, databaseError()
	}
	defer tx.Rollback()
	state, err := readCacheState(ctx, tx, owner)
	if err != nil {
		return BillList{}, err
	}
	if !state.Initialized {
		return BillList{}, fault.New("CACHE_EMPTY", "账单尚未缓存，请先执行 sync", 5)
	}
	result.CacheState = state
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM bills WHERE "+where, args...).Scan(&result.Total); err != nil {
		return BillList{}, databaseError()
	}
	rows, err := tx.QueryContext(ctx, "SELECT raw FROM bills WHERE "+where+" ORDER BY occurred_at DESC, length(id) DESC, id DESC LIMIT ? OFFSET ?", append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return BillList{}, databaseError()
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return BillList{}, databaseError()
		}
		result.Items = append(result.Items, json.RawMessage(raw))
	}
	if rows.Err() != nil {
		return BillList{}, databaseError()
	}
	if err := rows.Close(); err != nil {
		return BillList{}, databaseError()
	}
	if err := tx.Commit(); err != nil {
		return BillList{}, databaseError()
	}
	return result, nil
}

func (s *Store) GetBill(ctx context.Context, owner, id string) (json.RawMessage, error) {
	if !qianji.ValidID(id, false) {
		return nil, fault.Invalid("账单 ID 必须是正整数")
	}
	state, err := s.CacheState(ctx, owner)
	if err != nil {
		return nil, err
	}
	if !state.Initialized {
		return nil, fault.New("CACHE_EMPTY", "账单尚未缓存，请先执行 sync", 5)
	}
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT raw FROM bills WHERE owner=? AND id=?`, owner, id).Scan(&raw)
	if noRows(err) {
		return nil, fault.New("NOT_FOUND", "本地缓存中未找到该账单", 5)
	}
	if err != nil {
		return nil, databaseError()
	}
	return json.RawMessage(raw), nil
}

func invalidBill() error {
	return fault.New("SYNC_INVALID", "账单 ID、账本或时间字段无效，同步已回滚", 4)
}
