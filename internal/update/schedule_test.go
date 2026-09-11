package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ledger-lens/internal/fault"
)

func TestAutomaticCheckCachesFor24HoursAndManualCheckRefreshesImmediately(t *testing.T) {
	now := time.Now().UTC()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"tag_name":"v2.0.0"}`)
	}))
	defer server.Close()
	cachePath := filepath.Join(t.TempDir(), "update.json")
	newClient := func() *Client {
		return &Client{HTTP: server.Client(), URL: server.URL, CachePath: cachePath, now: func() time.Time { return now }}
	}
	check := func(current string, wantCalls int32, wantNotice bool) {
		t.Helper()
		result, err := newClient().CheckAutomatic(context.Background(), current)
		if err != nil || calls.Load() != wantCalls || (result.Notice() != nil) != wantNotice {
			t.Fatalf("unexpected automatic check: %+v / %v / calls=%d", result, err, calls.Load())
		}
	}
	check("1.0.0", 1, true)
	now = now.Add(24*time.Hour - time.Nanosecond)
	check("1.0.0", 1, true)
	check("2.0.0", 1, false)
	check("3.0.0", 1, false)
	now = now.Add(time.Nanosecond)
	check("1.0.0", 2, true)
	now = now.Add(time.Hour)
	if result, err := newClient().Check(context.Background(), "1.0.0"); err != nil || result.Source != "github" || calls.Load() != 3 {
		t.Fatalf("manual check reused the fresh cache: %+v / %v", result, err)
	}
	now = now.Add(24*time.Hour - time.Nanosecond)
	check("1.0.0", 3, true)
	now = now.Add(time.Nanosecond)
	check("1.0.0", 4, true)
}

func TestLegacyCacheRemainsUsableWithoutMakingAnHTTPCall(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "update.json")
	data, err := json.Marshal(map[string]any{"tag": "v2.0.0", "checked_at": now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	client := &Client{CachePath: path, now: func() time.Time { return now }}
	// No HTTP client is configured: a legacy, fresh cache must be sufficient.
	result, err := client.CheckAutomatic(context.Background(), "1.0.0")
	if err != nil || result.Source != "cache" || result.Notice() == nil {
		t.Fatalf("legacy cache lost: %+v / %v", result, err)
	}
}

func TestFailureCooldownPreservesKnownReleaseAndAllowsManualOverride(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			now := time.Now().UTC()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			client := &Client{HTTP: server.Client(), URL: server.URL, CachePath: filepath.Join(t.TempDir(), "update.json"), now: func() time.Time { return now }}
			if known {
				if err := client.saveCache(cachedRelease{Tag: "v2.0.0", CheckedAt: now.Add(-25 * time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			first, err := client.CheckAutomatic(context.Background(), "1.0.0")
			if !fault.Is(err, "UPDATE_CHECK_FAILED") || (first.Notice() != nil) != known {
				t.Fatalf("failure discarded known state: %+v / %v", first, err)
			}
			now = now.Add(time.Hour - time.Nanosecond)
			cached, err := client.CheckAutomatic(context.Background(), "1.0.0")
			if err != nil || calls.Load() != 1 || cached.CheckedAt != first.CheckedAt || (cached.Notice() != nil) != known {
				t.Fatal("cooldown did not survive a later invocation")
			}
			now = now.Add(time.Nanosecond)
			if _, err := client.CheckAutomatic(context.Background(), "1.0.0"); err == nil || calls.Load() != 2 {
				t.Fatal("retry was not permitted at the cooldown boundary")
			}
			if _, err := client.Check(context.Background(), "1.0.0"); err == nil || calls.Load() != 3 {
				t.Fatal("manual check was suppressed by backoff")
			}
		})
	}
}

func TestRateLimitHeadersExtendAutomaticCooldown(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		name    string
		headers http.Header
		wait    time.Duration
	}{
		{"delta seconds", http.Header{"Retry-After": {"7200"}}, 2 * time.Hour},
		{"HTTP date", http.Header{"Retry-After": {start.Add(3 * time.Hour).Format(http.TimeFormat)}}, 3 * time.Hour},
		{"reset takes precedence", http.Header{"Retry-After": {"7200"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(start.Add(4*time.Hour).Unix(), 10)}}, 4 * time.Hour},
		{"remaining quota", http.Header{"X-Ratelimit-Remaining": {"5"}, "X-Ratelimit-Reset": {strconv.FormatInt(start.Add(4*time.Hour).Unix(), 10)}}, time.Hour},
		{"minimum hour", http.Header{"Retry-After": {"60"}}, time.Hour},
		{"invalid headers", http.Header{"Retry-After": {"overflow"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"9223372036854775807"}}, time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := start
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, values := range tt.headers {
					w.Header()[key] = values
				}
				calls.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			client := &Client{HTTP: server.Client(), URL: server.URL, CachePath: filepath.Join(t.TempDir(), "update.json"), now: func() time.Time { return now }}
			if _, err := client.CheckAutomatic(context.Background(), "1.0.0"); !fault.Is(err, "UPDATE_RATE_LIMITED") {
				t.Fatal(err)
			}
			now = start.Add(tt.wait - time.Nanosecond)
			if _, err := client.CheckAutomatic(context.Background(), "1.0.0"); err != nil || calls.Load() != 1 {
				t.Fatal("server retry deadline was ignored")
			}
			now = now.Add(time.Nanosecond)
			if _, err := client.CheckAutomatic(context.Background(), "1.0.0"); !fault.Is(err, "UPDATE_RATE_LIMITED") || calls.Load() != 2 {
				t.Fatal("retry deadline never became eligible")
			}
		})
	}
}

func TestConcurrentAutomaticInvocationsMakeOneRequest(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			io.WriteString(w, `{"tag_name":"v2.0.0"}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer unblock()
	cachePath := filepath.Join(t.TempDir(), "update.json")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const count = 12
	start := make(chan struct{})
	done := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			<-start
			client := &Client{HTTP: server.Client(), URL: server.URL, CachePath: cachePath}
			_, err := client.CheckAutomatic(ctx, "1.0.0")
			done <- err
		}()
	}
	close(start)
	for i := 0; i < count-1; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("other invocations waited for the network request")
		}
	}
	// Other callers have returned while the one permitted request is still pending.
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("no invocation performed the due check")
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent requests: %d", calls.Load())
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the permitted request did not finish")
	}
}

func TestInterruptedProcessKeepsCooldownAndReleasesLock(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		io.WriteString(w, `{"tag_name":"v2.0.0"}`)
	}))
	defer server.Close()
	cachePath := filepath.Join(t.TempDir(), "update.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateProcessHelper$")
	cmd.Env = append(os.Environ(), "LEDGERLENS_UPDATE_HELPER_CACHE="+cachePath, "LEDGERLENS_UPDATE_HELPER_URL="+server.URL)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not start its check")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	client := &Client{HTTP: server.Client(), URL: server.URL, CachePath: cachePath}
	if _, err := client.CheckAutomatic(context.Background(), "1.0.0"); err != nil || calls.Load() != 1 {
		t.Fatal("interrupted check immediately retried")
	}
	client.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	result, err := client.CheckAutomatic(context.Background(), "1.0.0")
	if err != nil || calls.Load() != 2 || result.Notice() == nil {
		t.Fatalf("dead process left the update lock stuck: %+v / %v", result, err)
	}
}

func TestUpdateProcessHelper(t *testing.T) {
	cachePath := os.Getenv("LEDGERLENS_UPDATE_HELPER_CACHE")
	if cachePath == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &Client{HTTP: &http.Client{}, URL: os.Getenv("LEDGERLENS_UPDATE_HELPER_URL"), CachePath: cachePath}
	if _, err := client.CheckAutomatic(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
}
