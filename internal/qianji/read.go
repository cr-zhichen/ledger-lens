package qianji

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"ledger-lens/internal/fault"
)

func ValidID(value string, allowDefault bool) bool {
	if allowDefault && value == "-1" {
		return true
	}
	if value == "" || value[0] == '0' {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	return err == nil && id > 0
}

func ID(raw json.RawMessage, allowDefault bool) (string, error) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "\"") && json.Unmarshal(raw, &s) != nil {
		return "", invalidResponse()
	}
	if !ValidID(s, allowDefault) {
		return "", invalidResponse()
	}
	return s, nil
}

func (c *Client) list(ctx context.Context, path string, s Session, form url.Values) (json.RawMessage, error) {
	data, err := c.post(ctx, path, s, form)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil || !strings.HasPrefix(strings.TrimSpace(string(object["list"])), "[") {
		return nil, invalidResponse()
	}
	return data, nil
}

func (c *Client) Books(ctx context.Context, s Session, includeHidden bool) (json.RawMessage, error) {
	t := "1"
	if includeHidden {
		t = "-1"
	}
	return c.list(ctx, "/book/list", s, url.Values{"t": {t}})
}

func (c *Client) Members(ctx context.Context, s Session, book string) (json.RawMessage, error) {
	if !ValidID(book, true) {
		return nil, fault.Invalid("账本 ID 必须是正整数或 -1")
	}
	return c.list(ctx, "/book/members", s, url.Values{"bookid": {book}})
}

func (c *Client) Assets(ctx context.Context, s Session, status int) (json.RawMessage, error) {
	if status != 0 && status != 2 {
		return nil, fault.Invalid("资产状态必须为 0（正常）或 2（隐藏）")
	}
	return c.list(ctx, "/asset/list", s, url.Values{"status": {strconv.Itoa(status)}})
}

func (c *Client) Debts(ctx context.Context, s Session, direction, status int) (json.RawMessage, error) {
	if (direction != 51 && direction != 52) || (status != 0 && status != 1) {
		return nil, fault.Invalid("借贷方向必须为 51 或 52，状态必须为 0 或 1")
	}
	return c.list(ctx, "/asset/listloan", s, url.Values{"t": {strconv.Itoa(direction)}, "status": {strconv.Itoa(status)}})
}

func (c *Client) Categories(ctx context.Context, s Session, book string, kind int) (json.RawMessage, error) {
	if !ValidID(book, true) || kind < -1 || kind > 1 {
		return nil, fault.Invalid("分类需要有效账本 ID，类型必须为 -1、0 或 1")
	}
	return c.list(ctx, "/category/listv2", s, url.Values{"bookid": {book}, "t": {strconv.Itoa(kind)}})
}

func (c *Client) Tags(ctx context.Context, s Session, status int, lastTime int64) (json.RawMessage, error) {
	if (status != -1 && status != 1 && status != 2) || lastTime < 0 {
		return nil, fault.Invalid("标签状态必须为 -1、1 或 2，lasttime 必须非负")
	}
	return c.list(ctx, "/tag/list", s, url.Values{"status": {strconv.Itoa(status)}, "lasttime": {strconv.FormatInt(lastTime, 10)}})
}

func (c *Client) Currencies(ctx context.Context, s Session) (json.RawMessage, error) {
	return c.list(ctx, "/currency/listv2", s, url.Values{})
}

// Month is 0 for a yearly budget, otherwise 1–12.
func (c *Client) Budgets(ctx context.Context, s Session, book string, year, month int, periodRange string) (json.RawMessage, error) {
	if !ValidID(book, true) || year < 1 || year > 9999 || month < 0 || month > 12 {
		return nil, fault.Invalid("预算需要有效账本、年份及月份")
	}
	form := url.Values{"bookid": {book}, "flts": {dateFilter(year, month)}}
	if periodRange != "" {
		form.Set("range", periodRange)
	}
	return c.list(ctx, "/budget/list", s, form)
}

// dateFilter is the app's DateFilter encoding shared by budgets and asset trends.
func dateFilter(year, month int) string {
	filter := map[string]string{"year": strconv.Itoa(year)}
	if month > 0 {
		filter = map[string]string{"month": strconv.Itoa(year) + "," + strconv.Itoa(month)}
	}
	encoded, _ := json.Marshal(filter)
	return string(encoded)
}

func (c *Client) object(ctx context.Context, path string, s Session, form url.Values) (json.RawMessage, error) {
	data, err := c.post(ctx, path, s, form)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, invalidResponse()
	}
	return data, nil
}

// ListPage is one page of a paged directory. Fields keeps every top-level
// field of the response with its original numeric text.
type ListPage struct {
	Fields  map[string]json.RawMessage
	Items   []json.RawMessage
	HasMore bool
}

func (c *Client) page(ctx context.Context, path string, s Session, page int, form url.Values) (ListPage, error) {
	if page < 0 {
		return ListPage{}, fault.Invalid("页码必须非负")
	}
	form.Set("page", strconv.Itoa(page))
	data, err := c.object(ctx, path, s, form)
	if err != nil {
		return ListPage{}, err
	}
	var result ListPage
	_ = json.Unmarshal(data, &result.Fields)
	// An empty page may omit the list or send null.
	if raw := strings.TrimSpace(string(result.Fields["list"])); raw != "" && raw != "null" && json.Unmarshal(result.Fields["list"], &result.Items) != nil {
		return ListPage{}, invalidResponse()
	}
	switch strings.TrimSpace(string(result.Fields["hasmore"])) {
	case "", "null", "0", "false":
	case "1", "true":
		result.HasMore = true
	default:
		return ListPage{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) Installments(ctx context.Context, s Session, page int) (ListPage, error) {
	return c.page(ctx, "/installment/list", s, page, url.Values{})
}

func (c *Client) RepeatTasks(ctx context.Context, s Session, page int) (ListPage, error) {
	return c.page(ctx, "/repeat_task/list", s, page, url.Values{})
}

func (c *Client) Savings(ctx context.Context, s Session) (json.RawMessage, error) {
	return c.object(ctx, "/saving/list", s, url.Values{})
}

func (c *Client) AssetGroups(ctx context.Context, s Session) (json.RawMessage, error) {
	return c.object(ctx, "/asset_group/list", s, url.Values{})
}

func (c *Client) Asset(ctx context.Context, s Session, id string) (json.RawMessage, error) {
	if !ValidID(id, false) {
		return nil, fault.Invalid("资产 ID 必须是正整数")
	}
	return c.object(ctx, "/asset/info", s, url.Values{"did": {id}})
}

// Month is 0 for a yearly trend, otherwise 1–12.
func (c *Client) AssetTrend(ctx context.Context, s Session, id string, year, month int) (json.RawMessage, error) {
	if !ValidID(id, false) || year < 1 || year > 9999 || month < 0 || month > 12 {
		return nil, fault.Invalid("资产走势需要有效资产 ID、年份及月份")
	}
	return c.object(ctx, "/assetline/list", s, url.Values{"did": {id}, "flts": {dateFilter(year, month)}})
}

type Cursor struct {
	BookID     string          `json:"bookid"`
	PageOffset int64           `json:"pageoffset"`
	PageSign   string          `json:"pagesign"`
	LastTimes  json.RawMessage `json:"lasttimes,omitempty"`
}

func (cursor Cursor) Validate() error {
	if !ValidID(cursor.BookID, true) || cursor.PageOffset < 0 || (cursor.PageOffset > 0 && cursor.PageSign == "") || len(cursor.PageSign) > 4096 {
		return fault.Invalid("账单游标需要有效账本、非负偏移量及续页签名")
	}
	if len(cursor.LastTimes) > 0 && !json.Valid(cursor.LastTimes) {
		return fault.Invalid("lasttimes 必须是有效 JSON")
	}
	return nil
}

type PullPage struct {
	Changes    []json.RawMessage `json:"changes"`
	Deletes    []json.RawMessage `json:"deletes"`
	Categories []json.RawMessage `json:"categories"`
	HasMore    bool              `json:"has_more"`
	NextCursor Cursor            `json:"next_cursor"`
	LastTimes  json.RawMessage   `json:"lasttimes,omitempty"`
}

// PullBills returns one change page, not a snapshot of all current bills.
func (c *Client) PullBills(ctx context.Context, s Session, cursor Cursor) (PullPage, error) {
	if err := cursor.Validate(); err != nil {
		return PullPage{}, err
	}
	form := url.Values{"bookid": {cursor.BookID}, "pageoffset": {strconv.FormatInt(cursor.PageOffset, 10)}, "pagesign": {cursor.PageSign}}
	if len(cursor.LastTimes) > 0 {
		form.Set("lasttimes", string(cursor.LastTimes))
	}
	data, err := c.post(ctx, "/syncv2/pull", s, form)
	if err != nil {
		return PullPage{}, err
	}
	var raw struct {
		Changes    []json.RawMessage `json:"changes"`
		Deletes    []json.RawMessage `json:"deletes"`
		Categories []json.RawMessage `json:"categories"`
		BookID     json.RawMessage   `json:"bookid"`
		PageOffset *int64            `json:"pageoffset"`
		PageSign   *string           `json:"pagesign"`
		HasMore    *int              `json:"hasmore"`
		LastTimes  json.RawMessage   `json:"lasttimes"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Changes == nil || raw.Deletes == nil || raw.PageOffset == nil || raw.PageSign == nil || raw.HasMore == nil || (*raw.HasMore != 0 && *raw.HasMore != 1) {
		return PullPage{}, invalidResponse()
	}
	book, err := ID(raw.BookID, true)
	if err != nil {
		return PullPage{}, invalidResponse()
	}
	next := Cursor{BookID: book, PageOffset: *raw.PageOffset, PageSign: *raw.PageSign, LastTimes: raw.LastTimes}
	if next.PageOffset < 0 || len(next.PageSign) > 4096 {
		return PullPage{}, invalidResponse()
	}
	if *raw.HasMore == 1 && (next.PageSign == "" || (next.BookID == cursor.BookID && next.PageOffset == cursor.PageOffset && next.PageSign == cursor.PageSign)) {
		return PullPage{}, invalidResponse()
	}
	for _, bill := range raw.Changes {
		var object map[string]json.RawMessage
		if json.Unmarshal(bill, &object) != nil || object == nil {
			return PullPage{}, invalidResponse()
		}
		if _, err := ID(object["id"], false); err != nil {
			return PullPage{}, err
		}
	}
	for _, id := range raw.Deletes {
		if _, err := ID(id, false); err != nil {
			return PullPage{}, err
		}
	}
	if raw.Categories == nil {
		raw.Categories = []json.RawMessage{}
	}
	// The sync baseline stays fixed across pages, even if intermediate lasttimes differ.
	if *raw.HasMore == 1 {
		next.LastTimes = cursor.LastTimes
	}
	return PullPage{Changes: raw.Changes, Deletes: raw.Deletes, Categories: raw.Categories, HasMore: *raw.HasMore == 1, NextCursor: next, LastTimes: raw.LastTimes}, nil
}
