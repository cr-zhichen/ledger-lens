package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ledger-lens/internal/fault"
)

func TestLatestStableReleaseAndDevelopmentBuild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") == "" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("missing GitHub API request headers")
		}
		if r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
			t.Error("release check must not forward credentials")
		}
		io.WriteString(w, `{"tag_name":"v1.10.0","draft":false,"prerelease":false,"html_url":"https://untrusted.example/","body":"remote text"}`)
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), URL: server.URL}
	for _, current := range []string{"1.9.0", "1.10.0", "2.0.0", "dev"} {
		result, err := client.Check(context.Background(), current)
		if err != nil || result.LatestVersion != "1.10.0" || result.ReleaseURL != ReleasesURL+"/tag/v1.10.0" {
			t.Fatalf("invalid release result: %+v / %v", result, err)
		}
		if current == "dev" {
			if result.UpdateAvailable != nil || result.Notice() != nil {
				t.Fatal("unknown development version was compared as a release")
			}
			continue
		}
		available := current == "1.9.0"
		if result.UpdateAvailable == nil || *result.UpdateAvailable != available || (result.Notice() != nil) != available {
			t.Fatalf("wrong update decision for %s: %+v", current, result)
		}
	}
}

func TestUnavailableOrInvalidReleaseIsNotUpToDate(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"missing", 404, "private upstream details", "UPDATE_NOT_FOUND"},
		{"forbidden", 403, "private upstream details", "UPDATE_RATE_LIMITED"},
		{"rate limit", 429, "private upstream details", "UPDATE_RATE_LIMITED"},
		{"server error", 503, "private upstream details", "UPDATE_CHECK_FAILED"},
		{"bad json", 200, "private upstream details", "UPDATE_CHECK_FAILED"},
		{"trailing data", 200, `{"tag_name":"v2.0.0"}{}`, "UPDATE_CHECK_FAILED"},
		{"oversized", 200, strings.Repeat("x", maxResponseBytes+1), "UPDATE_CHECK_FAILED"},
		{"draft", 200, `{"tag_name":"v2.0.0","draft":true}`, "UPDATE_INVALID"},
		{"prerelease flag", 200, `{"tag_name":"v2.0.0","prerelease":true}`, "UPDATE_INVALID"},
		{"prerelease tag", 200, `{"tag_name":"v2.0.0-beta.1"}`, "UPDATE_INVALID"},
		{"noncanonical", 200, `{"tag_name":"v02.0.0"}`, "UPDATE_INVALID"},
		{"missing tag", 200, `{}`, "UPDATE_INVALID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			client := &Client{HTTP: server.Client(), URL: server.URL}
			result, err := client.Check(context.Background(), "1.0.0")
			if !fault.Is(err, tt.code) || result.Notice() != nil {
				t.Fatalf("failure became a valid release: %+v / %v", result, err)
			}
			if strings.Contains(err.Error(), "private upstream details") {
				t.Fatal("upstream body leaked into public error")
			}
		})
	}
}

func TestCheckHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), URL: server.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.Check(ctx, "1.0.0"); !fault.Is(err, "UPDATE_CHECK_FAILED") {
		t.Fatalf("expected bounded failed check, got %v", err)
	}
}

func TestKnownUpdateSurvivesRateLimitsAndStopsAfterUpgrade(t *testing.T) {
	var limited atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"tag_name":"v1.10.0"}`)
	}))
	defer server.Close()
	cachePath := filepath.Join(t.TempDir(), "cache", "update.json")
	client := &Client{HTTP: server.Client(), URL: server.URL, CachePath: cachePath}
	result, err := client.Check(context.Background(), "1.9.0")
	if err != nil || result.Source != "github" || result.Notice() == nil {
		t.Fatalf("online check failed: %+v / %v", result, err)
	}
	limited.Store(true)
	// A new client models the next CLI process; only the on-disk cache is reused.
	client = &Client{HTTP: server.Client(), URL: server.URL, CachePath: cachePath}
	result, err = client.Check(context.Background(), "1.9.0")
	if !fault.Is(err, "UPDATE_RATE_LIMITED") || result.Source != "cache" || result.Notice() == nil || !strings.Contains(result.Notice().Message, "上次检查") {
		t.Fatalf("known update was lost or cache presented as live: %+v / %v", result, err)
	}
	result, err = client.Check(context.Background(), "1.10.0")
	if !fault.Is(err, "UPDATE_RATE_LIMITED") || result.Notice() != nil {
		t.Fatal("an upgraded CLI kept showing the old update")
	}
	if err := os.WriteFile(cachePath, []byte("truncated JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = client.Check(context.Background(), "1.9.0")
	if !fault.Is(err, "UPDATE_RATE_LIMITED") || result.Notice() != nil {
		t.Fatal("corrupt cache changed error handling")
	}
	limited.Store(false)
	// A file in place of a cache directory makes persistence fail on every OS.
	client.CachePath = filepath.Join(cachePath, "unwritable.json")
	if result, err := client.Check(context.Background(), "1.9.0"); err != nil || result.Notice() == nil {
		t.Fatal("unwritable cache failed an otherwise successful check")
	}
}
