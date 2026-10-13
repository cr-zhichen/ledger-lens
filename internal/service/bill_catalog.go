package service

import (
	"context"
	"encoding/json"
	"strings"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

// Each write loads current catalogs once, without persisting a second cache.
type writeCatalog struct {
	service      *Service
	lists        map[string][]map[string]any
	baseCurrency string
}

func newWriteCatalog(s *Service) *writeCatalog {
	return &writeCatalog{service: s, lists: map[string][]map[string]any{}}
}

func (c *writeCatalog) load(ctx context.Context, key string, read func(*qianji.Client, qianji.Session) (json.RawMessage, error)) ([]map[string]any, error) {
	if list, exists := c.lists[key]; exists {
		return list, nil
	}
	data, err := c.service.Read(ctx, func(client *qianji.Client, session qianji.Session) (any, error) { return read(client, session) })
	if err != nil {
		return nil, err
	}
	object, err := decodeObject(data.(json.RawMessage))
	if err != nil {
		return nil, err
	}
	values, ok := object["list"].([]any)
	if !ok {
		return nil, invalidWriteData()
	}
	var list []map[string]any
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, invalidWriteData()
		}
		list = append(list, item)
	}
	c.lists[key] = list
	return list, nil
}

func (c *writeCatalog) validate(ctx context.Context, bill map[string]any) error {
	bookID := textValue(bill["bookid"])
	if bookID != "-1" {
		books, err := c.load(ctx, "books", func(client *qianji.Client, session qianji.Session) (json.RawMessage, error) {
			return client.Books(ctx, session, true)
		})
		if err != nil {
			return err
		}
		book := findCatalog(books, "bookid", bookID)
		if book == nil {
			return missingReference("账本")
		}
		if textValue(book["expired"]) == "1" {
			return fault.New("BOOK_EXPIRED", "账本已到期，请在钱迹中处理后重试", 2)
		}
	}
	kind := "0"
	if textValue(bill["type"]) == "1" {
		kind = "1"
	}
	categories, err := c.load(ctx, "categories:"+bookID, func(client *qianji.Client, session qianji.Session) (json.RawMessage, error) {
		return client.Categories(ctx, session, bookID, -1)
	})
	if err != nil {
		return err
	}
	categoryID := textValue(bill["cateid"])
	found := false
	for _, category := range categories {
		items := []map[string]any{category}
		if category["sublist"] != nil {
			children, ok := category["sublist"].([]any)
			if !ok {
				return invalidWriteData()
			}
			for _, child := range children {
				item, ok := child.(map[string]any)
				if !ok {
					return invalidWriteData()
				}
				items = append(items, item)
			}
		}
		for _, item := range items {
			if textValue(item["id"]) == categoryID && textValue(item["type"]) == kind {
				found = true
			}
		}
	}
	if !found {
		return missingReference("对应收支类型的分类")
	}
	assetID := textValue(bill["assetid"])
	if assetID != "-1" {
		if _, err := c.asset(ctx, assetID); err != nil {
			return err
		}
	}
	extra, err := billExtra(bill)
	if err != nil {
		return err
	}
	if err := validateBaseCurrencyPair(extra); err != nil {
		return err
	}
	if extra["tags"] == nil {
		return nil
	}
	tagIDs, ok := extra["tags"].([]any)
	if !ok {
		return invalidWriteData()
	}
	if len(tagIDs) == 0 {
		return nil
	}
	groups, err := c.load(ctx, "tags", func(client *qianji.Client, session qianji.Session) (json.RawMessage, error) {
		return client.Tags(ctx, session, -1, 0)
	})
	if err != nil {
		return err
	}
	available := map[string]bool{}
	for _, group := range groups {
		tags, ok := group["tags"].([]any)
		if !ok {
			return invalidWriteData()
		}
		for _, tag := range tags {
			item, ok := tag.(map[string]any)
			if !ok {
				return invalidWriteData()
			}
			available[textValue(item["id"])] = true
		}
	}
	for _, id := range tagIDs {
		if !available[textValue(id)] {
			return missingReference("标签")
		}
	}
	return nil
}

func (c *writeCatalog) asset(ctx context.Context, id string) (map[string]any, error) {
	assets, err := c.load(ctx, "assets", func(client *qianji.Client, session qianji.Session) (json.RawMessage, error) {
		return client.Assets(ctx, session, 0)
	})
	if err != nil {
		return nil, err
	}
	if asset := findCatalog(assets, "id", id); asset != nil {
		return asset, nil
	}
	hidden, err := c.load(ctx, "hidden-assets", func(client *qianji.Client, session qianji.Session) (json.RawMessage, error) {
		return client.Assets(ctx, session, 2)
	})
	if err != nil {
		return nil, err
	}
	if asset := findCatalog(hidden, "id", id); asset != nil {
		return asset, nil
	}
	return nil, missingReference("资产")
}

// An asset with no explicit currency uses the account's base currency.
func (c *writeCatalog) assetCurrency(ctx context.Context, id string) (string, error) {
	if id != "-1" {
		asset, err := c.asset(ctx, id)
		if err != nil {
			return "", err
		}
		if asset["currency"] != nil {
			currency, ok := asset["currency"].(string)
			if !ok {
				return "", invalidWriteData()
			}
			if currency = strings.TrimSpace(currency); currency != "" {
				return currency, nil
			}
		}
	}
	if c.baseCurrency == "" {
		data, err := c.service.Read(ctx, func(client *qianji.Client, session qianji.Session) (any, error) {
			return client.BaseCurrency(ctx, session)
		})
		if err != nil {
			return "", err
		}
		c.baseCurrency = data.(string)
	}
	return c.baseCurrency, nil
}

func (c *writeCatalog) setNativeCurrency(ctx context.Context, bill map[string]any, baseAmount *Money) error {
	id := textValue(bill["assetid"])
	if id == "-1" && baseAmount == nil {
		return nil
	}
	currency, err := c.assetCurrency(ctx, id)
	if err != nil {
		return err
	}
	base, err := c.assetCurrency(ctx, "-1")
	if err != nil {
		return err
	}
	if currency == base {
		if baseAmount != nil {
			return fault.Invalid("本位币账户无需折算，请移除 base_amount")
		}
		return nil
	}
	extra, err := billExtra(bill)
	if err != nil {
		return err
	}
	curr, _ := extra["curr"].(map[string]any)
	if curr == nil {
		discount, err := billDiscount(extra)
		if err != nil {
			return err
		}
		var net Money
		if err := net.UnmarshalJSON([]byte(textValue(bill["money"]))); err != nil || net+discount > 999999999999 {
			return invalidWriteData()
		}
		grossJSON, _ := (net + discount).MarshalJSON()
		// Source and target are both the selected asset currency. No rates are
		// requested; base currency and amount are only written together.
		curr = map[string]any{"ss": currency, "sv": json.Number(grossJSON), "ts": currency, "tv": bill["money"]}
		extra["curr"] = curr
	}
	if baseAmount != nil {
		baseJSON, _ := baseAmount.MarshalJSON()
		curr["bs"], curr["bv"] = base, json.Number(baseJSON)
	}
	bill["extra"] = extra
	return nil
}

// Validate the outgoing boundary after patches have replaced or removed any
// old snapshot. Never emit an inherited half-pair on metadata-only updates.
func validateBaseCurrencyPair(extra map[string]any) error {
	if extra["curr"] == nil {
		return nil
	}
	curr, ok := extra["curr"].(map[string]any)
	if !ok {
		return invalidWriteData()
	}
	symbol, hasSymbol := curr["bs"]
	amount, hasAmount := curr["bv"]
	if !hasSymbol && !hasAmount {
		return nil
	}
	var money Money
	currency, validSymbol := symbol.(string)
	if !hasSymbol || !hasAmount || !validSymbol || strings.TrimSpace(currency) == "" || money.UnmarshalJSON([]byte(textValue(amount))) != nil {
		return fault.New("RESPONSE_INVALID", "账单的本位币折算信息不完整或无效，请在钱迹中修正后重试", 4)
	}
	return nil
}

func (c *writeCatalog) validateNativeUpdate(ctx context.Context, bill map[string]any, nextAsset *string) error {
	previousID := textValue(bill["assetid"])
	previous, err := c.assetCurrency(ctx, previousID)
	if err != nil {
		return err
	}
	extra, err := billExtra(bill)
	if err != nil {
		return err
	}
	if extra["curr"] != nil {
		curr, ok := extra["curr"].(map[string]any)
		if !ok || textValue(curr["ss"]) == "" {
			return invalidWriteData()
		}
		if textValue(curr["ss"]) != previous || (curr["ts"] != nil && textValue(curr["ts"]) != previous) {
			return fault.New("CURRENCY_UNSUPPORTED", "账单币种与账户币种不同，请在钱迹中修改金额或账户", 2)
		}
	}
	if nextAsset != nil && *nextAsset != previousID {
		next, err := c.assetCurrency(ctx, *nextAsset)
		if err != nil {
			return err
		}
		if previous != next {
			return fault.New("CURRENCY_UNSUPPORTED", "目标账户币种与原账单不同，请选择同币种账户或在钱迹中修改", 2)
		}
	}
	return nil
}

func findCatalog(items []map[string]any, key, id string) map[string]any {
	for _, item := range items {
		if textValue(item[key]) == id {
			return item
		}
	}
	return nil
}

func missingReference(name string) error {
	return fault.New("REFERENCE_NOT_FOUND", "未找到可用的"+name+"，请查询后重新选择", 2)
}
