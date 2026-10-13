package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
)

// Catalog reads online by default; offline never invokes Read or reauthenticates.
func (s *Service) Catalog(ctx context.Context, name string, offline bool, read func(*qianji.Client, qianji.Session) (json.RawMessage, error)) (store.Catalog, error) {
	if offline {
		return s.Store.Catalog(ctx, s.Account.Owner(), name)
	}
	value, err := s.Read(ctx, func(c *qianji.Client, a qianji.Session) (any, error) { return read(c, a) })
	if err != nil {
		return store.Catalog{}, err
	}
	raw := value.(json.RawMessage)
	at, err := s.Store.SaveCatalogs(ctx, s.Account, map[string]json.RawMessage{name: raw})
	return store.Catalog{Data: raw, SyncedAt: at, Source: "remote"}, err
}

func (s *Service) UserInfo(ctx context.Context, offline bool) (store.Catalog, error) {
	catalog, err := s.Catalog(ctx, "user", offline, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Initialize(ctx, a) })
	if err != nil {
		return catalog, err
	}
	data, err := decodeObject(catalog.Data)
	if err != nil {
		return catalog, err
	}
	user, ok := data["userinfo"].(map[string]any)
	if !ok {
		return catalog, invalidWriteData()
	}
	config, _ := data["userconfigs"].(map[string]any)
	base := textValue(config["basecur"])
	if base == "" {
		base = "CNY"
	}
	result := map[string]any{"id": textValue(user["id"]), "name": user["name"], "avatar": user["avatar"], "registered_at": user["time"], "vip_type": user["viptype"], "vip_start": user["vipstart"], "vip_end": user["vipend"], "base_currency": base, "multi_currency_enabled": sameValue(config["mcurrency"], number(1)) || config["mcurrency"] == true}
	end, _ := jsonNumberInt(user["vipend"])
	start, _ := jsonNumberInt(user["vipstart"])
	now := time.Now().Unix()
	vipType, _ := jsonNumberInt(user["viptype"])
	result["is_vip"] = vipType >= 1 && start <= now && now < end
	catalog.Data, err = json.Marshal(result)
	return catalog, err
}

// Refresh gathers directories before committing them as one snapshot. A failed
// directory request preserves all previous directory entries.
func (s *Service) Refresh(ctx context.Context) (map[string]any, error) {
	syncResult, err := s.Sync(ctx, false)
	if err != nil {
		return nil, err
	}
	entries := map[string]json.RawMessage{}
	read := func(name string, fn func(*qianji.Client, qianji.Session) (json.RawMessage, error)) error {
		value, err := s.Read(ctx, func(c *qianji.Client, a qianji.Session) (any, error) { return fn(c, a) })
		if err == nil {
			entries[name] = value.(json.RawMessage)
		}
		return err
	}
	for _, item := range []struct {
		name string
		fn   func(*qianji.Client, qianji.Session) (json.RawMessage, error)
	}{
		{"user", func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Initialize(ctx, a) }},
		{"books", func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Books(ctx, a, true) }},
		{"assets:0", func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Assets(ctx, a, 0) }},
		{"assets:2", func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Assets(ctx, a, 2) }},
		{"tags", func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Tags(ctx, a, -1, 0) }},
		{"currencies", func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Currencies(ctx, a) }},
	} {
		if err := read(item.name, item.fn); err != nil {
			return nil, err
		}
	}
	books, err := catalogItems(entries["books"])
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{"-1": true}
	for _, book := range books {
		id := textValue(book["bookid"])
		if !qianji.ValidID(id, true) {
			return nil, invalidWriteData()
		}
		ids[id] = true
	}
	for id := range ids {
		if err := read("categories:"+id, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Categories(ctx, a, id, -1) }); err != nil {
			return nil, err
		}
	}
	at, err := s.Store.SaveCatalogs(ctx, s.Account, entries)
	if err != nil {
		return nil, err
	}
	return map[string]any{"sync": syncResult, "catalogs": len(entries), "synced_at": at}, nil
}

func catalogItems(raw json.RawMessage) ([]map[string]any, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	values, ok := object["list"].([]any)
	if !ok {
		return nil, invalidWriteData()
	}
	items := []map[string]any{}
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, invalidWriteData()
		}
		items = append(items, item)
	}
	return items, nil
}

func FilterCatalog(catalog store.Catalog, keyword string) (store.Catalog, error) {
	if keyword == "" {
		return catalog, nil
	}
	data, err := decodeObject(catalog.Data)
	if err != nil {
		return catalog, err
	}
	items, err := catalogItems(catalog.Data)
	if err != nil {
		return catalog, err
	}
	matched := []map[string]any{}
	for _, item := range items {
		if strings.Contains(strings.ToLower(textValue(item["name"])), strings.ToLower(keyword)) {
			matched = append(matched, item)
		}
	}
	data["list"] = matched
	catalog.Data, err = json.Marshal(data)
	return catalog, err
}

func jsonNumberInt(value any) (int64, error) {
	v, ok := value.(json.Number)
	if !ok {
		return 0, invalidWriteData()
	}
	n, err := v.Int64()
	if err != nil {
		return 0, invalidWriteData()
	}
	return n, nil
}

func (s *Service) reportCatalog(ctx context.Context, name string, offline bool, fn func(*qianji.Client, qianji.Session) (json.RawMessage, error)) (json.RawMessage, error) {
	result, err := s.Catalog(ctx, name, offline, fn)
	return result.Data, err
}

func budgetKey(book string, year, month int) string {
	return fmt.Sprintf("budget:%s:%d:%d", book, year, month)
}

func missingConversion() error {
	return fault.New("CURRENCY_AMOUNT_MISSING", "缺少所需币种的历史金额，请指定原币统计，或补充账单折算金额", 2)
}
