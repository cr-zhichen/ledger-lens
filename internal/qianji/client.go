package qianji

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ledger-lens/internal/fault"
)

const DefaultBaseURL = "https://api.qianjiapp.com"
const maxResponseBytes = 32 << 20

type Session struct {
	UID    string `json:"-"`
	Token  string `json:"-"`
	Device string `json:"-"`
}

type Options struct {
	BaseURL       string
	Timeout       time.Duration
	HTTPClient    *http.Client
	NextTimestamp func(string) (int64, error)
}

type Client struct {
	baseURL string
	http    *http.Client
	nextMS  func(string) (int64, error)
}

func New(options Options) (*Client, error) {
	base := options.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, fault.Invalid("API 地址必须是无路径、无凭据的 HTTPS 地址")
	}
	loopback := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback != nil && loopback.IsLoopback()) {
		return nil, fault.Invalid("API 地址必须使用 HTTPS；仅回环 IP 可使用 HTTP")
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	if options.Timeout < 0 {
		return nil, fault.Invalid("请求超时必须大于零")
	}
	hc := http.Client{}
	if options.HTTPClient != nil {
		hc = *options.HTTPClient
	}
	hc.Timeout = options.Timeout
	// Redirects must never forward account credentials to a different endpoint.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if options.NextTimestamp == nil {
		options.NextTimestamp = monotonicClock()
	}
	return &Client{baseURL: u.Scheme + "://" + u.Host, http: &hc, nextMS: options.NextTimestamp}, nil
}

func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) Login(ctx context.Context, account, digest, device string) (Session, error) {
	if strings.TrimSpace(account) == "" || device == "" || !ValidMD5(digest) {
		return Session{}, fault.Invalid("登录需要账号、32 位 MD5 和设备标识")
	}
	data, err := c.post(ctx, "/account/login", Session{Device: device}, url.Values{"v": {account}, "pwd": {strings.ToLower(digest)}})
	if err != nil {
		return Session{}, err
	}
	var result struct {
		User struct {
			ID json.RawMessage `json:"id"`
		} `json:"user"`
		Token string `json:"token"`
	}
	if json.Unmarshal(data, &result) != nil || strings.TrimSpace(result.Token) == "" {
		return Session{}, invalidResponse()
	}
	uid, err := loginUserID(result.User.ID)
	if err != nil {
		return Session{}, err
	}
	return Session{UID: uid, Token: result.Token, Device: device}, nil
}

// User IDs are opaque strings on some accounts, unlike numeric bill/book IDs.
func loginUserID(raw json.RawMessage) (string, error) {
	var uid string
	if json.Unmarshal(raw, &uid) == nil {
		if strings.TrimSpace(uid) == "" {
			return "", invalidResponse()
		}
		return uid, nil
	}
	return ID(raw, false)
}

func ValidMD5(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// This allowlist is also enforced at the transport boundary. No arbitrary route is exposed.
var allowedRoutes = map[string]bool{
	"/account/login":   false,
	"/book/list":       false,
	"/book/members":    false,
	"/asset/list":      false,
	"/asset/listloan":  false,
	"/category/listv2": false,
	"/tag/list":        true,
	"/budget/list":     false,
	"/currency/listv2": false,
	"/syncv2/pull":     true,
}

func (c *Client) post(ctx context.Context, path string, session Session, form url.Values) (json.RawMessage, error) {
	htoken, allowed := allowedRoutes[path]
	if !allowed {
		return nil, fault.New("READ_ONLY_VIOLATION", "此版本仅允许已定义的登录与读取接口", 2)
	}
	if path != "/account/login" {
		if session.UID == "" || session.Token == "" || session.Device == "" {
			return nil, fault.New("AUTH_REQUIRED", "请先登录", 3)
		}
		form.Set("uid", session.UID)
		form.Set("fr", session.UID)
	}
	ms, err := c.nextMS(path)
	if err != nil {
		return nil, err
	}
	reqid, tok := signature(path, ms)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fault.Invalid("无法创建请求")
	}
	for key, value := range map[string]string{
		"clang": "zh", "cregion": "CN", "timezoneoffset": "28800",
		"os": "1", "osvs": "36", "pkg": "com.mutangtech.qianji",
		"vs": "1207", "vsn": "4.5.1b3", "mk": "beta",
		"devbrand": "LedgerLens", "devname": "LedgerLens",
		"reqidv2": reqid, "tok": tok, "devid": session.Device,
		"content-type": "application/x-www-form-urlencoded",
	} {
		// The upstream HTTP/1 server requires lowercase protocol header names.
		// Header.Set canonicalizes e.g. "vs" to "Vs", which it rejects as an old client.
		req.Header[key] = []string{value}
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	req.Header["ctrl"] = []string{parts[0]}
	req.Header["act"] = []string{parts[1]}
	if session.Token != "" {
		req.Header["utoken"] = []string{session.Token}
	}
	if htoken {
		req.Header["htoken"] = []string{"1"}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fault.New("CANCELED", "请求已取消", 130)
		}
		return nil, fault.New("HTTP_ERROR", "无法连接钱迹服务或请求超时", 4)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, authenticationError(path)
	}
	// A bare 403 may be a signature or gateway rejection; it is not proof of token expiry.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fault.New("HTTP_ERROR", "钱迹服务返回 HTTP "+strconv.Itoa(resp.StatusCode), 4)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, invalidResponse()
	}
	if len(body) > maxResponseBytes {
		return nil, fault.New("RESPONSE_TOO_LARGE", "钱迹响应超过 32 MiB 限制", 4)
	}
	var envelope struct {
		Code    *int            `json:"ec"`
		Message json.RawMessage `json:"em"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil {
		return nil, invalidResponse()
	}
	if *envelope.Code != 200 {
		return nil, businessError(path, *envelope.Code, envelope.Message)
	}
	if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return nil, invalidResponse()
	}
	return envelope.Data, nil
}

func authenticationError(path string) error {
	if path == "/account/login" {
		return fault.New("LOGIN_REJECTED", "钱迹拒绝登录，请更新凭据或处理账号验证", 3)
	}
	return fault.New("TOKEN_EXPIRED", "钱迹登录状态已失效", 3)
}

func businessError(path string, code int, raw json.RawMessage) error {
	var hint string
	_ = json.Unmarshal(raw, &hint)
	var nested struct {
		Message string `json:"msg"`
	}
	if json.Unmarshal([]byte(hint), &nested) == nil && nested.Message != "" {
		hint = nested.Message
	}
	hint = strings.ToLower(hint)
	if strings.Contains(hint, "签名") || strings.Contains(hint, "signature") || strings.Contains(hint, "sign error") || strings.HasPrefix(hint, "tok ") || hint == "tok" {
		return fault.New("SIGNATURE_REJECTED", "钱迹请求签名被拒绝", 4)
	}
	if path == "/account/login" {
		return authenticationError(path)
	}
	for _, marker := range []string{"token", "登录", "login"} {
		if strings.Contains(hint, marker) {
			return authenticationError(path)
		}
	}
	return fault.New("BUSINESS_ERROR", "钱迹业务请求失败（ec="+strconv.Itoa(code)+"）", 4)
}

func invalidResponse() error { return fault.New("RESPONSE_INVALID", "钱迹返回了无效数据", 4) }
