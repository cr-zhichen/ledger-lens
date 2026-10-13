package service

import (
	"context"
	"encoding/json"
	"math/big"
	"regexp"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

var currencySymbol = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

func moneyValue(value any) (Money, error) {
	var m Money
	if err := m.UnmarshalJSON([]byte(textValue(value))); err != nil {
		return 0, invalidWriteData()
	}
	return m, nil
}
func moneyNumber(m Money) json.Number { raw, _ := m.MarshalJSON(); return json.Number(raw) }
func boolValue(p *bool) bool          { return p != nil && *p }
func currencyTouched(f BillFields) bool {
	return f.Money != nil || f.Discount != nil || f.AssetID != nil || f.Currency != nil || f.BaseAmount != nil || f.TargetAmount != nil || boolValue(f.AutoConvert)
}

func (c *writeCatalog) sourceCurrency(ctx context.Context, bill map[string]any) (string, error) {
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
	return c.assetCurrency(ctx, textValue(bill["assetid"]))
}

func grossMoney(bill map[string]any) (Money, error) {
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
	discount, err := billDiscount(extra)
	return net + discount, err
}

// converted uses exact decimals: half-up at ten rate decimals, then half-even
// to currency cents, matching the reference app's conversion stages.
func converted(amount Money, from, to *big.Rat) (Money, error) {
	rate := new(big.Rat).Quo(from, to)
	rate.Mul(rate, big.NewRat(10000000000, 1))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(rate.Num(), rate.Denom(), r)
	if new(big.Int).Lsh(r, 1).Cmp(rate.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	v := new(big.Rat).Mul(big.NewRat(int64(amount), 1), new(big.Rat).SetFrac(q, big.NewInt(10000000000)))
	q, r = new(big.Int), new(big.Int)
	q.QuoRem(v.Num(), v.Denom(), r)
	cmp := new(big.Int).Lsh(r, 1).Cmp(v.Denom())
	if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Sign() < 0 || q.Int64() > 999999999999 {
		return 0, fault.Invalid("折算金额超出范围，请填写折算金额")
	}
	return Money(q.Int64()), nil
}

func (c *writeCatalog) convert(ctx context.Context, amount Money, source, target string) (Money, error) {
	if source == target {
		return amount, nil
	}
	list, err := c.load(ctx, "currencies", func(client *qianji.Client, a qianji.Session) (json.RawMessage, error) {
		return client.Currencies(ctx, a)
	})
	if err != nil {
		return 0, err
	}
	prices := map[string]*big.Rat{}
	for _, item := range list {
		value, ok := new(big.Rat).SetString(textValue(item["baseprice"]))
		if ok && value.Sign() > 0 {
			prices[textValue(item["symbol"])] = value
		}
	}
	if prices[source] == nil || prices[target] == nil {
		return 0, fault.New("CURRENCY_PRICE_UNAVAILABLE", "所选币种缺少有效汇率，请填写到账金额和本位币金额", 2)
	}
	return converted(amount, prices[source], prices[target])
}

func (c *writeCatalog) currencyExtra(ctx context.Context, source, target string, gross, effective Money, fields BillFields, previous map[string]any, keep bool) (map[string]any, error) {
	base, err := c.assetCurrency(ctx, "-1")
	if err != nil {
		return nil, err
	}
	if !currencySymbol.MatchString(source) || !currencySymbol.MatchString(target) {
		return nil, invalidWriteData()
	}
	if source == base && target == base {
		if fields.BaseAmount != nil || fields.TargetAmount != nil {
			return nil, fault.Invalid("本位币账户无需折算，请移除 base_amount 和 target_amount")
		}
		return nil, nil
	}
	resolve := func(currency string, explicit *Money, symbol, amount string, required bool) (*Money, error) {
		if currency == source {
			if explicit != nil && *explicit != effective {
				return nil, fault.Invalid("同币种金额必须与实际金额一致")
			}
			v := effective
			return &v, nil
		}
		if explicit != nil {
			return explicit, nil
		}
		if keep && previous != nil && textValue(previous[symbol]) == currency {
			v, err := moneyValue(previous[amount])
			return &v, err
		}
		if boolValue(fields.AutoConvert) {
			v, err := c.convert(ctx, effective, source, currency)
			return &v, err
		}
		if required {
			return nil, fault.Invalid("跨币种记账需要 target_amount，或设置 auto_convert=true")
		}
		return nil, nil
	}
	// Base and target are the same currency: either explicit value supplies both.
	targetInput, baseInput := fields.TargetAmount, fields.BaseAmount
	if target == base {
		if targetInput != nil && baseInput != nil && *targetInput != *baseInput {
			return nil, fault.Invalid("到账币种为本位币时，target_amount 与 base_amount 必须相等")
		}
		if targetInput == nil {
			targetInput = baseInput
		}
		if baseInput == nil {
			baseInput = targetInput
		}
	}
	targetAmount, err := resolve(target, targetInput, "ts", "tv", true)
	if err != nil {
		return nil, err
	}
	baseAmount, err := resolve(base, baseInput, "bs", "bv", false)
	if err != nil {
		return nil, err
	}
	if target == base {
		baseAmount = targetAmount
	}
	curr := map[string]any{"ss": source, "sv": moneyNumber(gross), "ts": target, "tv": moneyNumber(*targetAmount)}
	if baseAmount != nil {
		curr["bs"], curr["bv"] = base, moneyNumber(*baseAmount)
	}
	return curr, nil
}

func (c *writeCatalog) applyCurrency(ctx context.Context, bill, before map[string]any, fields BillFields) error {
	if before != nil && !currencyTouched(fields) {
		return nil
	}
	if before == nil && textValue(bill["assetid"]) == "-1" && fields.Currency == nil && fields.BaseAmount == nil && fields.TargetAmount == nil && !boolValue(fields.AutoConvert) {
		return nil
	}
	target, err := c.assetCurrency(ctx, textValue(bill["assetid"]))
	if err != nil {
		return err
	}
	source := target
	if before != nil {
		source, err = c.sourceCurrency(ctx, before)
		if err != nil {
			return err
		}
	}
	if fields.Currency != nil {
		source = *fields.Currency
	}
	gross, err := grossMoney(bill)
	if err != nil {
		return err
	}
	if fields.Money != nil {
		gross = *fields.Money
	}
	extra, err := billExtra(bill)
	if err != nil {
		return err
	}
	discount, err := billDiscount(extra)
	if err != nil {
		return err
	}
	var previous map[string]any
	keep := false
	if before != nil {
		oldExtra, err := billExtra(before)
		if err != nil {
			return err
		}
		previous, _ = oldExtra["curr"].(map[string]any)
		oldGross, err := grossMoney(before)
		if err != nil {
			return err
		}
		oldDiscount, err := billDiscount(oldExtra)
		if err != nil {
			return err
		}
		oldSource, err := c.sourceCurrency(ctx, before)
		if err != nil {
			return err
		}
		keep = oldSource == source && oldGross == gross && oldDiscount == discount
	}
	curr, err := c.currencyExtra(ctx, source, target, gross, gross-discount, fields, previous, keep)
	if err != nil {
		return err
	}
	if curr == nil {
		delete(extra, "curr")
		bill["money"] = moneyNumber(gross - discount)
	} else {
		extra["curr"] = curr
		bill["money"] = curr["tv"]
	}
	bill["extra"] = extra
	return nil
}
