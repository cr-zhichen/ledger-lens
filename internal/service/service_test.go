package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
)

func seeded(t *testing.T, base string) (*store.Store, *Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, err := db.SaveAccount(context.Background(), store.Account{BaseURL: base, Identifier: "fixture", PasswordMD5: "5f4dcc3b5aa765d61d8327deb882cf99", AutoLogin: true, Session: qianji.Session{UID: "1", Token: "expired-token", Device: "fixture-device"}})
	if err != nil {
		t.Fatal(err)
	}
	return db, &Service{Store: db, Account: a, Timeout: time.Second}, path
}

func readBooks(ctx context.Context, s *Service) (any, error) {
	return s.Read(ctx, func(c *qianji.Client, a qianji.Session) (any, error) { return c.Books(ctx, a, false) })
}

func TestBoundedReauthentication(t *testing.T) {
	cases := []struct {
		name                       string
		loginUID, loginReply, want string
		reads, logins              int32
	}{
		{"expired", "1", "", "", 2, 1},
		{"expired_again", "1", "", "TOKEN_EXPIRED", 2, 1},
		{"different_user", "2", "", "ACCOUNT_MISMATCH", 1, 1},
		{"rejected_login", "1", `{"ec":401,"em":"wrong password"}`, "LOGIN_REJECTED", 1, 1},
		{"forbidden", "1", "", "HTTP_ERROR", 1, 0},
		{"signature", "1", "", "SIGNATURE_REJECTED", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reads, logins atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/account/login" {
					logins.Add(1)
					r.ParseForm()
					if r.Form.Get("pwd") != "5f4dcc3b5aa765d61d8327deb882cf99" || r.Header.Get("devid") != "fixture-device" {
						t.Error("reauthentication lost saved credentials/device")
					}
					if tc.loginReply != "" {
						io.WriteString(w, tc.loginReply)
					} else {
						io.WriteString(w, `{"ec":200,"data":{"user":{"id":`+tc.loginUID+`},"token":"new-token"}}`)
					}
					return
				}
				n := reads.Add(1)
				if tc.name == "forbidden" {
					w.WriteHeader(403)
					return
				}
				if tc.name == "signature" {
					io.WriteString(w, `{"ec":500,"em":"签名错误"}`)
					return
				}
				if n == 1 || tc.name == "expired_again" {
					io.WriteString(w, `{"ec":401,"em":"utoken expired"}`)
					return
				}
				if r.Header.Get("utoken") != "new-token" {
					t.Error("retry did not use new token")
				}
				io.WriteString(w, `{"ec":200,"data":{"list":[]}}`)
			}))
			defer server.Close()
			db, svc, _ := seeded(t, server.URL)
			_, err := readBooks(context.Background(), svc)
			if (tc.want == "" && err != nil) || (tc.want != "" && !fault.Is(err, tc.want)) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			if reads.Load() != tc.reads || logins.Load() != tc.logins {
				t.Fatalf("unbounded or missing retry: reads=%d logins=%d", reads.Load(), logins.Load())
			}
			a, err := db.Account(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (tc.name == "different_user" || tc.name == "rejected_login") && (a.UID != "1" || a.Token != "expired-token") {
				t.Fatal("rejected refresh replaced local account")
			}
		})
	}
}

func TestLogoutWinsAgainstInFlightRefresh(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/login" {
			close(entered)
			<-release
			io.WriteString(w, `{"ec":200,"data":{"user":{"id":1},"token":"new-token"}}`)
		} else {
			io.WriteString(w, `{"ec":401,"em":"utoken expired"}`)
		}
	}))
	defer server.Close()
	db, svc, path := seeded(t, server.URL)
	done := make(chan error, 1)
	go func() { _, err := readBooks(context.Background(), svc); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("refresh did not start")
	}
	other, err := store.Open(path)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := other.Logout(context.Background()); err != nil {
		close(release)
		t.Fatal(err)
	}
	other.Close()
	close(release)
	if err := <-done; !fault.Is(err, "STATE_CHANGED") {
		t.Fatalf("refresh resurrected logged-out session: %v", err)
	}
	a, err := db.Account(context.Background())
	if err != nil || a.Token != "" || a.PasswordMD5 != "" || a.AutoLogin {
		t.Fatal("logout was overwritten")
	}
}

func TestIncrementalBaselineAndCursorLoop(t *testing.T) {
	var calls atomic.Int32
	var loop atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		calls.Add(1)
		if r.Form.Get("lasttimes") != `{"v":100}` {
			t.Errorf("advanced the baseline during pagination: %s", r.Form.Get("lasttimes"))
		}
		if loop.Load() || r.Form.Get("pageoffset") == "0" {
			io.WriteString(w, `{"ec":200,"data":{"changes":[],"deletes":[],"bookid":42,"pageoffset":1,"pagesign":"next","hasmore":1,"lasttimes":{"v":150}}}`)
		} else {
			io.WriteString(w, `{"ec":200,"data":{"changes":[],"deletes":[],"bookid":42,"pageoffset":1,"pagesign":"","hasmore":0,"lasttimes":{"v":200}}}`)
		}
	}))
	defer server.Close()
	db, svc, _ := seeded(t, server.URL)
	before, err := db.ApplySync(context.Background(), svc.Account, store.CacheState{}, []qianji.PullPage{{LastTimes: json.RawMessage(`{"v":100}`)}}, true)
	if err != nil {
		t.Fatal(err)
	}
	loop.Store(true)
	_, err = svc.Sync(context.Background(), false)
	if err == nil || calls.Load() != 2 {
		t.Fatalf("repeated cursor did not stop: %v, calls %d", err, calls.Load())
	}
	after, err := db.CacheState(context.Background(), svc.Account.Owner())
	if err != nil || after.Revision != before.Revision || string(after.LastTimes) != `{"v":100}` {
		t.Fatal("cursor loop advanced the cache")
	}
	loop.Store(false)
	result, err := svc.Sync(context.Background(), false)
	if err != nil || result.Mode != "incremental" || result.Pages != 2 {
		t.Fatalf("incremental pagination failed: %+v %v", result, err)
	}
	state, err := db.CacheState(context.Background(), svc.Account.Owner())
	if err != nil || string(state.LastTimes) != `{"v":200}` {
		t.Fatal("final checkpoint not committed")
	}
}
