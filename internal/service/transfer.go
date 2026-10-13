package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"strconv"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type TransferFields struct {
	BillFields
	FromAssetID     *string `json:"from_asset_id,omitempty"`
	TargetAssetID   *string `json:"target_asset_id,omitempty"`
	Fee             *Money  `json:"fee,omitempty"`
	CreditRepayment *bool   `json:"credit_repayment,omitempty"`
}
type UpdateTransfer struct {
	ID    string         `json:"id"`
	Patch TransferFields `json:"patch"`
}

func (f TransferFields) validate() error {
	if err := f.BillFields.validate(); err != nil {
		return err
	}
	if f.CategoryID != nil || f.AssetID != nil || f.Reimbursable != nil || f.Currency != nil {
		return fault.Invalid("转账请使用 from_asset_id 和 target_asset_id，不填写分类、待报销状态或 currency")
	}
	for _, id := range []*string{f.FromAssetID, f.TargetAssetID} {
		if id != nil && !qianji.ValidID(*id, false) {
			return fault.Invalid("转出和转入账户必须是有效账户 ID")
		}
	}
	if f.Fee != nil && (*f.Fee < 0 || *f.Fee > 999999999999) {
		return fault.Invalid("手续费超出范围")
	}
	return nil
}

func newBillID() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(100000))
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(time.Now().UnixMilli()*1000000+100000+n.Int64(), 10), nil
}

func (s *Service) CreateTransfer(ctx context.Context, input TransferFields) (WriteResult, error) {
	if err := input.validate(); err != nil {
		return WriteResult{}, err
	}
	if input.Money == nil || input.FromAssetID == nil || input.TargetAssetID == nil {
		return WriteResult{}, fault.Invalid("转账需要 money、from_asset_id 和 target_asset_id")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	id, err := newBillID()
	if err != nil {
		return WriteResult{}, err
	}
	now := number(time.Now().Unix())
	bill := map[string]any{"id": json.Number(id), "userid": s.Account.UID, "bookid": number(-1), "time": now, "createtime": now, "type": number(2), "money": number(0), "cateid": number(-1), "assetid": number(-1), "fromid": json.Number(*input.FromAssetID), "targetid": json.Number(*input.TargetAssetID), "remark": "", "images": []any{}}
	if err := s.prepareTransfer(ctx, bill, nil, input); err != nil {
		return WriteResult{}, err
	}
	return s.writeBills(ctx, "create", qianji.BillChanges{Changes: []map[string]any{bill}})
}

func (s *Service) UpdateTransfer(ctx context.Context, input UpdateTransfer) (WriteResult, error) {
	if err := input.Patch.validate(); err != nil {
		return WriteResult{}, err
	}
	if !qianji.ValidID(input.ID, false) {
		return WriteResult{}, fault.Invalid("请提供有效账单 ID")
	}
	f := input.Patch
	if f.BillFields.empty() && f.FromAssetID == nil && f.TargetAssetID == nil && f.Fee == nil && f.CreditRepayment == nil {
		return WriteResult{}, fault.Invalid("patch 至少需要一个要修改的字段")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	bill, err := s.writableBill(ctx, input.ID)
	if err != nil {
		return WriteResult{}, err
	}
	if textValue(bill["type"]) != "2" && textValue(bill["type"]) != "3" {
		return WriteResult{}, fault.Invalid("此命令只修改转账或信用卡还款")
	}
	before := cloneBill(bill)
	if err := s.prepareTransfer(ctx, bill, before, f); err != nil {
		return WriteResult{}, err
	}
	return s.writeBills(ctx, "update", qianji.BillChanges{Changes: []map[string]any{bill}})
}

func (s *Service) prepareTransfer(ctx context.Context, bill, before map[string]any, f TransferFields) error {
	c := newWriteCatalog(s)
	if f.FromAssetID != nil {
		bill["fromid"] = json.Number(*f.FromAssetID)
	}
	if f.TargetAssetID != nil {
		bill["targetid"] = json.Number(*f.TargetAssetID)
	}
	fromID, targetID := textValue(bill["fromid"]), textValue(bill["targetid"])
	if fromID == targetID {
		return fault.Invalid("转出和转入账户不能相同")
	}
	from, err := c.asset(ctx, fromID)
	if err != nil {
		return err
	}
	target, err := c.asset(ctx, targetID)
	if err != nil {
		return err
	}
	for _, asset := range []map[string]any{from, target} {
		if textValue(asset["type"]) == "5" || textValue(asset["type"]) == "6" {
			return fault.Invalid("借贷账户不支持转账，请选择普通账户")
		}
	}
	if f.CreditRepayment != nil {
		bill["type"] = number(2)
		if *f.CreditRepayment {
			bill["type"] = number(3)
		}
	}
	if textValue(bill["type"]) == "3" && textValue(target["type"]) != "2" {
		return fault.Invalid("信用卡还款的转入账户必须是信用卡")
	}
	extra, err := billExtra(bill)
	if err != nil {
		return err
	}
	adjustment, err := signedMoney(extra["transfee"])
	if err != nil {
		return err
	}
	oldAdjustment := adjustment
	gross := Money(0)
	if before != nil {
		gross, err = billMainAmount(before)
		if err != nil {
			return err
		}
	}
	oldGross := gross
	if f.Money != nil {
		gross = *f.Money
	}
	fee, discount := max(Money(0), adjustment), max(Money(0), -adjustment)
	if f.Fee != nil {
		fee = *f.Fee
		discount = 0
	}
	if f.Discount != nil {
		discount = *f.Discount
		if f.Fee == nil {
			fee = 0
		}
	}
	if gross <= 0 || fee > gross || discount > gross || fee > 0 && discount > 0 {
		return fault.Invalid("手续费或优惠不能超过金额，也不能同时设置")
	}
	adjustment = fee - discount
	metadata := f.BillFields
	metadata.Money = nil
	metadata.Discount = nil
	if err := applyBillFields(bill, metadata); err != nil {
		return err
	}
	extra, err = billExtra(bill)
	if err != nil {
		return err
	}
	extra["transfee"] = moneyNumber(adjustment)
	financial := before == nil || gross != oldGross || adjustment != oldAdjustment || f.FromAssetID != nil || f.TargetAssetID != nil || f.BaseAmount != nil || f.TargetAmount != nil || boolValue(f.AutoConvert)
	if financial {
		source, err := c.assetCurrency(ctx, fromID)
		if err != nil {
			return err
		}
		targetCurrency, err := c.assetCurrency(ctx, targetID)
		if err != nil {
			return err
		}
		previous, _ := extra["curr"].(map[string]any)
		keep := before != nil && gross == oldGross && adjustment == oldAdjustment && textValue(before["fromid"]) == fromID && textValue(before["targetid"]) == targetID
		curr, err := c.currencyExtra(ctx, source, targetCurrency, gross, gross-fee, f.BillFields, previous, keep)
		if err != nil {
			return err
		}
		if curr == nil {
			delete(extra, "curr")
		} else {
			extra["curr"] = curr
		}
		// Qianji stores principal after removing the fee or discount from the
		// entered main amount. A discount reduces the debit, not the target credit.
		bill["money"] = moneyNumber(gross - absMoney(adjustment))
	}
	bill["extra"] = extra
	bill["updatetime"] = number(time.Now().Unix())
	bill["status"] = number(2)
	bill["fromact"], bill["targetact"] = textValue(from["name"]), textValue(target["name"])
	bill["descinfo"] = textValue(from["name"]) + "->" + textValue(target["name"])
	return c.validate(ctx, bill)
}
