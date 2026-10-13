package service

import (
	"context"
	"math/big"
	"sort"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

func signedMoney(value any) (Money, error) {
	if value == nil {
		return 0, nil
	}
	v, ok := new(big.Rat).SetString(textValue(value))
	if !ok {
		return 0, invalidWriteData()
	}
	v.Mul(v, big.NewRat(100, 1))
	if !v.IsInt() || !v.Num().IsInt64() {
		return 0, invalidWriteData()
	}
	return Money(v.Num().Int64()), nil
}
func absMoney(m Money) Money {
	if m < 0 {
		return -m
	}
	return m
}

func (r *readContext) amount(ctx context.Context, bill map[string]any, currency string) (Money, error) {
	extra, err := billExtra(bill)
	if err != nil {
		return 0, err
	}
	if extra["curr"] != nil {
		curr, ok := extra["curr"].(map[string]any)
		if !ok {
			return 0, invalidWriteData()
		}
		if err := validateBaseCurrencyPair(extra); err != nil {
			return 0, err
		}
		if textValue(curr["ss"]) == currency {
			gross, err := moneyValue(curr["sv"])
			if err != nil {
				return 0, err
			}
			adjustment, err := signedMoney(extra["transfee"])
			if err != nil {
				return 0, err
			}
			return gross - absMoney(adjustment), nil
		}
		for _, pair := range [][2]string{{"ts", "tv"}, {"bs", "bv"}} {
			if textValue(curr[pair[0]]) == currency {
				return moneyValue(curr[pair[1]])
			}
		}
		return 0, missingConversion()
	}
	source, err := r.currency(ctx, bill)
	if err != nil {
		return 0, err
	}
	if source != currency {
		return 0, missingConversion()
	}
	return moneyValue(bill["money"])
}

func relationship(extra map[string]any, key string) (map[string]Money, error) {
	result := map[string]Money{}
	if extra[key] == nil {
		return result, nil
	}
	values, ok := extra[key].(map[string]any)
	if !ok {
		return nil, invalidWriteData()
	}
	for id, value := range values {
		if !qianji.ValidID(id, false) {
			return nil, invalidWriteData()
		}
		amount, err := moneyValue(value)
		if err != nil {
			return nil, err
		}
		result[id] = amount
	}
	return result, nil
}

func (r *readContext) relatedAmount(ctx context.Context, extra map[string]any, key, currency string, all map[string]map[string]any) (Money, error) {
	rels, err := relationship(extra, key)
	if err != nil {
		return 0, err
	}
	var total Money
	for id := range rels {
		bill := all[id]
		if bill == nil {
			return 0, fault.New("RELATED_BILL_MISSING", "关联账单未完整缓存，请执行 sync --full 后重试", 5)
		}
		amount, err := r.amount(ctx, bill, currency)
		if err != nil {
			return 0, err
		}
		total += amount
	}
	return total, nil
}

type statItem map[string]Money

func newStatItem() statItem {
	r := statItem{}
	for _, key := range []string{"total_income", "total_spend", "balance", "pure_income", "pure_spend", "transfer_amount", "credit_repayment_amount", "reimbursement_amount", "reimbursement_income", "reimbursement_spend", "refund_amount", "refund_income", "refund_spend", "fee_amount", "discount_amount", "excluded_from_income_expense_amount", "excluded_from_budget_amount"} {
		r[key] = 0
	}
	return r
}

func (r *readContext) stat(ctx context.Context, bill map[string]any, currency string, all map[string]map[string]any, exclude int64) (statItem, error) {
	item := newStatItem()
	kind, err := jsonNumberInt(bill["type"])
	if err != nil {
		return nil, err
	}
	// Repayment, loan principal, balance adjustments and reimbursement children
	// never enter income/spend directly. Source relationships account for children.
	if !contains([]string{"0", "1", "2", "3", "5", "10", "11", "20"}, textValue(bill["type"])) {
		return item, nil
	}
	extra, err := billExtra(bill)
	if err != nil {
		return nil, err
	}
	if _, err := relatedIDs(extra); err != nil {
		return nil, err
	}
	flag, err := billFlag(extra)
	if err != nil {
		return nil, err
	}
	main, err := r.amount(ctx, bill, currency)
	if err != nil {
		return nil, err
	}
	refund, err := r.relatedAmount(ctx, extra, "rfds", currency, all)
	if err != nil {
		return nil, err
	}
	after := main - refund
	excluded := flag&exclude != 0
	switch kind {
	case 0, 10:
		main = after
		if !excluded {
			if after > 0 {
				item["pure_spend"] = after
			} else if after < 0 {
				item["refund_income"] = -after
			}
		}
		if main < 0 {
			main = 0
		}
	case 1, 11:
		main = after
		if !excluded {
			if after > 0 {
				item["pure_income"] = after
			} else if after < 0 {
				item["refund_spend"] = -after
			}
		}
		if main < 0 {
			main = 0
		}
	case 5:
		base := max(Money(0), after)
		item["reimbursement_amount"] = base
		reimbursed, err := r.relatedAmount(ctx, extra, "bxs", currency, all)
		if err != nil {
			return nil, err
		}
		if reimbursed > 0 {
			difference := base - reimbursed
			main = absMoney(difference)
			if !excluded {
				if difference > 0 {
					item["reimbursement_spend"] = difference
				} else {
					item["reimbursement_income"] = -difference
				}
			}
		}
		if after < 0 && !excluded {
			item["refund_income"] = -after
		}
	case 2:
		item["transfer_amount"] = main
		main = 0
	case 3:
		item["credit_repayment_amount"] = main
		main = 0
	case 20:
		item["refund_amount"] = main
		main = 0
	}
	adjustment, err := signedMoney(extra["transfee"])
	if err != nil {
		return nil, err
	}
	if adjustment != 0 && !excluded {
		if curr, ok := extra["curr"].(map[string]any); ok && textValue(curr["ss"]) != currency {
			gross, err := moneyValue(curr["sv"])
			if err != nil {
				return nil, err
			}
			stored, err := r.amount(ctx, bill, currency)
			if err != nil {
				return nil, err
			}
			if gross > 0 {
				convertedFee, err := converted(absMoney(adjustment), big.NewRat(int64(stored), 1), big.NewRat(int64(gross), 1))
				if err != nil {
					return nil, err
				}
				if adjustment < 0 {
					adjustment = -convertedFee
				} else {
					adjustment = convertedFee
				}
			}
		}
		if adjustment > 0 {
			item["fee_amount"] = adjustment
		} else {
			item["discount_amount"] = -adjustment
		}
	}
	if main != 0 {
		if flag&1 != 0 {
			item["excluded_from_income_expense_amount"] = main
		}
		if flag&2 != 0 {
			item["excluded_from_budget_amount"] = main
		}
	}
	item["total_income"] = item["pure_income"] + item["reimbursement_income"] + item["refund_income"]
	item["total_spend"] = item["pure_spend"] + item["reimbursement_spend"] + item["fee_amount"] + item["refund_spend"]
	item["balance"] = item["total_income"] - item["total_spend"]
	return item, nil
}

type StatisticsInput struct {
	BillQuery
	Unit string `json:"unit,omitempty"`
}
type breakdown struct {
	ID       string       `json:"id"`
	Book     string       `json:"book_id,omitempty"`
	Name     string       `json:"name,omitempty"`
	Income   Money        `json:"income"`
	Spend    Money        `json:"spend"`
	Balance  Money        `json:"balance"`
	Children []*breakdown `json:"children,omitempty"`
}

func addBreakdown(item *breakdown, stat statItem) {
	item.Income += stat["total_income"]
	item.Spend += stat["total_spend"]
	item.Balance = item.Income - item.Spend
}
func breakdownList(items map[string]*breakdown) []*breakdown {
	out := make([]*breakdown, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) Statistics(ctx context.Context, input StatisticsInput) (map[string]any, error) {
	if input.Unit == "" {
		input.Unit = "day"
	}
	if input.Unit != "day" && input.Unit != "month" && input.Unit != "year" {
		return nil, fault.Invalid("unit 请选择 day、month 或 year")
	}
	r := &readContext{service: s, offline: true}
	matched, all, state, err := s.querySnapshot(ctx, input.BillQuery, r)
	if err != nil {
		return nil, err
	}
	currency := input.Currency
	if currency == "" {
		currency, err = r.baseCurrency(ctx)
		if err != nil {
			return nil, err
		}
	}
	index := map[string]map[string]any{}
	for _, record := range all {
		index[textValue(record.bill["id"])] = record.bill
	}
	total := newStatItem()
	members, periods, categories := map[string]*breakdown{}, map[string]*breakdown{}, map[string]*breakdown{}
	counts := map[string]int{"matched_bills": len(matched), "income_items": 0, "spend_items": 0, "reimbursement_bills": 0}
	start, end := input.Since, input.Until
	for _, record := range matched {
		bill := record.bill
		item, err := r.stat(ctx, bill, currency, index, 1)
		if err != nil {
			return nil, err
		}
		for key, value := range item {
			total[key] += value
		}
		if item["total_income"] > 0 {
			counts["income_items"]++
		}
		if item["total_spend"] > 0 {
			counts["spend_items"]++
		}
		if textValue(bill["type"]) == "5" {
			counts["reimbursement_bills"]++
		}
		when, _ := jsonNumberInt(bill["time"])
		if input.Since == 0 && (start == 0 || when < start) {
			start = when
		}
		if input.Until == 0 && when >= end {
			end = when + 1
		}
		format := "2006-01-02"
		if input.Unit == "month" {
			format = "2006-01"
		}
		if input.Unit == "year" {
			format = "2006"
		}
		period := time.Unix(when, 0).In(shanghai).Format(format)
		member := textValue(bill["userid"])
		if members[member] == nil {
			members[member] = &breakdown{ID: member, Name: textValue(bill["username"])}
		}
		addBreakdown(members[member], item)
		if periods[period] == nil {
			periods[period] = &breakdown{ID: period}
		}
		addBreakdown(periods[period], item)
		if item["pure_income"]+item["pure_spend"]+item["reimbursement_spend"] > 0 {
			book, id := textValue(bill["bookid"]), textValue(bill["cateid"])
			key := book + ":" + id
			catalog, err := r.loadCategories(ctx, book)
			if err != nil {
				return nil, err
			}
			if categories[key] == nil {
				categories[key] = &breakdown{ID: id, Book: book, Name: textValue(catalog[id]["name"])}
			}
			part := statItem{"total_income": item["pure_income"], "total_spend": item["pure_spend"] + item["reimbursement_spend"]}
			addBreakdown(categories[key], part)
		}
	}
	flat := breakdownList(categories)
	for _, child := range flat {
		catalog, err := r.loadCategories(ctx, child.Book)
		if err != nil {
			return nil, err
		}
		parentID := textValue(catalog[child.ID]["parentid"])
		if parentID == "" || parentID == "-1" || parentID == child.ID {
			continue
		}
		key := child.Book + ":" + parentID
		parent := categories[key]
		if parent == nil {
			parent = &breakdown{ID: parentID, Book: child.Book, Name: textValue(catalog[parentID]["name"])}
			categories[key] = parent
		}
		parent.Children = append(parent.Children, child)
		parent.Income += child.Income
		parent.Spend += child.Spend
		parent.Balance = parent.Income - parent.Spend
		delete(categories, child.Book+":"+child.ID)
	}
	periodCount := len(periods)
	if input.Unit == "day" {
		now := time.Now().In(shanghai)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, shanghai).Unix()
		if start == 0 || end <= start {
			periodCount = 0
		} else {
			day := time.Unix(start, 0).In(shanghai)
			dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, shanghai).Unix()
			periodCount = max(0, int((min(end, today)-dayStart)/86400))
		}
	}
	average := map[string]any{"unit": input.Unit, "period_count": periodCount, "income": Money(0), "spend": Money(0), "balance": Money(0)}
	if periodCount > 0 {
		income := Money(int64(total["total_income"]) / int64(periodCount))
		spend := Money(int64(total["total_spend"]) / int64(periodCount))
		average["income"], average["spend"], average["balance"] = income, spend, income-spend
	}
	return map[string]any{"source": "cache", "synced_at": state.SyncedAt, "since": start, "until": end, "currency": currency, "summary": total, "counts": counts, "average": average, "categories": breakdownList(categories), "members": breakdownList(members), "time_series": breakdownList(periods)}, nil
}
