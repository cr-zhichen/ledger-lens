package service

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type BudgetInput struct {
	Book    string
	Year    int
	Month   int
	Daily   bool
	Offline bool
}
type budgetCategory struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Limit     Money             `json:"limit"`
	Used      Money             `json:"used"`
	Remaining Money             `json:"remaining"`
	Count     int               `json:"count"`
	Children  []*budgetCategory `json:"children,omitempty"`
}

func (s *Service) Budget(ctx context.Context, input BudgetInput) (map[string]any, error) {
	if input.Book == "" {
		input.Book = "-1"
	}
	if !qianji.ValidID(input.Book, true) || input.Year < 1 || input.Year > 9999 || input.Month < 0 || input.Month > 12 {
		return nil, fault.Invalid("请提供有效的账本与月份或年份")
	}
	r := &readContext{service: s, offline: input.Offline}
	base, err := r.baseCurrency(ctx)
	if err != nil {
		return nil, err
	}
	startDay := 1
	periodRange := ""
	books, err := s.reportCatalog(ctx, "books", input.Offline, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Books(ctx, a, true) })
	if err != nil {
		return nil, err
	}
	bookItems, err := catalogItems(books)
	if err != nil {
		return nil, err
	}
	for _, book := range bookItems {
		if textValue(book["bookid"]) == input.Book {
			config, _ := book["config"].(map[string]any)
			value := textValue(config["range"])
			periodRange = value
			if strings.HasPrefix(value, "m") {
				startDay, err = strconv.Atoi(strings.TrimPrefix(value, "m"))
				if err != nil || startDay < 1 || startDay > 31 {
					return nil, invalidWriteData()
				}
			}
		}
	}
	start := time.Date(input.Year, 1, 1, 0, 0, 0, 0, shanghai)
	end := start.AddDate(1, 0, 0)
	if input.Month > 0 {
		start = time.Date(input.Year, time.Month(input.Month), startDay, 0, 0, 0, 0, shanghai)
		end = time.Date(input.Year, time.Month(input.Month)+1, startDay, 0, 0, 0, 0, shanghai)
	}
	definitions, err := s.reportCatalog(ctx, budgetKey(input.Book, input.Year, input.Month), input.Offline, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
		return c.Budgets(ctx, a, input.Book, input.Year, input.Month, periodRange)
	})
	if err != nil {
		return nil, err
	}
	items, err := catalogItems(definitions)
	if err != nil {
		return nil, err
	}
	index, err := r.loadCategories(ctx, input.Book)
	if err != nil {
		return nil, err
	}
	var fullLimit Money
	categories := map[string]*budgetCategory{}
	for _, item := range items {
		flag := textValue(item["flag"])
		if flag != "1" && flag != "2" {
			continue
		}
		amount, err := moneyValue(item["money"])
		if err != nil {
			return nil, err
		}
		if flag == "1" {
			fullLimit = amount
			continue
		}
		id := textValue(item["cateid"])
		if amount > 0 && qianji.ValidID(id, false) {
			categories[id] = &budgetCategory{ID: id, Name: textValue(index[id]["name"]), Limit: amount}
		}
	}
	roots := map[string]*budgetCategory{}
	for id, category := range categories {
		parent := textValue(index[id]["parentid"])
		if categories[parent] != nil {
			categories[parent].Children = append(categories[parent].Children, category)
		} else {
			roots[id] = category
		}
	}
	var categoryLimit Money
	for _, root := range roots {
		var children Money
		for _, child := range root.Children {
			children += child.Limit
		}
		root.Limit = max(root.Limit, children)
		categoryLimit += root.Limit
	}
	limit := max(fullLimit, categoryLimit)
	if limit == 0 {
		return map[string]any{"configured": false, "currency": base, "since": start.Unix(), "until": end.Unix(), "categories": []any{}}, nil
	}
	q := BillQuery{Book: input.Book, Since: start.Unix(), Until: end.Unix()}
	matched, all, state, err := s.querySnapshot(ctx, q, r)
	if err != nil {
		return nil, err
	}
	allBills := map[string]map[string]any{}
	for _, record := range all {
		allBills[textValue(record.bill["id"])] = record.bill
	}
	var allSpend, excluded Money
	days := map[string]Money{}
	allExpenses := fullLimit > categoryLimit
	for _, record := range matched {
		bill := record.bill
		if !contains([]string{"0", "2", "3", "5", "10"}, textValue(bill["type"])) {
			continue
		}
		item, err := r.stat(ctx, bill, base, allBills, 2)
		if err != nil {
			return nil, err
		}
		spend := item["total_spend"]
		allSpend += spend
		excluded += item["excluded_from_budget_amount"]
		id := textValue(bill["cateid"])
		parent := textValue(index[id]["parentid"])
		if categories[id] != nil {
			categories[id].Used += spend
			categories[id].Count++
		}
		if parent != "" && parent != "-1" && categories[parent] != nil {
			categories[parent].Used += spend
			categories[parent].Count++
		}
		included := allExpenses || roots[id] != nil || roots[parent] != nil
		if included {
			when, _ := jsonNumberInt(bill["time"])
			day := time.Unix(when, 0).In(shanghai).Format("2006-01-02")
			days[day] += spend
		}
	}
	var used Money
	out := make([]*budgetCategory, 0, len(roots))
	for _, category := range categories {
		category.Remaining = category.Limit - category.Used
		sort.Slice(category.Children, func(i, j int) bool { return category.Children[i].ID < category.Children[j].ID })
	}
	for _, root := range roots {
		used += root.Used
		out = append(out, root)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if allExpenses {
		used = allSpend
	}
	scope := "budgeted_categories"
	if allExpenses {
		scope = "all_expenses"
	}
	summary := map[string]any{"scope": scope, "limit": limit, "used": used, "remaining": limit - used, "excluded_from_budget_amount": excluded, "daily_average_budget": Money(int64(limit) / int64(end.Sub(start).Hours()/24))}
	now := time.Now().In(shanghai)
	if input.Month > 0 && !now.Before(start) && now.Before(end) && limit-used >= 0 {
		remainingDays := int64(end.Sub(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, shanghai)).Hours() / 24)
		summary["remaining_daily_average"] = Money(int64(limit-used) / remainingDays)
	}
	result := map[string]any{"configured": true, "currency": base, "since": start.Unix(), "until": end.Unix(), "synced_at": state.SyncedAt, "summary": summary, "categories": out}
	if input.Daily {
		daily := []map[string]any{}
		var cumulative Money
		for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
			key := day.Format("2006-01-02")
			cumulative += days[key]
			daily = append(daily, map[string]any{"date": key, "spend": days[key], "cumulative": cumulative, "remaining": limit - cumulative})
		}
		result["daily_statistics"] = daily
	}
	return result, nil
}
