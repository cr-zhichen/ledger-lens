package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

// AssetTrend filters the full snapshot list. The app only ever requests every
// snapshot (did=-1, empty filter), so the period and asset are applied here.
func (s *Service) AssetTrend(ctx context.Context, offline bool, assetID string, year, month int) (map[string]any, error) {
	if assetID != "" && !qianji.ValidID(assetID, false) {
		return nil, fault.Invalid("资产 ID 必须是正整数")
	}
	catalog, err := s.Catalog(ctx, "assetline", offline, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
		return c.AssetTrend(ctx, a)
	})
	if err != nil {
		return nil, err
	}
	data, err := decodeObject(catalog.Data)
	if err != nil {
		return nil, err
	}
	values, _ := data["list"].([]any)
	if data["list"] != nil && values == nil {
		return nil, invalidWriteData()
	}
	var since, until int64
	if year > 0 {
		start, end := periodBounds(year, month)
		since, until = start.Unix(), end.Unix()
	}
	list := []any{}
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, invalidWriteData()
		}
		if assetID != "" && textValue(item["assetid"]) != assetID {
			continue
		}
		if year > 0 {
			when, err := jsonNumberInt(item["time"])
			if err != nil || when < since || when >= until {
				continue
			}
		}
		list = append(list, item)
	}
	result := map[string]any{"list": list, "total": len(list), "source": catalog.Source, "synced_at": catalog.SyncedAt}
	if year > 0 {
		result["since"], result["until"] = since, until
	}
	return result, nil
}

// Month is 0 for a whole year.
func periodBounds(year, month int) (time.Time, time.Time) {
	if month == 0 {
		start := time.Date(year, 1, 1, 0, 0, 0, 0, shanghai)
		return start, start.AddDate(1, 0, 0)
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, shanghai)
	return start, start.AddDate(0, 1, 0)
}

// SignedMoney stores cents and, unlike Money, accepts negative balances.
type SignedMoney int64

var signedMoneyPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,9})(\.[0-9]{1,2})?$`)

func (m *SignedMoney) UnmarshalJSON(raw []byte) error {
	value := string(raw)
	if !signedMoneyPattern.MatchString(value) {
		return fault.Invalid("余额必须是最多两位小数的数字，范围为 -9999999999.99～9999999999.99")
	}
	negative := strings.HasPrefix(value, "-")
	var amount Money
	if err := amount.UnmarshalJSON([]byte(strings.TrimPrefix(value, "-"))); err != nil {
		return err
	}
	*m = SignedMoney(amount)
	if negative {
		*m = -*m
	}
	return nil
}

func (m SignedMoney) MarshalJSON() ([]byte, error) { return Money(m).MarshalJSON() }

type SetAssetBalance struct {
	AssetID                  string       `json:"asset_id"`
	Balance                  *SignedMoney `json:"balance"`
	RecordDiff               bool         `json:"record_diff,omitempty"`
	CategoryID               *string      `json:"category_id,omitempty"`
	Remark                   *string      `json:"remark,omitempty"`
	ExcludeFromIncomeExpense bool         `json:"exclude_from_income_expense,omitempty"`
	ExcludeFromBudget        bool         `json:"exclude_from_budget,omitempty"`
}

type AssetBalanceResult struct {
	AssetID         string          `json:"asset_id"`
	PreviousBalance SignedMoney     `json:"previous_balance"`
	Balance         SignedMoney     `json:"balance"`
	Status          string          `json:"status"`
	Confirmed       bool            `json:"confirmed"`
	DiffBillID      string          `json:"diff_bill_id,omitempty"`
	Asset           json.RawMessage `json:"asset,omitempty"`
	SyncedAt        string          `json:"synced_at,omitempty"`
}

// Gson fields of AssetAccount; asset/submit sends the whole object.
var assetAccountKeys = []string{"color", "createtime", "credit", "currency", "extra", "groupid", "icon", "id", "incount", "installs", "lastPayTime", "loan", "money", "name", "remark", "sort", "status", "stype", "type", "usecount", "userid"}

func (s *Service) SetAssetBalance(ctx context.Context, input SetAssetBalance) (AssetBalanceResult, error) {
	if !qianji.ValidID(input.AssetID, false) || input.Balance == nil {
		return AssetBalanceResult{}, fault.Invalid("需要 asset_id 和 balance")
	}
	if !input.RecordDiff && (input.CategoryID != nil || input.Remark != nil || input.ExcludeFromIncomeExpense || input.ExcludeFromBudget) {
		return AssetBalanceResult{}, fault.Invalid("category_id、remark 和统计开关只用于差额账单，请同时设置 record_diff=true")
	}
	if input.CategoryID != nil && !qianji.ValidID(*input.CategoryID, false) {
		return AssetBalanceResult{}, fault.Invalid("分类 ID 无效，请查询 categories list")
	}
	if input.Remark != nil && (utf8.RuneCountInString(*input.Remark) > 500 || strings.IndexFunc(*input.Remark, unicode.IsControl) >= 0) {
		return AssetBalanceResult{}, fault.Invalid("备注不能含控制字符，且不超过 500 个字符")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return AssetBalanceResult{}, err
	}
	defer lock.Close()
	asset, err := s.freshAsset(ctx, input.AssetID)
	if err != nil {
		return AssetBalanceResult{}, err
	}
	if textValue(asset["type"]) == "5" {
		return AssetBalanceResult{}, fault.New("ASSET_UNSUPPORTED", "借贷账户请在钱迹中修改", 2)
	}
	if installs, _ := asset["installs"].([]any); len(installs) > 0 {
		return AssetBalanceResult{}, fault.New("ASSET_UNSUPPORTED", "有分期的信用卡请在钱迹中校准余额", 2)
	}
	previous, err := signedMoneyValue(asset["money"])
	if err != nil {
		return AssetBalanceResult{}, err
	}
	if previous == *input.Balance {
		return AssetBalanceResult{}, fault.Invalid("余额与当前余额相同，无需校准")
	}
	if input.CategoryID != nil {
		// The diff bill is an expense when the balance drops and income otherwise;
		// upstream answers HTTP 500 for a category of the other type.
		index, err := (&readContext{service: s}).loadCategories(ctx, "-1")
		if err != nil {
			return AssetBalanceResult{}, err
		}
		category := index[*input.CategoryID]
		if category == nil {
			return AssetBalanceResult{}, missingReference("分类")
		}
		want := "1"
		if *input.Balance < previous {
			want = "0"
		}
		if textValue(category["type"]) != want {
			return AssetBalanceResult{}, fault.Invalid("余额减少时差额账单为支出，增加时为收入，category_id 的类型须一致")
		}
	}
	payload := map[string]any{}
	for _, key := range assetAccountKeys {
		if value, ok := asset[key]; ok {
			payload[key] = value
		}
	}
	balanceJSON, _ := input.Balance.MarshalJSON()
	payload["money"] = json.Number(balanceJSON)
	var diff map[string]any
	if input.RecordDiff {
		category := json.Number("-1")
		if input.CategoryID != nil {
			category = json.Number(*input.CategoryID)
		}
		flag := 0
		if input.ExcludeFromIncomeExpense {
			flag |= 1
		}
		if input.ExcludeFromBudget {
			flag |= 2
		}
		diff = map[string]any{"cateid": category, "billflag": flag}
		if input.Remark != nil && strings.TrimSpace(*input.Remark) != "" {
			diff["remark"] = strings.TrimSpace(*input.Remark)
		}
	}
	result := AssetBalanceResult{AssetID: input.AssetID, PreviousBalance: previous, Balance: *input.Balance, Status: "unknown"}
	client, err := s.writeClient(ctx)
	if err != nil {
		return result, err
	}
	data, err := client.SubmitAsset(ctx, s.Account.Session, payload, diff)
	if err != nil {
		if definiteFailure(err) {
			return result, err
		}
		return outcomeFailure(result, "WRITE_UNCERTAIN", "余额校准结果无法确认，请执行 assets list 核对余额，不要直接重试")
	}
	result.Status = "applied"
	var returned struct {
		Bill map[string]any `json:"bill"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	_ = decoder.Decode(&returned)
	if returned.Bill != nil {
		result.DiffBillID = textValue(returned.Bill["id"])
	}
	if err := s.Store.InvalidateAssets(ctx, s.Account.Owner()); err != nil {
		return outcomeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受校准，但账户缓存未刷新，请执行 cache refresh")
	}
	actual, err := s.freshAsset(ctx, input.AssetID)
	if err == nil && sameValue(actual["money"], json.Number(balanceJSON)) {
		result.Confirmed = true
		result.Asset, _ = json.Marshal(actual)
	}
	if input.RecordDiff {
		sync, err := s.Sync(ctx, false)
		if err != nil {
			result.Confirmed = false
		} else {
			result.SyncedAt = sync.SyncedAt
		}
	}
	if !result.Confirmed {
		return outcomeFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受校准，但尚未核对到新余额，请执行 assets list 核对，不要重复校准")
	}
	return result, nil
}

// freshAsset reads both active and hidden asset lists online and caches them.
func (s *Service) freshAsset(ctx context.Context, id string) (map[string]any, error) {
	for _, status := range []int{0, 2} {
		status := status
		catalog, err := s.Catalog(ctx, fmt.Sprintf("assets:%d", status), false, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return c.Assets(ctx, a, status)
		})
		if err != nil {
			return nil, err
		}
		items, err := catalogItems(catalog.Data)
		if err != nil {
			return nil, err
		}
		if asset := findCatalog(items, "id", id); asset != nil {
			return asset, nil
		}
	}
	return nil, missingReference("资产")
}

func signedMoneyValue(value any) (SignedMoney, error) {
	var m SignedMoney
	text := textValue(value)
	if text == "" {
		return 0, invalidWriteData()
	}
	// Upstream balances may carry more than two decimals in float form.
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		text = strconv.FormatFloat(f, 'f', 2, 64)
		text = strings.TrimSuffix(strings.TrimSuffix(text, "0"), ".0")
		text = strings.TrimSuffix(text, ".")
	}
	if err := m.UnmarshalJSON([]byte(text)); err != nil {
		return 0, invalidWriteData()
	}
	return m, nil
}

// outcomeFailure attaches the partial result to a write error.
func outcomeFailure[T any](result T, code, message string) (T, error) {
	err := fault.New(code, message, 4)
	err.Details = result
	return result, err
}
