package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// br021Result is what the child `backup --init-draft` writes next to the
// draft; the fake child writes it on the real run.
const br021Result = `{"kind":"backup","app":"shop","ts":"2026-11-05T100000Z","started_at":"2026-11-05T10:00:00Z","finished_at":"2026-11-05T10:01:00Z","ok":true,"status":"ok","instances":[]}`

func br021Child(t *testing.T, dir string, code int) func(args []string) int {
	return func(args []string) int {
		if hasArg(args, "--dry-run") {
			return 0
		}
		if err := os.WriteFile(resultPath(draftPath(appYAML(dir))), []byte(br021Result), 0o600); err != nil {
			t.Fatal(err)
		}
		return code
	}
}

func br021Secrets(dir string) string {
	return filepath.Join(filepath.Dir(appYAML(dir)), config.SecretsFile)
}

// BR-021: the app is created in the account only after the proof passed:
// at finish the real run is done, the draft has no hosted: and
// secrets.env no token; then the token and hosted: are written and the
// proof run is reported once with that token.
func TestBR021FinishAfterProof(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	srv := newReportServer(t)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: []string{"shop", "--web", "--server", srv.URL}, pair: fp, reportHTTP: srv.Client()}
	w.selfRun = br021Child(t, dir, 0)
	fp.onFinish = func() {
		if len(w.selfLog) != 4 || hasArg(w.selfLog[2], "--dry-run") {
			t.Errorf("finish before the real run: %v", w.selfLog)
		}
		draft, err := config.Load(draftPath(appYAML(dir)))
		if err != nil || draft.Hosted != nil {
			t.Errorf("draft at finish: %+v %v", draft, err)
		}
		env, _ := os.ReadFile(br021Secrets(dir))
		if strings.Contains(string(env), hostedTokenEnv) {
			t.Errorf("token before finish:\n%s", env)
		}
	}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if fp.openApp != "shop" || strings.Join(fp.finished, ",") != "true" {
		t.Errorf("open %q finish %v", fp.openApp, fp.finished)
	}
	for _, want := range []string{
		"Write these files and, once the first backup passes, create app shop in your account? [y/N]",
		"App shop created in your account on " + srv.URL + ".",
		"✓ reported to " + srv.URL,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "brc_test") {
		t.Errorf("token printed:\n%s", out)
	}
	got := srv.got()
	if len(got) != 1 || !strings.Contains(got[0], `"ts":"2026-11-05T100000Z"`) || srv.auth[0] != "Bearer brc_test" {
		t.Errorf("proof report: %v %v", got, srv.auth)
	}
	if _, err := os.Stat(resultPath(draftPath(appYAML(dir)))); !os.IsNotExist(err) {
		t.Errorf("result file left: %v", err)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil || cfg.Hosted == nil || cfg.Hosted.Server != srv.URL {
		t.Fatalf("app.yaml: %+v %v", cfg, err)
	}
}

// BR-021: a failed proof never confirms: the session is closed with
// finish(false), nothing is in the account, nothing stays on the machine.
func TestBR021FailedProofNoApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	w.selfRun = br021Child(t, dir, 1)
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != exitError || strings.Join(fp.finished, ",") != "false" {
		t.Fatalf("exit %d finish %v\n%s\n%s", code, fp.finished, out, errOut)
	}
	for _, p := range []string{appYAML(dir), draftPath(appYAML(dir)), resultPath(draftPath(appYAML(dir))), br021Secrets(dir)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left: %v", p, err)
		}
	}
}

// BR-021: continuing an app says so before anything is written: at the
// claim (when it last reported, from where), at the summary, at the end.
func TestBR021ContinueSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.events = []json.RawMessage{json.RawMessage(`{"type":"claimed","account":"k***@example.com","app":{"name":"shop","continues":true,"app_id":"a1","last_report_at":"2026-11-04T03:00:00Z","hostname":"old-vps"}}`)}
	fp.react = webHappy
	fp.continued = true
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	last := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	for _, want := range []string{
		"Claimed by k***@example.com on the web, to continue app shop (last report " + last + " from old-vps): its history stays; the token of its earlier setup stops working once this init passes. Continue with that account? [y/N]",
		"once the first backup passes, continue app shop in your account? [y/N]",
		"App shop continued in your account on https://app.test; the token of its earlier setup no longer works.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// BR-021: the web confirmed another app than the command's: stop.
func TestBR021ClaimForAnotherApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.events = []json.RawMessage{json.RawMessage(`{"type":"claimed","account":"k","app":{"name":"blog"}}`)}
	code, _, errOut := runWorld(t, dir, &world{sh: containerWorld(), args: webArgs, pair: fp}, answers(s3Answers, noEncryption)...)
	if code != exitError || !strings.Contains(errOut, `the web confirmed app "blog", not shop`) || strings.Join(fp.finished, ",") != "false" {
		t.Errorf("exit %d finish %v\n%s", code, fp.finished, errOut)
	}
}

// BR-021: the token came but this machine could not be set up with it:
// the token is not printed, nothing stays, and the customer learns the
// app is in the account and that running again continues it.
func TestBR021ErrorAfterFinish(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	w.selfRun = br021Child(t, dir, 0)
	fp.onFinish = func() {
		// The draft cannot be written again: a directory with a file in it.
		draft := draftPath(appYAML(dir))
		os.Remove(draft)
		os.MkdirAll(filepath.Join(draft, "x"), 0o700)
	}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != exitError || strings.Contains(out+errOut, "brc_test") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "App shop is in your account, but this machine was not set up to report to it.\nFix: run bracer init shop --web again: it continues the app with a new token.") {
		t.Errorf("stdout:\n%s", out)
	}
	if strings.Join(fp.finished, ",") != "true" {
		t.Errorf("finish %v", fp.finished)
	}
	if _, err := os.Stat(br021Secrets(dir)); !os.IsNotExist(err) {
		t.Errorf("secrets.env with the token left: %v", err)
	}
	if _, err := os.Stat(appYAML(dir)); !os.IsNotExist(err) {
		t.Error("app.yaml written")
	}
}

// BR-021: finish lost on the network, then 410: the answer may be what was
// lost, so the CLI does not claim nothing was added.
func TestBR021FinishAnswerLost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	fp.finishErrs = []error{&netError{errors.New("connection reset")}, errPairClosed}
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld(), args: webArgs, pair: fp}, answers(s3Answers, noEncryption, webAnswers)...)
	if code != exitError || !strings.Contains(out, "not added to your account: no answer from the server (if it added the app, running again continues it).") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(br021Secrets(dir)); !os.IsNotExist(err) {
		t.Errorf("secrets.env left: %v", err)
	}
}

// BR-021: the session is kept alive while the proof runs, and the re-posts
// stop before finish.
func TestBR021KeepAlive(t *testing.T) {
	old := pairKeepAlive
	pairKeepAlive = 5 * time.Millisecond
	t.Cleanup(func() { pairKeepAlive = old })
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	w.selfRun = func(args []string) int {
		if !hasArg(args, "--dry-run") {
			time.Sleep(60 * time.Millisecond)
		}
		return 0
	}
	var atFinish int
	fp.onFinish = func() { atFinish = len(fp.states) }
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	steps := fp.steps()
	if !strings.HasPrefix(steps, "instances terminal databases checks terminal confirm confirm") || atFinish != len(fp.states) {
		t.Errorf("states %s; %d at finish, %d after", steps, atFinish, len(fp.states))
	}
}

// BR-021 (audit): a Postgres on this machine is kind local for the web,
// not url.
func TestBR021LocalKind(t *testing.T) {
	got := describeCandidate(pg.Candidate{Local: &pg.Local{Dir: "/var/run/postgresql", Port: 5432}})
	if got.ID != "local:5432" || got.Kind != "local" {
		t.Errorf("%+v", got)
	}
}

// BR-021 (audit): a database skipped at the external question reaches the
// web as skipped, not as a passed check; one skipped on the web carries
// the flag, not a "skipped: " prefix in its reason.
func TestBR021ExternalSkipIsSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sh := containerWorld()
	sh.out[psql+"select 'function '"] = "function f() (language plpython3u)\n"
	gated := &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "select 'function '") && envOf(c.Env, "PGDATABASE") != "shop" {
			return pg.Result{}, nil, true
		}
		return pg.Result{}, nil, false
	}}
	fp := newFakePair()
	fp.react = func(state map[string]any) []string {
		switch state["step"] {
		case "instances":
			return []string{`{"type":"select_instances","instances":["container:shop-db"]}`}
		case "databases":
			return []string{`{"type":"select_databases","instance_id":"container:shop-db","databases":["postgres","shop"]}`}
		}
		return nil
	}
	code, out, errOut := runWorld(t, dir, &world{sh: gated, args: webArgs, pair: fp},
		answers(s3Answers, noEncryption, []string{"y", "", "s"}, drillOff, sched, []string{"y"})...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var checks []any
	for _, s := range fp.states {
		if s["step"] == "checks" {
			checks = s["checks"].([]any)
		}
	}
	if len(checks) != 2 {
		t.Fatalf("checks: %v", checks)
	}
	pgc, shop := checks[0].(map[string]any), checks[1].(map[string]any)
	if pgc["skipped"] != nil || shop["skipped"] != true || shop["reason"] != "depends on something outside Postgres" {
		t.Errorf("checks: %v", checks)
	}
}

// BR-021: `backup --init-draft` keeps its Result next to the draft (600),
// redacted like run.json, for init to report once it has the token.
func TestBR021ChildKeepsResult(t *testing.T) {
	srv := newReportServer(t)
	d, out, errb := urlApp(t, srv, map[string]string{"main": "postgres://u:" + br008Password + "@10.0.0.1:abc/db"})
	p := config.AppPath(testHome(t), "shop")
	if err := os.Rename(p, draftPath(p)); err != nil {
		t.Fatal(err)
	}
	runBackup(context.Background(), []string{"shop", "--init-draft"}, d)
	data, err := os.ReadFile(resultPath(draftPath(p)))
	if err != nil {
		t.Fatalf("%v\n%s%s", err, out, errb)
	}
	var res backup.Result
	if err := json.Unmarshal(data, &res); err != nil || res.TS == "" || res.App != "shop" || strings.Contains(string(data), br008Password) {
		t.Errorf("result %v:\n%s", err, data)
	}
	if fi, _ := os.Stat(resultPath(draftPath(p))); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}
