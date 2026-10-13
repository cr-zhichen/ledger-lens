package service

import (
	"context"
	"encoding/json"
	"math/big"
	"sort"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

type ReimburseInput struct {
	SourceIDs []string `json:"source_ids"`
	RelatedFields
	ConfirmUpgrade bool `json:"confirm_upgrade,omitempty"`
}
type reimbursementSource struct {
	id        string
	bill      map[string]any
	relations map[string]Money
	amount    Money
	curr      map[string]any
}

func (s *Service) Reimburse(ctx context.Context, input ReimburseInput) (WriteResult, error) {
	if err := validateBatch(input.SourceIDs); err != nil {
		return WriteResult{}, err
	}
	if err := input.fields().validate(); err != nil {
		return WriteResult{}, err
	}
	if input.Money == nil {
		return WriteResult{}, fault.Invalid("报销需要 money 总金额")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return WriteResult{}, err
	}
	defer lock.Close()
	catalog := newWriteCatalog(s)
	asset := "-1"
	if input.AssetID != nil {
		asset = *input.AssetID
	}
	when := time.Now().Unix()
	if input.Time != nil {
		when = *input.Time
	}
	remark := ""
	if input.Remark != nil {
		remark = *input.Remark
	}
	tags := []string{}
	if input.TagIDs != nil {
		tags = *input.TagIDs
	}
	var sources []reimbursementSource
	load := func() (qianji.ReimbursementRequest, error) {
		if _, err := s.Sync(ctx, false); err != nil {
			return qianji.ReimbursementRequest{}, err
		}
		sources = nil
		sourceCurrency := ""
		for _, id := range input.SourceIDs {
			bill, err := s.writableBill(ctx, id)
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			if textValue(bill["type"]) != "5" {
				return qianji.ReimbursementRequest{}, fault.Invalid("报销只能选择待报销支出")
			}
			extra, err := billExtra(bill)
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			// The old marker is permitted only as a migration precondition; no
			// legacy relationship is interpreted or carried into new allocations.
			rels, err := relationship(extra, "bxs")
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			currency, err := catalog.sourceCurrency(ctx, bill)
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			if sourceCurrency != "" && sourceCurrency != currency {
				return qianji.ReimbursementRequest{}, fault.Invalid("同一批报销的原币币种必须一致")
			}
			sourceCurrency = currency
			sources = append(sources, reimbursementSource{id: id, bill: bill, relations: rels})
		}
		sort.Slice(sources, func(i, j int) bool {
			a, _ := jsonNumberInt(sources[i].bill["time"])
			b, _ := jsonNumberInt(sources[j].bill["time"])
			if a != b {
				return a < b
			}
			if len(sources[i].id) != len(sources[j].id) {
				return len(sources[i].id) < len(sources[j].id)
			}
			return sources[i].id < sources[j].id
		})
		validation := cloneBill(sources[0].bill)
		validation["assetid"] = json.Number(asset)
		validation["extra"] = map[string]any{"tags": stringValues(tags)}
		if err := catalog.validate(ctx, validation); err != nil {
			return qianji.ReimbursementRequest{}, err
		}
		target, err := catalog.assetCurrency(ctx, asset)
		if err != nil {
			return qianji.ReimbursementRequest{}, err
		}
		curr, err := catalog.currencyExtra(ctx, sourceCurrency, target, *input.Money, *input.Money, input.fields(), nil, false)
		if err != nil {
			return qianji.ReimbursementRequest{}, err
		}
		remaining := *input.Money
		amounts := make([]Money, len(sources))
		for i := range sources {
			extra, err := billExtra(sources[i].bill)
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			gross, err := billMainAmount(sources[i].bill)
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			adjustment, err := signedMoney(extra["transfee"])
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			refunds, err := relationship(extra, "rfds")
			if err != nil {
				return qianji.ReimbursementRequest{}, err
			}
			available := max(Money(0), gross-absMoney(adjustment)-relationSum(refunds)-relationSum(sources[i].relations))
			amount := min(available, remaining)
			if i == len(sources)-1 {
				amount = remaining
			}
			sources[i].amount = amount
			amounts[i] = amount
			remaining -= amount
		}
		parts := map[string][]Money{}
		if curr != nil {
			for _, key := range []string{"bv", "tv"} {
				if curr[key] != nil {
					total, err := moneyValue(curr[key])
					if err != nil {
						return qianji.ReimbursementRequest{}, err
					}
					parts[key] = splitAmount(total, amounts, *input.Money)
				}
			}
		}
		allocations := map[string]any{}
		for i := range sources {
			source := &sources[i]
			if curr == nil {
				allocations[source.id] = map[string]any{"money": moneyNumber(source.amount)}
			} else {
				source.curr = cloneBill(curr)
				source.curr["sv"] = moneyNumber(source.amount)
				for key, values := range parts {
					source.curr[key] = moneyNumber(values[i])
				}
				allocations[source.id] = map[string]any{"curr": source.curr}
			}
		}
		return qianji.ReimbursementRequest{Allocations: allocations, AssetID: asset, Time: when, Remark: remark, TagIDs: tags}, nil
	}
	request, err := load()
	if err != nil {
		return WriteResult{}, err
	}
	result := resultIDs(input.SourceIDs...)
	client, err := s.mutationClient(ctx)
	if err != nil {
		return result, err
	}
	returned, err := client.Reimburse(ctx, s.Account.Session, request)
	if fault.Is(err, "REIMBURSEMENT_UPGRADE_REQUIRED") && input.ConfirmUpgrade {
		if err := client.UpgradeReimbursement(ctx, s.Account.Session); err != nil {
			return mutationError(result, err)
		}
		request, err = load()
		if err != nil {
			return writeFailure(result, "REIMBURSEMENT_UPGRADED_BUT_FAILED", "报销已升级，但账单未重新同步；请核对后再报销")
		}
		client, err = s.mutationClient(ctx)
		if err != nil {
			return result, err
		}
		returned, err = client.Reimburse(ctx, s.Account.Session, request)
	}
	if err != nil {
		return mutationError(result, err)
	}
	result, err = s.acceptRelated(ctx, result, returned)
	if err != nil {
		return result, err
	}
	ids := append([]string{}, input.SourceIDs...)
	for _, source := range sources {
		bill, err := s.writableBill(ctx, source.id)
		if err != nil {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销后源账单未同步，请核对")
		}
		extra, err := billExtra(bill)
		if err != nil {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销后源账单无效，请核对")
		}
		after, err := relationship(extra, "bxs")
		if err != nil || relationSum(after)-relationSum(source.relations) != source.amount || !relationsUnchanged(source.relations, after, "") {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销金额或原关联未核对一致，请同步检查")
		}
		newIDs := []string{}
		for id := range after {
			if !hasRelationship(source.relations, id) {
				newIDs = append(newIDs, id)
			}
		}
		if len(newIDs) > 1 || source.amount > 0 && len(newIDs) != 1 {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销关联记录未核对一致，请同步检查")
		}
		for _, id := range newIDs {
			child, err := s.writableBill(ctx, id)
			if err != nil {
				return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销入账未同步，请核对")
			}
			childExtra, err := billExtra(child)
			if err != nil {
				return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销入账无效，请核对")
			}
			if textValue(child["type"]) != "21" || textValue(childExtra["bxsid"]) != source.id || !sameValue(child["money"], moneyNumber(source.amount)) || !sameValue(childExtra["curr"], source.curr) || textValue(child["assetid"]) != asset || textValue(child["remark"]) != remark || !sameValue(child["time"], number(when)) || !sameValue(tagStrings(childExtra), tags) {
				return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "报销入账内容未核对一致，请同步检查")
			}
			ids = append(ids, id)
		}
	}
	return s.confirmedRecords(ctx, ids, result.SyncedAt)
}

func relationSum(m map[string]Money) Money {
	var total Money
	for _, v := range m {
		total += v
	}
	return total
}

// Largest remainders allocate whole cents and preserve the supplied batch total.
func splitAmount(total Money, weights []Money, sum Money) []Money {
	parts := make([]Money, len(weights))
	remainders := make([]*big.Int, len(weights))
	var used Money
	for i, w := range weights {
		n := new(big.Int).Mul(big.NewInt(int64(total)), big.NewInt(int64(w)))
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(n, big.NewInt(int64(sum)), r)
		parts[i] = Money(q.Int64())
		remainders[i] = r
		used += parts[i]
	}
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return remainders[order[i]].Cmp(remainders[order[j]]) > 0 })
	for i := Money(0); i < total-used; i++ {
		parts[order[int(i)]]++
	}
	return parts
}

func (s *Service) CancelReimbursements(ctx context.Context, ids []string) (WriteResult, error) {
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
	children := map[string]string{}
	for _, id := range ids {
		source, err := s.writableBill(ctx, id)
		if err != nil {
			return WriteResult{}, err
		}
		if textValue(source["type"]) != "5" {
			return WriteResult{}, fault.Invalid("取消报销需要待报销源账单 ID")
		}
		extra, err := billExtra(source)
		if err != nil {
			return WriteResult{}, err
		}
		rels, err := relationship(extra, "bxs")
		if err != nil {
			return WriteResult{}, err
		}
		if len(rels) == 0 {
			return WriteResult{}, fault.Invalid("所选账单没有报销记录")
		}
		for child := range rels {
			bill, err := s.writableBill(ctx, child)
			if err != nil {
				return WriteResult{}, err
			}
			childExtra, err := billExtra(bill)
			if err != nil {
				return WriteResult{}, err
			}
			if textValue(bill["type"]) != "21" || textValue(childExtra["bxsid"]) != id {
				return WriteResult{}, invalidWriteData()
			}
			children[child] = id
		}
	}
	result := resultIDs(ids...)
	client, err := s.mutationClient(ctx)
	if err != nil {
		return result, err
	}
	if err := client.CancelReimbursement(ctx, s.Account.Session, ids); err != nil {
		return mutationError(result, err)
	}
	result, err = s.acceptRelated(ctx, result, nil)
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		source, err := s.writableBill(ctx, id)
		if err != nil {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "取消报销后源账单未同步，请核对")
		}
		extra, err := billExtra(source)
		if err != nil {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "取消报销后源账单无效，请核对")
		}
		rels, err := relationship(extra, "bxs")
		if err != nil || len(rels) > 0 {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "取消报销后关联仍存在，请同步核对")
		}
	}
	for id := range children {
		if _, err := s.Store.GetBill(ctx, s.Account.Owner(), id); !fault.Is(err, "NOT_FOUND") {
			return writeFailure(result, "WRITE_CONFIRMATION_FAILED", "取消报销后入账记录仍存在，请同步核对")
		}
	}
	return s.confirmedRecords(ctx, ids, result.SyncedAt)
}
