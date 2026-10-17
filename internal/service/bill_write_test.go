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
	mu                sync.Mutex
	bills             map[string]map[string]any
	deleted           []json.Number
	writes            int
	mode              string
	wire              string
	baseCurrency      string
	rateReads         int
	needsUpgrade      bool
	upgrades          int
	reimburseAttempts int
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
			io.WriteString(w, `{"ec":200,"data":{"list":[{"id":8,"name":"现金","type":0,"currency":"CNY"},{"id":9,"name":"美元","type":0,"currency":"USD"},{"id":10,"name":"美元信用卡","type":2,"currency":"USD"},{"id":11,"name":"欧元","type":0,"currency":"EUR"},{"id":12,"name":"默认币种","type":0,"currency":""}]}}`)
		case "/client/init":
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"userinfo": map[string]any{"id": 1, "name": "fixture", "viptype": 1, "vipstart": 0, "vipend": 2100000000}, "userconfigs": map[string]any{"basecur": f.baseCurrency, "mcurrency": 1}}})
		case "/currency/listv2":
			f.rateReads++
			io.WriteString(w, `{"ec":200,"data":{"list":[{"symbol":"CNY","baseprice":1},{"symbol":"USD","baseprice":7},{"symbol":"EUR","baseprice":8}]}}`)
		case "/budget/list":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"flag":1,"money":100,"bookid":-1},{"flag":2,"money":60,"cateid":2}]}}`)
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
						// Sync responses omit unset optional IDs instead of echoing
						// every field from the write request.
						for _, key := range []string{"cateid", "assetid", "fromid", "targetid"} {
							if textValue(bill[key]) == "-1" {
								delete(bill, key)
							}
						}
						f.bills[id] = bill
					}
				}
			}
			if ids, ok := payload["dellist"].([]any); ok {
				for _, value := range ids {
					id := value.(json.Number)
					if bill := f.bills[string(id)]; bill != nil {
						extra, _ := billExtra(bill)
						for key, sourceKey := range map[string]string{"rfds": "refundsid", "bxs": "bxsid"} {
							source := f.bills[textValue(extra[sourceKey])]
							if source != nil {
								sourceExtra, _ := billExtra(source)
								if rels, ok := sourceExtra[key].(map[string]any); ok {
									delete(rels, string(id))
								}
							}
						}
					}
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
		case "/bill/refund2", "/baoxiao/baoxiao", "/baoxiao/upgradev2", "/baoxiao/cancelbaoxiao":
			f.serveRelated(t, w, r)
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
	bill, err := decodeObject([]byte(`{"id":` + id + `,"userid":"1","bookid":-1,"type":0,"money":10.25,"cateid":2,"assetid":-1,"time":1770000000,"createtime":1770000000,"remark":"original","extra":{"flag":2},"server_only":9007199254740993}`))
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

func TestUpdateAndDeleteBillsWithOmittedIDs(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
	}{
		{"expense", "0"}, {"income", "1"}, {"reimbursable", "5"},
		{"transfer", "2"}, {"repayment", "3"}, {"unassigned", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, svc, db := newWriteFixture(t)
			ctx := context.Background()
			const id = "9007199254740997"
			bill := fixtureBill(t, id)
			bill["type"], bill["assetid"] = json.Number(tc.kind), number(9)
			if tc.kind == "1" {
				bill["cateid"] = number(3)
			}
			transfer := tc.kind == "2" || tc.kind == "3"
			patch := fixturePatch(t, `{"base_amount":20.48}`)
			if transfer {
				bill["fromid"], bill["targetid"] = number(9), number(10)
				delete(bill, "assetid")
				delete(bill, "cateid")
				patch = fixturePatch(t, `{"remark":"updated"}`)
			} else if tc.name == "unassigned" {
				delete(bill, "assetid")
				delete(bill, "bookid")
				patch = fixturePatch(t, `{"remark":"updated"}`)
			}
			f.bills[id] = bill
			var result WriteResult
			var err error
			if transfer {
				result, err = svc.UpdateTransfer(ctx, UpdateTransfer{ID: id, Patch: TransferFields{BillFields: patch}})
			} else {
				result, err = svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: patch}})
			}
			if err != nil || len(result.Items) != 1 || !result.Items[0].Confirmed {
				t.Fatalf("update rejected omitted IDs: %+v %v", result, err)
			}
			actual, err := decodeObject(result.Items[0].Bill)
			if err != nil {
				t.Fatal(err)
			}
			if textValue(actual["money"]) != "10.25" || textValue(actual["server_only"]) != "9007199254740993" || textValue(actual["id"]) != id {
				t.Fatalf("update changed untouched fields: %s", result.Items[0].Bill)
			}
			if patch.BaseAmount != nil {
				extra, _ := billExtra(actual)
				curr := extra["curr"].(map[string]any)
				if textValue(curr["bv"]) != "20.48" || curr["bs"] != "CNY" || curr["ss"] != "USD" {
					t.Fatalf("wrong conversion: %s", result.Items[0].Bill)
				}
			} else if actual["remark"] != "updated" {
				t.Fatalf("remark not updated: %s", result.Items[0].Bill)
			}
			result, err = svc.DeleteBills(ctx, []DeleteBill{{ID: id}})
			if err != nil || len(result.Items) != 1 || !result.Items[0].Confirmed {
				t.Fatalf("delete rejected omitted IDs: %+v %v", result, err)
			}
			if _, err := db.GetBill(ctx, svc.Account.Owner(), id); !fault.Is(err, "NOT_FOUND") || f.bills[id] != nil {
				t.Fatalf("deleted bill remains: %v", err)
			}
		})
	}
}

func TestInvalidBillIDsPreventWrites(t *testing.T) {
	for _, tc := range []struct {
		name, kind, field string
		value             any
		missing           bool
	}{
		{name: "missing-category", kind: "0", field: "cateid", missing: true},
		{name: "missing-transfer-source", kind: "2", field: "fromid", missing: true},
		{name: "missing-transfer-target", kind: "3", field: "targetid", missing: true},
		{name: "null-optional", kind: "0", field: "fromid", value: nil},
		{name: "empty-optional", kind: "0", field: "targetid", value: ""},
		{name: "object-optional", kind: "2", field: "assetid", value: map[string]any{}},
		{name: "fractional-optional", kind: "0", field: "assetid", value: json.Number("1.5")},
		{name: "negative-optional", kind: "0", field: "fromid", value: number(-2)},
		{name: "overflow-optional", kind: "0", field: "assetid", value: json.Number("9223372036854775808")},
	} {
		for _, operation := range []string{"update", "delete"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				f, svc, _ := newWriteFixture(t)
				bill := fixtureBill(t, "100")
				bill["type"] = json.Number(tc.kind)
				if tc.kind == "2" || tc.kind == "3" {
					bill["fromid"], bill["targetid"] = number(9), number(10)
				}
				if tc.missing {
					delete(bill, tc.field)
				} else {
					bill[tc.field] = tc.value
				}
				f.bills["100"] = bill
				ctx := context.Background()
				var err error
				if operation == "delete" {
					_, err = svc.DeleteBills(ctx, []DeleteBill{{ID: "100"}})
				} else if tc.kind == "2" || tc.kind == "3" {
					_, err = svc.UpdateTransfer(ctx, UpdateTransfer{ID: "100", Patch: TransferFields{BillFields: fixturePatch(t, `{"remark":"updated"}`)}})
				} else {
					_, err = svc.UpdateBills(ctx, []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"remark":"updated"}`)}})
				}
				if !fault.Is(err, "RESPONSE_INVALID") || f.writes != 0 || f.bills["100"]["remark"] != "original" {
					t.Fatalf("invalid ID was accepted: error=%v writes=%d", err, f.writes)
				}
			})
		}
	}
}

func TestBillConfirmationDoesNotIgnoreSetOrInvalidIDs(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		expected    any
		actual      any
		omitActual  bool
	}{
		{"lost-account", "assetid", number(9), nil, true},
		{"unexpected-account", "assetid", nil, number(9), false},
		{"wrong-account", "assetid", number(9), number(10), false},
		{"lost-category", "cateid", number(2), nil, true},
		{"null-optional", "fromid", number(-1), nil, false},
		{"empty-optional", "targetid", number(-1), "", false},
		{"object-optional", "fromid", number(-1), map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := fixtureBill(t, "100")
			actual := cloneBill(expected)
			if tc.expected == nil {
				delete(expected, tc.field)
			} else {
				expected[tc.field] = tc.expected
			}
			if tc.omitActual {
				delete(actual, tc.field)
			} else {
				actual[tc.field] = tc.actual
			}
			if billMatches(actual, expected) {
				t.Fatal("confirmed missing, changed, or malformed ID")
			}
		})
	}
}
