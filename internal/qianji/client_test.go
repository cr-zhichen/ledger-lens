package qianji

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ledger-lens/internal/fault"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Options{BaseURL: server.URL, NextTimestamp: func(string) (int64, error) { return 1700000000000, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestLoginWireAndExactUID(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		want := url.Values{"v": {"alice+test@example.test"}, "pwd": {"5f4dcc3b5aa765d61d8327deb882cf99"}}
		if r.Method != "POST" || r.URL.Path != "/account/login" || !reflect.DeepEqual(r.PostForm, want) {
			t.Errorf("wrong login wire: %s %s %v", r.Method, r.URL.Path, r.PostForm)
		}
		for key, want := range map[string]string{"reqidv2": "de1256b8a1df8c961898fd341f20c92a", "tok": "07eeb15f2e5d3d2da165594f0dd0cec0", "ctrl": "account", "act": "login", "devid": "fixture-device", "vs": "1207", "vsn": "4.5.1b3", "Content-Type": "application/x-www-form-urlencoded"} {
			if got := r.Header.Get(key); got != want {
				t.Errorf("header %s: %q, want %q", key, got, want)
			}
		}
		if r.Header.Get("utoken") != "" {
			t.Error("login unexpectedly sent a session")
		}
		io.WriteString(w, `{"ec":200,"data":{"user":{"id":9223372036854775807},"token":"fixture-token"}}`)
	})
	session, err := client.Login(context.Background(), "alice+test@example.test", "5F4DCC3B5AA765D61D8327DEB882CF99", "fixture-device")
	if err != nil {
		t.Fatal(err)
	}
	if session.UID != "9223372036854775807" || session.Token != "fixture-token" {
		t.Fatalf("unexpected session identity: %s", session.UID)
	}
}

func TestReadWireContracts(t *testing.T) {
	a := Session{UID: "9007199254740993", Token: "fixture-session", Device: "fixture-device"}
	cases := []struct {
		name, path string
		form       url.Values
		htoken     bool
		call       func(*Client) (json.RawMessage, error)
	}{
		{"books", "/book/list", url.Values{"t": {"-1"}}, false, func(c *Client) (json.RawMessage, error) { return c.Books(context.Background(), a, true) }},
		{"members", "/book/members", url.Values{"bookid": {"9007199254740995"}}, false, func(c *Client) (json.RawMessage, error) {
			return c.Members(context.Background(), a, "9007199254740995")
		}},
		{"assets", "/asset/list", url.Values{"status": {"2"}}, false, func(c *Client) (json.RawMessage, error) { return c.Assets(context.Background(), a, 2) }},
		{"debts", "/asset/listloan", url.Values{"t": {"51"}, "status": {"1"}}, false, func(c *Client) (json.RawMessage, error) { return c.Debts(context.Background(), a, 51, 1) }},
		{"categories", "/category/listv2", url.Values{"bookid": {"-1"}, "t": {"0"}}, false, func(c *Client) (json.RawMessage, error) { return c.Categories(context.Background(), a, "-1", 0) }},
		{"tags", "/tag/list", url.Values{"status": {"-1"}, "lasttime": {"123"}}, true, func(c *Client) (json.RawMessage, error) { return c.Tags(context.Background(), a, -1, 123) }},
		{"currencies", "/currency/listv2", url.Values{}, false, func(c *Client) (json.RawMessage, error) { return c.Currencies(context.Background(), a) }},
		{"budget_month", "/budget/list", url.Values{"bookid": {"-1"}, "flts": {`{"month":"2026,9"}`}}, false, func(c *Client) (json.RawMessage, error) { return c.Budgets(context.Background(), a, "-1", 2026, 9) }},
		{"budget_year", "/budget/list", url.Values{"bookid": {"-1"}, "flts": {`{"year":"2026"}`}}, false, func(c *Client) (json.RawMessage, error) { return c.Budgets(context.Background(), a, "-1", 2026, 0) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				tc.form.Set("uid", a.UID)
				tc.form.Set("fr", a.UID)
				if r.Method != "POST" || r.URL.Path != tc.path || !reflect.DeepEqual(r.PostForm, tc.form) {
					t.Errorf("unexpected read wire: %s %s %v", r.Method, r.URL.Path, r.PostForm)
				}
				if r.Header.Get("utoken") != a.Token || r.Header.Get("devid") != a.Device || (r.Header.Get("htoken") == "1") != tc.htoken {
					t.Error("incorrect auth headers")
				}
				io.WriteString(w, `{"ec":200,"data":{"list":[{"id":9007199254740993,"money":1234567890.123456789}],"groups":[]}}`)
			})
			raw, err := tc.call(client)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "9007199254740993") || !strings.Contains(string(raw), "1234567890.123456789") {
				t.Fatal("read response lost numeric precision")
			}
		})
	}
}

func TestLoginOpaqueUserID(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"opaque", `"fixture_user_abc123xyz"`, "fixture_user_abc123xyz"},
		{"empty", `""`, ""},
		{"whitespace", `"   "`, ""},
		{"null", `null`, ""},
		{"object", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"ec":200,"data":{"user":{"id":`+tc.raw+`},"token":"fixture-token"}}`)
			})
			session, err := client.Login(context.Background(), "fixture-account", "5f4dcc3b5aa765d61d8327deb882cf99", "fixture-device")
			if tc.want == "" {
				if !fault.Is(err, "RESPONSE_INVALID") {
					t.Fatalf("invalid user ID accepted: %v", err)
				}
				return
			}
			if err != nil || session.UID != tc.want {
				t.Fatalf("opaque user ID was not preserved: %v", err)
			}
		})
	}
}

func TestLowercaseProtocolHeadersOnHTTP1Wire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		var wire bytes.Buffer
		request, err := http.ReadRequest(bufio.NewReader(io.TeeReader(conn, &wire)))
		if err != nil {
			done <- err
			return
		}
		if _, err = io.Copy(io.Discard, request.Body); err != nil {
			done <- err
			return
		}
		request.Body.Close()
		for _, key := range []string{"vs", "vsn", "os", "pkg", "ctrl", "act", "reqidv2", "tok", "utoken", "devid", "htoken"} {
			if !strings.Contains(wire.String(), "\r\n"+key+": ") {
				done <- fmt.Errorf("protocol header %s was not lowercase on the wire", key)
				return
			}
		}
		body := `{"ec":200,"data":{"list":[]}}`
		_, err = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		done <- err
	}()
	client, err := New(Options{BaseURL: "http://" + listener.Addr().String(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := client.Tags(context.Background(), Session{UID: "fixture_user_abc123xyz", Token: "fixture-token", Device: "fixture-device"}, -1, 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
}

func TestReadBoundaryAndRedirect(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	})
	_, err := client.post(context.Background(), "/bill/syncall", Session{}, url.Values{})
	if !fault.Is(err, "READ_ONLY_VIOLATION") {
		t.Fatalf("write was not blocked: %v", err)
	}
	_, err = client.Books(context.Background(), Session{UID: "1", Token: "secret", Device: "d"}, false)
	if !fault.Is(err, "HTTP_ERROR") || calls.Load() != 0 {
		t.Fatalf("redirect followed or misclassified: %v, calls %d", err, calls.Load())
	}
}

func TestResponseErrors(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"html", "<html>upstream failure secret</html>", "RESPONSE_INVALID", 200},
		{"missing_ec", `{"data":{"list":[]}}`, "RESPONSE_INVALID", 200},
		{"empty_data", `{"ec":200,"data":null}`, "RESPONSE_INVALID", 200},
		{"bad_list", `{"ec":200,"data":{"list":{}}}`, "RESPONSE_INVALID", 200},
		{"signature", `{"ec":500,"em":"签名错误 secret"}`, "SIGNATURE_REJECTED", 200},
		{"expired", `{"ec":401,"em":"{\"msg\":\"登录已过期 secret\"}"}`, "TOKEN_EXPIRED", 200},
		{"business", `{"ec":502,"em":"budget invalid secret"}`, "BUSINESS_ERROR", 200},
		{"forbidden", "secret", "HTTP_ERROR", 403},
		{"unauthorized", "secret", "TOKEN_EXPIRED", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) })
			_, err := client.Books(context.Background(), Session{UID: "1", Token: "t", Device: "d"}, false)
			if !fault.Is(err, tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Error("upstream response leaked through error")
			}
		})
	}
}

func TestPullPageKeepsBaselineAcrossBooks(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Header.Get("htoken") != "1" || r.Form.Get("lasttimes") != `{"v":9007199254740993}` {
			t.Error("incorrect sync baseline")
		}
		io.WriteString(w, `{"ec":200,"data":{"changes":[{"id":9007199254740993,"money":0.100000000000000001}],"deletes":[9223372036854775807],"bookid":42,"pageoffset":1,"pagesign":"next","hasmore":1,"lasttimes":{"v":999}}}`)
	})
	page, err := client.PullBills(context.Background(), Session{UID: "1", Token: "t", Device: "d"}, Cursor{BookID: "-1", LastTimes: json.RawMessage(`{"v":9007199254740993}`)})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor.BookID != "42" || string(page.NextCursor.LastTimes) != `{"v":9007199254740993}` || string(page.LastTimes) != `{"v":999}` {
		t.Fatalf("incorrect continuation: %+v", page.NextCursor)
	}
}

func TestTimeoutAndCanceledRequest(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	client, err := New(Options{BaseURL: server.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Books(context.Background(), Session{UID: "1", Token: "t", Device: "d"}, false)
	if !fault.Is(err, "HTTP_ERROR") {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Books(ctx, Session{UID: "1", Token: "t", Device: "d"}, false)
	if !fault.Is(err, "CANCELED") {
		t.Fatalf("cancellation: %v", err)
	}
}
