package service

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type SetBudgets struct {
	BookID     string           `json:"book_id,omitempty"`
	Month      string           `json:"month,omitempty"`
	Year       int              `json:"year,omitempty"`
	Total      *Money           `json:"total,omitempty"`
	Categories []BudgetCategory `json:"categories,omitempty"`
}

// BudgetCategory with money 0 removes that category's budget.
type BudgetCategory struct {
	CategoryID string `json:"category_id"`
	Money      *Money `json:"money"`
}

type BudgetSetResult struct {
	BookID    string `json:"book_id"`
	Year      int    `json:"year"`
	Month     int    `json:"month,omitempty"`
	Status    string `json:"status"`
	Confirmed bool   `json:"confirmed"`
	// Total is raised to the category sum like the app does.
	Total      Money             `json:"total"`
	Categories []budgetSetResult `json:"categories"`
	SyncedAt   string            `json:"synced_at,omitempty"`
}

type budgetSetResult struct {
	CategoryID string `json:"category_id"`
	Money      Money  `json:"money"`
}

// Gson fields of Budget.toAPIJson(); budget/submitv2 sends the whole period set.
var budgetKeys = []string{"bookid", "budgetid", "cateid", "count", "flag", "money", "totalCateLimit", "totalCateUsed", "notbudget", "type", "typeinfo", "used", "userid"}

func (input SetBudgets) period() (int, int, error) {
	if (input.Month == "") == (input.Year == 0) {
		return 0, 0, fault.Invalid("请提供 month（YYYY-MM）或 year 中的一项")
	}
	if input.Month != "" {
		date, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return 0, 0, fault.Invalid("month 格式必须为 YYYY-MM")
		}
		return date.Year(), int(date.Month()), nil
	}
	if input.Year < 1 || input.Year > 9999 {
		return 0, 0, fault.Invalid("year 无效")
	}
	return input.Year, 0, nil
}

func (s *Service) SetBudgets(ctx context.Context, input SetBudgets) (BudgetSetResult, error) {
	if input.BookID == "" {
		input.BookID = "-1"
	}
	year, month, err := input.period()
	if err != nil {
		return BudgetSetResult{}, err
	}
	if !qianji.ValidID(input.BookID, true) {
		return BudgetSetResult{}, fault.Invalid("账本 ID 无效，请查询 books list")
	}
	if input.Total == nil && len(input.Categories) == 0 {
		return BudgetSetResult{}, fault.Invalid("需要 total 或 categories")
	}
	if input.Total != nil && *input.Total <= 0 {
		return BudgetSetResult{}, fault.Invalid("总预算必须大于 0，总预算不能删除")
	}
	seen := map[string]bool{}
	for _, item := range input.Categories {
		if !qianji.ValidID(item.CategoryID, false) || item.Money == nil {
			return BudgetSetResult{}, fault.Invalid("每项分类预算需要 category_id 和 money，money 为 0 表示删除")
		}
		if seen[item.CategoryID] {
			return BudgetSetResult{}, fault.Invalid("分类预算不能重复")
		}
		seen[item.CategoryID] = true
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return BudgetSetResult{}, err
	}
	defer lock.Close()
	periodRange, err := s.bookRange(ctx, input.BookID)
	if err != nil {
		return BudgetSetResult{}, err
	}
	key := budgetKey(input.BookID, year, month)
	read := func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
		return c.Budgets(ctx, a, input.BookID, year, month, periodRange)
	}
	current, err := s.Catalog(ctx, key, false, read)
	if err != nil {
		return BudgetSetResult{}, err
	}
	items, err := catalogItems(current.Data)
	if err != nil {
		return BudgetSetResult{}, err
	}
	index, err := (&readContext{service: s}).loadCategories(ctx, input.BookID)
	if err != nil {
		return BudgetSetResult{}, err
	}
	typeInfo := strconv.Itoa(year)
	kind := 2
	if month > 0 {
		typeInfo, kind = typeInfo+","+strconv.Itoa(month), 1
	}
	zero := json.Number("0")
	fresh := func(flag, category string) map[string]any {
		return map[string]any{"bookid": json.Number(input.BookID), "cateid": json.Number(category), "count": zero, "flag": json.Number(flag), "money": zero, "totalCateLimit": zero, "totalCateUsed": zero, "notbudget": zero, "type": json.Number(strconv.Itoa(kind)), "typeinfo": typeInfo, "used": zero, "userid": s.Account.Session.UID}
	}
	var total map[string]any
	categories := []map[string]any{}
	for _, item := range items {
		switch textValue(item["flag"]) {
		case "1":
			total = item
		case "2":
			categories = append(categories, item)
		}
	}
	for _, change := range input.Categories {
		category := index[change.CategoryID]
		if category == nil {
			return BudgetSetResult{}, missingReference("分类")
		}
		if textValue(category["type"]) != "0" {
			return BudgetSetResult{}, fault.Invalid("只能为支出分类设置预算")
		}
		position := -1
		for i, item := range categories {
			if textValue(item["cateid"]) == change.CategoryID {
				position = i
			}
		}
		amount := moneyNumber(*change.Money)
		switch {
		case *change.Money == 0 && position >= 0:
			categories = append(categories[:position], categories[position+1:]...)
		case *change.Money == 0:
		case position >= 0:
			categories[position]["money"] = amount
		default:
			added := fresh("2", change.CategoryID)
			added["money"] = amount
			categories = append(categories, added)
		}
	}
	limit, used, err := categorySum(categories, index)
	if err != nil {
		return BudgetSetResult{}, err
	}
	if input.Total != nil {
		if *input.Total < limit {
			return BudgetSetResult{}, fault.Invalid("总预算不能小于一级分类预算之和")
		}
		if total == nil {
			total = fresh("1", "0")
		}
		total["money"] = moneyNumber(*input.Total)
	}
	if total != nil {
		existing, err := moneyValue(total["money"])
		if err != nil {
			return BudgetSetResult{}, err
		}
		total["money"], total["totalCateLimit"], total["totalCateUsed"] = moneyNumber(max(existing, limit)), moneyNumber(limit), moneyNumber(used)
	}
	changes := []map[string]any{}
	if total != nil {
		changes = append(changes, budgetPayload(total))
	}
	for _, item := range categories {
		changes = append(changes, budgetPayload(item))
	}
	if len(changes) == 0 {
		return BudgetSetResult{}, fault.Invalid("钱迹不支持清空当期全部预算，请在钱迹中操作")
	}
	expected, err := budgetSummary(changes)
	if err != nil {
		return BudgetSetResult{}, err
	}
	result := BudgetSetResult{BookID: input.BookID, Year: year, Month: month, Status: "unknown", Categories: []budgetSetResult{}}
	client, err := s.writeClient(ctx)
	if err != nil {
		return result, err
	}
	data, err := client.SubmitBudgets(ctx, s.Account.Session, input.BookID, year, month, periodRange, changes)
	if err != nil {
		if definiteFailure(err) {
			return result, err
		}
		return outcomeFailure(result, "WRITE_UNCERTAIN", "预算提交结果无法确认，请执行 budgets list 核对，不要直接重试")
	}
	result.Status = "applied"
	for attempt := 0; attempt < 2 && !result.Confirmed; attempt++ {
		if attempt == 1 {
			data, err = s.readOnline(ctx, read)
			if err != nil {
				break
			}
		}
		actual, err := catalogItems(data)
		if err != nil {
			continue
		}
		summary, err := budgetSummary(actual)
		if err == nil && summary.equal(expected) {
			result.Confirmed = true
			result.Total, result.Categories = summary.total, summary.list()
			result.SyncedAt, err = s.Store.SaveCatalogs(ctx, s.Account, map[string]json.RawMessage{key: data})
			if err != nil {
				return outcomeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受预算，但缓存未更新，请执行 budgets list 刷新")
			}
		}
	}
	if !result.Confirmed {
		return outcomeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受预算，但尚未核对到新预算，请执行 budgets list 核对，不要重复提交")
	}
	return result, nil
}

func (s *Service) readOnline(ctx context.Context, read func(*qianji.Client, qianji.Session) (json.RawMessage, error)) (json.RawMessage, error) {
	value, err := s.Read(ctx, func(c *qianji.Client, a qianji.Session) (any, error) { return read(c, a) })
	if err != nil {
		return nil, err
	}
	return value.(json.RawMessage), nil
}

// bookRange reads the book's budget period configuration online.
func (s *Service) bookRange(ctx context.Context, book string) (string, error) {
	catalog, err := s.Catalog(ctx, "books", false, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Books(ctx, a, true) })
	if err != nil {
		return "", err
	}
	items, err := catalogItems(catalog.Data)
	if err != nil {
		return "", err
	}
	if book != "-1" && findCatalog(items, "bookid", book) == nil {
		return "", missingReference("账本")
	}
	for _, item := range items {
		if textValue(item["bookid"]) == book {
			config, _ := item["config"].(map[string]any)
			return textValue(config["range"]), nil
		}
	}
	return "", nil
}

// categorySum mirrors BudgetSetNew: parent budgets plus sub-category budgets
// whose parent has none.
func categorySum(categories []map[string]any, index map[string]map[string]any) (Money, Money, error) {
	parents := map[string]bool{}
	for _, item := range categories {
		id := textValue(item["cateid"])
		if parent := textValue(index[id]["parentid"]); parent == "" || parent == "-1" || parent == "0" {
			parents[id] = true
		}
	}
	var limit, used Money
	for _, item := range categories {
		id := textValue(item["cateid"])
		if !parents[id] && parents[textValue(index[id]["parentid"])] {
			continue
		}
		amount, err := moneyValue(item["money"])
		if err != nil {
			return 0, 0, err
		}
		spent, _ := moneyValue(item["used"])
		limit, used = limit+amount, used+spent
	}
	return limit, used, nil
}

func budgetPayload(item map[string]any) map[string]any {
	payload := map[string]any{}
	for _, key := range budgetKeys {
		if value, ok := item[key]; ok && value != nil {
			payload[key] = value
		}
	}
	return payload
}

type budgetState struct {
	total      Money
	categories map[string]Money
}

func budgetSummary(items []map[string]any) (budgetState, error) {
	state := budgetState{categories: map[string]Money{}}
	for _, item := range items {
		amount, err := moneyValue(item["money"])
		if err != nil {
			return state, err
		}
		switch textValue(item["flag"]) {
		case "1":
			state.total = amount
		case "2":
			state.categories[textValue(item["cateid"])] = amount
		}
	}
	return state, nil
}

func (b budgetState) equal(other budgetState) bool {
	if b.total != other.total || len(b.categories) != len(other.categories) {
		return false
	}
	for id, amount := range b.categories {
		if value, ok := other.categories[id]; !ok || value != amount {
			return false
		}
	}
	return true
}

func (b budgetState) list() []budgetSetResult {
	list := []budgetSetResult{}
	for id, amount := range b.categories {
		list = append(list, budgetSetResult{CategoryID: id, Money: amount})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CategoryID < list[j].CategoryID })
	return list
}
