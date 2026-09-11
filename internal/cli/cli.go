package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/service"
	"ledger-lens/internal/store"
	"ledger-lens/internal/update"
	"ledger-lens/internal/version"
)

const help = `账镜 · LedgerLens
用法：ledgerlens [--db PATH] [--timeout 30s] <命令> [参数]

版本与更新：
  version / --version                      查看版本、提交与构建时间
  update check                            在线检查最新正式版
  --no-update-check                       本次调用不自动检查更新

认证与缓存：
  init           --credentials-stdin       登录并完成首次账单缓存
  login          --credentials-stdin       仅登录并保存会话
  auth status                             本地会话与缓存状态
  logout                                  清除 MD5、Token，保留账单
  sync           [--full]                 增量同步；--full 完整重建缓存

本地账单：
  bills list     [--book ID] [--since YYYY-MM-DD] [--until YYYY-MM-DD]
                 [--limit 100] [--offset 0] [--fresh]
  bills get      --id ID [--fresh]

远端只读接口：
  books list     [--include-hidden]
  books members  [--book -1]
  assets list    [--status 0|2]
  assets debts   --direction 51|52 [--status 0|1]
  categories list [--book -1] [--type -1|0|1]
  tags list      [--status -1|1|2] [--lasttime 0]
  budgets list   [--book -1] --month YYYY-MM 或 --year YYYY
  currencies list
  bills pull     [--book -1] [--pageoffset 0] [--pagesign SIGN]
                 [--lasttimes JSON] 或 --cursor-stdin

登录支持 --account VALUE 加 --password VALUE 或 --password-md5 VALUE。
优先 --credentials-stdin，接收 {"account":"...","password":"..."}。
全程无交互；--non-interactive 为兼容参数。--db、--timeout 也可放在命令参数中。
日期按 Asia/Shanghai 解释，since 包含当日，until 不包含当日。
bills pull 仅返回一页变更，不更新缓存。使用 sync 获取完整缓存。
成功 JSON 写 stdout，错误 JSON 写 stderr；失败返回非零退出码。
正式版成功检查后 24 小时内复用缓存；失败至少等待 1 小时，限流遵循服务器要求。
已发现的更新每次提示，写入 stderr 的 notice 字段；到期联网检查最多 2 秒。
update check 不受自动检查间隔限制，主动联网检查。
网络异常不影响原命令；LEDGERLENS_NO_UPDATE_CHECK=1 可关闭自动检查。
`

type options struct {
	db                                           string
	timeout                                      time.Duration
	noUpdateCheck                                bool
	account, password, digest, baseURL           string
	credentialsStdin, nonInteractive             bool
	book, month, since, until, billID            string
	includeHidden, full, fresh, cursorStdin      bool
	status, direction, kind, year, limit, offset int
	lastTime, pageOffset                         int64
	pageSign, lastTimes                          string
}

type credentials struct {
	Account     string `json:"account"`
	Password    string `json:"password"`
	PasswordMD5 string `json:"password_md5"`
}

func common(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.db, "db", o.db, "SQLite 文件路径")
	fs.DurationVar(&o.timeout, "timeout", o.timeout, "单个 HTTP 请求超时")
	fs.BoolVar(&o.noUpdateCheck, "no-update-check", o.noUpdateCheck, "关闭本次自动更新检查")
}

func flags() *flag.FlagSet {
	fs := flag.NewFlagSet("ledgerlens", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(args []string) (string, options, bool, error) {
	o := options{timeout: 30 * time.Second, noUpdateCheck: os.Getenv("LEDGERLENS_NO_UPDATE_CHECK") == "1"}
	root := flags()
	common(root, &o)
	showVersion := root.Bool("version", false, "查看程序版本")
	if err := root.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return "", o, true, nil
		}
		return "", o, false, fault.Invalid("全局参数无效，请使用 --help 查看用法")
	}
	args = root.Args()
	if *showVersion {
		if len(args) != 0 || o.timeout <= 0 {
			return "", o, false, fault.Invalid("--version 不能与其他命令混用，超时必须大于零")
		}
		return "version", o, false, nil
	}
	if len(args) == 0 {
		return "", o, true, nil
	}
	if args[0] == "help" {
		return "", o, true, nil
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "auth", "books", "assets", "categories", "tags", "budgets", "currencies", "bills", "update":
		if len(rest) == 0 || rest[0] == "--help" || rest[0] == "-h" {
			return "", o, true, nil
		}
		command += " " + rest[0]
		rest = rest[1:]
	}
	fs := flags()
	common(fs, &o)
	switch command {
	case "init", "login":
		fs.StringVar(&o.account, "account", "", "登录账号")
		fs.StringVar(&o.password, "password", "", "原始密码")
		fs.StringVar(&o.digest, "password-md5", "", "密码 MD5")
		fs.BoolVar(&o.credentialsStdin, "credentials-stdin", false, "通过标准输入读取凭据 JSON")
		fs.BoolVar(&o.nonInteractive, "non-interactive", false, "使用无交互模式")
		fs.StringVar(&o.baseURL, "api-url", "", "API 根地址")
	case "auth status", "logout", "currencies list", "version", "update check":
	case "sync":
		fs.BoolVar(&o.full, "full", false, "重新全量同步")
	case "books list":
		fs.BoolVar(&o.includeHidden, "include-hidden", false, "包含隐藏账本")
	case "books members":
		fs.StringVar(&o.book, "book", "-1", "账本 ID")
	case "assets list":
		fs.IntVar(&o.status, "status", 0, "资产状态")
	case "assets debts":
		fs.IntVar(&o.direction, "direction", 0, "借贷方向")
		fs.IntVar(&o.status, "status", 0, "借贷状态")
	case "categories list":
		fs.StringVar(&o.book, "book", "-1", "账本 ID")
		fs.IntVar(&o.kind, "type", -1, "分类类型")
	case "tags list":
		fs.IntVar(&o.status, "status", -1, "标签状态")
		fs.Int64Var(&o.lastTime, "lasttime", 0, "标签时间戳")
	case "budgets list":
		fs.StringVar(&o.book, "book", "-1", "账本 ID")
		fs.StringVar(&o.month, "month", "", "月份 YYYY-MM")
		fs.IntVar(&o.year, "year", 0, "年份")
	case "bills pull":
		fs.StringVar(&o.book, "book", "-1", "同步账本 ID")
		fs.Int64Var(&o.pageOffset, "pageoffset", 0, "分页偏移")
		fs.StringVar(&o.pageSign, "pagesign", "", "分页签名")
		fs.StringVar(&o.lastTimes, "lasttimes", "", "同步基线 JSON")
		fs.BoolVar(&o.cursorStdin, "cursor-stdin", false, "标准输入读取 next_cursor JSON")
	case "bills list":
		fs.StringVar(&o.book, "book", "", "筛选账本；省略则查询全部")
		fs.StringVar(&o.since, "since", "", "起始日期（含）")
		fs.StringVar(&o.until, "until", "", "结束日期（不含）")
		fs.IntVar(&o.limit, "limit", 100, "结果数量")
		fs.IntVar(&o.offset, "offset", 0, "结果偏移")
		fs.BoolVar(&o.fresh, "fresh", false, "先同步成功再查询")
	case "bills get":
		fs.StringVar(&o.billID, "id", "", "账单 ID")
		fs.BoolVar(&o.fresh, "fresh", false, "先同步成功再查询")
	default:
		return "", o, false, fault.Invalid("未知命令，请使用 --help 查看只读命令")
	}
	if err := fs.Parse(rest); err != nil {
		if err == flag.ErrHelp {
			return command, o, true, nil
		}
		return "", o, false, fault.Invalid("命令参数无效，请使用 --help 查看用法")
	}
	if fs.NArg() != 0 || o.timeout <= 0 {
		return "", o, false, fault.Invalid("参数位置或超时设置无效")
	}
	if o.credentialsStdin && (o.account != "" || o.password != "" || o.digest != "") {
		return "", o, false, fault.Invalid("凭据标准输入不能与账号、密码参数混用")
	}
	if o.cursorStdin {
		conflict := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "book" || f.Name == "pageoffset" || f.Name == "pagesign" || f.Name == "lasttimes" {
				conflict = true
			}
		})
		if conflict {
			return "", o, false, fault.Invalid("游标标准输入不能与游标参数混用")
		}
	}
	return command, o, false, nil
}

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
	return run(ctx, args, in, out, stderr, version.Current(), update.NewClient())
}

func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, info version.Info, checker updateChecker) int {
	command, o, showHelp, err := parse(args)
	finishUpdate := startUpdateCheck(ctx, checker, info.Version, o.noUpdateCheck || (command == "update check" && !showHelp))
	if err == nil && showHelp {
		_, err = io.WriteString(out, help)
	}
	var data any
	var explicitNotice *update.Notice
	if err == nil && !showHelp {
		switch command {
		case "version":
			data = info
		case "update check":
			checkCtx, cancel := context.WithTimeout(ctx, o.timeout)
			var result update.Result
			result, err = checker.Check(checkCtx, info.Version)
			data, explicitNotice = result, result.Notice()
			cancel()
			if ctx.Err() != nil {
				err = fault.New("CANCELED", "请求已取消", 130)
			}
		default:
			data, err = execute(ctx, command, o, in)
		}
	}
	if err == nil && !showHelp {
		var payload []byte
		payload, err = outputJSON(data)
		if err == nil {
			_, err = out.Write(append(payload, '\n'))
		}
	}
	notice := finishUpdate()
	if command == "update check" && err == nil && !showHelp {
		notice = explicitNotice
	}
	if err != nil {
		public := fault.Public(err)
		_ = json.NewEncoder(stderr).Encode(struct {
			OK     bool           `json:"ok"`
			Error  *fault.Error   `json:"error"`
			Notice *update.Notice `json:"notice,omitempty"`
		}{Error: public, Notice: notice})
		return public.Exit
	}
	if notice != nil {
		_ = json.NewEncoder(stderr).Encode(struct {
			Notice *update.Notice `json:"notice"`
		}{Notice: notice})
	}
	return 0
}

func execute(ctx context.Context, command string, o options, in io.Reader) (any, error) {
	var err error
	if o.db == "" {
		o.db, err = store.DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	db, err := store.Open(o.db)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	svc, err := service.New(ctx, db, o.timeout)
	if err != nil {
		return nil, err
	}
	switch command {
	case "login", "init":
		input := credentials{Account: o.account, Password: o.password, PasswordMD5: o.digest}
		if o.credentialsStdin {
			if err := readJSON(in, &input); err != nil {
				return nil, err
			}
		}
		if err := svc.Login(ctx, input.Account, input.Password, input.PasswordMD5, o.baseURL); err != nil {
			return nil, err
		}
		if command == "init" {
			result, err := svc.Sync(ctx, false)
			if err != nil {
				return nil, fault.New("INIT_SYNC_FAILED", "登录信息已保存，同步失败（"+fault.Public(err).Code+"）；请重试 sync", fault.Public(err).Exit)
			}
			return map[string]any{"authenticated": true, "uid": svc.Account.UID, "sync": result}, nil
		}
		return map[string]any{"authenticated": true, "uid": svc.Account.UID, "auto_login": true}, nil
	case "auth status":
		state, err := db.CacheState(ctx, svc.Account.Owner())
		if err != nil {
			return nil, err
		}
		return map[string]any{"has_session": svc.Account.Token != "", "auto_login": svc.Account.AutoLogin, "uid": svc.Account.UID, "cache": state}, nil
	case "logout":
		if err := db.Logout(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"logged_out": true}, nil
	case "sync":
		return svc.Sync(ctx, o.full)
	case "bills list":
		filter, err := billFilter(o)
		if err != nil {
			return nil, err
		}
		if o.fresh {
			if _, err := svc.Sync(ctx, false); err != nil {
				return nil, err
			}
		}
		return db.ListBills(ctx, svc.Account.Owner(), filter)
	case "bills get":
		if !qianji.ValidID(o.billID, false) {
			return nil, fault.Invalid("请提供有效的 --id")
		}
		if o.fresh {
			if _, err := svc.Sync(ctx, false); err != nil {
				return nil, err
			}
		}
		return db.GetBill(ctx, svc.Account.Owner(), o.billID)
	}
	cursor := qianji.Cursor{BookID: o.book, PageOffset: o.pageOffset, PageSign: o.pageSign, LastTimes: json.RawMessage(o.lastTimes)}
	if command == "bills pull" && o.cursorStdin {
		cursor = qianji.Cursor{}
		if err := readJSON(in, &cursor); err != nil {
			return nil, err
		}
	}
	month := 0
	if command == "budgets list" {
		if (o.month == "") == (o.year == 0) {
			return nil, fault.Invalid("请提供 --month YYYY-MM 或 --year YYYY 中的一项")
		}
		if o.month != "" {
			date, err := time.Parse("2006-01", o.month)
			if err != nil {
				return nil, fault.Invalid("月份格式必须为 YYYY-MM")
			}
			o.year, month = date.Year(), int(date.Month())
		}
	}
	return svc.Read(ctx, func(c *qianji.Client, a qianji.Session) (any, error) {
		switch command {
		case "books list":
			return c.Books(ctx, a, o.includeHidden)
		case "books members":
			return c.Members(ctx, a, o.book)
		case "assets list":
			return c.Assets(ctx, a, o.status)
		case "assets debts":
			return c.Debts(ctx, a, o.direction, o.status)
		case "categories list":
			return c.Categories(ctx, a, o.book, o.kind)
		case "tags list":
			return c.Tags(ctx, a, o.status, o.lastTime)
		case "currencies list":
			return c.Currencies(ctx, a)
		case "budgets list":
			return c.Budgets(ctx, a, o.book, o.year, month)
		case "bills pull":
			return c.PullBills(ctx, a, cursor)
		default:
			return nil, fault.Invalid("未知读取命令")
		}
	})
}

func readJSON(in io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(in, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return fault.Invalid("标准输入无效或超过 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return fault.Invalid("标准输入必须是符合命令约定的 JSON 对象")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fault.Invalid("标准输入只能包含一个 JSON 对象")
	}
	return nil
}

func billFilter(o options) (store.Filter, error) {
	f := store.Filter{Book: o.book, Limit: o.limit, Offset: o.offset}
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	for _, item := range []struct {
		text  string
		value *int64
	}{{o.since, &f.Since}, {o.until, &f.Until}} {
		if item.text == "" {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", item.text, zone)
		if err != nil {
			return f, fault.Invalid("日期格式必须为 YYYY-MM-DD")
		}
		*item.value = date.Unix()
	}
	return f, f.Validate()
}

func outputJSON(data any) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	redact(decoded)
	return json.Marshal(struct {
		OK   bool `json:"ok"`
		Data any  `json:"data"`
	}{true, decoded})
}

func redact(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "password") || lower == "pwd" {
				value[key] = "[REDACTED]"
			} else {
				redact(item)
			}
		}
	case []any:
		for _, item := range value {
			redact(item)
		}
	}
}
