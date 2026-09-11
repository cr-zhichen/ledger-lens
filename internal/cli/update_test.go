package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ledger-lens/internal/update"
	"ledger-lens/internal/version"
)

func TestEveryInvocationReportsUpdateWithoutBreakingOutput(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"tag_name":"v1.10.0"}`)
	}))
	defer server.Close()
	client := &update.Client{HTTP: server.Client(), URL: server.URL, CachePath: filepath.Join(t.TempDir(), "update.json")}
	db := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	for _, tt := range []struct {
		args     []string
		code     int
		help     bool
		requests int32
	}{
		{[]string{"version"}, 0, false, 1},
		{[]string{"version"}, 0, false, 0}, // Repeat the notice without another HTTP request.
		{[]string{"--version"}, 0, false, 0},
		{[]string{"update", "check"}, 0, false, 1},
		{[]string{"--help"}, 0, true, 0},
		{nil, 0, true, 0},
		{[]string{"update", "check", "--help"}, 0, true, 0},
		{[]string{"unknown"}, 2, false, 0},
		{[]string{"version", "--invalid"}, 2, false, 0},
	} {
		var out, stderr bytes.Buffer
		before := calls.Load()
		code := run(context.Background(), append([]string{"--db", db}, tt.args...), strings.NewReader(""), &out, &stderr, version.Info{Version: "1.9.0"}, client)
		if code != tt.code || calls.Load() != before+tt.requests {
			t.Fatalf("%v: exit %d, check count %d", tt.args, code, calls.Load()-before)
		}
		var diagnostic struct {
			Error  *struct{ Code string } `json:"error"`
			Notice *update.Notice         `json:"notice"`
		}
		if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil || diagnostic.Notice == nil || diagnostic.Notice.Code != "UPDATE_AVAILABLE" {
			t.Fatalf("%v: missing notice or invalid stderr JSON: %s", tt.args, stderr.String())
		}
		if strings.Contains(diagnostic.Notice.Message, "本次在线检查未成功") {
			t.Fatal("normal cache reuse was mislabeled as a network failure")
		}
		if code != 0 {
			if out.Len() != 0 || diagnostic.Error == nil || diagnostic.Error.Code != "INVALID_ARGUMENT" {
				t.Fatalf("error code or stream changed: %s / %s", out.String(), stderr.String())
			}
		} else if !tt.help && !json.Valid(out.Bytes()) {
			t.Fatalf("notice corrupted stdout: %s", out.String())
		} else if tt.help && !strings.HasPrefix(out.String(), "账镜") {
			t.Fatalf("help output changed: %s", out.String())
		}
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("version, help or update opened the account database")
	}
}

func TestAutomaticFailuresAreSilentButExplicitCheckFails(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := &update.Client{HTTP: server.Client(), URL: server.URL, CachePath: filepath.Join(t.TempDir(), "update.json")}
	for _, args := range [][]string{{"version"}, {"update", "check"}} {
		var out, stderr bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(""), &out, &stderr, version.Info{Version: "1.0.0"}, client)
		if args[0] == "version" {
			if code != 0 || stderr.Len() != 0 || !json.Valid(out.Bytes()) {
				t.Fatal("automatic network failure changed the main command")
			}
		} else if code != 4 || out.Len() != 0 || !strings.Contains(stderr.String(), "UPDATE_RATE_LIMITED") {
			t.Fatalf("explicit check silently succeeded: %d / %s", code, stderr.String())
		}
	}
}

func TestAutomaticTimeoutStillReportsPreviouslyKnownUpdate(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	var hang atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			<-r.Context().Done()
			return
		}
		io.WriteString(w, `{"tag_name":"v2.0.0"}`)
	}))
	defer server.Close()
	client := &update.Client{HTTP: server.Client(), URL: server.URL, CachePath: filepath.Join(t.TempDir(), "update.json")}
	if _, err := client.Check(context.Background(), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	stale, err := json.Marshal(map[string]any{"tag": "v2.0.0", "checked_at": time.Now().Add(-25 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(client.CachePath, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	automaticDone := make(chan struct{})
	checker := automaticCompletionChecker{updateChecker: client, done: automaticDone}
	var out, stderr bytes.Buffer
	code := run(ctx, []string{"version"}, strings.NewReader(""), &out, &stderr, version.Info{Version: "1.0.0"}, checker)
	select {
	case <-automaticDone:
	case <-time.After(time.Second):
		t.Fatal("automatic update check did not stop after its context expired")
	}
	if code != 0 || !json.Valid(out.Bytes()) || !json.Valid(stderr.Bytes()) || !strings.Contains(stderr.String(), `"source":"cache"`) {
		t.Fatalf("timeout lost cached notice or changed command: %d / %s", code, stderr.String())
	}
}

func TestOfflineOptOutAndDevelopmentBuild(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"tag_name":"v2.0.0"}`)
	}))
	defer server.Close()
	client := &update.Client{HTTP: server.Client(), URL: server.URL}
	for _, tt := range []struct {
		args    []string
		current string
	}{
		{[]string{"--no-update-check", "version"}, "1.0.0"},
		{[]string{"version", "--no-update-check"}, "1.0.0"},
		{[]string{"version"}, "dev"},
	} {
		var out, stderr bytes.Buffer
		if code := run(context.Background(), tt.args, strings.NewReader(""), &out, &stderr, version.Info{Version: tt.current}, client); code != 0 || stderr.Len() != 0 {
			t.Fatalf("disabled check changed command: %d / %s", code, stderr.String())
		}
	}
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "1")
	var out, stderr bytes.Buffer
	run(context.Background(), []string{"version"}, strings.NewReader(""), &out, &stderr, version.Info{Version: "1.0.0"}, client)
	if calls.Load() != 0 {
		t.Fatal("disabled automatic check made network requests")
	}
	out.Reset()
	stderr.Reset()
	code := run(context.Background(), []string{"update", "check"}, strings.NewReader(""), &out, &stderr, version.Info{Version: "dev"}, client)
	if code != 0 || calls.Load() != 1 || !strings.Contains(out.String(), `"update_available":null`) || stderr.Len() != 0 {
		t.Fatalf("development build explicit check failed: %d / %s / %s", code, out.String(), stderr.String())
	}
}

type checkerFunc func(context.Context, string) (update.Result, error)

func (f checkerFunc) Cached(current string) update.Result {
	return update.Result{CurrentVersion: current}
}

func (f checkerFunc) Check(ctx context.Context, current string) (update.Result, error) {
	return f(ctx, current)
}

func (f checkerFunc) CheckAutomatic(ctx context.Context, current string) (update.Result, error) {
	return f(ctx, current)
}

type automaticCompletionChecker struct {
	updateChecker
	done chan struct{}
}

func (c automaticCompletionChecker) CheckAutomatic(ctx context.Context, current string) (update.Result, error) {
	defer close(c.done)
	return c.updateChecker.CheckAutomatic(ctx, current)
}

type signalingWriter struct {
	bytes.Buffer
	wrote chan struct{}
}

func (w *signalingWriter) Write(p []byte) (int, error) {
	close(w.wrote)
	return w.Buffer.Write(p)
}

func TestMainCommandRunsWhileUpdateCheckIsPending(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	out := &signalingWriter{wrote: make(chan struct{})}
	var stderr bytes.Buffer
	checker := checkerFunc(func(ctx context.Context, current string) (update.Result, error) {
		select {
		case <-out.wrote:
			available := true
			return update.Result{CurrentVersion: current, LatestVersion: "2.0.0", UpdateAvailable: &available}, nil
		case <-ctx.Done():
			return update.Result{}, ctx.Err()
		}
	})
	code := run(context.Background(), []string{"version"}, strings.NewReader(""), out, &stderr, version.Info{Version: "1.0.0"}, checker)
	if code != 0 || !strings.Contains(stderr.String(), "UPDATE_AVAILABLE") {
		t.Fatal("update check blocked command output instead of running alongside it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	checker = checkerFunc(func(ctx context.Context, _ string) (update.Result, error) {
		<-ctx.Done()
		return update.Result{}, ctx.Err()
	})
	var normalOut bytes.Buffer
	stderr.Reset()
	if code := run(ctx, []string{"version"}, strings.NewReader(""), &normalOut, &stderr, version.Info{Version: "1.0.0"}, checker); code != 0 || stderr.Len() != 0 {
		t.Fatal("update cancellation changed the completed command")
	}
}
