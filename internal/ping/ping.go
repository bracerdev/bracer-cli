// Package ping sends heartbeats for any job to a Bracer server
// (spec backup/ping). It does not read app.yaml and needs no Docker.
package ping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultServer is the default hosted server when neither --server nor BRACER_SERVER is set. [unverified] domain.
const DefaultServer = "https://app.bracer.dev"

// EnvServer is the environment variable that overrides the server.
const EnvServer = "BRACER_SERVER"

// MaxBody is the body size limit the server stores (spec ping): the tail is kept.
const MaxBody = 1024

// ErrUnknownID: the server does not know the heartbeat id (404).
var ErrUnknownID = errors.New("unknown heartbeat id")

var reID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidID checks the id before any network call.
func ValidID(id string) bool { return reID.MatchString(id) }

// ResolveServer picks the server: flag > env > default; trailing "/" dropped; http(s) only.
func ResolveServer(flag string, getenv func(string) string) (string, error) {
	s := flag
	if s == "" {
		s = getenv(EnvServer)
	}
	if s == "" {
		s = DefaultServer
	}
	s = strings.TrimRight(s, "/")
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("server must be an http(s) URL, got %q", s)
	}
	return s, nil
}

// Client sends pings with retry; tests replace HTTP and Sleep.
type Client struct {
	HTTP      *http.Client
	Server    string
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

// Path builds the path for a variant: "", "start", "fail", or an exit code.
func Path(id, variant string) string {
	if variant == "" {
		return "/ping/" + id
	}
	return "/ping/" + id + "/" + variant
}

// Send sends a GET (no body) or POST (with body, truncated to MaxBody keeping the tail),
// trying Attempts times with waits of 1 s, 2 s… 2xx → nil; 404 → ErrUnknownID (no retry).
func (c *Client) Send(ctx context.Context, id, variant string, body []byte) error {
	c.defaults()
	body = Tail(body, MaxBody)
	u := c.Server + Path(id, variant)
	var last error
	for i := 0; i < c.Attempts; i++ {
		if i > 0 {
			c.Sleep(time.Duration(i) * time.Second)
		}
		method := http.MethodGet
		var rd io.Reader
		if len(body) > 0 {
			method, rd = http.MethodPost, bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		if rd != nil {
			req.Header.Set("Content-Type", "text/plain")
		}
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
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
			return fmt.Errorf("%w: %s", ErrUnknownID, id)
		default:
			last = fmt.Errorf("server returned %s", resp.Status)
		}
	}
	return last
}

// Tail keeps the last n bytes.
func Tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// ringBuffer keeps the last n bytes of everything written to it (tees the child's output).
type ringBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.n {
		r.buf = r.buf[len(r.buf)-r.n:]
	}
	return len(p), nil
}

func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}

// Wrap runs a command in wrapped mode: /start → run (stdio passed through, keeping
// the last MaxBody of stdout+stderr) → /<exit> with body. Returns the command's exit
// code; ping errors go only to Log. If it cannot be started → /fail, 127.
func (c *Client) Wrap(ctx context.Context, id string, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c.defaults()
	if err := c.Send(ctx, id, "start", nil); err != nil {
		c.Log("ping start failed: %v", err)
	}
	tail := &ringBuffer{n: MaxBody}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = io.MultiWriter(stdout, tail)
	cmd.Stderr = io.MultiWriter(stderr, tail)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "bracer ping: cannot run %s: %v\n", argv[0], err)
		if perr := c.Send(ctx, id, "fail", []byte(err.Error())); perr != nil {
			c.Log("ping fail failed: %v", perr)
		}
		return 127
	}
	// Forward signals to the child; ctx cancelled (Ctrl-C) → SIGTERM.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			if code < 0 { // killed by a signal
				if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
					code = 128 + int(st.Signal())
				} else {
					code = 1
				}
			}
		} else {
			code = 1
		}
	}
	variant := strconv.Itoa(code)
	if code != 0 && ctx.Err() != nil {
		variant = "fail"
	}
	// Send the result with a fresh context: ctx may already be cancelled by Ctrl-C.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
	defer cancel()
	if perr := c.Send(sctx, id, variant, tail.Bytes()); perr != nil {
		c.Log("ping %s failed: %v", variant, perr)
	}
	return code
}

// ReadStdinBody reads stdin when it is a pipe (not a TTY), at most MaxBody + 1
// to know whether it was truncated; TTY → nil.
func ReadStdinBody(f *os.File) []byte {
	if f == nil {
		return nil
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, 64*1024))
	return Tail(b, MaxBody)
}
