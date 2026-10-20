package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type tagFixture struct {
	mu     sync.Mutex
	tags   []map[string]any
	writes int
	mode   string
}

func newTagFixture(t *testing.T) (*tagFixture, *Service) {
	t.Helper()
	f := &tagFixture{tags: []map[string]any{{"id": "t1", "name": "餐饮", "status": 1}, {"id": "t2", "name": "旧项目", "status": 2}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		switch r.URL.Path {
		case "/tag/list":
			status := r.Form.Get("status")
			tags := []map[string]any{}
			for _, tag := range f.tags {
				if status == "-1" || fmt.Sprint(tag["status"]) == status {
					tags = append(tags, tag)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": map[string]any{"list": []any{map[string]any{"id": "g1", "name": "默认", "tags": tags}}}})
		case "/tag/submittag":
			f.writes++
			var fields map[string]string
			json.Unmarshal([]byte(r.Form.Get("v")), &fields)
			if f.mode != "unlisted" {
				f.tags = append(f.tags, map[string]any{"id": "t9", "name": fields["name"], "status": 1})
			}
			if f.mode == "lost-response" {
				w.WriteHeader(502)
				return
			}
			io.WriteString(w, `{"ec":200,"data":{"id":"t9","name":"`+fields["name"]+`","status":1}}`)
		case "/tag/archivetag":
			f.writes++
			for _, tag := range f.tags {
				if tag["id"] == r.Form.Get("did") && f.mode != "no-op" {
					tag["status"] = 2
				}
			}
			io.WriteString(w, `{"ec":200,"data":{}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	_, svc, _ := seeded(t, server.URL)
	return f, svc
}

func TestCreateTagConfirmsAndRefusesDuplicates(t *testing.T) {
	ctx := context.Background()
	f, svc := newTagFixture(t)
	result, err := svc.CreateTag(ctx, CreateTag{Name: " 出差 "})
	if err != nil || result.ID != "t9" || !result.Confirmed {
		t.Fatalf("create: %+v %v", result, err)
	}
	// The cached directory used by bill tag validation includes the new tag.
	if !cachedTagIDs(t, svc)["t9"] {
		t.Fatal("tag cache not refreshed")
	}
	for _, name := range []string{"出差", "旧项目"} {
		_, err = svc.CreateTag(ctx, CreateTag{Name: name})
		if !fault.Is(err, "TAG_EXISTS") {
			t.Fatalf("duplicate %s accepted: %v", name, err)
		}
	}
	missing := "g404"
	if _, err := svc.CreateTag(ctx, CreateTag{Name: "新标签", GroupID: &missing}); !fault.Is(err, "INVALID_ARGUMENT") {
		t.Fatalf("unknown group accepted: %v", err)
	}
	if f.writes != 1 {
		t.Fatalf("refused creates reached the server: %d writes", f.writes)
	}
}

func TestCreateTagUncertainOutcomes(t *testing.T) {
	for mode, code := range map[string]string{"lost-response": "WRITE_UNCERTAIN", "unlisted": "WRITE_CONFIRMATION_FAILED"} {
		f, svc := newTagFixture(t)
		f.mode = mode
		_, err := svc.CreateTag(context.Background(), CreateTag{Name: "出差"})
		if !fault.Is(err, code) {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestArchiveTagOnlyArchivesActiveTags(t *testing.T) {
	ctx := context.Background()
	f, svc := newTagFixture(t)
	if _, err := svc.ArchiveTag(ctx, ArchiveTag{ID: "t2"}); !fault.Is(err, "TAG_NOT_ACTIVE") {
		t.Fatalf("archived tag accepted: %v", err)
	}
	if f.writes != 0 {
		t.Fatal("refused archive reached the server")
	}
	result, err := svc.ArchiveTag(ctx, ArchiveTag{ID: "t1"})
	if err != nil || !result.Confirmed {
		t.Fatalf("archive: %+v %v", result, err)
	}
	f, svc = newTagFixture(t)
	f.mode = "no-op"
	if _, err := svc.ArchiveTag(ctx, ArchiveTag{ID: "t1"}); !fault.Is(err, "WRITE_UNCERTAIN") {
		t.Fatalf("unconfirmed archive reported success: %v", err)
	}
}

func cachedTagIDs(t *testing.T, svc *Service) map[string]bool {
	t.Helper()
	catalog, err := svc.Store.Catalog(context.Background(), svc.Account.Owner(), "tags")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := catalogItems(catalog.Data)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, group := range groups {
		for _, value := range group["tags"].([]any) {
			ids[textValue(value.(map[string]any)["id"])] = true
		}
	}
	return ids
}

func TestAllPagesFollowsEitherPageBase(t *testing.T) {
	page := func(more bool, ids ...string) qianji.ListPage {
		items := []json.RawMessage{}
		for _, id := range ids {
			items = append(items, json.RawMessage(`{"id":`+id+`}`))
		}
		return qianji.ListPage{Fields: map[string]json.RawMessage{"total": json.RawMessage(`3`)}, Items: items, HasMore: more}
	}
	for name, pages := range map[string][]qianji.ListPage{
		"zero-based":          {page(true, "1", "2"), page(false, "9007199254740993")},
		"one-based":           {page(false), page(true, "1", "2"), page(false, "9007199254740993")},
		"zero-aliases-one":    {page(true, "1", "2"), page(true, "1", "2"), page(false, "9007199254740993")},
		"stalled-cursor-ends": {page(true, "1", "2", "9007199254740993"), page(true, "1"), page(true, "2"), page(true, "3")},
	} {
		requested := 0
		raw, err := AllPages(func(n int) (qianji.ListPage, error) {
			requested++
			return pages[n], nil
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var result struct {
			List  []map[string]json.Number `json:"list"`
			Total json.Number              `json:"total"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.List) != 3 || result.List[2]["id"] != "9007199254740993" || result.Total != "3" {
			t.Fatalf("%s: %s", name, raw)
		}
		if name == "stalled-cursor-ends" && requested != 3 {
			t.Fatalf("stalled cursor read %d pages", requested)
		}
	}
	_, err := AllPages(func(n int) (qianji.ListPage, error) { return page(true, strconv.Itoa(n+1)), nil })
	if !fault.Is(err, "LIST_LIMIT") {
		t.Fatalf("endless pages accepted: %v", err)
	}
}
