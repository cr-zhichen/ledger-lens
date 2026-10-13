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
		{"update", `{"updates":[{"id":"1","patch":{}}]}`},
		{"update", `{"updates":[{"id":"1","patch":{"type":1}}]}`},
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
