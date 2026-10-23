// Package ping is the heartbeat client of backup (ADR-0025): one request to
// a Healthchecks-style URL when a run starts and one with its exit code when
// it ends, a POST carrying the run's warnings when there are any
// (ADR-0029). The URL is a secret and never appears in an error.
package ping

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnknownURL: the service does not know the URL (404).
var ErrUnknownURL = errors.New("unknown heartbeat URL (404)")

// Client sends heartbeats with retry; tests replace HTTP and Sleep.
type Client struct {
	HTTP      *http.Client
	UserAgent string
	Attempts  int                 // default 3
	Sleep     func(time.Duration) // default time.Sleep
	Log       func(format string, a ...any)
}

func (c *Client) defaults() {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if c.Attempts <= 0 {
		c.Attempts = 3
	}
	if c.Sleep == nil {
		c.Sleep = time.Sleep
	}
	if c.Log == nil {
		c.Log = func(string, ...any) {}
	}
}

// ValidHeartbeatURL checks a heartbeat URL (ADR-0025): http(s) with a host.
func ValidHeartbeatURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("the heartbeat URL must be an http(s) URL with a host")
	}
	return nil
}

// HeartbeatURL appends a variant ("start", "log", an exit code) to the path
// of a Healthchecks-style URL, keeping its query (ADR-0025).
func HeartbeatURL(base, variant string) (string, error) {
	if err := ValidHeartbeatURL(base); err != nil {
		return "", err
	}
	u, _ := url.Parse(base)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + variant
	u.RawPath = ""
	return u.String(), nil
}

// SendURL sends to base + "/" + variant, a GET without body or, when body is
// not empty, a POST of it as text/plain, trying Attempts times with waits of
// 1 s, 2 s…: 2xx → nil; 404 → ErrUnknownURL (no retry).
func (c *Client) SendURL(ctx context.Context, base, variant, body string) error {
	u, err := HeartbeatURL(base, variant)
	if err != nil {
		return err
	}
	err = c.send(ctx, u, body)
	// The URL is a secret: never in the error.
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *Client) send(ctx context.Context, u, body string) error {
	c.defaults()
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	var last error
	for i := 0; i < c.Attempts; i++ {
		if i > 0 {
			c.Sleep(time.Duration(i) * time.Second)
		}
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, r)
		if err != nil {
			return err
		}
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}
		if body != "" {
			req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}
		c.Log("%s %s", method, u)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusNotFound:
			return ErrUnknownURL
		default:
			last = fmt.Errorf("server returned %s", resp.Status)
		}
	}
	return last
}
