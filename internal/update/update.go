package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/version"
)

const (
	Repository       = "cr-zhichen/ledger-lens"
	ReleasesURL      = "https://github.com/" + Repository + "/releases"
	latestURL        = "https://api.github.com/repos/" + Repository + "/releases/latest"
	maxResponseBytes = 1 << 20
)

type Result struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable *bool  `json:"update_available"`
	ReleaseURL      string `json:"release_url"`
	CheckedAt       string `json:"checked_at,omitempty"`
	Source          string `json:"source,omitempty"`
}

type Client struct {
	HTTP      *http.Client
	URL       string
	CachePath string
	now       func() time.Time
}

func NewClient() *Client {
	client := &Client{HTTP: &http.Client{}, URL: latestURL}
	if dir, err := os.UserCacheDir(); err == nil {
		client.CachePath = filepath.Join(dir, "ledger-lens", "update.json")
	}
	return client
}

func (c *Client) Check(ctx context.Context, current string) (Result, error) {
	return c.check(ctx, current, false)
}

func (c *Client) CheckAutomatic(ctx context.Context, current string) (Result, error) {
	return c.check(ctx, current, true)
}

func (c *Client) nowUTC() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *Client) check(ctx context.Context, current string, automatic bool) (Result, error) {
	cache := c.loadCache()
	if automatic && !cache.due(c.nowUTC()) {
		return cache.result(current, "cache"), nil
	}
	lock, err := c.lockCache(ctx, !automatic)
	if err != nil && automatic {
		// Without writable shared state, skip automatic traffic rather than flood GitHub.
		return cache.result(current, "cache"), nil
	}
	if lock != nil {
		defer lock.Close()
		cache = c.loadCache()
		if automatic && !cache.due(c.nowUTC()) {
			return cache.result(current, "cache"), nil
		}
	}
	if ctx.Err() != nil {
		return cache.result(current, "cache"), checkError()
	}
	// Reserve the cooldown before HTTP so an interrupted CLI cannot immediately retry.
	attemptEnd := c.nowUTC()
	if deadline, ok := ctx.Deadline(); ok && deadline.After(attemptEnd) {
		attemptEnd = deadline
	}
	cache.RetryAt = later(cache.RetryAt, attemptEnd.Add(failureCooldown))
	if err := c.saveCache(cache); err != nil && automatic {
		return cache.result(current, "cache"), nil
	}
	fresh, headers, err := c.fetch(ctx, current)
	if err == nil {
		_ = c.saveCache(fresh)
		return fresh.result(current, "github"), nil
	}
	cache.RetryAt = later(cache.RetryAt, retryAt(headers, c.nowUTC()))
	_ = c.saveCache(cache)
	return cache.result(current, "cache"), err
}

func (c *Client) fetch(ctx context.Context, current string) (cachedRelease, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return cachedRelease{}, nil, checkError()
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "LedgerLens/"+current)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return cachedRelease{}, nil, checkError()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return cachedRelease{}, resp.Header, fault.New("UPDATE_NOT_FOUND", "未找到公开的正式版 Release；仓库可能尚未发布或不可访问", 4)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return cachedRelease{}, resp.Header, fault.New("UPDATE_RATE_LIMITED", "GitHub 拒绝请求或已限流，请稍后重试检查更新", 4)
	}
	if resp.StatusCode != http.StatusOK {
		return cachedRelease{}, resp.Header, checkError()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return cachedRelease{}, resp.Header, checkError()
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return cachedRelease{}, resp.Header, checkError()
	}
	_, valid := version.FromTag(release.Tag)
	if !valid || release.Draft || release.Prerelease {
		return cachedRelease{}, resp.Header, fault.New("UPDATE_INVALID", "GitHub 返回的 Release 不是有效的 vX.Y.Z 正式版", 4)
	}
	return cachedRelease{Tag: release.Tag, CheckedAt: c.nowUTC()}, resp.Header, nil
}

func retryAt(headers http.Header, now time.Time) time.Time {
	retry := now.Add(failureCooldown)
	value := headers.Get("Retry-After")
	if delay, err := time.ParseDuration(value + "s"); err == nil && delay > 0 {
		retry = later(retry, now.Add(delay))
	} else if date, err := http.ParseTime(value); err == nil {
		retry = later(retry, date)
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		if seconds, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset := time.Unix(seconds, 0)
			if reset.Year() >= 1 && reset.Year() <= 9999 {
				retry = later(retry, reset)
			}
		}
	}
	return retry
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func releaseResult(current, latest string, checkedAt time.Time, source string) Result {
	// Construct the URL from the validated version; never print arbitrary remote text.
	result := Result{
		CurrentVersion: current, LatestVersion: latest,
		ReleaseURL: ReleasesURL + "/tag/v" + latest,
		CheckedAt:  checkedAt.UTC().Format(time.RFC3339), Source: source,
	}
	if cmp, comparable := version.Compare(latest, current); comparable {
		available := cmp > 0
		result.UpdateAvailable = &available
	}
	return result
}

func checkError() error {
	return fault.New("UPDATE_CHECK_FAILED", "检查更新失败，请检查网络后重试", 4)
}

type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Result
}

func (r Result) Notice() *Notice {
	if r.UpdateAvailable == nil || !*r.UpdateAvailable {
		return nil
	}
	message := fmt.Sprintf("发现 LedgerLens 新版本 %s（当前 %s），请下载对应平台的压缩包：%s", r.LatestVersion, r.CurrentVersion, r.ReleaseURL)
	if r.Source == "cache" {
		message = fmt.Sprintf("上次检查（%s）发现 LedgerLens 新版本 %s（当前 %s），请下载对应平台的压缩包：%s", r.CheckedAt, r.LatestVersion, r.CurrentVersion, r.ReleaseURL)
	}
	return &Notice{
		Code:    "UPDATE_AVAILABLE",
		Message: message,
		Result:  r,
	}
}
