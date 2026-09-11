package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
}

func NewClient() *Client {
	client := &Client{HTTP: &http.Client{}, URL: latestURL}
	if dir, err := os.UserCacheDir(); err == nil {
		client.CachePath = filepath.Join(dir, "ledger-lens", "update.json")
	}
	return client
}

func (c *Client) Check(ctx context.Context, current string) (Result, error) {
	result := c.Cached(current)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return result, checkError()
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "LedgerLens/"+current)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return result, checkError()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return result, fault.New("UPDATE_NOT_FOUND", "未找到公开的正式版 Release；仓库可能尚未发布或不可访问", 4)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return result, fault.New("UPDATE_RATE_LIMITED", "GitHub 拒绝请求或已限流，请稍后重试检查更新", 4)
	}
	if resp.StatusCode != http.StatusOK {
		return result, checkError()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return result, checkError()
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return result, checkError()
	}
	latest, valid := version.FromTag(release.Tag)
	if !valid || release.Draft || release.Prerelease {
		return result, fault.New("UPDATE_INVALID", "GitHub 返回的 Release 不是有效的 vX.Y.Z 正式版", 4)
	}
	checkedAt := time.Now().UTC()
	result = releaseResult(current, latest, checkedAt, "github")
	c.saveCache(cachedRelease{Tag: release.Tag, CheckedAt: checkedAt})
	return result, nil
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
		message = fmt.Sprintf("上次检查（%s）发现 LedgerLens 新版本 %s（当前 %s），本次在线检查未成功：%s", r.CheckedAt, r.LatestVersion, r.CurrentVersion, r.ReleaseURL)
	}
	return &Notice{
		Code:    "UPDATE_AVAILABLE",
		Message: message,
		Result:  r,
	}
}
