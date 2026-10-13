package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

// Money stores cents. Neither input validation nor arithmetic uses float64.
type Money int64

var moneyPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})(\.[0-9]{1,2})?$`)

func (m *Money) UnmarshalJSON(raw []byte) error {
	value := string(raw)
	if !moneyPattern.MatchString(value) {
		return fault.Invalid("金额必须是最多两位小数的数字，范围为 0～9999999999.99")
	}
	parts := strings.SplitN(value, ".", 2)
	whole, _ := strconv.ParseInt(parts[0], 10, 64)
	var cents int64
	if len(parts) == 2 {
		cents, _ = strconv.ParseInt((parts[1] + "0")[:2], 10, 64)
	}
	*m = Money(whole*100 + cents)
	return nil
}

func (m Money) MarshalJSON() ([]byte, error) {
	sign := ""
	if m < 0 {
		sign = "-"
		m = -m
	}
	return []byte(fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)), nil
}

type BillFields struct {
	BookID       *string   `json:"book_id,omitempty"`
	Time         *int64    `json:"time,omitempty"`
	Money        *Money    `json:"money,omitempty"`
	Discount     *Money    `json:"discount,omitempty"`
	CategoryID   *string   `json:"category_id,omitempty"`
	AssetID      *string   `json:"asset_id,omitempty"`
	Remark       *string   `json:"remark,omitempty"`
	TagIDs       *[]string `json:"tag_ids,omitempty"`
	Reimbursable *bool     `json:"reimbursable,omitempty"`
}

type CreateBill struct {
	Type *int `json:"type"`
	BillFields
}

type UpdateBill struct {
	ID    string     `json:"id"`
	Patch BillFields `json:"patch"`
}

type DeleteBill struct {
	ID             string `json:"id"`
	CascadeRelated bool   `json:"cascade_related,omitempty"`
}

func (f BillFields) validate() error {
	for _, item := range []struct {
		id       *string
		optional bool
	}{{f.BookID, true}, {f.AssetID, true}, {f.CategoryID, false}} {
		if item.id != nil && !qianji.ValidID(*item.id, item.optional) {
			return fault.Invalid("账本、分类或资产 ID 无效，请先查询可用资料")
		}
	}
	if f.Money != nil && (*f.Money <= 0 || *f.Money > 999999999999) {
		return fault.Invalid("账单金额必须大于零且不超过 9999999999.99")
	}
	if f.Discount != nil && (*f.Discount < 0 || *f.Discount > 999999999999) {
		return fault.Invalid("优惠金额超出范围，请检查金额")
	}
	if f.Time != nil && *f.Time <= 0 {
		return fault.Invalid("time 必须是大于零的 Unix 秒时间戳")
	}
	if f.TagIDs != nil {
		seen := map[string]bool{}
		for _, id := range *f.TagIDs {
			if strings.TrimSpace(id) == "" || seen[id] {
				return fault.Invalid("标签 ID 不能为空或重复，请查询 tags list")
			}
			seen[id] = true
		}
	}
	return nil
}

func validateBatch(ids []string) error {
	if len(ids) == 0 {
		return fault.Invalid("账单列表不能为空")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !qianji.ValidID(id, false) || seen[id] {
			return fault.Invalid("账单 ID 必须是互不重复的正整数字符串")
		}
		seen[id] = true
	}
	return nil
}

func (f BillFields) empty() bool {
	return f.BookID == nil && f.Time == nil && f.Money == nil && f.Discount == nil && f.CategoryID == nil && f.AssetID == nil && f.Remark == nil && f.TagIDs == nil && f.Reimbursable == nil
}

func number(value int64) json.Number { return json.Number(strconv.FormatInt(value, 10)) }
