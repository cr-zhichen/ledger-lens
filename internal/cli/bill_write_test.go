package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestBillWriteInputBoundary(t *testing.T) {
	for _, tc := range []struct{ command, input string }{
		{"create", `{"bills":[]}`}, {"create", `{"bills":[{"money":1,"category_id":"2"}]}`},
		{"create", `{"bills":[{"type":0,"money":1.001,"category_id":"2"}]}`},
		{"create", `{"bills":[{"type":0,"money":10000000000,"category_id":"2"}]}`},
		{"create", `{"bills":[{"type":0,"money":"12.34","category_id":"2"}]}`},
		{"create", `{"bills":[{"type":0,"money":1,"category_id":2}]}`},
		{"create", `{"bills":[{"type":0,"money":1,"category_id":"2","exclude_from_income_expense":"true"}]}`},
		{"create", `{"bills":[{"type":0,"money":1,"category_id":"2","exclude_from_budget":1}]}`},
		{"create", `{"bills":[{"type":0,"money":1,"category_id":"2","base_amount":-1}]}`},
		{"create", `{"bills":[{"type":0,"money":1,"category_id":"2","base_amount":10000000000}]}`},
		{"create", `{"bills":[{"type":0,"money":1,"category_id":"2","base_amount":"7.25"}]}`},
		{"update", `{"updates":[{"id":"1","patch":{}}]}`},
		{"update", `{"updates":[{"id":"1","patch":{"type":1}}]}`},
		{"update", `{"updates":[{"id":"1","patch":{"exclude_from_budget":"false"}}]}`},
		{"update", `{"updates":[{"id":"1","patch":{"base_amount":7.001}}]}`},
		{"update", `{"updates":[{"id":"1","patch":{"bv":7}}]}`},
		{"delete", `{"deletions":[{"id":"1"},{"id":"1"}]}`},
		{"delete", `{"deletions":[{"id":"9223372036854775808"}]}`},
		{"delete", `null`}, {"delete", `{"deletions":[]} {}`},
	} {
		t.Run(tc.command+tc.input, func(t *testing.T) {
			code, _, stderr := invoke(t, filepath.Join(t.TempDir(), "db"), tc.input, "bills", tc.command, "--input-stdin")
			if code != 2 || !strings.Contains(stderr, "INVALID_ARGUMENT") {
				t.Fatalf("invalid input accepted: %s", stderr)
			}
		})
	}
}

func TestCLIFlagsAndNativeMoneyRoundTrip(t *testing.T) {
	db := filepath.Join(t.TempDir(), "db")
	var saved json.RawMessage
	var id json.Number
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account/login":
			io.WriteString(w, `{"ec":200,"data":{"user":{"id":1},"token":"fixture"}}`)
		case "/syncv2/pull":
			changes := []json.RawMessage{}
			if saved != nil {
				changes = append(changes, saved)
			}
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"changes": changes, "deletes": []any{}, "bookid": -1, "pageoffset": 0, "pagesign": "", "hasmore": 0, "lasttimes": map[string]int{"v": writes}}})
		case "/category/listv2":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"id":123,"type":0}]}}`)
		case "/asset/list":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"id":9,"currency":"USD"}]}}`)
		case "/client/init":
			io.WriteString(w, `{"ec":200,"data":{"userconfigs":{"basecur":"CNY"}}}`)
		case "/bill/syncall":
			r.ParseForm()
			var payload struct {
				Bills struct {
					Changes []json.RawMessage `json:"changelist"`
				} `json:"bills"`
			}
			if err := json.Unmarshal([]byte(r.Form.Get("v")), &payload); err != nil || len(payload.Bills.Changes) != 1 {
				t.Error("invalid write payload")
				w.WriteHeader(400)
				return
			}
			saved = payload.Bills.Changes[0]
			var bill struct {
				ID json.Number `json:"id"`
			}
			if err := json.Unmarshal(saved, &bill); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			id = bill.ID
			var synced map[string]json.RawMessage
			if err := json.Unmarshal(saved, &synced); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			delete(synced, "fromid")
			delete(synced, "targetid")
			saved, _ = json.Marshal(synced)
			created, updated := []json.Number{}, []json.Number{}
			if writes == 0 {
				created = append(created, id)
			} else {
				updated = append(updated, id)
			}
			writes++
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"sync_result": map[string]any{"bill": map[string]any{"new_ids": created, "update_ids": updated, "del_ids": []any{}, "conf_ids": []any{}, "has_failed": false}}}})
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	requireOK(t, db, `{"account":"fixture","password":"fixture"}`, "login", "--credentials-stdin", "--api-url", server.URL)
	created := requireOK(t, db, `{"bills":[{"type":0,"money":10.25,"base_amount":72.34,"category_id":"123","asset_id":"9","exclude_from_income_expense":true,"exclude_from_budget":true}]}`, "bills", "create", "--input-stdin")
	if !strings.Contains(created, `"bs":"CNY"`) || !strings.Contains(created, `"bv":72.34`) {
		t.Fatalf("CLI did not write supplied conversion: %s", created)
	}
	code, out, stderr := invoke(t, db, `{"updates":[{"id":"`+string(id)+`","patch":{"money":20.01,"exclude_from_income_expense":false}}]}`, "bills", "update", "--input-stdin")
	if code != 0 || !strings.Contains(out, `"confirmed":true`) || !strings.Contains(out, `"flag":2`) || !strings.Contains(out, `"ss":"USD"`) || !strings.Contains(out, `"money":20.01`) || strings.Contains(out, `"bv"`) {
		t.Fatalf("CLI did not preserve native amount and independent switches: %d / %s / %s", code, out, stderr)
	}
	out = requireOK(t, db, `{"updates":[{"id":"`+string(id)+`","patch":{"base_amount":139.12}}]}`, "bills", "update", "--input-stdin")
	if !strings.Contains(out, `"bs":"CNY"`) || !strings.Contains(out, `"bv":139.12`) || !strings.Contains(out, `"money":20.01`) || !strings.Contains(out, `"flag":2`) {
		t.Fatalf("CLI did not patch conversion independently: %s", out)
	}
}

func TestCLIWriteReturnsStructuredUncertainResult(t *testing.T) {
	db := filepath.Join(t.TempDir(), "db")
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account/login":
			io.WriteString(w, `{"ec":200,"data":{"user":{"id":1},"token":"secret"}}`)
		case "/syncv2/pull":
			io.WriteString(w, `{"ec":200,"data":{"changes":[],"deletes":[],"bookid":-1,"pageoffset":0,"pagesign":"","hasmore":0,"lasttimes":{}}}`)
		case "/category/listv2":
			io.WriteString(w, `{"ec":200,"data":{"list":[{"id":123,"type":0}]}}`)
		case "/bill/syncall":
			writes++
			w.WriteHeader(502)
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	requireOK(t, db, `{"account":"fixture","password":"fixture"}`, "login", "--credentials-stdin", "--api-url", server.URL)
	code, _, stderr := invoke(t, db, `{"bills":[{"type":0,"money":18.50,"category_id":"123"}]}`, "bills", "create", "--input-stdin")
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Items []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"items"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatal(err)
	}
	if code != 4 || envelope.Error.Code != "WRITE_UNCERTAIN" || writes != 1 || len(envelope.Error.Details.Items) != 1 || envelope.Error.Details.Items[0].ID == "" || envelope.Error.Details.Items[0].Status != "unknown" || strings.Contains(stderr, "secret") {
		t.Fatalf("bad write error: %d / %s", code, stderr)
	}
}
