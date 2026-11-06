package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
)

// BR-021 review D1: Ctrl-C while POST /pair/finish is in flight. The server
// may have committed (on a continue the old token is already revoked), so
// the CLI does not say nothing happened: it says the app may be in the
// account and that running again continues it.
func TestBR021InterruptDuringFinish(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fp := newFakePair()
	fp.react = webHappy
	fp.onFinish = cancel
	fp.finishErrs = []error{&netError{context.Canceled}}
	w := &world{sh: containerWorld(), args: webArgs, pair: fp, ctx: ctx}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	want := "The backup passed, but init was interrupted while the server was answering: app shop may be in your account (created or continued).\nFix: run bracer init shop --web again: it continues the app with a new token."
	if code != exitError || !strings.Contains(out, want) || strings.Contains(errOut, "No app.yaml written") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if _, err := os.Stat(br021Secrets(dir)); !os.IsNotExist(err) {
		t.Errorf("secrets.env left: %v", err)
	}
}

// BR-021 review D3/R2: Cancel on the web during the proof stops the proof
// at the next keepalive: the child's context ends (SIGINT, as on Ctrl-C),
// nothing is confirmed, one line says why.
func TestBR021WebCancelStopsProof(t *testing.T) {
	old := pairKeepAlive
	pairKeepAlive = 5 * time.Millisecond
	t.Cleanup(func() { pairKeepAlive = old })
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	confirms := 0
	fp.stateErr = func(state map[string]any) error {
		if state["step"] != "confirm" {
			return nil
		}
		confirms++
		if confirms == 1 {
			return nil // the summary's own post
		}
		fp.events = append(fp.events, json.RawMessage(`{"type":"cancelled","reason":"user"}`))
		return errPairClosed
	}
	var stoppedBy error
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	w.selfRunCtx = func(ctx context.Context, args []string) int {
		if hasArg(args, "--dry-run") {
			return 0
		}
		select {
		case <-ctx.Done():
			stoppedBy = ctx.Err()
			return 130
		case <-time.After(3 * time.Second):
			return 0
		}
	}
	start := time.Now()
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != exitError || stoppedBy == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("exit %d stopped %v after %s\nstdout:\n%s\nstderr:\n%s", code, stoppedBy, time.Since(start), out, errOut)
	}
	if !strings.Contains(out, "The setup was cancelled on the web while the first backup ran, so app shop was not added or changed.") || strings.Count(errOut, "web:") > 0 {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if strings.Contains(strings.Join(fp.finished, ","), "true") || confirms != 2 {
		t.Errorf("finish %v, confirms %d", fp.finished, confirms)
	}
	for _, p := range []string{appYAML(dir), draftPath(appYAML(dir)), br021Secrets(dir)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left: %v", p, err)
		}
	}
}

// BR-021 review D6: a finish the server refuses otherwise still closes the
// session, so the web does not wait on "Finish in your terminal".
func TestBR021RefusedFinishClosesSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	fp.finishErrs = []error{&pairError{Status: 409, Code: "NOT_CLAIMED"}}
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld(), args: webArgs, pair: fp}, answers(s3Answers, noEncryption, webAnswers)...)
	if code != exitError || strings.Join(fp.finished, ",") != "true,false" {
		t.Fatalf("exit %d finish %v\n%s\n%s", code, fp.finished, out, errOut)
	}
}

// BR-021 review D8/R4: the proof report refused with 401 says what to do,
// and that the app shows no run until the next backup reports.
func TestBR021ProofReport401(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(srv.Close)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: []string{"shop", "--web", "--server", srv.URL}, pair: fp, reportHTTP: srv.Client()}
	w.selfRun = br021Child(t, dir, 0)
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"✗ not reported to " + srv.URL + ": the server answered 401 Unauthorized; app shop shows no run on the web until its next backup reports",
		"Fix: app shop was paired again from another machine or deleted on the web; run bracer init shop --web to pair this machine again, or bracer remove shop to stop backups here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// BR-021 review D8: the proof's result unreadable is a warning with a Fix.
func TestBR021ProofResultMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	srv := newReportServer(t)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: []string{"shop", "--web", "--server", srv.URL}, pair: fp, reportHTTP: srv.Client()}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 || !strings.Contains(errOut, "warning: the first backup was not reported") || !strings.Contains(errOut, "app shop shows no run on the web until its next backup reports\nFix: bracer backup shop") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// BR-021 review D8: the "is in your account" line ends with one Fix line.
func TestBR021InAccountHasFix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	w.selfRun = br021Child(t, dir, 0)
	fp.onFinish = func() {
		draft := draftPath(appYAML(dir))
		os.Remove(draft)
		os.MkdirAll(draft+"/x", 0o700)
	}
	_, out, _ := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if !strings.Contains(out, "App shop is in your account, but this machine was not set up to report to it.\nFix: run bracer init shop --web again: it continues the app with a new token.") {
		t.Errorf("stdout:\n%s", out)
	}
}

// BR-021 review R4: a nightly backup refused with 401 says why and what
// to run.
func TestBR021BackupReport401Fix(t *testing.T) {
	srv := &reportServer{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(srv.Close)
	d, out, errb := urlApp(t, srv, map[string]string{"main": "postgres://u:" + br008Password + "@10.0.0.1:abc/db"})
	runBackup(context.Background(), []string{"shop"}, d)
	if !strings.Contains(out.String(), "✗ not reported to "+srv.URL+": the server answered 401 Unauthorized\nFix: app shop was paired again from another machine or deleted on the web; run bracer init shop --web to pair this machine again, or bracer remove shop to stop backups here") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errb)
	}
}

// BR-021 + BR-023 under --web: the proof run, reported after finish, keeps
// the draft's since (its own period); the installed app.yaml has the later
// one stamped just before the rename.
func TestBR021WebSince(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	srv := newReportServer(t)
	fp := newFakePair()
	fp.react = webHappy
	t0 := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	clock := t0
	w := &world{sh: containerWorld(), args: []string{"shop", "--web", "--server", srv.URL}, pair: fp, reportHTTP: srv.Client(), now: func() time.Time { return clock }}
	child := br021Child(t, dir, 0)
	var draftSince string
	w.selfRun = func(args []string) int {
		if !hasArg(args, "--dry-run") {
			raw, _ := os.ReadFile(draftPath(appYAML(dir)))
			cfg, err := config.Parse(raw)
			if err != nil {
				t.Errorf("draft: %v", err)
				return 1
			}
			draftSince = cfg.Schedule.Since
			clock = t0.Add(90 * time.Second) // the run took 90 s
		}
		return child(args)
	}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	got := srv.got()
	if len(got) != 1 {
		t.Fatalf("reports: %v", got)
	}
	var m struct {
		Schedule map[string]any `json:"schedule"`
	}
	if err := json.Unmarshal([]byte(got[0]), &m); err != nil || m.Schedule["since"] != "2026-11-05T03:00:00Z" || draftSince != "2026-11-05T03:00:00Z" {
		t.Errorf("proof report since %v, draft since %q", m.Schedule["since"], draftSince)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil || cfg.Schedule.Since != "2026-11-05T03:01:30Z" || cfg.Hosted == nil {
		t.Errorf("app.yaml: %+v %v", cfg, err)
	}
}
