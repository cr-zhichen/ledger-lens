package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
	"ledger-lens/internal/update"
	"ledger-lens/internal/version"
)

func TestBusinessCommandsUseOfflineSnapshotAfterLogout(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	a, err := db.SaveAccount(ctx, store.Account{BaseURL: server.URL, Session: qianji.Session{UID: "1", Token: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ApplySync(ctx, a, store.CacheState{}, []qianji.PullPage{{LastTimes: json.RawMessage(`{"v":1}`), Changes: []json.RawMessage{
		json.RawMessage(`{"id":9007199254740993,"bookid":-1,"time":1712707200,"type":0,"money":12.34,"cateid":2,"assetid":8,"userid":"1","remark":"coffee","extra":{"tags":["food"]}}`),
		json.RawMessage(`{"id":2,"bookid":-1,"time":1712707201,"type":1,"money":20,"cateid":3,"assetid":8,"userid":"1","extra":{}}`),
		json.RawMessage(`{"id":3,"bookid":-1,"time":1712707202,"type":0,"money":8,"cateid":2,"assetid":8,"userid":"1","extra":{"flag":3}}`),
	}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveCatalogs(ctx, a, map[string]json.RawMessage{
		"user":             json.RawMessage(`{"userinfo":{"id":"1","name":"fixture"},"userconfigs":{"basecur":"CNY"}}`),
		"books":            json.RawMessage(`{"list":[{"bookid":-1,"name":"个人账本","visible":1,"config":{}},{"bookid":5,"name":"隐藏账本","visible":0}]}`),
		"assets:0":         json.RawMessage(`{"list":[{"id":8,"name":"现金","currency":"CNY"}]}`),
		"assets:2":         json.RawMessage(`{"list":[]}`),
		"categories:-1":    json.RawMessage(`{"list":[{"id":2,"parentid":-1,"type":0,"name":"餐饮"},{"id":3,"parentid":-1,"type":1,"name":"工资"}]}`),
		"budget:-1:2024:4": json.RawMessage(`{"list":[{"flag":1,"money":100}]}`),
		"installments":     json.RawMessage(`{"list":[{"id":9007199254740995,"name":"手机分期"}],"pages":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checker := &update.Client{HTTP: server.Client(), URL: server.URL, CachePath: filepath.Join(t.TempDir(), "update.json")}
	for _, test := range []struct {
		args    []string
		input   string
		want    []string
		missing string
	}{
		{args: []string{"user", "info"}, want: []string{`"name":"fixture"`, `"base_currency":"CNY"`, `"source":"cache"`}},
		{args: []string{"books", "list", "--name", "账本"}, want: []string{"个人账本"}, missing: "隐藏账本"},
		{args: []string{"bills", "list", "--filter-stdin"}, input: `{"tag_ids":["food"],"remark_keyword":"COFFEE","currency":"CNY","limit":1}`, want: []string{`"total":1`, `9007199254740993`, `"money":12.34`}},
		{args: []string{"bills", "stats", "--since", "2024-04-01", "--until", "2024-05-01"}, want: []string{`"total_spend":12.34`, `"total_income":20.00`, `"balance":7.66`}},
		{args: []string{"budgets", "get", "--month", "2024-04", "--daily"}, want: []string{`"used":12.34`, `"remaining":87.66`, `"date":"2024-04-30"`}},
		{args: []string{"installments", "list"}, want: []string{`9007199254740995`, `"source":"cache"`}},
	} {
		var out, stderr bytes.Buffer
		args := append([]string{"--db", path}, test.args...)
		args = append(args, "--offline")
		code := run(ctx, args, strings.NewReader(test.input), &out, &stderr, version.Info{Version: "1.0.0"}, checker)
		if code != 0 || !json.Valid(out.Bytes()) || stderr.Len() != 0 {
			t.Fatalf("%v: exit %d, %s / %s", test.args, code, &out, &stderr)
		}
		for _, want := range test.want {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v: missing %s in %s", test.args, want, &out)
			}
		}
		if test.missing != "" && strings.Contains(out.String(), test.missing) {
			t.Fatalf("%v: included filtered result: %s", test.args, &out)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("offline commands made %d HTTP requests", requests.Load())
	}
}

func TestBusinessCommandArgumentsRejectBeforeAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	for _, test := range []struct {
		args  []string
		input string
	}{
		{args: []string{"transfers", "create"}},
		{args: []string{"refunds", "delete"}},
		{args: []string{"reimbursements", "cancel"}},
		{args: []string{"bills", "list", "--filter-stdin", "--currency", "USD"}, input: `{}`},
		{args: []string{"bills", "stats", "--offline", "--fresh"}},
		{args: []string{"budgets", "get", "--month", "2024-04", "--year", "2024"}},
		{args: []string{"refunds", "update", "--input-stdin"}, input: `{"id":"1","patch":{"unknown_field":1}}`},
		{args: []string{"reimbursements", "create", "--input-stdin"}, input: `{"source_ids":["1"],"money":1.001}`},
		{args: []string{"tags", "create"}},
		{args: []string{"tags", "create", "--input-stdin"}, input: `{"name":"  "}`},
		{args: []string{"tags", "create", "--input-stdin"}, input: `{"name":"出差","color":"blue"}`},
		{args: []string{"tags", "archive", "--input-stdin"}, input: `{"id":"a b"}`},
		{args: []string{"assets", "get"}},
		{args: []string{"assets", "trend", "--id", "8"}},
	} {
		code, _, stderr := invoke(t, path, test.input, test.args...)
		if code != 2 || !strings.Contains(stderr, "INVALID_ARGUMENT") {
			t.Fatalf("%v: expected argument failure, got %d / %s", test.args, code, stderr)
		}
	}
}
