package cli

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/ping"
)

// heartbeat is the one signal per run to the Healthchecks-style URL of
// app.yaml (ADR-0025): /start when the backup begins, /<code> when it ends
// (the drill included). A run that passed with warnings sends them as the
// body of /0, so the service keeps them in its log without alerting
// (ADR-0029). The service behind the URL notices a missing or failed
// signal; bracer only sends it. A nil heartbeat sends nothing.
type heartbeat struct {
	client *ping.Client
	url    string
	log    *slog.Logger
}

// newHeartbeat is nil when app.yaml has no heartbeat_url_env; loadApp has
// checked that the secret is there.
func newHeartbeat(cfg *config.App, secrets *config.Secrets, d backupDeps, logger *slog.Logger) *heartbeat {
	if cfg.HeartbeatURLEnv == "" {
		return nil
	}
	u, _ := secrets.Require(cfg.HeartbeatURLEnv)
	return &heartbeat{client: heartbeatClient(d.http), url: u, log: logger}
}

// heartbeatSleep waits between attempts; tests make it instant.
var heartbeatSleep = time.Sleep

// heartbeatClient is the heartbeat HTTP client over h: 10 s, no redirects.
func heartbeatClient(h *http.Client) *ping.Client {
	c := http.Client{}
	if h != nil {
		c = *h
	}
	c.Timeout = 10 * time.Second
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ping.Client{HTTP: &c, UserAgent: "bracer/" + Version, Sleep: heartbeatSleep}
}

// send sends one variant with body (POST when it is not empty, else GET);
// a failure only warns, the job's exit stays.
func (h *heartbeat) send(ctx context.Context, variant, body string) {
	if h == nil {
		return
	}
	// Sent even after Ctrl-C: the result is what the service waits for.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
	defer cancel()
	if err := h.client.SendURL(sctx, h.url, variant, body); err != nil {
		h.log.Warn("heartbeat not sent", "signal", variant, "err", firstLine(err.Error()))
	}
}

// finish sends /<code>; the warnings go with a 0, one per line.
func (h *heartbeat) finish(ctx context.Context, code int, warnings []string) {
	body := ""
	if code == exitOK && len(warnings) > 0 {
		body = strings.Join(warnings, "\n") + "\n"
	}
	h.send(ctx, strconv.Itoa(code), body)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
