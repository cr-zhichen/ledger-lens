package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ledger-lens/internal/fault"
)

type assetFixture struct {
	mu       sync.Mutex
	assets   []map[string]any
	budgets  []map[string]any
	snapshot []map[string]any
	writes   []map[string]string
	mode     string
}

func newAssetFixture(t *testing.T) (*assetFixture, *Service) {
	t.Helper()
	f := &assetFixture{
		assets: []map[string]any{
			{"id": json.Number("9007199254740993"), "name": "现金", "type": 1, "money": json.Number("100.5"), "icon": "cash", "status": 0, "extra": map[string]any{"initmoney": 100.5}},
			{"id": json.Number("7"), "name": "借出", "type": 5, "money": json.Number("10"), "status": 0},
			{"id": json.Number("8"), "name": "信用卡", "type": 2, "money": json.Number("-10"), "status": 0, "installs": []any{map[string]any{"id": 1}}},
		},
		budgets: []map[string]any{
			{"budgetid": 11, "flag": 1, "cateid": 0, "money": json.Number("1000"), "type": 1, "typeinfo": "2026,10", "used": json.Number("30"), "category": map[string]any{"id": 0}},
			{"budgetid": 12, "flag": 2, "cateid": 101, "money": json.Number("300"), "type": 1, "typeinfo": "2026,10", "used": json.Number("30")},
			{"budgetid": 13, "flag": 2, "cateid": 102, "money": json.Number("200"), "type": 1, "typeinfo": "2026,10", "used": json.Number("0")},
		},
		snapshot: []map[string]any{
			{"assetid": json.Number("9007199254740993"), "time": 1790784000, "value": 1},
			{"assetid": json.Number("9007199254740993"), "time": 1790783999, "value": 2},
			{"assetid": json.Number("8"), "time": 1790784000, "value": 3},
		},
	}
	categories := []any{
		map[string]any{"id": 101, "name": "餐饮", "type": 0, "parentid": -1, "sublist": []any{map[string]any{"id": 103, "name": "早餐", "type": 0, "parentid": 101}}},
		map[string]any{"id": 102, "name": "交通", "type": 0, "parentid": -1},
		map[string]any{"id": 104, "name": "购物", "type": 0, "parentid": -1},
		map[string]any{"id": 201, "name": "工资", "type": 1, "parentid": -1},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		reply := func(data any) { json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": data}) }
		switch r.URL.Path {
		case "/asset/list":
			reply(map[string]any{"list": f.assets})
		case "/category/listv2":
			reply(map[string]any{"list": categories})
		case "/book/list":
			reply(map[string]any{"list": []any{map[string]any{"bookid": -1, "name": "日常", "config": map[string]any{"range": "m5"}}}})
		case "/assetline/list":
			reply(map[string]any{"list": f.snapshot})
		case "/budget/list":
			if f.mode == "vip" {
				io.WriteString(w, `{"ec":8888,"em":"{\"msg\":\"请升级会员后再使用此功能\"}","data":{}}`)
				return
			}
			reply(map[string]any{"list": f.budgets})
		case "/asset/submit", "/budget/submitv2":
			f.writes = append(f.writes, map[string]string{"path": r.URL.Path, "v": r.Form.Get("v"), "diffparams": r.Form.Get("diffparams"), "changelist": r.Form.Get("changelist"), "range": r.Form.Get("range"), "flts": r.Form.Get("flts")})
			if r.URL.Path == "/budget/submitv2" {
				var list []map[string]any
				d := json.NewDecoder(strings.NewReader(r.Form.Get("changelist")))
				d.UseNumber()
				d.Decode(&list)
				f.budgets = list
				reply(map[string]any{"list": list})
				return
			}
			var asset map[string]any
			d := json.NewDecoder(strings.NewReader(r.Form.Get("v")))
			d.UseNumber()
			d.Decode(&asset)
			if f.mode != "unapplied" {
				f.assets[0] = asset
			}
			if f.mode == "lost-response" {
				w.WriteHeader(500)
				return
			}
			reply(map[string]any{"asset": asset})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	_, svc, _ := seeded(t, server.URL)
	return f, svc
}

func balance(t *testing.T, value string) *SignedMoney {
	t.Helper()
	var m SignedMoney
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestSetAssetBalanceSendsWholeAccount(t *testing.T) {
	f, svc := newAssetFixture(t)
	result, err := svc.SetAssetBalance(context.Background(), SetAssetBalance{AssetID: "9007199254740993", Balance: balance(t, "-20.5")})
	if err != nil || !result.Confirmed || result.PreviousBalance != 10050 || result.Balance != -2050 {
		t.Fatalf("set balance: %+v %v", result, err)
	}
	if len(f.writes) != 1 || f.writes[0]["diffparams"] != "" {
		t.Fatalf("unexpected writes: %v", f.writes)
	}
	var sent map[string]any
	d := json.NewDecoder(strings.NewReader(f.writes[0]["v"]))
	d.UseNumber()
	d.Decode(&sent)
	if sent["id"] != json.Number("9007199254740993") || sent["money"] != json.Number("-20.50") || sent["icon"] != "cash" || sent["extra"] == nil {
		t.Fatalf("asset not sent whole: %s", f.writes[0]["v"])
	}
}

func TestSetAssetBalanceRefusals(t *testing.T) {
	ctx := context.Background()
	f, svc := newAssetFixture(t)
	income, expense := "201", "101"
	for name, tc := range map[string]struct {
		input SetAssetBalance
		code  string
	}{
		"loan":             {SetAssetBalance{AssetID: "7", Balance: balance(t, "1")}, "ASSET_UNSUPPORTED"},
		"installments":     {SetAssetBalance{AssetID: "8", Balance: balance(t, "1")}, "ASSET_UNSUPPORTED"},
		"unchanged":        {SetAssetBalance{AssetID: "9007199254740993", Balance: balance(t, "100.50")}, "INVALID_ARGUMENT"},
		"missing":          {SetAssetBalance{AssetID: "99", Balance: balance(t, "1")}, "REFERENCE_NOT_FOUND"},
		"diff-only-fields": {SetAssetBalance{AssetID: "9007199254740993", Balance: balance(t, "1"), CategoryID: &expense}, "INVALID_ARGUMENT"},
		// A drop records an expense; upstream fails with HTTP 500 for an income category.
		"direction": {SetAssetBalance{AssetID: "9007199254740993", Balance: balance(t, "1"), RecordDiff: true, CategoryID: &income}, "INVALID_ARGUMENT"},
	} {
		if _, err := svc.SetAssetBalance(ctx, tc.input); !fault.Is(err, tc.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(f.writes) != 0 {
		t.Fatalf("refused calibrations reached the server: %v", f.writes)
	}
}

func TestSetAssetBalanceUncertainOutcomes(t *testing.T) {
	for mode, code := range map[string]string{"lost-response": "WRITE_UNCERTAIN", "unapplied": "WRITE_CONFIRMATION_FAILED"} {
		f, svc := newAssetFixture(t)
		f.mode = mode
		_, err := svc.SetAssetBalance(context.Background(), SetAssetBalance{AssetID: "9007199254740993", Balance: balance(t, "1")})
		if !fault.Is(err, code) {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestAssetTrendFiltersSnapshots(t *testing.T) {
	_, svc := newAssetFixture(t)
	result, err := svc.AssetTrend(context.Background(), false, "9007199254740993", 2026, 10)
	if err != nil {
		t.Fatal(err)
	}
	list := result["list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["value"] != json.Number("1") {
		t.Fatalf("snapshots: %v", list)
	}
	all, err := svc.AssetTrend(context.Background(), true, "", 0, 0)
	if err != nil || all["total"] != 3 {
		t.Fatalf("offline full list: %v %v", all, err)
	}
}

func money(t *testing.T, value string) *Money {
	t.Helper()
	var m Money
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestSetBudgetsSubmitsWholePeriod(t *testing.T) {
	f, svc := newAssetFixture(t)
	result, err := svc.SetBudgets(context.Background(), SetBudgets{Month: "2026-10", Categories: []BudgetCategory{
		{CategoryID: "102", Money: money(t, "0")},
		{CategoryID: "104", Money: money(t, "900")},
		{CategoryID: "103", Money: money(t, "50")},
	}})
	if err != nil || !result.Confirmed {
		t.Fatalf("set budgets: %+v %v", result, err)
	}
	// The total rises to the root category sum 300+900; the sub-category
	// budget sits under its budgeted parent and does not count.
	if result.Total != 120000 || len(result.Categories) != 3 {
		t.Fatalf("result: %+v", result)
	}
	write := f.writes[0]
	if write["range"] != "m5" || write["flts"] != `{"month":"2026,10"}` {
		t.Fatalf("period: %v", write)
	}
	var sent []map[string]any
	json.Unmarshal([]byte(write["changelist"]), &sent)
	for _, item := range sent {
		if item["category"] != nil || item["cateid"] == float64(102) {
			t.Fatalf("unexpected budget item: %v", item)
		}
		if item["cateid"] == float64(104) && (item["flag"] != float64(2) || item["typeinfo"] != "2026,10" || item["type"] != float64(1) || item["userid"] != "1") {
			t.Fatalf("new budget item: %v", item)
		}
	}
	cached, err := svc.Store.Catalog(context.Background(), svc.Account.Owner(), budgetKey("-1", 2026, 10))
	if err != nil || string(cached.Data) == "" {
		t.Fatalf("budget cache: %v", err)
	}
}

func TestSetBudgetsRefusals(t *testing.T) {
	ctx := context.Background()
	f, svc := newAssetFixture(t)
	for name, tc := range map[string]struct {
		input SetBudgets
		code  string
	}{
		"below-categories": {SetBudgets{Month: "2026-10", Total: money(t, "400")}, "INVALID_ARGUMENT"},
		"income-category":  {SetBudgets{Year: 2026, Categories: []BudgetCategory{{CategoryID: "201", Money: money(t, "1")}}}, "INVALID_ARGUMENT"},
		"unknown-category": {SetBudgets{Month: "2026-10", Categories: []BudgetCategory{{CategoryID: "999", Money: money(t, "1")}}}, "REFERENCE_NOT_FOUND"},
		"both-periods":     {SetBudgets{Month: "2026-10", Year: 2026, Total: money(t, "1")}, "INVALID_ARGUMENT"},
	} {
		if _, err := svc.SetBudgets(ctx, tc.input); !fault.Is(err, tc.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	f.budgets = []map[string]any{{"flag": 2, "cateid": 101, "money": json.Number("300")}}
	if _, err := svc.SetBudgets(ctx, SetBudgets{Month: "2026-10", Categories: []BudgetCategory{{CategoryID: "101", Money: money(t, "0")}}}); !fault.Is(err, "INVALID_ARGUMENT") {
		t.Fatalf("emptying the period accepted: %v", err)
	}
	f.mode = "vip"
	if _, err := svc.SetBudgets(ctx, SetBudgets{Month: "2026-10", Total: money(t, "5000")}); !fault.Is(err, "VIP_REQUIRED") {
		t.Fatalf("non-member: %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("refused budgets reached the server: %v", f.writes)
	}
}
