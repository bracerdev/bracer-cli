package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/ping"
)

// checkHeartbeatURL is GET <url>/log: the service records an event without
// changing the check's state (ADR-0025).
func checkHeartbeatURL(ctx context.Context, u string) error {
	return heartbeatClient(nil).SendURL(ctx, u, "log", "")
}

// askHeartbeat is step 13b: an optional Healthchecks-style URL that gets one
// signal per backup run, its test-restore included (ADR-0025, ADR-0026).
func (r *initRun) askHeartbeat() error {
	prompt := "Heartbeat URL, Healthchecks-style (e.g. https://hc-ping.com/<uuid>; Enter to skip)"
	r.p.printf("\nA heartbeat URL gets one signal each time backup runs (its test-restore included): its exit code, and when it passes with warnings, the warning lines, which can name instances, databases and row counts. The service behind it (Healthchecks or compatible) tells you when the signal fails or does not come.\n")
	for {
		v, err := r.p.askSecret(prompt)
		if err != nil {
			return err
		}
		v = strings.TrimSpace(v)
		if v == "" {
			r.cfg.HeartbeatURLEnv = ""
			return nil
		}
		if err := ping.ValidHeartbeatURL(v); err != nil {
			r.p.printf("  %v\n", err)
			continue
		}
		if err := r.checkHeartbeat(r.ctx, v); err != nil {
			r.p.printf("  ✗ %s: %v\n", hostOf(v), err)
			continue
		}
		r.p.printf("  ✓ the heartbeat URL at %s answers\n", hostOf(v))
		name := config.EnvVarName(r.cfg.App, "heartbeat", "url")
		r.secrets[name] = v
		r.cfg.HeartbeatURLEnv = name
		return nil
	}
}

// heartbeatURL is the URL app.yaml points at, typed in this run.
func (r *initRun) heartbeatURL() string {
	if r.cfg.HeartbeatURLEnv == "" {
		return ""
	}
	return r.secrets[r.cfg.HeartbeatURLEnv]
}

// printHeartbeat is step 19 with a heartbeat: what the customer sets on the
// check, since the service, not bracer, notices a missing signal.
func (r *initRun) printHeartbeat() {
	u := r.heartbeatURL()
	if u == "" || r.cfg.Schedule == nil {
		return
	}
	sched := "schedule: " + scheduleWhen(*r.cfg.Schedule)
	r.p.printf("\nHeartbeat: set the check at %s to\n  %s\n  time zone: %s\n  grace time: at least 1 hour (more if the backup and its test-restore take longer)\nbracer only sends the signal; %s is what notices a missing one.\n", hostOf(u), sched, machineZone(), hostOf(u))
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return "the heartbeat URL"
}

// machineZone is the time zone the timers run in: the IANA name of
// /etc/localtime, else the UTC offset.
func machineZone() string {
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	_, off := time.Now().Zone()
	if off == 0 {
		return "UTC"
	}
	return fmt.Sprintf("UTC%+03d:%02d", off/3600, (abs(off)%3600)/60)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
