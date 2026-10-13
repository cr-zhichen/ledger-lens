package service

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
)

type BillQuery struct {
	Book                     string   `json:"book_id,omitempty"`
	Since                    int64    `json:"since,omitempty"`
	Until                    int64    `json:"until,omitempty"`
	CreatedSince             int64    `json:"created_since,omitempty"`
	CreatedUntil             int64    `json:"created_until,omitempty"`
	Types                    []int    `json:"types,omitempty"`
	MinMoney                 *Money   `json:"min_money,omitempty"`
	MaxMoney                 *Money   `json:"max_money,omitempty"`
	CategoryIDs              []string `json:"category_ids,omitempty"`
	IncludeSubcategories     *bool    `json:"include_subcategories,omitempty"`
	TagIDs                   []string `json:"tag_ids,omitempty"`
	TagMatch                 string   `json:"tag_match,omitempty"`
	Remark                   string   `json:"remark_keyword,omitempty"`
	AssetIDs                 []string `json:"asset_ids,omitempty"`
	AssetID                  string   `json:"asset_id,omitempty"`
	FromAssetID              string   `json:"from_asset_id,omitempty"`
	TargetAssetID            string   `json:"target_asset_id,omitempty"`
	Source                   *int     `json:"source,omitempty"`
	MemberIDs                []string `json:"member_ids,omitempty"`
	Currency                 string   `json:"currency,omitempty"`
	NoAsset                  bool     `json:"no_asset,omitempty"`
	NoTags                   bool     `json:"no_tags,omitempty"`
	ExcludeFromIncomeExpense *bool    `json:"exclude_from_income_expense,omitempty"`
	ExcludeFromBudget        *bool    `json:"exclude_from_budget,omitempty"`
	Sort                     string   `json:"sort,omitempty"`
	Limit                    int      `json:"limit,omitempty"`
	Offset                   int      `json:"offset,omitempty"`
}

func (q BillQuery) validate() error {
	if q.Limit == 0 {
		q.Limit = 100
	}
	if err := (store.Filter{Book: q.Book, Since: q.Since, Until: q.Until, Limit: q.Limit, Offset: q.Offset}).Validate(); err != nil {
		return err
	}
	if q.CreatedSince < 0 || q.CreatedUntil < 0 || (q.CreatedUntil > 0 && q.CreatedUntil <= q.CreatedSince) {
		return fault.Invalid("创建时间范围必须递增")
	}
	if q.MinMoney != nil && q.MaxMoney != nil && *q.MinMoney > *q.MaxMoney {
		return fault.Invalid("min_money 不能大于 max_money")
	}
	for _, id := range append(append([]string{}, q.CategoryIDs...), q.AssetIDs...) {
		if !qianji.ValidID(id, false) {
			return fault.Invalid("分类和账户筛选需要有效 ID")
		}
	}
	for _, id := range []string{q.AssetID, q.FromAssetID, q.TargetAssetID} {
		if id != "" && !qianji.ValidID(id, false) {
			return fault.Invalid("账户筛选需要有效 ID")
		}
	}
	if q.NoAsset && (len(q.AssetIDs) > 0 || q.AssetID != "" || q.FromAssetID != "" || q.TargetAssetID != "") {
		return fault.Invalid("no_asset 不能同时指定账户")
	}
	if q.NoTags && len(q.TagIDs) > 0 {
		return fault.Invalid("no_tags 不能同时指定标签")
	}
	if q.TagMatch != "" && q.TagMatch != "any" && q.TagMatch != "all" {
		return fault.Invalid("tag_match 请选择 any 或 all")
	}
	if q.Sort != "" && q.Sort != "time_desc" && q.Sort != "time_asc" && q.Sort != "money_desc" && q.Sort != "money_asc" {
		return fault.Invalid("sort 请选择 time_desc、time_asc、money_desc 或 money_asc")
	}
	if q.Currency != "" && !currencySymbol.MatchString(q.Currency) {
		return fault.Invalid("currency 必须是大写币种代码")
	}
	for _, kind := range q.Types {
		if !knownBillType(kind) {
			return fault.Invalid("types 包含未知账单类型")
		}
	}
	if q.Source != nil {
		switch *q.Source {
		case 0, 2, 120, 121, 122:
		default:
			return fault.Invalid("source 请选择 0、2、120、121 或 122")
		}
	}
	for _, values := range [][]string{q.TagIDs, q.MemberIDs} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fault.Invalid("标签和成员 ID 不能为空")
			}
		}
	}
	return nil
}
func knownBillType(n int) bool {
	switch n {
	case 0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 20, 21, 22:
		return true
	}
	return false
}
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

type billRecord struct {
	raw  json.RawMessage
	bill map[string]any
}
type readContext struct {
	service      *Service
	offline      bool
	base         string
	assets       map[string]string
	assetsLoaded [2]bool
	categories   map[string]map[string]any
}

func (r *readContext) baseCurrency(ctx context.Context) (string, error) {
	if r.base != "" {
		return r.base, nil
	}
	value, err := r.service.UserInfo(ctx, r.offline)
	if err != nil {
		return "", err
	}
	user, err := decodeObject(value.Data)
	if err != nil {
		return "", err
	}
	r.base = textValue(user["base_currency"])
	if !currencySymbol.MatchString(r.base) {
		return "", invalidWriteData()
	}
	return r.base, nil
}
func (r *readContext) currency(ctx context.Context, bill map[string]any) (string, error) {
	extra, err := billExtra(bill)
	if err != nil {
		return "", err
	}
	if extra["curr"] != nil {
		curr, ok := extra["curr"].(map[string]any)
		if !ok || !currencySymbol.MatchString(textValue(curr["ss"])) {
			return "", invalidWriteData()
		}
		return textValue(curr["ss"]), nil
	}
	asset := textValue(bill["assetid"])
	if kind := textValue(bill["type"]); kind == "2" || kind == "3" {
		asset = textValue(bill["fromid"])
	}
	if asset != "" && asset != "-1" {
		if r.assets == nil {
			r.assets = map[string]string{}
		}
		for i, status := range []int{0, 2} {
			if currency, exists := r.assets[asset]; exists {
				if currency == "" {
					return r.baseCurrency(ctx)
				}
				return currency, nil
			}
			if !r.assetsLoaded[i] {
				value, err := r.service.reportCatalog(ctx, fmtAssetKey(status), r.offline, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Assets(ctx, a, status) })
				if err != nil {
					return "", err
				}
				items, err := catalogItems(value)
				if err != nil {
					return "", err
				}
				for _, item := range items {
					currency := strings.TrimSpace(textValue(item["currency"]))
					if currency != "" && !currencySymbol.MatchString(currency) {
						return "", invalidWriteData()
					}
					r.assets[textValue(item["id"])] = currency
				}
				r.assetsLoaded[i] = true
			}
		}
		if currency, exists := r.assets[asset]; exists {
			if currency == "" {
				return r.baseCurrency(ctx)
			}
			return currency, nil
		}
		return "", fault.New("ASSET_CURRENCY_MISSING", "账单账户的币种资料缺失，请刷新账户资料后重试", 5)
	}
	return r.baseCurrency(ctx)
}
func fmtAssetKey(status int) string {
	if status == 2 {
		return "assets:2"
	}
	return "assets:0"
}

func (r *readContext) loadCategories(ctx context.Context, book string) (map[string]map[string]any, error) {
	if r.categories == nil {
		r.categories = map[string]map[string]any{}
	}
	if cached, ok := r.categories[book]; ok {
		return categoryIndex(cached["list"].([]any)), nil
	}
	raw, err := r.service.reportCatalog(ctx, "categories:"+book, r.offline, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
		return c.Categories(ctx, a, book, -1)
	})
	if err != nil {
		return nil, err
	}
	object, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	items, ok := object["list"].([]any)
	if !ok {
		return nil, invalidWriteData()
	}
	r.categories[book] = object
	return categoryIndex(items), nil
}
func categoryIndex(items []any) map[string]map[string]any {
	index := map[string]map[string]any{}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		id := textValue(item["id"])
		index[id] = item
		if children, ok := item["sublist"].([]any); ok {
			for childID, child := range categoryIndex(children) {
				copy := cloneBill(child)
				if textValue(copy["parentid"]) == "" || textValue(copy["parentid"]) == "-1" {
					copy["parentid"] = id
				}
				index[childID] = copy
			}
		}
	}
	return index
}

func (s *Service) querySnapshot(ctx context.Context, q BillQuery, r *readContext) ([]billRecord, []billRecord, store.CacheState, error) {
	if err := q.validate(); err != nil {
		return nil, nil, store.CacheState{}, err
	}
	raws, state, err := s.Store.Snapshot(ctx, s.Account.Owner())
	if err != nil {
		return nil, nil, state, err
	}
	all := make([]billRecord, 0, len(raws))
	matched := []billRecord{}
	for _, raw := range raws {
		bill, err := decodeObject(raw)
		if err != nil {
			return nil, nil, state, err
		}
		record := billRecord{raw: raw, bill: bill}
		all = append(all, record)
		ok, err := q.matches(ctx, bill, r)
		if err != nil {
			return nil, nil, state, err
		}
		if ok {
			matched = append(matched, record)
		}
	}
	return matched, all, state, nil
}

func (q BillQuery) matches(ctx context.Context, bill map[string]any, r *readContext) (bool, error) {
	when, err := jsonNumberInt(bill["time"])
	if err != nil {
		return false, err
	}
	if q.Book != "" && textValue(bill["bookid"]) != q.Book || q.Since > 0 && when < q.Since || q.Until > 0 && when >= q.Until {
		return false, nil
	}
	if q.CreatedSince > 0 || q.CreatedUntil > 0 {
		created, err := jsonNumberInt(bill["createtime"])
		if err != nil {
			return false, err
		}
		if created < q.CreatedSince || q.CreatedUntil > 0 && created >= q.CreatedUntil {
			return false, nil
		}
	}
	if len(q.Types) > 0 {
		kind, err := jsonNumberInt(bill["type"])
		if err != nil {
			return false, err
		}
		found := false
		for _, t := range q.Types {
			found = found || int64(t) == kind
		}
		if !found {
			return false, nil
		}
	}
	if q.MinMoney != nil || q.MaxMoney != nil {
		amount, err := billMainAmount(bill)
		if err != nil {
			return false, err
		}
		if q.MinMoney != nil && amount < *q.MinMoney || q.MaxMoney != nil && amount > *q.MaxMoney {
			return false, nil
		}
	}
	if q.Remark != "" && !strings.Contains(strings.ToLower(textValue(bill["remark"])), strings.ToLower(q.Remark)) {
		return false, nil
	}
	for key, want := range map[string]string{"assetid": q.AssetID, "fromid": q.FromAssetID, "targetid": q.TargetAssetID} {
		if want != "" && textValue(bill[key]) != want {
			return false, nil
		}
	}
	assets := []string{textValue(bill["assetid"]), textValue(bill["fromid"]), textValue(bill["targetid"])}
	if len(q.AssetIDs) > 0 {
		found := false
		for _, asset := range assets {
			found = found || contains(q.AssetIDs, asset)
		}
		if !found {
			return false, nil
		}
	}
	if q.NoAsset {
		for _, asset := range assets {
			if asset != "" && asset != "-1" {
				return false, nil
			}
		}
	}
	if len(q.MemberIDs) > 0 && !contains(q.MemberIDs, textValue(bill["userid"])) {
		return false, nil
	}
	extra, err := billExtra(bill)
	if err != nil {
		return false, err
	}
	if q.Source != nil {
		platform := int64(0)
		if bill["platform"] != nil {
			platform, err = jsonNumberInt(bill["platform"])
			if err != nil {
				return false, err
			}
		}
		source := int64(*q.Source)
		matches := platform == source
		if source == 2 {
			matches = platform >= 2 && platform != 120 && platform != 121 && platform != 122
		}
		if !matches {
			return false, nil
		}
	}
	tags := []string{}
	if values, ok := extra["tags"].([]any); ok {
		for _, tag := range values {
			tags = append(tags, textValue(tag))
		}
	}
	if q.NoTags && len(tags) > 0 {
		return false, nil
	}
	if len(q.TagIDs) > 0 {
		count := 0
		for _, tag := range q.TagIDs {
			if contains(tags, tag) {
				count++
			}
		}
		if count == 0 || (q.TagMatch == "all" && count != len(q.TagIDs)) {
			return false, nil
		}
	}
	flag, err := billFlag(extra)
	if err != nil {
		return false, err
	}
	if q.ExcludeFromIncomeExpense != nil && ((flag&1) != 0) != *q.ExcludeFromIncomeExpense || q.ExcludeFromBudget != nil && ((flag&2) != 0) != *q.ExcludeFromBudget {
		return false, nil
	}
	if len(q.CategoryIDs) > 0 && !contains(q.CategoryIDs, textValue(bill["cateid"])) {
		if q.IncludeSubcategories != nil && !*q.IncludeSubcategories {
			return false, nil
		}
		categories, err := r.loadCategories(ctx, textValue(bill["bookid"]))
		if err != nil {
			return false, err
		}
		category := categories[textValue(bill["cateid"])]
		if !contains(q.CategoryIDs, textValue(category["parentid"])) {
			return false, nil
		}
	}
	if q.Currency != "" {
		currency, err := r.currency(ctx, bill)
		if err != nil {
			return false, err
		}
		if currency != q.Currency {
			return false, nil
		}
	}
	return true, nil
}

func billFlag(extra map[string]any) (int64, error) {
	if extra["flag"] == nil {
		return 0, nil
	}
	n, err := jsonNumberInt(extra["flag"])
	if err != nil || n < 0 {
		return 0, invalidWriteData()
	}
	return n, nil
}
func billMainAmount(bill map[string]any) (Money, error) {
	extra, err := billExtra(bill)
	if err != nil {
		return 0, err
	}
	if extra["curr"] != nil {
		curr, ok := extra["curr"].(map[string]any)
		if !ok {
			return 0, invalidWriteData()
		}
		return moneyValue(curr["sv"])
	}
	net, err := moneyValue(bill["money"])
	if err != nil {
		return 0, err
	}
	adjustment, err := signedMoney(extra["transfee"])
	if err != nil {
		return 0, err
	}
	if contains([]string{"0", "2", "3", "5"}, textValue(bill["type"])) {
		return net + absMoney(adjustment), nil
	}
	return net, nil
}

func (s *Service) ListBills(ctx context.Context, q BillQuery) (store.BillList, error) {
	if q.Limit == 0 {
		q.Limit = 100
	}
	r := &readContext{service: s, offline: true}
	matched, _, state, err := s.querySnapshot(ctx, q, r)
	if err != nil {
		return store.BillList{}, err
	}
	amounts := map[string]Money{}
	if strings.HasPrefix(q.Sort, "money_") {
		for _, record := range matched {
			amount, err := billMainAmount(record.bill)
			if err != nil {
				return store.BillList{}, err
			}
			amounts[textValue(record.bill["id"])] = amount
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i].bill, matched[j].bill
		left, right := textValue(a["id"]), textValue(b["id"])
		if strings.HasPrefix(q.Sort, "money_") && amounts[left] != amounts[right] {
			if q.Sort == "money_asc" {
				return amounts[left] < amounts[right]
			}
			return amounts[left] > amounts[right]
		}
		ta, _ := jsonNumberInt(a["time"])
		tb, _ := jsonNumberInt(b["time"])
		if ta != tb {
			if q.Sort == "time_asc" {
				return ta < tb
			}
			return ta > tb
		}
		if len(left) != len(right) {
			return len(left) > len(right)
		}
		return left > right
	})
	result := store.BillList{Items: []json.RawMessage{}, Total: int64(len(matched)), Limit: q.Limit, Offset: q.Offset, Source: "cache", CacheState: state}
	for i := q.Offset; i < len(matched) && i-q.Offset < q.Limit; i++ {
		result.Items = append(result.Items, matched[i].raw)
	}
	return result, nil
}
