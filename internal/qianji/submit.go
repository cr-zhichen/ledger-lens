package qianji

import (
	"context"
	"encoding/json"
	"net/url"

	"ledger-lens/internal/fault"
)

// SubmitAsset sends an AssetAccount object; diff, when present, asks the
// server to record the balance difference as a bill (DiffParams.toJson()).
func (c *Client) SubmitAsset(ctx context.Context, s Session, asset map[string]any, diff map[string]any) (json.RawMessage, error) {
	if !ValidID(textID(asset["id"]), false) {
		return nil, fault.Invalid("资产 ID 无效")
	}
	raw, err := json.Marshal(asset)
	if err != nil {
		return nil, err
	}
	form := url.Values{"v": {string(raw)}}
	if diff != nil {
		encoded, err := json.Marshal(diff)
		if err != nil {
			return nil, err
		}
		form.Set("diffparams", string(encoded))
	}
	data, err := c.object(ctx, "/asset/submit", s, form)
	if err != nil {
		return nil, err
	}
	var result struct {
		Asset map[string]json.RawMessage `json:"asset"`
	}
	if json.Unmarshal(data, &result) != nil || result.Asset == nil {
		return nil, invalidResponse()
	}
	return data, nil
}

// SubmitBudgets replaces the budget set of one period. The app always sends
// the complete set (Budget.toAPIJson() of each item) and omits changelist only
// when the set is empty, which callers must not do.
func (c *Client) SubmitBudgets(ctx context.Context, s Session, book string, year, month int, periodRange string, budgets []map[string]any) (json.RawMessage, error) {
	if !ValidID(book, true) || year < 1 || year > 9999 || month < 0 || month > 12 || len(budgets) == 0 {
		return nil, fault.Invalid("预算提交需要有效账本、期间和至少一项预算")
	}
	raw, err := json.Marshal(budgets)
	if err != nil {
		return nil, err
	}
	form := url.Values{"bookid": {book}, "flts": {dateFilter(year, month)}, "changelist": {string(raw)}}
	if periodRange != "" {
		form.Set("range", periodRange)
	}
	return c.list(ctx, "/budget/submitv2", s, form)
}

func textID(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return string(v)
	}
	return ""
}
