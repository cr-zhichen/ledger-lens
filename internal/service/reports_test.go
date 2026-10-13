package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

func TestReportsSeparateExclusionsAndRelatedMoney(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	for _, id := range []string{"100", "101", "102", "103", "104", "105", "106"} {
		f.bills[id] = fixtureBill(t, id)
		f.bills[id]["money"] = number(10)
		f.bills[id]["extra"] = map[string]any{}
	}
	f.bills["100"]["extra"] = map[string]any{"rfds": map[string]any{"101": number(2)}}
	f.bills["101"]["type"] = number(20)
	f.bills["101"]["money"] = number(2)
	f.bills["101"]["extra"] = map[string]any{"refundsid": number(100)}
	f.bills["102"]["extra"] = map[string]any{"flag": number(1)}
	f.bills["103"]["extra"] = map[string]any{"flag": number(2)}
	f.bills["104"]["type"] = number(1)
	f.bills["104"]["money"] = number(30)
	f.bills["104"]["cateid"] = number(3)
	f.bills["105"]["type"] = number(5)
	f.bills["105"]["extra"] = map[string]any{"bxs": map[string]any{"106": number(7)}}
	f.bills["106"]["type"] = number(21)
	f.bills["106"]["money"] = number(7)
	f.bills["106"]["extra"] = map[string]any{"bxsid": number(105)}
	if _, err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := svc.Statistics(ctx, StatisticsInput{BillQuery: BillQuery{Book: "-1"}})
	if err != nil {
		t.Fatal(err)
	}
	summary := stats["summary"].(statItem)
	if summary["total_spend"] != 2100 || summary["total_income"] != 3000 || summary["balance"] != 900 || summary["refund_amount"] != 200 || summary["excluded_from_income_expense_amount"] != 1000 {
		t.Fatalf("double counted relations or flags: %+v", summary)
	}
	when, _ := jsonNumberInt(f.bills["100"]["time"])
	date := time.Unix(when, 0).In(shanghai)
	budget, err := svc.Budget(ctx, BudgetInput{Book: "-1", Year: date.Year(), Month: int(date.Month()), Daily: true})
	if err != nil {
		t.Fatal(err)
	}
	b := budget["summary"].(map[string]any)
	if b["used"] != Money(2100) || b["remaining"] != Money(7900) || b["excluded_from_budget_amount"] != Money(1000) {
		t.Fatalf("budget used the wrong flag: %+v", b)
	}
	if _, err := svc.Budget(ctx, BudgetInput{Book: "-1", Year: date.Year(), Month: int(date.Month()), Offline: true}); err != nil {
		t.Fatal(err)
	}
	// New foreign records without historical conversions cannot silently become CNY.
	f.bills["107"] = fixtureBill(t, "107")
	f.bills["107"]["assetid"] = number(9)
	f.bills["107"]["extra"] = map[string]any{"curr": map[string]any{"ss": "USD", "sv": json.Number("10.25"), "ts": "USD", "tv": json.Number("10.25")}}
	if _, err := svc.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Statistics(ctx, StatisticsInput{}); !fault.Is(err, "CURRENCY_AMOUNT_MISSING") {
		t.Fatalf("mixed currency sum accepted: %v", err)
	}
	usd, err := svc.Statistics(ctx, StatisticsInput{BillQuery: BillQuery{Currency: "USD"}})
	if err != nil || usd["summary"].(statItem)["total_spend"] != 1025 {
		t.Fatalf("native statistics: %v %v", usd, err)
	}
}

func TestAdvancedQueryFiltersBeforePagingAndKeepsPreciseIDs(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	for _, id := range []string{"9007199254740993", "9007199254740994", "9007199254740995"} {
		f.bills[id] = fixtureBill(t, id)
		f.bills[id]["extra"] = map[string]any{"tags": []any{"tag-food", "travel"}}
		f.bills[id]["remark"] = "COFFEE"
		f.bills[id]["assetid"] = number(8)
		f.bills[id]["platform"] = number(122)
	}
	f.bills["9007199254740993"]["cateid"] = json.Number("9007199254740993")
	f.bills["9007199254740993"]["money"] = number(30)
	f.bills["9007199254740995"]["remark"] = "unrelated"
	if _, err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	var query BillQuery
	json.Unmarshal([]byte(`{"category_ids":["2"],"tag_ids":["tag-food","travel"],"tag_match":"all","remark_keyword":"coffee","asset_ids":["8"],"source":122,"member_ids":["1"],"min_money":10,"currency":"CNY","sort":"money_desc","limit":1}`), &query)
	result, err := svc.ListBills(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Items) != 1 {
		t.Fatalf("filtered pagination: %+v", result)
	}
	bill, _ := decodeObject(result.Items[0])
	if textValue(bill["id"]) != "9007199254740993" {
		t.Fatalf("wrong precise ID/order: %s", result.Items[0])
	}
	query.Offset = 1
	result, err = svc.ListBills(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	bill, _ = decodeObject(result.Items[0])
	if textValue(bill["id"]) != "9007199254740994" {
		t.Fatal("page repeats first result")
	}
}

func TestCachedCatalogDoesNotReadAndIsAccountIsolated(t *testing.T) {
	_, svc, db := newWriteFixture(t)
	ctx := context.Background()
	if _, err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, db, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	value, err := svc.Catalog(ctx, "books", true, func(*qianji.Client, qianji.Session) (json.RawMessage, error) {
		t.Fatal("offline read used network")
		return nil, nil
	})
	if err != nil || value.Source != "cache" || value.SyncedAt == "" {
		t.Fatalf("offline catalog: %+v %v", value, err)
	}
	a := svc.Account
	a.UID = "different"
	if _, err := db.SaveAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	svc, err = New(ctx, db, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UserInfo(ctx, true); !fault.Is(err, "CACHE_EMPTY") {
		t.Fatalf("another account read cached user: %v", err)
	}
}

func TestReportCurrencyRequiresKnownAssetAndLoadsOnlyNeededCatalog(t *testing.T) {
	_, svc, db := newWriteFixture(t)
	ctx := context.Background()
	if _, err := db.SaveCatalogs(ctx, svc.Account, map[string]json.RawMessage{
		"assets:0": json.RawMessage(`{"list":[{"id":9,"currency":"USD"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	r := &readContext{service: svc, offline: true}
	bill := fixtureBill(t, "100")
	bill["assetid"] = number(9)
	bill["extra"] = map[string]any{}
	if currency, err := r.currency(ctx, bill); err != nil || currency != "USD" {
		t.Fatalf("known active account depended on absent hidden catalog: %q %v", currency, err)
	}
	bill["type"], bill["assetid"], bill["fromid"] = number(2), number(-1), number(9)
	if currency, err := r.currency(ctx, bill); err != nil || currency != "USD" {
		t.Fatalf("transfer did not use debit account currency: %q %v", currency, err)
	}
	if _, err := db.SaveCatalogs(ctx, svc.Account, map[string]json.RawMessage{"assets:2": json.RawMessage(`{"list":[]}`)}); err != nil {
		t.Fatal(err)
	}
	bill["fromid"] = number(999)
	if _, err := r.currency(ctx, bill); !fault.Is(err, "ASSET_CURRENCY_MISSING") {
		t.Fatalf("unknown account silently became base currency: %v", err)
	}
}
