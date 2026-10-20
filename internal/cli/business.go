package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/service"
	"ledger-lens/internal/store"
)

func writeCommand(command string) bool {
	switch command {
	case "bills create", "bills update", "bills delete", "transfers create", "transfers update", "refunds create", "refunds update", "refunds delete", "reimbursements create", "reimbursements delete", "reimbursements cancel", "tags create", "tags archive", "assets set-balance", "budgets set":
		return true
	}
	return false
}

func executeBusiness(ctx context.Context, svc *service.Service, command string, in io.Reader) (any, error) {
	switch command {
	case "transfers create":
		var input service.TransferFields
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.CreateTransfer(ctx, input)
	case "transfers update":
		var input service.UpdateTransfer
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.UpdateTransfer(ctx, input)
	case "refunds create":
		var input service.CreateRefund
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.CreateRefund(ctx, input)
	case "refunds update":
		var input service.UpdateRefund
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.UpdateRefund(ctx, input)
	case "refunds delete", "reimbursements delete":
		var input struct {
			ID string `json:"id"`
		}
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		kind := "refund"
		if command == "reimbursements delete" {
			kind = "reimbursement"
		}
		return svc.DeleteRelated(ctx, input.ID, kind)
	case "reimbursements create":
		var input service.ReimburseInput
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.Reimburse(ctx, input)
	case "reimbursements cancel":
		var input struct {
			IDs []string `json:"source_ids"`
		}
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.CancelReimbursements(ctx, input.IDs)
	case "tags create":
		var input service.CreateTag
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.CreateTag(ctx, input)
	case "tags archive":
		var input service.ArchiveTag
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.ArchiveTag(ctx, input)
	case "assets set-balance":
		var input service.SetAssetBalance
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.SetAssetBalance(ctx, input)
	case "budgets set":
		var input service.SetBudgets
		if err := readJSON(in, &input); err != nil {
			return nil, err
		}
		return svc.SetBudgets(ctx, input)
	}
	return nil, fault.Invalid("未知账务命令")
}

func queryInput(o options, in io.Reader) (service.BillQuery, error) {
	f, err := billFilter(o)
	if err != nil {
		return service.BillQuery{}, err
	}
	q := service.BillQuery{Book: f.Book, Since: f.Since, Until: f.Until, Limit: f.Limit, Offset: f.Offset, Currency: o.currency}
	if o.filterStdin {
		q = service.BillQuery{}
		if err := readJSON(in, &q); err != nil {
			return q, err
		}
	}
	return q, nil
}

func catalogOutput(catalog store.Catalog) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(catalog.Data))
	d.UseNumber()
	var data map[string]any
	if d.Decode(&data) != nil || data == nil {
		return nil, fault.New("RESPONSE_INVALID", "资料格式无效，请重新查询", 4)
	}
	data["source"], data["synced_at"] = catalog.Source, catalog.SyncedAt
	return data, nil
}

func executeCatalog(ctx context.Context, svc *service.Service, command string, o options) (any, error) {
	if (command == "categories list" && (o.kind < -1 || o.kind > 1)) || (command == "assets list" && o.status != 0 && o.status != 2) || (command == "assets debts" && (o.direction != 51 && o.direction != 52 || o.status < 0 || o.status > 1)) || (command == "tags list" && (o.status != -1 && o.status != 1 && o.status != 2 || o.lastTime < 0)) {
		return nil, fault.Invalid("目录筛选参数无效，请核对 --help")
	}
	if (command == "books members" || command == "categories list" || command == "budgets list") && !qianji.ValidID(o.book, true) {
		return nil, fault.Invalid("请提供有效的账本 ID")
	}
	if command == "assets get" && !qianji.ValidID(o.assetID, false) {
		return nil, fault.Invalid("请提供有效的 --id 资产 ID")
	}
	if command == "assets trend" {
		year, month := 0, 0
		if o.month != "" || o.year != 0 {
			var err error
			if year, month, err = budgetPeriod(o); err != nil {
				return nil, err
			}
		}
		return svc.AssetTrend(ctx, o.offline, o.assetID, year, month)
	}
	if command == "user info" {
		catalog, err := svc.UserInfo(ctx, o.offline)
		if err != nil {
			return nil, err
		}
		return catalogOutput(catalog)
	}
	var read func(*qianji.Client, qianji.Session) (json.RawMessage, error)
	name := ""
	switch command {
	case "books list":
		name = "books"
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Books(ctx, a, true) }
	case "books members":
		name = "members:" + o.book
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Members(ctx, a, o.book) }
	case "assets list":
		name = fmt.Sprintf("assets:%d", o.status)
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Assets(ctx, a, o.status) }
	case "assets debts":
		name = fmt.Sprintf("debts:%d:%d", o.direction, o.status)
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return c.Debts(ctx, a, o.direction, o.status)
		}
	case "categories list":
		name = "categories:" + o.book
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return c.Categories(ctx, a, o.book, -1)
		}
	case "tags list":
		// The full directory is read and the status filter applied locally.
		name = "tags"
		if o.lastTime != 0 {
			name = fmt.Sprintf("tags:-1:%d", o.lastTime)
		}
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return c.Tags(ctx, a, -1, o.lastTime)
		}
	case "currencies list":
		name = "currencies"
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Currencies(ctx, a) }
	case "assets get":
		name = "asset:" + o.assetID
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Asset(ctx, a, o.assetID) }
	case "assets groups":
		name = "asset_groups"
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.AssetGroups(ctx, a) }
	case "installments list":
		name = "installments"
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return service.AllPages(func(page int) (qianji.ListPage, error) { return c.Installments(ctx, a, page) })
		}
	case "recurring list":
		name = "recurring"
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return service.AllPages(func(page int) (qianji.ListPage, error) { return c.RepeatTasks(ctx, a, page) })
		}
	case "savings list":
		name = "savings"
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Savings(ctx, a) }
	case "budgets list":
		year, month, err := budgetPeriod(o)
		if err != nil {
			return nil, err
		}
		name = fmt.Sprintf("budget:%s:%d:%d", o.book, year, month)
		read = func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) {
			return c.Budgets(ctx, a, o.book, year, month, "")
		}
	default:
		return nil, fault.Invalid("未知读取命令")
	}
	catalog, err := svc.Catalog(ctx, name, o.offline, read)
	if err != nil {
		return nil, err
	}
	catalog, err = service.FilterCatalog(catalog, o.nameKeyword)
	if err != nil {
		return nil, err
	}
	data, err := catalogOutput(catalog)
	if err != nil {
		return nil, err
	}
	if command == "books list" && !o.includeHidden || command == "categories list" && o.kind != -1 {
		values, _ := data["list"].([]any)
		filtered := []any{}
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				return nil, fault.New("RESPONSE_INVALID", "资料格式无效，请重新查询", 4)
			}
			if command == "books list" && fmt.Sprint(item["visible"]) == "0" {
				continue
			}
			if command == "categories list" && fmt.Sprint(item["type"]) != strconv.Itoa(o.kind) {
				continue
			}
			filtered = append(filtered, item)
		}
		data["list"] = filtered
	}
	if command == "tags list" && o.status != -1 {
		groups, _ := data["list"].([]any)
		for _, value := range groups {
			group, ok := value.(map[string]any)
			if !ok {
				return nil, fault.New("RESPONSE_INVALID", "资料格式无效，请重新查询", 4)
			}
			tags, _ := group["tags"].([]any)
			kept := []any{}
			for _, tag := range tags {
				if item, ok := tag.(map[string]any); ok && fmt.Sprint(item["status"]) == strconv.Itoa(o.status) {
					kept = append(kept, item)
				}
			}
			group["tags"] = kept
		}
	}
	return data, nil
}

func budgetPeriod(o options) (int, int, error) {
	if (o.month == "") == (o.year == 0) {
		return 0, 0, fault.Invalid("请提供 --month YYYY-MM 或 --year YYYY 中的一项")
	}
	if o.month != "" {
		date, err := time.Parse("2006-01", o.month)
		if err != nil {
			return 0, 0, fault.Invalid("月份格式必须为 YYYY-MM")
		}
		return date.Year(), int(date.Month()), nil
	}
	return o.year, 0, nil
}

func catalogCommand(command string) bool {
	switch command {
	case "categories list", "tags list", "currencies list", "budgets list", "user info", "installments list", "recurring list", "savings list":
		return true
	}
	return strings.HasPrefix(command, "books ") || strings.HasPrefix(command, "assets ") && !writeCommand(command)
}
