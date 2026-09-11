package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

func openFixture(t *testing.T) (*Store, Account, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, err := db.SaveAccount(context.Background(), Account{BaseURL: qianji.DefaultBaseURL, Session: qianji.Session{UID: "1", Token: "fixture", Device: "device"}})
	if err != nil {
		t.Fatal(err)
	}
	return db, a, path
}

func TestDatabaseUsesExactNativePathWithURISpecialCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "账镜 #+%", "ledger.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveAccount(context.Background(), Account{Session: qianji.Session{UID: "path-fixture"}})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatal("SQLite did not use the requested native path")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account, err := db.Account(context.Background())
	if err != nil || account.UID != "path-fixture" {
		t.Fatal("data did not survive reopening the native path")
	}
}

func fixturePage(last string, bills ...string) []qianji.PullPage {
	page := qianji.PullPage{Changes: []json.RawMessage{}, Deletes: []json.RawMessage{}, LastTimes: json.RawMessage(last)}
	for _, bill := range bills {
		page.Changes = append(page.Changes, json.RawMessage(bill))
	}
	return []qianji.PullPage{page}
}

func TestSyncCommitIsAtomicAndRejectsStaleWriters(t *testing.T) {
	ctx := context.Background()
	db, a, _ := openFixture(t)
	state, err := db.ApplySync(ctx, a, CacheState{}, fixturePage(`{"v":1}`, `{"id":1,"time":100,"money":1.01}`), true)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a real storage failure after DELETE and the first INSERT in a full sync.
	_, err = db.db.Exec(`CREATE TRIGGER reject_fixture BEFORE INSERT ON bills WHEN NEW.id='3' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ApplySync(ctx, a, state, fixturePage(`{"v":2}`, `{"id":2,"time":200,"money":2.02}`, `{"id":3,"time":300,"money":3.03}`), true)
	if !fault.Is(err, "DATABASE_ERROR") {
		t.Fatalf("storage failure did not abort: %v", err)
	}
	list, err := db.ListBills(ctx, a.Owner(), Filter{Limit: 100})
	if err != nil || list.Total != 1 || string(list.Items[0]) != `{"id":1,"time":100,"money":1.01}` || list.Revision != state.Revision {
		t.Fatalf("failed transaction changed cache: %+v %v", list, err)
	}
	_, err = db.ApplySync(ctx, a, CacheState{}, fixturePage(`{"v":3}`, `{"id":9,"time":400}`), true)
	if !fault.Is(err, "STATE_CHANGED") {
		t.Fatalf("stale cache writer was accepted: %v", err)
	}
	if err := db.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = db.ApplySync(ctx, a, state, fixturePage(`{"v":4}`), true)
	if !fault.Is(err, "STATE_CHANGED") {
		t.Fatalf("pre-logout sync was committed: %v", err)
	}
}

func TestCacheIsolatesAccountsAndFiltersExactIDs(t *testing.T) {
	ctx := context.Background()
	db, a, _ := openFixture(t)
	_, err := db.ApplySync(ctx, a, CacheState{}, fixturePage(`{"v":1}`,
		`{"id":9007199254740993,"bookid":42,"time":100,"money":0.100000000000000001}`,
		`{"id":9223372036854775807,"bookid":42,"time":100,"money":20.12}`,
		`{"id":8,"bookid":43,"time":200,"money":3}`), true)
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.ListBills(ctx, a.Owner(), Filter{Book: "42", Since: 100, Until: 200, Limit: 1, Offset: 1})
	if err != nil || list.Total != 2 || len(list.Items) != 1 || string(list.Items[0]) != `{"id":9007199254740993,"bookid":42,"time":100,"money":0.100000000000000001}` {
		t.Fatalf("filter/order/precision failure: %+v %v", list, err)
	}
	old := a
	a.UID = "2"
	a, err = db.SaveAccount(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ListBills(ctx, a.Owner(), Filter{Limit: 100})
	if !fault.Is(err, "CACHE_EMPTY") {
		t.Fatalf("new account inherited previous cache: %v", err)
	}
	_, err = db.ApplySync(ctx, a, CacheState{}, fixturePage(`{"v":1}`, `{"id":9007199254740993,"time":500,"money":7}`), true)
	if err != nil {
		t.Fatal(err)
	}
	bill, err := db.GetBill(ctx, old.Owner(), "9007199254740993")
	if err != nil || string(bill) != `{"id":9007199254740993,"bookid":42,"time":100,"money":0.100000000000000001}` {
		t.Fatal("account switch overwrote another account's bill")
	}
}

func TestTombstoneWinsAcrossPages(t *testing.T) {
	ctx := context.Background()
	db, a, _ := openFixture(t)
	pages := fixturePage(`{"v":1}`, `{"id":1,"time":100}`)
	pages[0].Deletes = []json.RawMessage{json.RawMessage("1")}
	pages = append(pages, fixturePage(`{"v":2}`, `{"id":1,"time":101}`)...)
	_, err := db.ApplySync(ctx, a, CacheState{}, pages, true)
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.ListBills(ctx, a.Owner(), Filter{Limit: 100})
	if err != nil || list.Total != 0 || !list.Initialized {
		t.Fatalf("tombstone resurrected or empty cache not initialized: %+v %v", list, err)
	}
}

func TestRequestClockAcrossConnections(t *testing.T) {
	db, _, path := openFixture(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	const n = 40
	values := make(chan int64, n)
	errors := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := db
			if i%2 == 0 {
				s = other
			}
			value, err := s.NextTimestamp(context.Background(), "/book/list")
			if err != nil {
				errors <- err
			} else {
				values <- value
			}
		}(i)
	}
	wg.Wait()
	close(values)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	seen := map[int64]bool{}
	for value := range values {
		if seen[value] {
			t.Error("duplicate request timestamp across processes")
		}
		seen[value] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique timestamps, want %d", len(seen), n)
	}
}
