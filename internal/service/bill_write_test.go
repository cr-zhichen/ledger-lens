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
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
)

type writeFixture struct {
	mu           sync.Mutex
	bills        map[string]map[string]any
	deleted      []json.Number
	writes       int
	mode         string
	wire         string
	baseCurrency string
}

func newWriteFixture(t *testing.T) (*writeFixture, *Service, *store.Store) {
	t.Helper()
	f := &writeFixture{bills: map[string]map[string]any{}, deleted: []json.Number{}, baseCurrency: "CNY"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		switch r.URL.Path {
		case "/category/listv2":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"id":2,"type":0,"sublist":[{"id":9007199254740993,"type":0}]},{"id":3,"type":1}]}}`)
		case "/book/list":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"bookid":42}]}}`)
		case "/asset/list":
			if r.Form.Get("status") == "2" {
				io.WriteString(w, `{"ec":200,"data":{"list":[{"id":88,"currency":"CNY"},{"id":89,"currency":"USD"}]}}`)
				return
			}
			io.WriteString(w, `{"ec":200,"data":{"list":[{"id":8,"currency":"CNY"},{"id":9,"currency":"USD"},{"id":10,"currency":"USD"},{"id":11,"currency":"EUR"},{"id":12,"currency":""}]}}`)
		case "/client/init":
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"userconfigs": map[string]any{"basecur": f.baseCurrency}}})
		case "/tag/list":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"tags":[{"id":"tag-food"}]}]}}`)
		case "/syncv2/pull":
			if f.writes > 0 && f.mode == "sync-fail" {
				w.WriteHeader(503)
				return
			}
			bills := []map[string]any{}
			for _, bill := range f.bills {
				bills = append(bills, bill)
			}
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"changes": bills, "deletes": f.deleted, "bookid": -1, "pageoffset": 0, "pagesign": "", "hasmore": 0, "lasttimes": map[string]int{"v": f.writes}}})
		case "/bill/syncall":
			f.writes++
			f.wire = r.Form.Get("v")
			if r.Header.Get("htoken") != "1" || r.Form.Get("uid") != "1" || r.Header.Get("utoken") == "" {
				t.Error("write authentication missing")
			}
			if f.mode == "expired" {
				w.WriteHeader(401)
				return
			}
			object, err := decodeObject([]byte(f.wire))
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			payload := object["bills"].(map[string]any)
			created, updated, deleted, conflicts := []json.Number{}, []json.Number{}, []json.Number{}, []json.Number{}
			if changes, ok := payload["changelist"].([]any); ok {
				for i, value := range changes {
					bill := value.(map[string]any)
					id := textValue(bill["id"])
					if f.mode == "partial" && i == 1 {
						conflicts = append(conflicts, json.Number(id))
						continue
					}
					if _, exists := f.bills[id]; exists {
						updated = append(updated, json.Number(id))
					} else {
						created = append(created, json.Number(id))
					}
					if f.mode != "not-applied" {
						if f.mode == "omit-empty-extra" {
							delete(bill, "extra")
						}
						if f.mode == "drop-flags" {
							delete(bill["extra"].(map[string]any), "flag")
						}
						if f.mode == "wrong-currency" {
							bill["extra"].(map[string]any)["curr"].(map[string]any)["ss"] = "CNY"
						}
						if f.mode == "stale-base-currency" {
							curr := bill["extra"].(map[string]any)["curr"].(map[string]any)
							curr["bs"], curr["bv"] = "CNY", number(70)
						}
						if f.mode == "drop-base-symbol" {
							delete(bill["extra"].(map[string]any)["curr"].(map[string]any), "bs")
						}
						f.bills[id] = bill
					}
				}
			}
			if ids, ok := payload["dellist"].([]any); ok {
				for _, value := range ids {
					id := value.(json.Number)
					delete(f.bills, string(id))
					f.deleted = append(f.deleted, id)
					deleted = append(deleted, id)
				}
			}
			if f.mode == "lost-response" {
				w.WriteHeader(502)
				return
			}
			if f.mode == "malformed" {
				io.WriteString(w, `{"ec":200,"data":{}}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"sync_result": map[string]any{"bill": map[string]any{"new_ids": created, "update_ids": updated, "del_ids": deleted, "conf_ids": conflicts, "has_failed": len(conflicts) > 0}}}})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	db, svc, _ := seeded(t, server.URL)
	return f, svc, db
}

func fixtureCreate(t *testing.T, raw string) []CreateBill {
	t.Helper()
	var inputs []CreateBill
	if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
		t.Fatal(err)
	}
	return inputs
}

func fixturePatch(t *testing.T, raw string) BillFields {
	t.Helper()
	var patch BillFields
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatal(err)
	}
	return patch
}

func fixtureBill(t *testing.T, id string) map[string]any {
	t.Helper()
	bill, err := decodeObject([]byte(`{"id":` + id + `,"userid":"1","bookid":-1,"type":0,"money":10.25,"cateid":2,"assetid":-1,"fromid":-1,"targetid":-1,"time":1770000000,"createtime":1770000000,"remark":"original","extra":{"flag":2},"server_only":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	return bill
}

func TestBillCRUDExactAmountsAndPreservedFields(t *testing.T) {
	f, svc, db := newWriteFixture(t)
	ctx := context.Background()
	result, err := svc.CreateBills(ctx, fixtureCreate(t, `[{"type":0,"money":18.50,"discount":0.20,"category_id":"9007199254740993","asset_id":"8","book_id":"42","tag_ids":["tag-food"],"remark":"9007199254740993","time":1770000000}]`))
	if err != nil {
		t.Fatal(err)
	}
	id := result.Items[0].ID
	if !result.Items[0].Confirmed || !strings.Contains(string(result.Items[0].Bill), `"money":18.30`) || !strings.Contains(f.wire, `"cateid":9007199254740993`) || !strings.Contains(f.wire, `"remark":"9007199254740993"`) {
		t.Fatalf("incorrect creation: %+v / %s", result, f.wire)
	}
	f.bills[id]["server_only"] = json.Number("9223372036854775807")
	f.bills[id]["extra"].(map[string]any)["flag"] = number(2)
	result, err = svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: fixturePatch(t, `{"money":20.01,"remark":"","tag_ids":[],"reimbursable":true}`)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"money":19.81`, `"server_only":9223372036854775807`, `"flag":2`, `"type":5`, `"remark":""`, `"tags":[]`, `"transfee":-0.20`} {
		if !strings.Contains(f.wire, expected) {
			t.Errorf("missing %s in %s", expected, f.wire)
		}
	}
	result, err = svc.DeleteBills(ctx, []DeleteBill{{ID: id}})
	if err != nil || !result.Items[0].Confirmed || !strings.Contains(f.wire, `"dellist":[`+id+`]`) {
		t.Fatalf("delete: %+v %v", result, err)
	}
	if _, err := db.GetBill(ctx, svc.Account.Owner(), id); !fault.Is(err, "NOT_FOUND") {
		t.Fatalf("deleted bill remains: %v", err)
	}
}

func TestWriteOutcomeAndNoReplay(t *testing.T) {
	for _, tc := range []struct {
		mode, code string
		confirmed  bool
	}{
		{"partial", "WRITE_PARTIAL", true}, {"lost-response", "WRITE_UNCERTAIN", false},
		{"malformed", "WRITE_UNCERTAIN", false}, {"sync-fail", "WRITE_CONFIRMATION_FAILED", false},
		{"not-applied", "WRITE_CONFIRMATION_FAILED", false}, {"expired", "TOKEN_EXPIRED", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			f.mode = tc.mode
			result, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[{"type":0,"money":1.01,"category_id":"2"},{"type":1,"money":2.02,"category_id":"3"}]`))
			if !fault.Is(err, tc.code) || f.writes != 1 {
				t.Fatalf("result=%+v error=%v writes=%d", result, err, f.writes)
			}
			if result.Items[0].Confirmed != tc.confirmed {
				t.Fatalf("confirmation: %+v", result)
			}
			if tc.mode != "expired" && fault.Public(err).Details == nil {
				t.Fatal("missing item results on failure")
			}
			if tc.mode == "partial" && (result.Items[0].Status != "applied" || result.Items[1].Status != "conflict") {
				t.Fatalf("partial result: %+v", result)
			}
		})
	}
}

func TestBatchValidationPreventsEveryWrite(t *testing.T) {
	for _, tc := range []struct{ input, code string }{
		{`[{"type":0,"money":1,"category_id":"2"},{"type":0,"money":2,"category_id":"999"}]`, "REFERENCE_NOT_FOUND"},
		{`[{"type":1,"money":1,"category_id":"2"}]`, "REFERENCE_NOT_FOUND"},
		{`[{"type":0,"money":1,"category_id":"2","asset_id":"999"}]`, "REFERENCE_NOT_FOUND"},
		{`[{"type":0,"money":1,"discount":2,"category_id":"2"}]`, "INVALID_ARGUMENT"},
		{`[{"type":1,"money":1,"category_id":"3","reimbursable":true}]`, "INVALID_ARGUMENT"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			_, err := svc.CreateBills(context.Background(), fixtureCreate(t, tc.input))
			if !fault.Is(err, tc.code) || f.writes != 0 {
				t.Fatalf("%v, writes %d", err, f.writes)
			}
		})
	}
}

func TestOwnedBillsAndExplicitCascade(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["101"] = fixtureBill(t, "101")
	f.bills["100"]["extra"] = map[string]any{"rfds": map[string]any{"101": number(1)}}
	f.bills["101"]["type"] = number(20)
	f.bills["101"]["extra"] = map[string]any{"refundsid": number(100)}
	_, err := svc.DeleteBills(context.Background(), []DeleteBill{{ID: "100"}})
	if !fault.Is(err, "RELATED_BILLS_EXIST") || f.writes != 0 {
		t.Fatalf("cascade accepted: %v", err)
	}
	_, err = svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"money":5}`)}})
	if !fault.Is(err, "RELATED_BILLS_EXIST") || f.writes != 0 {
		t.Fatalf("linked financial change accepted: %v", err)
	}
	f.bills["101"]["userid"] = "another-user"
	_, err = svc.DeleteBills(context.Background(), []DeleteBill{{ID: "100", CascadeRelated: true}})
	if !fault.Is(err, "BILL_NOT_OWNED") || f.writes != 0 {
		t.Fatalf("foreign child accepted: %v", err)
	}
	f.bills["101"]["userid"] = "1"
	result, err := svc.DeleteBills(context.Background(), []DeleteBill{{ID: "100", CascadeRelated: true}})
	if err != nil || len(result.Items) != 2 || !result.Items[1].Confirmed || len(f.bills) != 0 {
		t.Fatalf("cascade failed: %+v %v", result, err)
	}
}

func TestReceiptRejectsWrongOperationAndForeignIDs(t *testing.T) {
	for _, receipt := range []qianji.WriteReceipt{
		{Updated: []string{"1"}}, {Created: []string{"1", "2"}}, {Created: []string{"1", "1"}}, {Created: []string{"1"}, Failed: true},
	} {
		result := WriteResult{Items: []WriteItem{{ID: "1", Status: "unknown"}}}
		if applyReceipt(&result, "create", receipt) {
			t.Fatalf("invalid receipt accepted: %+v", receipt)
		}
	}
}

func TestWriteLockExcludesAnotherServiceAndReleases(t *testing.T) {
	f, svc, db := newWriteFixture(t)
	lock, err := db.LockWrites()
	if err != nil {
		t.Fatal(err)
	}
	input := fixtureCreate(t, `[{"type":0,"money":1,"category_id":"2"}]`)
	_, err = svc.CreateBills(context.Background(), input)
	if !fault.Is(err, "WRITE_BUSY") || f.writes != 0 {
		t.Fatalf("concurrent write accepted: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateBills(context.Background(), input); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestWriteConfirmationAcceptsOmittedEmptyFields(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.mode = "omit-empty-extra"
	result, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[{"type":0,"money":1.01,"category_id":"2","tag_ids":[]}]`))
	if err != nil || !result.Items[0].Confirmed {
		t.Fatalf("empty fields caused false failure: %+v %v", result, err)
	}
}

func TestUpdateRemarkOnHiddenAssetPreservesFinancialFields(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["100"]["assetid"] = number(88)
	result, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"remark":"修正备注"}`)}})
	if err != nil || !result.Items[0].Confirmed {
		t.Fatalf("hidden asset blocked update: %+v %v", result, err)
	}
	for _, expected := range []string{`"money":10.25`, `"assetid":88`, `"flag":2`, `"remark":"修正备注"`} {
		if !strings.Contains(f.wire, expected) {
			t.Fatalf("historical bill changed incorrectly: %s", f.wire)
		}
	}
}
