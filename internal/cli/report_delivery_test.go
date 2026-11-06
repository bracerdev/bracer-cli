package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
)

// scriptedServer answers each POST /report with the next status of its
// script (the last one repeats); 0 drops the connection without an answer.
// A redirect points at /elsewhere, which answers 200 like a web page.
type scriptedServer struct {
	*httptest.Server
	mu     sync.Mutex
	script []int
	seen   []string // method, path and Authorization of every request
	// onRequest runs after each answer (a test cancels its context there).
	onRequest func()
}

func newScriptedServer(t *testing.T, script ...int) *scriptedServer {
	s := &scriptedServer{script: script}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		code := s.script[0]
		if len(s.script) > 1 {
			s.script = s.script[1:]
		}
		s.mu.Unlock()
		switch {
		case r.URL.Path != "/report":
			w.Write([]byte("<html>index</html>"))
		case code == 0:
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case code >= 300 && code < 400:
			http.Redirect(w, r, "/elsewhere", code)
		case code == 429:
			http.Error(w, `{"error":"TOO_MANY_REPORTS"}`, code)
		case code == tooManyRuns:
			http.Error(w, `{"error":"TOO_MANY_RUNS"}`, 429)
		default:
			w.WriteHeader(code)
		}
		if s.onRequest != nil {
			s.onRequest()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// tooManyRuns in a script is 429 {"error":"TOO_MANY_RUNS"}, the server's
// daily cap of an app; a plain 429 is its per-minute rate limit.
const tooManyRuns = 1429

func (s *scriptedServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// deliver sends body to server with report and returns its line and the
// waits between attempts.
func deliver(t *testing.T, server string, client *http.Client, body any) (string, []time.Duration) {
	t.Helper()
	return deliverCtx(t, context.Background(), server, client, body, nil)
}

// deliverCtx is deliver within ctx; during, when set, runs in each wait.
func deliverCtx(t *testing.T, ctx context.Context, server string, client *http.Client, body any, during func()) (string, []time.Duration) {
	t.Helper()
	var waits []time.Duration
	old := reportWait
	reportWait = func(_ context.Context, d time.Duration) {
		waits = append(waits, d)
		if during != nil {
			during()
		}
	}
	t.Cleanup(func() { reportWait = old })
	cfg := &config.App{App: "shop", Hosted: &config.Hosted{Server: server, TokenEnv: "BRACER_TOKEN"}}
	secrets := config.NewSecrets(map[string]string{"BRACER_TOKEN": earlyToken}, "test")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	line := report(ctx, cfg, secrets, backupDeps{http: client}, logger, body)
	return line, waits
}

// BR-014: the wait between attempts ends as soon as the context does
// (SIGTERM), and a run cancelled during an attempt or a wait is not tried
// again.
func TestBR014ReportStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	reportWait(ctx, time.Hour)
	if time.Since(start) > time.Second {
		t.Fatal("the wait outlived its context")
	}
	result := &backup.Result{Kind: "backup", App: "shop", TS: "2026-11-05T030000Z", Status: backup.ResultOK}
	// Cancelled while the server answers 503 (the client sees the 503 or
	// its own cancel, both retried otherwise): no wait, no second attempt.
	srv := newScriptedServer(t, 503)
	ctx, cancel = context.WithCancel(context.Background())
	srv.onRequest = cancel
	line, waits := deliverCtx(t, ctx, srv.URL, srv.Client(), result, nil)
	if len(waits) != 0 || len(srv.requests()) != 1 || !strings.HasPrefix(line, "✗ not reported to ") || strings.Contains(line, "attempts") {
		t.Errorf("cancelled in an attempt: %q, waits %v, requests %v", line, waits, srv.requests())
	}
	// Cancelled during the wait: no second attempt.
	srv = newScriptedServer(t, 503)
	ctx, cancel = context.WithCancel(context.Background())
	line, waits = deliverCtx(t, ctx, srv.URL, srv.Client(), result, cancel)
	if len(waits) != 1 || len(srv.requests()) != 1 || !strings.HasSuffix(line, "the server answered 503 Service Unavailable") {
		t.Errorf("cancelled in a wait: %q, waits %v, requests %v", line, waits, srv.requests())
	}
}

// BR-014: a report is delivered only when the server answers 202; network
// errors, 429 and 5xx are tried again a bounded number of times, a run
// with a ts always (the server overwrites (app, ts)), a failure report
// (keyed by its arrival) only when the server surely did not store it;
// any other answer is not retried.
func TestBR014ReportDelivery(t *testing.T) {
	result := &backup.Result{Kind: "backup", App: "shop", TS: "2026-11-05T030000Z", Status: backup.ResultOK, OK: true}
	failure := failureReport("backup", "shop", time.Now(), time.Now(), errors.New("lock"), nil)
	cases := []struct {
		name     string
		body     any
		script   []int
		ok       bool
		requests int
	}{
		{"accepted", result, []int{202}, true, 1},
		{"result: restart, then stored", result, []int{503, 502, 504, 202}, true, 4},
		{"result: connection dropped", result, []int{0, 202}, true, 2},
		{"result: always 500", result, []int{500}, false, reportAttempts},
		{"failure: 429 and 503 are not stored", failure, []int{429, 503, 500, 202}, true, 4},
		{"result: rate limited, then stored", result, []int{429, 202}, true, 2},
		{"result: TOO_MANY_RUNS is not retried", result, []int{tooManyRuns, 202}, false, 1},
		{"failure: TOO_MANY_RUNS is not retried", failure, []int{tooManyRuns, 202}, false, 1},
		{"failure: 502 may have been stored", failure, []int{502, 202}, false, 1},
		{"failure: 504 may have been stored", failure, []int{504, 202}, false, 1},
		{"failure: dropped after sending", failure, []int{0, 202}, false, 1},
		{"200 is not a stored report", result, []int{200}, false, 1},
		{"400 is not retried", result, []int{400, 202}, false, 1},
		{"401 is not retried", failure, []int{401, 202}, false, 1},
	}
	for _, c := range cases {
		srv := newScriptedServer(t, c.script...)
		line, waits := deliver(t, srv.URL, srv.Client(), c.body)
		reqs := srv.requests()
		if strings.HasPrefix(line, "✓ reported to ") != c.ok || len(reqs) != c.requests {
			t.Errorf("%s: %q after %d requests %v", c.name, line, len(reqs), reqs)
		}
		if len(waits) != len(reqs)-1 {
			t.Errorf("%s: waits %v", c.name, waits)
		}
	}
	srv := newScriptedServer(t, 500)
	line, waits := deliver(t, srv.URL, srv.Client(), result)
	if !strings.HasSuffix(line, "the server answered 500 Internal Server Error (4 attempts)") || len(waits) != 3 || waits[0] != 2*time.Second || waits[2] != 8*time.Second {
		t.Errorf("bounded: %q, waits %v", line, waits)
	}
}

// BR-014: nothing listens (the API restarting): no connection was made, so
// even a failure report is tried again, and the time stays bounded.
func TestBR014ReportNotConnected(t *testing.T) {
	srv := newScriptedServer(t, 202)
	url := srv.URL
	srv.Close()
	for _, body := range []any{failureReport("backup", "shop", time.Now(), time.Now(), errors.New("lock"), nil),
		&backup.Result{Kind: "backup", App: "shop", TS: "2026-11-05T030000Z", Status: backup.ResultOK}} {
		line, waits := deliver(t, url, &http.Client{Timeout: time.Second}, body)
		if !strings.HasPrefix(line, "✗ not reported to ") || !strings.HasSuffix(line, "(4 attempts)") || len(waits) != 3 {
			t.Errorf("%q, waits %v", line, waits)
		}
	}
}

// BR-014: a redirect of POST /report is not followed: the client would
// send a GET without the body and take the 200 of a web page for a stored
// report, every night; the token goes nowhere else.
func TestBR014ReportRedirectNotFollowed(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		srv := newScriptedServer(t, code)
		line, _ := deliver(t, srv.URL, srv.Client(), &backup.Result{Kind: "backup", App: "shop", TS: "2026-11-05T030000Z", Status: backup.ResultOK})
		reqs := srv.requests()
		if !strings.HasPrefix(line, "✗ not reported to ") || len(reqs) != 1 || reqs[0] != "POST /report Bearer "+earlyToken {
			t.Errorf("%d: %q, requests %v", code, line, reqs)
		}
	}
}
