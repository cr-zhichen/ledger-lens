package service

import (
	"context"
	"encoding/json"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type RelatedFields struct {
	Money        *Money    `json:"money,omitempty"`
	AssetID      *string   `json:"asset_id,omitempty"`
	Time         *int64    `json:"time,omitempty"`
	Remark       *string   `json:"remark,omitempty"`
	TagIDs       *[]string `json:"tag_ids,omitempty"`
	BaseAmount   *Money    `json:"base_amount,omitempty"`
	TargetAmount *Money    `json:"target_amount,omitempty"`
	AutoConvert  *bool     `json:"auto_convert,omitempty"`
}

func (f RelatedFields) fields() BillFields {
	return BillFields{Money: f.Money, AssetID: f.AssetID, Time: f.Time, Remark: f.Remark, TagIDs: f.TagIDs, BaseAmount: f.BaseAmount, TargetAmount: f.TargetAmount, AutoConvert: f.AutoConvert}
}

type CreateRefund struct {
	SourceID string `json:"source_id"`
	RelatedFields
}
type UpdateRefund struct {
	ID    string        `json:"id"`
	Patch RelatedFields `json:"patch"`
}

func (s *Service) mutationClient(ctx context.Context) (*qianji.Client, error) {
	a, err := s.Store.Account(ctx)
	if err != nil {
		return nil, err
	}
	if a.Revision != s.Account.Revision {
		return nil, fault.New("STATE_CHANGED", "登录状态已改变，请核对账号后重新执行", 1)
	}
	return s.client(ctx, s.Account.BaseURL)
}
func resultIDs(ids ...string) WriteResult {
	r := WriteResult{Items: []WriteItem{}}
	for _, id := range ids {
		r.Items = append(r.Items, WriteItem{Index: len(r.Items), ID: id, Status: "unknown"})
	}
	return r
}
func mutationError(result WriteResult, err error) (WriteResult, error) {
	for _, code := range []string{"AUTH_REQUIRED", "TOKEN_EXPIRED", "BUSINESS_ERROR", "SIGNATURE_REJECTED", "REIMBURSEMENT_UPGRADE_REQUIRED", "STATE_CHANGED", "VIP_REQUIRED"} {
		if fault.Is(err, code) {
			return result, err
		}
	}
	return writeFailure(result, "WRITE_UNCERTAIN", "写入结果无法确认，请先同步核对关联账单，不要直接重试")
}
func (s *Service) acceptRelated(ctx context.Context, result WriteResult, returned []json.RawMessage) (WriteResult, error) {
	for i := range result.Items {
		result.Items[i].Status = "applied"
	}
	if len(returned) > 0 {
		before, err := s.Store.CacheState(ctx, s.Account.Owner())
		if err != nil {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受操作，但缓存未更新，请同步核对")
		}
		if _, err := s.Store.ApplySync(ctx, s.Account, before, []qianji.PullPage{{Changes: returned, LastTimes: before.LastTimes}}, false); err != nil {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受操作，但返回账单无法缓存，请同步核对")
		}
	}
	if err := s.Store.InvalidateAssets(ctx, s.Account.Owner()); err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受操作，账户缓存未刷新，请同步核对")
	}
	sync, err := s.Sync(ctx, false)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受操作，但同步未完成，请同步核对，不要重复写入")
	}
	result.SyncedAt = sync.SyncedAt
	return result, nil
}
func (s *Service) confirmedRecords(ctx context.Context, ids []string, syncedAt string) (WriteResult, error) {
	r := resultIDs(ids...)
	r.SyncedAt = syncedAt
	for i, id := range ids {
		raw, err := s.Store.GetBill(ctx, s.Account.Owner(), id)
		if err != nil {
			return writeFailure(r, "WRITE_CONFIRMATION_FAILED", "同步中缺少关联账单，请同步核对")
		}
		r.Items[i].Status = "applied"
		r.Items[i].Confirmed = true
		r.Items[i].Bill = raw
	}
	return r, nil
}

func (s *Service) CreateRefund(ctx context.Context, input CreateRefund) (WriteResult, error) {
	return s.refund(ctx, input.SourceID, "", input.RelatedFields)
}
func (s *Service) UpdateRefund(ctx context.Context, input UpdateRefund) (WriteResult, error) {
	if input.Patch.fields().empty() {
		return WriteResult{}, fault.Invalid("patch 至少需要一个要修改的字段")
	}
	return s.refund(ctx, "", input.ID, input.Patch)
}

func (s *Service) refund(ctx context.Context, sourceID, refundID string, f RelatedFields) (WriteResult, error) {
	if err := f.fields().validate(); err != nil {
		return WriteResult{}, err
	}
	if refundID == "" {
		if !qianji.ValidID(sourceID, false) || f.Money == nil {
			return WriteResult{}, fault.Invalid("新增退款需要 source_id 和 money")
		}
	} else if !qianji.ValidID(refundID, false) {
		return WriteResult{}, fault.Invalid("退款 ID 无效")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	var previous map[string]any
	if refundID != "" {
		previous, err = s.writableBill(ctx, refundID)
		if err != nil {
			return WriteResult{}, err
		}
		if textValue(previous["type"]) != "20" {
			return WriteResult{}, fault.Invalid("此命令只修改退款账单")
		}
		extra, err := billExtra(previous)
		if err != nil {
			return WriteResult{}, err
		}
		sourceID = textValue(extra["refundsid"])
		if !qianji.ValidID(sourceID, false) {
			return WriteResult{}, invalidWriteData()
		}
	}
	source, err := s.writableBill(ctx, sourceID)
	if err != nil {
		return WriteResult{}, err
	}
	if textValue(source["type"]) != "0" && textValue(source["type"]) != "5" {
		return WriteResult{}, fault.Invalid("退款源账单必须是普通支出或待报销支出")
	}
	extra, err := billExtra(source)
	if err != nil {
		return WriteResult{}, err
	}
	if _, err := relatedIDs(extra); err != nil {
		return WriteResult{}, err
	}
	before, err := relationship(extra, "rfds")
	if err != nil {
		return WriteResult{}, err
	}
	if refundID == "" && len(before) >= 25 {
		return WriteResult{}, fault.Invalid("此账单的退款记录已满，请先在钱迹中处理")
	}
	if refundID != "" {
		if _, ok := before[refundID]; !ok {
			return WriteResult{}, invalidWriteData()
		}
	}
	asset := textValue(source["assetid"])
	amount := Money(0)
	remark := ""
	tags := []string{}
	when := time.Now().Unix()
	sourceTime, err := jsonNumberInt(source["time"])
	if err != nil {
		return WriteResult{}, err
	}
	when = max(when, sourceTime+1)
	var oldCurrency map[string]any
	if previous != nil {
		asset = textValue(previous["assetid"])
		amount, err = billMainAmount(previous)
		if err != nil {
			return WriteResult{}, err
		}
		remark = textValue(previous["remark"])
		when, err = jsonNumberInt(previous["time"])
		if err != nil {
			return WriteResult{}, err
		}
		prevExtra, err := billExtra(previous)
		if err != nil {
			return WriteResult{}, err
		}
		oldCurrency, _ = prevExtra["curr"].(map[string]any)
		tags = tagStrings(prevExtra)
	}
	oldAmount, oldAsset := amount, asset
	if f.AssetID != nil {
		asset = *f.AssetID
	}
	if f.Money != nil {
		amount = *f.Money
	}
	if f.Time != nil {
		when = *f.Time
	}
	if f.Remark != nil {
		remark = *f.Remark
	}
	if f.TagIDs != nil {
		tags = *f.TagIDs
	}
	if when <= sourceTime {
		return WriteResult{}, fault.Invalid("退款时间必须晚于原账单时间")
	}
	catalog := newWriteCatalog(s)
	validation := cloneBill(source)
	validation["assetid"] = json.Number(asset)
	validation["extra"] = map[string]any{"tags": stringValues(tags)}
	if err := catalog.validate(ctx, validation); err != nil {
		return WriteResult{}, err
	}
	sourceCurrency, err := catalog.sourceCurrency(ctx, source)
	if err != nil {
		return WriteResult{}, err
	}
	targetCurrency, err := catalog.assetCurrency(ctx, asset)
	if err != nil {
		return WriteResult{}, err
	}
	curr, err := catalog.currencyExtra(ctx, sourceCurrency, targetCurrency, amount, amount, f.fields(), oldCurrency, previous != nil && oldAmount == amount && oldAsset == asset)
	if err != nil {
		return WriteResult{}, err
	}
	payload := map[string]any{"money": moneyNumber(amount), "time": number(when)}
	if asset != "-1" {
		payload["assetid"] = json.Number(asset)
	}
	if remark != "" {
		payload["remark"] = remark
	}
	if len(tags) > 0 {
		payload["tags"] = tags
	}
	if refundID != "" {
		payload["billid"] = json.Number(refundID)
	}
	if curr != nil {
		raw, _ := json.Marshal(curr)
		payload["currency"] = string(raw)
	}
	result := resultIDs(sourceID)
	if refundID != "" {
		result = resultIDs(sourceID, refundID)
	}
	client, err := s.mutationClient(ctx)
	if err != nil {
		return result, err
	}
	returned, err := client.Refund(ctx, s.Account.Session, sourceID, payload)
	if err != nil {
		return mutationError(result, err)
	}
	result, err = s.acceptRelated(ctx, result, returned)
	if err != nil {
		return result, err
	}
	updated, err := s.writableBill(ctx, sourceID)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款后未找到原账单，请同步核对")
	}
	updatedExtra, err := billExtra(updated)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款后关联信息无效，请同步核对")
	}
	after, err := relationship(updatedExtra, "rfds")
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款后关联信息无效，请同步核对")
	}
	candidates := []string{}
	for id, value := range after {
		if (refundID != "" && id == refundID) || (refundID == "" && !hasRelationship(before, id)) {
			if value == amount {
				candidates = append(candidates, id)
			}
		}
	}
	expectedCount := len(before)
	if refundID == "" {
		expectedCount++
	}
	if len(candidates) != 1 || len(after) != expectedCount || !relationsUnchanged(before, after, refundID) {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款金额或原账单关联未核对一致，请同步检查")
	}
	id := candidates[0]
	bill, err := s.writableBill(ctx, id)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款记录未同步，请核对")
	}
	billExtra, err := billExtra(bill)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款记录无效，请核对")
	}
	if textValue(bill["type"]) != "20" || textValue(billExtra["refundsid"]) != sourceID || !sameValue(bill["money"], moneyNumber(amount)) || textValue(bill["assetid"]) != asset || textValue(bill["remark"]) != remark || !sameValue(bill["time"], number(when)) || !sameValue(tagStrings(billExtra), tags) || !sameValue(billExtra["curr"], curr) {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "退款内容未核对一致，请同步检查")
	}
	return s.confirmedRecords(ctx, []string{sourceID, id}, result.SyncedAt)
}
func tagStrings(extra map[string]any) []string {
	tags := []string{}
	if values, ok := extra["tags"].([]any); ok {
		for _, value := range values {
			tags = append(tags, textValue(value))
		}
	}
	return tags
}
func stringValues(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
func hasRelationship(m map[string]Money, id string) bool { _, ok := m[id]; return ok }
func relationsUnchanged(before, after map[string]Money, except string) bool {
	for id, amount := range before {
		if id != except {
			value, ok := after[id]
			if !ok || value != amount {
				return false
			}
		}
	}
	return true
}

func (s *Service) DeleteRelated(ctx context.Context, id, kind string) (WriteResult, error) {
	if !qianji.ValidID(id, false) {
		return WriteResult{}, fault.Invalid("账单 ID 无效")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	if _, err := s.Sync(ctx, false); err != nil {
		return WriteResult{}, err
	}
	bill, err := s.writableBill(ctx, id)
	if err != nil {
		return WriteResult{}, err
	}
	typeID, key, sourceKey := "20", "rfds", "refundsid"
	if kind == "reimbursement" {
		typeID, key, sourceKey = "21", "bxs", "bxsid"
	}
	if textValue(bill["type"]) != typeID {
		return WriteResult{}, fault.Invalid("账单类型与删除命令不符")
	}
	extra, err := billExtra(bill)
	if err != nil {
		return WriteResult{}, err
	}
	sourceID := textValue(extra[sourceKey])
	if !qianji.ValidID(sourceID, false) {
		return WriteResult{}, invalidWriteData()
	}
	source, err := s.writableBill(ctx, sourceID)
	if err != nil {
		return WriteResult{}, err
	}
	sourceExtra, err := billExtra(source)
	if err != nil {
		return WriteResult{}, err
	}
	before, err := relationship(sourceExtra, key)
	if err != nil {
		return WriteResult{}, err
	}
	if !hasRelationship(before, id) {
		return WriteResult{}, invalidWriteData()
	}
	result, err := s.writeBills(ctx, "delete", qianji.BillChanges{Deletes: []json.Number{json.Number(id)}})
	if err != nil {
		return result, err
	}
	source, err = s.writableBill(ctx, sourceID)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "删除后原账单未同步，请核对")
	}
	sourceExtra, err = billExtra(source)
	if err != nil {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "删除后原账单无效，请核对")
	}
	after, err := relationship(sourceExtra, key)
	if err != nil || len(after) != len(before)-1 || hasRelationship(after, id) || !relationsUnchanged(before, after, id) {
		return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "关联账单已删除，但原账单关联未核对一致，请同步检查")
	}
	return result, nil
}
