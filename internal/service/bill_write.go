package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type WriteItem struct {
	Index     int             `json:"index"`
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Confirmed bool            `json:"confirmed"`
	Bill      json.RawMessage `json:"bill,omitempty"`
}

type WriteResult struct {
	Items    []WriteItem `json:"items"`
	SyncedAt string      `json:"synced_at,omitempty"`
}

func (s *Service) CreateBills(ctx context.Context, inputs []CreateBill) (WriteResult, error) {
	if len(inputs) == 0 {
		return WriteResult{}, fault.Invalid("bills 不能为空")
	}
	for _, input := range inputs {
		if input.Type == nil || (*input.Type != 0 && *input.Type != 1) || input.Money == nil || input.CategoryID == nil {
			return WriteResult{}, fault.Invalid("新增账单需要 type（0 支出或 1 收入）、money 和 category_id")
		}
		if err := input.validate(); err != nil {
			return WriteResult{}, err
		}
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	catalog := newWriteCatalog(s)
	changes := qianji.BillChanges{}
	seen := map[string]bool{}
	for _, input := range inputs {
		var id string
		for id == "" || seen[id] {
			random, err := rand.Int(rand.Reader, big.NewInt(100000))
			if err != nil {
				return WriteResult{}, err
			}
			id = strconv.FormatInt(time.Now().UnixMilli()*1000000+100000+random.Int64(), 10)
		}
		seen[id] = true
		now := number(time.Now().Unix())
		bill := map[string]any{"id": json.Number(id), "userid": s.Account.UID, "bookid": number(-1), "time": now,
			"type": number(int64(*input.Type)), "money": number(0), "remark": "", "status": number(2),
			"cateid": json.Number(*input.CategoryID), "assetid": number(-1), "fromid": number(-1), "targetid": number(-1),
			"createtime": now, "updatetime": now, "platform": number(0), "images": []any{}}
		if err := applyBillFields(bill, input.BillFields); err != nil {
			return WriteResult{}, err
		}
		if err := catalog.applyCurrency(ctx, bill, nil, input.BillFields); err != nil {
			return WriteResult{}, err
		}
		if err := catalog.validate(ctx, bill); err != nil {
			return WriteResult{}, err
		}
		changes.Changes = append(changes.Changes, bill)
	}
	return s.writeBills(ctx, "create", changes)
}

func (s *Service) UpdateBills(ctx context.Context, inputs []UpdateBill) (WriteResult, error) {
	ids := make([]string, len(inputs))
	for i, input := range inputs {
		ids[i] = input.ID
		if input.Patch.empty() {
			return WriteResult{}, fault.Invalid("patch 至少需要一个要修改的字段")
		}
		if err := input.Patch.validate(); err != nil {
			return WriteResult{}, err
		}
	}
	if err := validateBatch(ids); err != nil {
		return WriteResult{}, err
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	catalog := newWriteCatalog(s)
	changes := qianji.BillChanges{}
	for _, input := range inputs {
		bill, err := s.writableBill(ctx, input.ID)
		if err != nil {
			return WriteResult{}, err
		}
		kind := textValue(bill["type"])
		if kind != "0" && kind != "1" && kind != "5" {
			return WriteResult{}, unsupportedBill()
		}
		extra, err := billExtra(bill)
		if err != nil {
			return WriteResult{}, err
		}
		related, err := relatedIDs(extra)
		if err != nil {
			return WriteResult{}, err
		}
		// Relationship-bearing bills keep their financial fields untouched here;
		// their specialized refund/reimbursement flows are outside this command.
		financial := currencyTouched(input.Patch) || input.Patch.BookID != nil || input.Patch.Reimbursable != nil
		if len(related) > 0 && financial {
			return WriteResult{}, fault.New("RELATED_BILLS_EXIST", "账单已有退款或报销，请在钱迹中修改原币金额、折算金额、资产、账本或报销状态", 2)
		}
		before := cloneBill(bill)
		if err := applyBillFields(bill, input.Patch); err != nil {
			return WriteResult{}, err
		}
		if err := catalog.applyCurrency(ctx, bill, before, input.Patch); err != nil {
			return WriteResult{}, err
		}
		if err := catalog.validate(ctx, bill); err != nil {
			return WriteResult{}, err
		}
		bill["updatetime"] = number(time.Now().Unix())
		bill["status"] = number(2)
		for _, field := range []string{"category", "fromact", "targetact", "paytype", "bookname"} {
			delete(bill, field)
		}
		changes.Changes = append(changes.Changes, bill)
	}
	return s.writeBills(ctx, "update", changes)
}

func (s *Service) DeleteBills(ctx context.Context, inputs []DeleteBill) (WriteResult, error) {
	ids := make([]string, len(inputs))
	for i, input := range inputs {
		ids[i] = input.ID
	}
	if err := validateBatch(ids); err != nil {
		return WriteResult{}, err
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	changes := qianji.BillChanges{}
	seen := map[string]bool{}
	for _, input := range inputs {
		bill, err := s.writableBill(ctx, input.ID)
		if err != nil {
			return WriteResult{}, err
		}
		switch textValue(bill["type"]) {
		case "0", "1", "2", "3", "5":
		default:
			return WriteResult{}, unsupportedBill()
		}
		extra, err := billExtra(bill)
		if err != nil {
			return WriteResult{}, err
		}
		related, err := relatedIDs(extra)
		if err != nil {
			return WriteResult{}, err
		}
		if len(related) > 0 && !input.CascadeRelated {
			return WriteResult{}, fault.New("RELATED_BILLS_EXIST", "删除此账单还会删除关联退款或报销；确认全部删除后设置 cascade_related=true", 2)
		}
		for _, id := range related {
			child, err := s.writableBill(ctx, id)
			if err != nil {
				return WriteResult{}, err
			}
			childExtra, err := billExtra(child)
			if err != nil {
				return WriteResult{}, err
			}
			if textValue(childExtra["refundsid"]) != input.ID && textValue(childExtra["bxsid"]) != input.ID {
				return WriteResult{}, fault.New("RELATED_BILLS_EXIST", "关联账单与源账单不一致，请在钱迹中核对后删除", 2)
			}
		}
		for _, id := range append([]string{input.ID}, related...) {
			if !seen[id] {
				changes.Deletes = append(changes.Deletes, json.Number(id))
				seen[id] = true
			}
		}
	}
	return s.writeBills(ctx, "delete", changes)
}

func (s *Service) writableBill(ctx context.Context, id string) (map[string]any, error) {
	raw, err := s.Store.GetBill(ctx, s.Account.Owner(), id)
	if err != nil {
		return nil, err
	}
	bill, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if textValue(bill["userid"]) != s.Account.UID {
		return nil, fault.New("BILL_NOT_OWNED", "只能修改或删除当前账号创建的账单", 2)
	}
	for _, field := range []string{"id", "bookid", "cateid", "assetid", "fromid", "targetid"} {
		value := textValue(bill[field])
		if !qianji.ValidID(value, field != "id") {
			return nil, invalidWriteData()
		}
		bill[field] = json.Number(value)
	}
	return bill, nil
}

func applyBillFields(bill map[string]any, patch BillFields) error {
	extra, err := billExtra(bill)
	if err != nil {
		return err
	}
	if patch.ExcludeFromIncomeExpense != nil || patch.ExcludeFromBudget != nil {
		var flag uint64
		if extra["flag"] != nil {
			flag, err = strconv.ParseUint(textValue(extra["flag"]), 10, 64)
			if err != nil {
				return invalidWriteData()
			}
		}
		for _, option := range []struct {
			value *bool
			bit   uint64
		}{{patch.ExcludeFromIncomeExpense, 1}, {patch.ExcludeFromBudget, 2}} {
			if option.value != nil {
				if *option.value {
					flag |= option.bit
				} else {
					flag &^= option.bit
				}
			}
		}
		extra["flag"] = json.Number(strconv.FormatUint(flag, 10))
	}
	if patch.Reimbursable != nil {
		if textValue(bill["type"]) == "1" && *patch.Reimbursable {
			return fault.Invalid("收入不能标为待报销")
		}
		if textValue(bill["type"]) != "1" {
			bill["type"] = number(0)
			if *patch.Reimbursable {
				bill["type"] = number(5)
			}
		}
	}
	if patch.Money != nil || patch.Discount != nil {
		discount, err := billDiscount(extra)
		if err != nil {
			return err
		}
		if patch.Discount != nil {
			discount = *patch.Discount
		}
		var amount Money
		if patch.Money != nil {
			amount = *patch.Money
		} else {
			amount, err = grossMoney(bill)
			if err != nil {
				return err
			}
		}
		if amount <= 0 || amount > 999999999999 || discount > amount || (textValue(bill["type"]) == "1" && discount != 0) {
			return fault.Invalid("优惠不能超过账单金额，收入不能设置优惠")
		}
		moneyJSON, _ := (amount - discount).MarshalJSON()
		feeJSON, _ := (-discount).MarshalJSON()
		bill["money"] = json.Number(moneyJSON)
		extra["transfee"] = json.Number(feeJSON)

	}
	for _, item := range []struct {
		key   string
		value *string
	}{{"bookid", patch.BookID}, {"cateid", patch.CategoryID}, {"assetid", patch.AssetID}} {
		if item.value != nil {
			bill[item.key] = json.Number(*item.value)
		}
	}
	if patch.Time != nil {
		bill["time"] = number(*patch.Time)
	}
	if patch.Remark != nil {
		bill["remark"] = *patch.Remark
	}
	if patch.TagIDs != nil {
		tags := make([]any, len(*patch.TagIDs))
		for i, tag := range *patch.TagIDs {
			tags[i] = tag
		}
		extra["tags"] = tags
	}
	if len(extra) > 0 {
		bill["extra"] = extra
	}
	return nil
}

func billDiscount(extra map[string]any) (Money, error) {
	if extra["transfee"] == nil {
		return 0, nil
	}
	value, ok := new(big.Rat).SetString(textValue(extra["transfee"]))
	if !ok {
		return 0, invalidWriteData()
	}
	value.Mul(value, big.NewRat(100, 1))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, invalidWriteData()
	}
	adjustment := Money(value.Num().Int64())
	if adjustment < -999999999999 || adjustment > 999999999999 {
		return 0, invalidWriteData()
	}
	if adjustment > 0 {
		return 0, fault.New("BILL_UNSUPPORTED", "带手续费的账单请在钱迹中修改金额", 2)
	}
	return -adjustment, nil
}

// One request only: a timeout, cancellation, or invalid receipt can follow a
// committed write. Read's automatic authentication retry must not wrap this call.
func (s *Service) writeBills(ctx context.Context, operation string, changes qianji.BillChanges) (WriteResult, error) {
	result := WriteResult{Items: []WriteItem{}}
	for _, bill := range changes.Changes {
		result.Items = append(result.Items, WriteItem{Index: len(result.Items), ID: textValue(bill["id"]), Status: "unknown"})
	}
	for _, id := range changes.Deletes {
		result.Items = append(result.Items, WriteItem{Index: len(result.Items), ID: string(id), Status: "unknown"})
	}
	account, err := s.Store.Account(ctx)
	if err != nil {
		return result, err
	}
	if account.Revision != s.Account.Revision {
		return result, fault.New("STATE_CHANGED", "登录状态已改变，请核对账号后重新执行", 1)
	}
	client, err := s.client(ctx, s.Account.BaseURL)
	if err != nil {
		return result, err
	}
	receipt, err := client.SyncBills(ctx, s.Account.Session, changes)
	if err != nil {
		for _, code := range []string{"TOKEN_EXPIRED", "AUTH_REQUIRED", "BUSINESS_ERROR", "SIGNATURE_REJECTED"} {
			if fault.Is(err, code) {
				return result, err
			}
		}
		return writeFailure(result, "WRITE_UNCERTAIN", "写入结果无法确认，请先执行 sync 并按返回的账单 ID 核对，不要直接重试")
	}
	valid := applyReceipt(&result, operation, receipt)
	if err := s.Store.InvalidateAssets(ctx, s.Account.Owner()); err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受操作，账户缓存未刷新，请同步核对")
	}
	syncResult, syncErr := s.Sync(ctx, false)
	if syncErr == nil {
		result.SyncedAt = syncResult.SyncedAt
		for i := range result.Items {
			item := &result.Items[i]
			if item.Status != "applied" {
				continue
			}
			raw, readErr := s.Store.GetBill(ctx, s.Account.Owner(), item.ID)
			if operation == "delete" {
				item.Confirmed = fault.Is(readErr, "NOT_FOUND")
				continue
			}
			if readErr != nil {
				continue
			}
			actual, decodeErr := decodeObject(raw)
			if decodeErr == nil && billMatches(actual, changes.Changes[i]) {
				item.Confirmed = true
				item.Bill = raw
			}
		}
	}
	if !valid {
		return writeFailure(result, "WRITE_UNCERTAIN", "钱迹返回的写入结果不完整，请执行 sync 并逐项核对，不要直接重试")
	}
	counts := map[string]int{}
	for _, item := range result.Items {
		counts[item.Status]++
	}
	if counts["applied"] != len(result.Items) {
		code := "WRITE_UNCERTAIN"
		if counts["applied"] > 0 {
			code = "WRITE_PARTIAL"
		} else if counts["conflict"] == len(result.Items) {
			code = "WRITE_CONFLICT"
		}
		return writeFailure(result, code, "账单未全部写入，请按逐项结果核对；已成功的项目不要重试")
	}
	for _, item := range result.Items {
		if !item.Confirmed {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受写入，但本地尚未确认，请执行 sync 后核对，不要重复写入")
		}
	}
	return result, nil
}

func applyReceipt(result *WriteResult, operation string, receipt qianji.WriteReceipt) bool {
	requested := map[string]int{}
	for i, item := range result.Items {
		requested[item.ID] = i
	}
	seen := map[string]bool{}
	valid := true
	for _, group := range []struct {
		operation, status string
		ids               []string
	}{
		{"create", "applied", receipt.Created}, {"update", "applied", receipt.Updated}, {"delete", "applied", receipt.Deleted}, {operation, "conflict", receipt.Conflicts},
	} {
		for _, id := range group.ids {
			i, exists := requested[id]
			if !exists || seen[id] || group.operation != operation {
				valid = false
				if exists {
					result.Items[i].Status = "unknown"
					seen[id] = true
				}
				continue
			}
			seen[id] = true
			result.Items[i].Status = group.status
		}
	}
	if len(seen) != len(requested) && !receipt.Failed {
		valid = false
	}
	allApplied := true
	for _, item := range result.Items {
		allApplied = allApplied && item.Status == "applied"
	}
	return valid && !(receipt.Failed && allApplied)
}

func writeFailure(result WriteResult, code, message string) (WriteResult, error) {
	err := fault.New(code, message, 4)
	// Errors carry identifiers and outcomes only, not raw bill contents.
	details := result
	details.Items = append([]WriteItem(nil), result.Items...)
	for i := range details.Items {
		details.Items[i].Bill = nil
	}
	err.Details = details
	return result, err
}

func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if decoder.Decode(&object) != nil || object == nil {
		return nil, invalidWriteData()
	}
	return object, nil
}

func textValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return string(value)
	default:
		return ""
	}
}

func billExtra(bill map[string]any) (map[string]any, error) {
	if bill["extra"] == nil {
		return map[string]any{}, nil
	}
	extra, ok := bill["extra"].(map[string]any)
	if !ok {
		return nil, invalidWriteData()
	}
	return extra, nil
}

func relatedIDs(extra map[string]any) ([]string, error) {
	// Do not reinterpret the old relationship format. Ask the app to handle it.
	if (extra["refundid"] != nil && textValue(extra["refundid"]) != "0" && textValue(extra["refundid"]) != "-1") || textValue(extra["baoxiaoed"]) == "1" {
		return nil, fault.New("BILL_UNSUPPORTED", "此账单的退款或报销记录需在钱迹中处理", 2)
	}
	seen := map[string]bool{}
	for _, field := range []string{"rfds", "bxs"} {
		if extra[field] == nil {
			continue
		}
		ids, ok := extra[field].(map[string]any)
		if !ok {
			return nil, invalidWriteData()
		}
		for id := range ids {
			if !qianji.ValidID(id, false) {
				return nil, invalidWriteData()
			}
			seen[id] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func billMatches(actual, expected map[string]any) bool {
	for _, key := range []string{"id", "userid", "bookid", "type", "money", "cateid", "assetid", "fromid", "targetid", "time", "remark"} {
		if !sameValue(actual[key], expected[key]) {
			return false
		}
	}
	expectedExtra, err := billExtra(expected)
	if err != nil {
		return false
	}
	actualExtra, err := billExtra(actual)
	if err != nil {
		return false
	}
	for _, key := range []string{"transfee", "tags", "curr", "flag"} {
		actualValue := actualExtra[key]
		// The app omits empty optional fields; clearing tags/discounts must also
		// accept their absence in the synchronized bill.
		if actualValue == nil {
			switch key {
			case "transfee", "flag":
				actualValue = number(0)
			case "tags":
				actualValue = []any{}
			}
		}
		if expectedExtra[key] != nil && !sameValue(actualValue, expectedExtra[key]) {
			return false
		}
	}
	return true
}

func sameValue(actual, expected any) bool {
	if value, ok := expected.(json.Number); ok {
		a, aOK := new(big.Rat).SetString(textValue(actual))
		b, bOK := new(big.Rat).SetString(string(value))
		return aOK && bOK && a.Cmp(b) == 0
	}
	if object, ok := expected.(map[string]any); ok {
		if object == nil {
			return actual == nil
		}
		other, ok := actual.(map[string]any)
		if !ok || len(other) != len(object) {
			return false
		}
		for key, value := range object {
			if !sameValue(other[key], value) {
				return false
			}
		}
		return true
	}
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(expected)
	return bytes.Equal(a, b)
}

func invalidWriteData() error {
	return fault.New("RESPONSE_INVALID", "账单或基础资料无效，请同步后重试", 4)
}
func unsupportedBill() error {
	return fault.New("BILL_UNSUPPORTED", "此账单类型暂不支持，请在钱迹中操作", 2)
}

func cloneBill(bill map[string]any) map[string]any {
	raw, _ := json.Marshal(bill)
	copied, _ := decodeObject(raw)
	return copied
}
