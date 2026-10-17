package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"ledger-lens/internal/fault"
)

func (f *writeFixture) serveRelated(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Header.Get("htoken") != "" {
		t.Error("special write must not use htoken")
	}
	if r.URL.Path == "/baoxiao/upgradev2" {
		f.upgrades++
		f.needsUpgrade = false
		io.WriteString(w, `{"ec":200,"data":1}`)
		return
	}
	if r.URL.Path == "/baoxiao/baoxiao" {
		f.reimburseAttempts++
		if f.needsUpgrade {
			io.WriteString(w, `{"ec":500,"em":"报销需要升级"}`)
			return
		}
	}
	f.writes++
	f.wire = r.Form.Get("v")
	if r.URL.Path == "/baoxiao/cancelbaoxiao" {
		var ids []json.Number
		if json.Unmarshal([]byte(f.wire), &ids) != nil {
			t.Fatal("bad cancel IDs")
		}
		for _, id := range ids {
			source := f.bills[string(id)]
			extra, _ := billExtra(source)
			rels, _ := relationship(extra, "bxs")
			for child := range rels {
				delete(f.bills, child)
				f.deleted = append(f.deleted, json.Number(child))
			}
			extra["bxs"] = map[string]any{}
		}
		io.WriteString(w, `{"ec":200,"data":1}`)
		return
	}
	payload, err := decodeObject([]byte(f.wire))
	if err != nil {
		t.Error(err)
		w.WriteHeader(400)
		return
	}
	changed := []map[string]any{}
	makeChild := func(sourceID, childID, kind string, money any, asset any, when any, remark any, tags any, curr any) {
		source := f.bills[sourceID]
		if source == nil {
			t.Fatal("missing source")
		}
		if childID == "" {
			childID = strconv.FormatInt(9007199254740000+int64(len(f.bills))+int64(f.writes)*100, 10)
		}
		key, sourceKey := "rfds", "refundsid"
		if kind == "21" {
			key, sourceKey = "bxs", "bxsid"
		}
		sourceExtra, _ := billExtra(source)
		if sourceExtra[key] == nil {
			sourceExtra[key] = map[string]any{}
		}
		sourceExtra[key].(map[string]any)[childID] = money
		source["extra"] = sourceExtra
		if asset == nil {
			asset = number(-1)
		}
		if remark == nil {
			remark = ""
		}
		if tags == nil {
			tags = []any{}
		}
		extra := map[string]any{sourceKey: json.Number(sourceID), "tags": tags}
		if curr != nil {
			extra["curr"] = curr
		}
		child := map[string]any{"id": json.Number(childID), "userid": "1", "bookid": source["bookid"], "cateid": source["cateid"], "assetid": asset, "type": json.Number(kind), "money": money, "time": when, "createtime": when, "remark": remark, "extra": extra}
		if f.mode == "wrong-relation" {
			extra[sourceKey] = number(999)
		}
		f.bills[childID] = child
		changed = append(changed, source, child)
	}
	if r.URL.Path == "/bill/refund2" {
		var curr any
		if raw, ok := payload["currency"].(string); ok {
			curr, err = decodeObject([]byte(raw))
			if err != nil {
				t.Error(err)
			}
		}
		makeChild(r.Form.Get("did"), textValue(payload["billid"]), "20", payload["money"], payload["assetid"], payload["time"], payload["remark"], payload["tags"], curr)
	} else {
		for sourceID, v := range payload {
			allocation := v.(map[string]any)
			money := allocation["money"]
			curr := allocation["curr"]
			if curr != nil {
				money = curr.(map[string]any)["sv"]
			}
			if textValue(money) == "0.00" {
				continue
			}
			asset := number(-1)
			if r.Form.Get("did") != "" {
				asset = json.Number(r.Form.Get("did"))
			}
			var tags []any
			if r.Form.Get("tags") != "" {
				json.Unmarshal([]byte(r.Form.Get("tags")), &tags)
			}
			makeChild(sourceID, "", "21", money, asset, json.Number(r.Form.Get("bxtime")), r.Form.Get("remark"), tags, curr)
		}
	}
	if f.mode == "lost-response" {
		w.WriteHeader(502)
		return
	}
	key := "list"
	if r.URL.Path == "/baoxiao/baoxiao" {
		key = "bills"
	}
	json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{key: changed}})
}

func TestTransferFXAndRefundLifecycle(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	var input TransferFields
	json.Unmarshal([]byte(`{"money":10,"fee":1,"from_asset_id":"9","target_asset_id":"8","auto_convert":true}`), &input)
	created, err := svc.CreateTransfer(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"money":9.00`, `"sv":10.00`, `"tv":63.00`, `"bv":63.00`, `"transfee":1.00`} {
		if !strings.Contains(string(created.Items[0].Bill), fragment) {
			t.Fatalf("missing %s: %s", fragment, created.Items[0].Bill)
		}
	}
	var patch TransferFields
	json.Unmarshal([]byte(`{"discount":2,"target_amount":56}`), &patch)
	updated, err := svc.UpdateTransfer(ctx, UpdateTransfer{ID: created.Items[0].ID, Patch: patch})
	if err != nil || !strings.Contains(string(updated.Items[0].Bill), `"money":8.00`) {
		t.Fatalf("transfer patch: %+v %v", updated, err)
	}
	f.bills["100"] = fixtureBill(t, "100")
	var refund CreateRefund
	json.Unmarshal([]byte(`{"source_id":"100","money":3.21,"asset_id":"8","remark":"returned"}`), &refund)
	result, err := svc.CreateRefund(ctx, refund)
	if err != nil {
		t.Fatal(err)
	}
	refundID := result.Items[1].ID
	var update UpdateRefund
	json.Unmarshal([]byte(`{"patch":{"money":1.23,"remark":"adjusted"}}`), &update)
	update.ID = refundID
	result, err = svc.UpdateRefund(ctx, update)
	if err != nil || !strings.Contains(string(result.Items[1].Bill), `"money":1.23`) {
		t.Fatalf("refund update: %+v %v", result, err)
	}
	result, err = svc.DeleteRelated(ctx, refundID, "refund")
	if err != nil || !result.Items[0].Confirmed {
		t.Fatalf("refund delete: %+v %v", result, err)
	}
	if _, exists := f.bills[refundID]; exists {
		t.Fatal("refund was not removed")
	}
	extra, _ := billExtra(f.bills["100"])
	rels, _ := relationship(extra, "rfds")
	if len(rels) != 0 {
		t.Fatal("source still references deleted refund")
	}
}

func TestTransferDiscountKeepsTargetCreditAndFeePatchKeepsMainAmount(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	var input TransferFields
	json.Unmarshal([]byte(`{"money":100,"discount":10,"from_asset_id":"9","target_asset_id":"8","auto_convert":true}`), &input)
	created, err := svc.CreateTransfer(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	bill, _ := decodeObject(created.Items[0].Bill)
	extra, _ := billExtra(bill)
	curr := extra["curr"].(map[string]any)
	if textValue(bill["money"]) != "90.00" || textValue(curr["sv"]) != "100.00" || textValue(curr["tv"]) != "700.00" || textValue(curr["bv"]) != "700.00" {
		t.Fatalf("discount reduced target credit: %s", created.Items[0].Bill)
	}
	// An app-created same-currency transfer stores principal 90 plus fee 10.
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["100"]["type"], f.bills["100"]["money"] = number(2), number(90)
	f.bills["100"]["assetid"], f.bills["100"]["cateid"] = number(-1), number(-1)
	f.bills["100"]["fromid"], f.bills["100"]["targetid"] = number(8), number(88)
	f.bills["100"]["extra"] = map[string]any{"transfee": number(10)}
	if _, err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	minimum := Money(9500)
	list, err := svc.ListBills(ctx, BillQuery{Types: []int{2}, Currency: "CNY", MinMoney: &minimum})
	if err != nil || list.Total != 1 {
		t.Fatalf("main-amount filter lost transfer: %+v %v", list, err)
	}
	stats, err := svc.Statistics(ctx, StatisticsInput{BillQuery: BillQuery{Currency: "CNY"}})
	if err != nil {
		t.Fatal(err)
	}
	summary := stats["summary"].(statItem)
	if summary["transfer_amount"] != 9000 || summary["fee_amount"] != 1000 || summary["total_spend"] != 1000 {
		t.Fatalf("fee was subtracted twice: %+v", summary)
	}
	zero := Money(0)
	updated, err := svc.UpdateTransfer(ctx, UpdateTransfer{ID: "100", Patch: TransferFields{Fee: &zero}})
	if err != nil || !strings.Contains(string(updated.Items[0].Bill), `"money":100.00`) {
		t.Fatalf("clearing fee changed entered main amount: %+v %v", updated, err)
	}
}

func TestReimbursementDistributionMigrationAndCancellation(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	for _, id := range []string{"100", "101"} {
		f.bills[id] = fixtureBill(t, id)
		f.bills[id]["type"] = number(5)
	}
	var input ReimburseInput
	json.Unmarshal([]byte(`{"source_ids":["101","100"],"money":15.01,"asset_id":"9","target_amount":2.15}`), &input)
	f.needsUpgrade = true
	_, err := svc.Reimburse(ctx, input)
	if !fault.Is(err, "REIMBURSEMENT_UPGRADE_REQUIRED") || f.upgrades != 0 {
		t.Fatalf("upgrade was not gated: %v", err)
	}
	input.ConfirmUpgrade = true
	result, err := svc.Reimburse(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if f.upgrades != 1 || f.reimburseAttempts != 3 || len(result.Items) != 4 {
		t.Fatalf("bad upgrade/result: %+v", result)
	}
	var targetTotal Money
	for _, item := range result.Items[2:] {
		bill, _ := decodeObject(item.Bill)
		extra, _ := billExtra(bill)
		curr := extra["curr"].(map[string]any)
		value, _ := moneyValue(curr["tv"])
		targetTotal += value
	}
	if targetTotal != 215 {
		t.Fatalf("rounding changed batch total: %d", targetTotal)
	}
	for id, want := range map[string]Money{"100": 1025, "101": 476} {
		extra, _ := billExtra(f.bills[id])
		rels, _ := relationship(extra, "bxs")
		if relationSum(rels) != want {
			t.Fatalf("wrong allocation %s: %v", id, rels)
		}
	}
	result, err = svc.CancelReimbursements(ctx, []string{"100", "101"})
	if err != nil || len(result.Items) != 2 || len(f.bills) != 2 {
		t.Fatalf("cancellation: %+v %v", result, err)
	}
}

func TestRelatedWriteUncertaintyNeverReplays(t *testing.T) {
	for _, mode := range []string{"lost-response", "wrong-relation"} {
		t.Run(mode, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			f.mode = mode
			f.bills["100"] = fixtureBill(t, "100")
			var input CreateRefund
			json.Unmarshal([]byte(`{"source_id":"100","money":2}`), &input)
			_, err := svc.CreateRefund(context.Background(), input)
			want := "WRITE_UNCERTAIN"
			if mode == "wrong-relation" {
				want = "WRITE_CONFIRMATION_FAILED"
			}
			if !fault.Is(err, want) || f.writes != 1 || len(f.bills) != 2 {
				t.Fatalf("write replay or false confirmation: %v writes=%d", err, f.writes)
			}
		})
	}
}

func TestAutoConversionExplicitAmountsAndOriginalUpdate(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	created, err := svc.CreateBills(ctx, fixtureCreate(t, `[{"type":0,"money":10.01,"currency":"USD","asset_id":"8","category_id":"2","auto_convert":true,"base_amount":69.99}]`))
	if err != nil {
		t.Fatal(err)
	}
	if f.rateReads != 0 || !strings.Contains(string(created.Items[0].Bill), `"money":69.99`) {
		t.Fatalf("explicit amount was overridden: %s", created.Items[0].Bill)
	}
	result, err := svc.UpdateBills(ctx, []UpdateBill{{ID: created.Items[0].ID, Patch: fixturePatch(t, `{"money":20.01,"auto_convert":true}`)}})
	if err != nil || !strings.Contains(string(result.Items[0].Bill), `"money":140.07`) || !strings.Contains(string(result.Items[0].Bill), `"sv":20.01`) || f.rateReads != 1 {
		t.Fatalf("cross-currency update: %+v %v", result, err)
	}
}
