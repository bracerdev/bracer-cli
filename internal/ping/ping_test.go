package ping

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ADR-0025: the variant goes at the end of the URL's path, the query stays.
func TestHeartbeatURL(t *testing.T) {
	for _, tc := range []struct{ base, variant, want string }{
		{"https://hc-ping.com/uuid", "start", "https://hc-ping.com/uuid/start"},
		{"https://hc-ping.com/uuid/", "0", "https://hc-ping.com/uuid/0"},
		{"https://hc.example/ping/key/slug?create=1", "log", "https://hc.example/ping/key/slug/log?create=1"},
	} {
		if got, err := HeartbeatURL(tc.base, tc.variant); err != nil || got != tc.want {
			t.Errorf("HeartbeatURL(%q, %q) = %q %v", tc.base, tc.variant, got, err)
		}
	}
	for _, bad := range []string{"hc-ping.com/uuid", "ftp://x/y", "https://"} {
		if _, err := HeartbeatURL(bad, "0"); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// A 5xx is retried after 1 s then 2 s; a 404 is not.
func TestSendURLRetry(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		n := len(paths)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusNotFound)
		case n < 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	var slept []time.Duration
	c := &Client{HTTP: srv.Client(), Sleep: func(d time.Duration) { slept = append(slept, d) }}
	if err := c.SendURL(context.Background(), srv.URL+"/id", "start", ""); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths[0] != "/id/start" || len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Errorf("paths %v slept %v", paths, slept)
	}
	if err := c.SendURL(context.Background(), srv.URL+"/id", "gone", ""); err != ErrUnknownURL || len(paths) != 4 {
		t.Errorf("404: %v after %v", err, paths)
	}
}

// ADR-0029: a body makes the signal a POST of text/plain; without one it is
// a GET.
func TestSendURLBody(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Sleep: func(time.Duration) {}}
	if err := c.SendURL(context.Background(), srv.URL+"/id", "0", "main/shop dropped from 2000 to 100 rows\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendURL(context.Background(), srv.URL+"/id", "1", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "POST /id/0 text/plain; charset=utf-8 main/shop dropped from 2000 to 100 rows\n|GET /id/1  " {
		t.Errorf("requests: %q", got)
	}
}

// The URL is a secret: SendURL's errors never contain it.
func TestSendURLHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := &Client{HTTP: srv.Client(), Sleep: func(time.Duration) {}}
	if err := c.SendURL(context.Background(), srv.URL+"/secret", "0", ""); err == nil || err.Error() != "unknown heartbeat URL (404)" {
		t.Errorf("404: %v", err)
	}
	srv.Close()
	err := c.SendURL(context.Background(), srv.URL+"/secret", "0", "")
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("network error shows the URL: %v", err)
	}
}
