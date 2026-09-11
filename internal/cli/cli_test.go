package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"ledger-lens/internal/store"
)

func invoke(t *testing.T, db, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(context.Background(), append([]string{"--db", db}, args...), strings.NewReader(input), &out, &stderr)
	if code == 0 && !json.Valid(out.Bytes()) {
		t.Fatalf("success was not JSON: %q", out.String())
	}
	if code != 0 && (out.Len() != 0 || !json.Valid(stderr.Bytes())) {
		t.Fatalf("failure output contract broken: %q / %q", out.String(), stderr.String())
	}
	return code, out.String(), stderr.String()
}

func requireOK(t *testing.T, db, input string, args ...string) string {
	t.Helper()
	code, out, err := invoke(t, db, input, args...)
	if code != 0 {
		t.Fatalf("command %v failed: %s", args, err)
	}
	return out
}

func TestInitIncrementalRollbackAndOffline(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "private", "ledger.sqlite")
	var phase, pulls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/account/login" {
			if r.Form.Get("pwd") != "5f4dcc3b5aa765d61d8327deb882cf99" || r.Form.Get("v") != "fixture@example.test" {
				t.Error("raw password was not converted once")
			}
			io.WriteString(w, `{"ec":200,"data":{"user":{"id":"fixture_user_abc123xyz"},"token":"fixture-secret-token"}}`)
			return
		}
		if r.URL.Path != "/syncv2/pull" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		pulls.Add(1)
		switch phase.Load() {
		case 0:
			if r.Form.Has("lasttimes") {
				t.Error("full pagination advanced sync baseline")
			}
			if r.Form.Get("pageoffset") == "0" {
				io.WriteString(w, `{"ec":200,"data":{"changes":[{"id":9007199254740993,"bookid":42,"time":1789000000,"money":12.340000000000000001},{"id":2,"time":1789000001,"money":5.01}],"deletes":[],"bookid":42,"pageoffset":1,"pagesign":"page-2","hasmore":1,"lasttimes":{"v":100}}}`)
			} else {
				if r.Form.Get("bookid") != "42" || r.Form.Get("pagesign") != "page-2" {
					t.Error("did not follow cross-book cursor")
				}
				io.WriteString(w, `{"ec":200,"data":{"changes":[{"id":3,"bookid":42,"time":1789000002,"money":8.01}],"deletes":[2],"bookid":-1,"pageoffset":0,"pagesign":"","hasmore":0,"lasttimes":{"v":200}}}`)
			}
		case 1:
			if r.Form.Get("lasttimes") != `{"v":200}` {
				t.Errorf("wrong incremental checkpoint: %s", r.Form.Get("lasttimes"))
			}
			io.WriteString(w, `{"ec":200,"data":{"changes":[{"id":9007199254740993,"bookid":42,"time":1789000000,"money":1.111111111111111111}],"deletes":[3],"bookid":-1,"pageoffset":0,"pagesign":"","hasmore":0,"lasttimes":{"v":300}}}`)
		case 2:
			if r.Form.Get("pageoffset") == "0" {
				io.WriteString(w, `{"ec":200,"data":{"changes":[{"id":9007199254740993,"bookid":42,"time":1789000000,"money":999}],"deletes":[],"bookid":42,"pageoffset":1,"pagesign":"fail-next","hasmore":1,"lasttimes":{"v":400}}}`)
			} else {
				w.WriteHeader(500)
			}
		case 3:
			io.WriteString(w, `{"ec":200,"data":{"changes":[{"id":4,"time":1789000000,"money":1},{"id":5,"time":"invalid","money":2}],"deletes":[],"bookid":-1,"pageoffset":0,"pagesign":"","hasmore":0,"lasttimes":{"v":500}}}`)
		}
	}))
	defer server.Close()
	init := requireOK(t, dbPath, `{"account":"fixture@example.test","password":"password"}`, "init", "--credentials-stdin", "--non-interactive", "--api-url", server.URL)
	if pulls.Load() != 2 || !strings.Contains(init, `"mode":"full"`) {
		t.Fatalf("init did not complete full pagination: %s", init)
	}
	if strings.Contains(init, "fixture-secret-token") || strings.Contains(init, "5f4dcc3b") {
		t.Fatal("init exposed credentials")
	}
	list := requireOK(t, dbPath, "", "bills", "list")
	if !strings.Contains(list, `"total":2`) || !strings.Contains(list, "12.340000000000000001") || !strings.Contains(list, "9007199254740993") {
		t.Fatalf("bad initial cache: %s", list)
	}
	phase.Store(1)
	requireOK(t, dbPath, "", "sync")
	list = requireOK(t, dbPath, "", "bills", "list", "--book", "42")
	if !strings.Contains(list, `"total":1`) || !strings.Contains(list, "1.111111111111111111") {
		t.Fatalf("update/delete not applied: %s", list)
	}
	before := cacheState(t, dbPath)
	phase.Store(2)
	code, _, errText := invoke(t, dbPath, "", "bills", "list", "--fresh")
	if code == 0 || !strings.Contains(errText, "HTTP_ERROR") {
		t.Fatalf("fresh silently used stale data: %s", errText)
	}
	code, _, _ = invoke(t, dbPath, "", "sync", "--full")
	if code == 0 {
		t.Fatal("partial full sync succeeded")
	}
	phase.Store(3)
	code, _, errText = invoke(t, dbPath, "", "sync", "--full")
	if code == 0 || !strings.Contains(errText, "SYNC_INVALID") {
		t.Fatalf("invalid bill was committed: %s", errText)
	}
	after := cacheState(t, dbPath)
	if string(before.LastTimes) != string(after.LastTimes) || before.Revision != after.Revision || before.SyncedAt != after.SyncedAt {
		t.Fatal("failed sync advanced cache state")
	}
	list = requireOK(t, dbPath, "", "bills", "list")
	if !strings.Contains(list, `"total":1`) || !strings.Contains(list, "1.111111111111111111") || strings.Contains(list, `"money":999`) {
		t.Fatalf("failed sync changed bills: %s", list)
	}
	requireOK(t, dbPath, "", "logout")
	server.Close()
	requireOK(t, dbPath, "", "bills", "get", "--id", "9007199254740993")
	status := requireOK(t, dbPath, "", "auth", "status")
	if !strings.Contains(status, `"has_session":false`) || !strings.Contains(status, `"auto_login":false`) {
		t.Fatalf("logout failed: %s", status)
	}
	code, _, errText = invoke(t, dbPath, "", "books", "list")
	if code != 3 || !strings.Contains(errText, "AUTH_REQUIRED") {
		t.Fatalf("logout reauthenticated: %s", errText)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.Account(context.Background())
	if err != nil || a.Token != "" || a.PasswordMD5 != "" || a.AutoLogin {
		t.Fatal("logout kept automatic login credentials")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows uses ACLs; os.Chmod only controls its read-only attribute.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("database is not restricted to current user")
	}
}

func cacheState(t *testing.T, path string) store.CacheState {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.CacheState(context.Background(), a.Owner())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestInitFailureKeepsLoginAndExplicitDigest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.sqlite")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/login" {
			r.ParseForm()
			if r.Form.Get("pwd") != "5f4dcc3b5aa765d61d8327deb882cf99" {
				t.Error("explicit MD5 was hashed a second time")
			}
			io.WriteString(w, `{"ec":200,"data":{"user":{"id":1},"token":"fixture-token"}}`)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	code, _, errText := invoke(t, dbPath, `{"account":"fixture","password_md5":"5F4DCC3B5AA765D61D8327DEB882CF99"}`, "init", "--credentials-stdin", "--api-url", server.URL)
	if code == 0 || !strings.Contains(errText, "INIT_SYNC_FAILED") {
		t.Fatalf("wrong init error: %s", errText)
	}
	status := requireOK(t, dbPath, "", "auth", "status")
	if !strings.Contains(status, `"has_session":true`) || !strings.Contains(status, `"initialized":false`) {
		t.Fatalf("failed sync discarded login: %s", status)
	}
	code, _, errText = invoke(t, dbPath, "", "bills", "list")
	if code != 5 || !strings.Contains(errText, "CACHE_EMPTY") {
		t.Fatalf("uncached account looked empty: %s", errText)
	}
}

func TestCLIRejectsWritesAndRedactsFailures(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.sqlite")
	for _, args := range [][]string{
		{"bills", "delete", "--id", "1"},
		{"request", "/bill/syncall"},
		{"login", "--password", "do-not-print", "--unknown"},
		{"login", "--credentials-stdin", "--password", "do-not-print"},
		{"bills", "pull", "--cursor-stdin", "--book", "42"},
	} {
		code, _, errText := invoke(t, dbPath, "", args...)
		if code != 2 || strings.Contains(errText, "do-not-print") {
			t.Fatalf("bad argument rejection: %v -> %s", args, errText)
		}
	}
	for _, input := range []string{`{"account":"fixture","password":"do-not-print","unexpected":true}`, `{"password":"do-not-print"} {}`, `{"password":"a","password_md5":"b"}`} {
		code, _, errText := invoke(t, dbPath, input, "login", "--credentials-stdin")
		if code != 2 || strings.Contains(errText, "do-not-print") {
			t.Fatalf("bad JSON credential rejection: %s", errText)
		}
	}
}

func TestReadOutputRedactionAndPrecision(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.sqlite")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/login" {
			io.WriteString(w, `{"ec":200,"data":{"user":{"id":1},"token":"login-secret"}}`)
			return
		}
		io.WriteString(w, `{"ec":200,"data":{"list":[{"id":9223372036854775807,"money":0.100000000000000001,"nested":{"utoken":"hidden-secret","password_md5":"digest-secret"}}]}}`)
	}))
	defer server.Close()
	requireOK(t, dbPath, "", "login", "--account", "fixture", "--password", "password", "--api-url", server.URL)
	out := requireOK(t, dbPath, "", "books", "list")
	for _, secret := range []string{"login-secret", "hidden-secret", "digest-secret"} {
		if strings.Contains(out, secret) {
			t.Fatal("read response leaked credential")
		}
	}
	if !strings.Contains(out, "9223372036854775807") || !strings.Contains(out, "0.100000000000000001") {
		t.Fatal("CLI lost numeric precision")
	}
}
