package ping

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type hit struct {
	method, path, body string
}

func server(t *testing.T, status func(path string) int) (*httptest.Server, *[]hit) {
	t.Helper()
	var mu sync.Mutex
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, hit{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		w.WriteHeader(status(r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestResolveServer(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if s, _ := ResolveServer("", get); s != DefaultServer {
		t.Error(s)
	}
	env[EnvServer] = "http://localhost:8080/"
	if s, _ := ResolveServer("", get); s != "http://localhost:8080" {
		t.Error(s)
	}
	if s, _ := ResolveServer("https://x.test", get); s != "https://x.test" {
		t.Error(s)
	}
	for _, bad := range []string{"localhost:8080", "ftp://x", "http://"} {
		if _, err := ResolveServer(bad, get); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if !ValidID("hb_Abc-123") || ValidID("") || ValidID("a/b") || ValidID("a b") {
		t.Error("ValidID")
	}
}

func TestSendVariantsBodyAndRetry(t *testing.T) {
	calls := 0
	srv, hits := server(t, func(p string) int {
		if strings.HasSuffix(p, "/flaky") {
			calls++
			if calls < 3 {
				return 503
			}
		}
		if strings.Contains(p, "/nope") {
			return 404
		}
		return 200
	})
	var slept []time.Duration
	c := &Client{Server: srv.URL, Sleep: func(d time.Duration) { slept = append(slept, d) }}
	ctx := context.Background()
	if err := c.Send(ctx, "hb1", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, "hb1", "start", nil); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), 2000)
	big = append(big, []byte("TAIL")...)
	if err := c.Send(ctx, "hb1", "3", big); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, "hb1", "flaky", nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := c.Send(ctx, "nope", "", nil); err == nil || !strings.Contains(err.Error(), "unknown heartbeat id") {
		t.Errorf("404: %v", err)
	}
	h := *hits
	if len(h) != 7 || h[0].method != "GET" || h[0].path != "/ping/hb1" || h[1].path != "/ping/hb1/start" {
		t.Fatalf("hits: %+v", h)
	}
	if h[2].method != "POST" || h[2].path != "/ping/hb1/3" || len(h[2].body) != MaxBody || !strings.HasSuffix(h[2].body, "TAIL") {
		t.Errorf("body must be POST, 1 KB tail: %s %d", h[2].method, len(h[2].body))
	}
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Errorf("backoff: %v", slept)
	}
	if h[6].path != "/ping/nope" || len(h) != 7 {
		t.Errorf("404 must not retry: %+v", h[4:])
	}
}

func TestSendNetworkErrorAfterAttempts(t *testing.T) {
	c := &Client{Server: "http://127.0.0.1:1", Sleep: func(time.Duration) {}, HTTP: &http.Client{Timeout: 200 * time.Millisecond}}
	if err := c.Send(context.Background(), "hb", "", nil); err == nil {
		t.Error("must fail")
	}
}

func TestWrap(t *testing.T) {
	srv, hits := server(t, func(string) int { return 200 })
	c := &Client{Server: srv.URL, Sleep: func(time.Duration) {}}
	var out, errb bytes.Buffer
	code := c.Wrap(context.Background(), "hb", []string{"sh", "-c", "echo hello; echo oops >&2; exit 3"}, nil, &out, &errb)
	if code != 3 || out.String() != "hello\n" || errb.String() != "oops\n" {
		t.Errorf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	h := *hits
	if len(h) != 2 || h[0].path != "/ping/hb/start" || h[1].path != "/ping/hb/3" || h[1].method != "POST" || !strings.Contains(h[1].body, "oops") || !strings.Contains(h[1].body, "hello") {
		t.Errorf("hits: %+v", h)
	}

	*hits = nil
	if code := c.Wrap(context.Background(), "hb", []string{"sh", "-c", "exit 0"}, nil, &out, &errb); code != 0 {
		t.Error(code)
	}
	if h := *hits; len(h) != 2 || h[1].path != "/ping/hb/0" || h[1].method != "GET" {
		t.Errorf("success: %+v", h)
	}

	*hits = nil
	if code := c.Wrap(context.Background(), "hb", []string{"/no/such/binary"}, nil, &out, &errb); code != 127 {
		t.Error(code)
	}
	if h := *hits; len(h) != 2 || h[1].path != "/ping/hb/fail" {
		t.Errorf("not found: %+v", h)
	}

	// Dead server: the exit code is still the command's.
	dead := &Client{Server: "http://127.0.0.1:1", Sleep: func(time.Duration) {}, HTTP: &http.Client{Timeout: 100 * time.Millisecond}}
	if code := dead.Wrap(context.Background(), "hb", []string{"sh", "-c", "exit 5"}, nil, &out, &errb); code != 5 {
		t.Errorf("exit code must survive ping failure: %d", code)
	}
}

func TestWrapSignal(t *testing.T) {
	srv, hits := server(t, func(string) int { return 200 })
	c := &Client{Server: srv.URL, Sleep: func(time.Duration) {}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	code := c.Wrap(ctx, "hb", []string{"sh", "-c", "sleep 5"}, nil, io.Discard, io.Discard)
	if code != 128+15 {
		t.Errorf("SIGTERM → 143, got %d", code)
	}
	if h := *hits; len(h) != 2 || h[1].path != "/ping/hb/fail" {
		t.Errorf("interrupted must send /fail: %+v", h)
	}
}
