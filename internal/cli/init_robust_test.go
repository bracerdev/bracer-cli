package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// BR-016 item 1: SIGTERM (systemctl stop, shutdown) and SIGHUP (SSH drop)
// cancel the command like Ctrl-C, so its cleanup runs. Unhandled, either
// signal kills this test binary.
func TestBR016StopSignalsCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		ctx, stop := signalContext()
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Errorf("%v did not cancel the command", sig)
		}
		stop()
	}
}

// BR-016 item 2: an interrupted init signals its proof run like Ctrl-C and
// waits for it, so the run's cleanup happens; it is not killed outright.
func TestBR016ProofRunCleansUpOnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	var out, errb lockedBuffer
	script := `trap 'echo cleaned up; exit 3' INT; while :; do sleep 0.1; done`
	start := time.Now()
	code := runChild(ctx, "/bin/sh", []string{"-c", script}, []string{"PATH=/usr/bin:/bin"}, &out, &errb)
	if code != 3 || !strings.Contains(out.String(), "cleaned up") {
		t.Errorf("exit %d, out %q, err %q: the child must get SIGINT and finish its cleanup", code, out.String(), errb.String())
	}
	if time.Since(start) > childGrace {
		t.Errorf("waited %s", time.Since(start))
	}
}

// BR-016 item 3: Ctrl-C at a prompt ends the question at once, without
// waiting for Enter.
func TestBR016PromptEndsOnInterrupt(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := newPrompter(ctx, pr, io.Discard)
	time.AfterFunc(100*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		_, err := p.ask("Bucket", "")
		done <- err
	}()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("err %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the question still waits for Enter after Ctrl-C")
	}
	// Every later question returns at once too, without reading.
	if _, err := p.yesNo("Encrypt?", true); err != context.Canceled {
		t.Errorf("after the interrupt: %v", err)
	}
}

// BR-016 item 3: init interrupted at its first question stops cleanly:
// "interrupted", nothing written, no retry menu or "context canceled".
func TestBR016InitInterruptedAtPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	type result struct {
		code     int
		out, err string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errOut := runWorld(t, dir, &world{sh: containerWorld(), ctx: ctx, in: pr})
		done <- result{code, out, errOut}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("init still waits at the prompt after Ctrl-C")
	}
	if res.code != exitError || !strings.Contains(res.err, "interrupted. No app.yaml written.") || strings.Contains(res.out+res.err, "context canceled") || strings.Contains(res.out, "[r]") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.out, res.err)
	}
	if _, err := os.Stat(filepath.Dir(appYAML(dir))); !os.IsNotExist(err) {
		t.Errorf("the app directory must not exist: %v", err)
	}
}

// BR-016 item 4: a proof that fails takes back secrets.env: no live
// credential stays on the machine without an app.yaml naming it, and
// without encryption no app directory is left either.
func TestBR016FailedProofRemovesSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld(), selfRun: func(args []string) int {
		if hasArg(args, "--dry-run") {
			return 0
		}
		return 1
	}}
	code, out, _ := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
	if code != exitError || !strings.Contains(out, "Secrets saved to") || !strings.Contains(out, "the real run failed (exit 1). No app.yaml written.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Dir(appYAML(dir))); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(filepath.Dir(appYAML(dir)))
		t.Errorf("app directory left behind: %v %v", err, entries)
	}

	// With a key, the key alone stays: the next init reuses it (ADR-0030).
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	w.selfLog = nil
	code, out, _ = runWorld(t, dir, w, answers(s3Answers, []string{"", ""}, pickAll, drillOn, schedDefault)...)
	entries, _ := os.ReadDir(filepath.Dir(appYAML(dir)))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if code != exitError || strings.Join(names, ",") != config.KeyFile {
		t.Errorf("exit %d, left %v:\n%s", code, names, out)
	}
}

// BR-016 item 4: interrupted after the rename and the timer (at the rescue
// card), init says what is there, not "No app.yaml written".
func TestBR016InterruptedAfterInstallSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &world{sh: containerWorld(), ctx: ctx, onInstall: cancel}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, []string{"", ""}, pickAll, drillOn, schedDefault, []string{"copied"})...)
	key := filepath.Join(filepath.Dir(appYAML(dir)), config.KeyFile)
	want := "interrupted. " + appYAML(dir) + " written and its timer installed, but " + key + " was not confirmed copied off this machine."
	if code != exitError || !strings.Contains(errOut, want) || strings.Contains(errOut, "No app.yaml written") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if _, err := config.Load(appYAML(dir)); err != nil {
		t.Errorf("app.yaml: %v", err)
	}
}

// containerAndTCP is a machine with the container shop-db and Postgres on
// 127.0.0.1:5433 (password pw), for init --web with two instances.
func containerAndTCP() (*gateShell, *fakeShell) {
	_, sh := localWorld("pw")
	sh.out["docker ps"] = containerWorld().out["docker ps"]
	// Only the TCP Postgres wants the password; the container trusts its socket.
	return &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if !hasArg(c.Args, "host") || len(c.Args) == 0 || c.Args[len(c.Args)-1] != "select 1" {
			return pg.Result{}, nil, false
		}
		if !hasEnv(c.Env, "PGPASSWORD=pw") {
			return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 2, Stderr: []byte(`FATAL:  password authentication failed for user "bracer_backup"`)}, true
		}
		if c.Stdout != nil {
			io.WriteString(c.Stdout, "1\n")
			return pg.Result{}, nil, true
		}
		return pg.Result{Stdout: []byte("1\n")}, nil, true
	}}, sh
}

// webAll is the person on the web picking every instance and every database.
func webAll(state map[string]any) []string {
	switch state["step"] {
	case "instances":
		var ids []string
		for _, in := range state["instances"].([]any) {
			ids = append(ids, in.(map[string]any)["id"].(string))
		}
		b, _ := json.Marshal(map[string]any{"type": "select_instances", "instances": ids})
		return []string{string(b)}
	case "databases":
		d := state["databases"].(map[string]any)
		b, _ := json.Marshal(map[string]any{"type": "select_databases", "instance_id": d["instance_id"], "databases": d["names"]})
		return []string{string(b)}
	}
	return nil
}

// BR-016 item 4: the URL (with its password) of an instance that is not
// written never reaches secrets.env.
func TestBR016DroppedInstanceSecretNotWritten(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	gated, sh := containerAndTCP()
	// 127.0.0.1:5433 is a pooler: dropped after its URL was built.
	delete(sh.fail, "docker run --rm -i --network host postgres:17-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -0 -c show pool_mode")
	sh.out["docker run --rm -i --network host postgres:17-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -0 -c show pool_mode"] = "transaction\n"
	fp := newFakePair()
	fp.react = webAll
	w := &world{sh: gated, args: webArgs, pair: fp}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption,
		[]string{"y", ""},      // claim; container name
		[]string{"", "", "pw"}, // 127.0.0.1:5433: name, user, password
		drillOff, sched, []string{"y"})...)
	if code != 0 || !strings.Contains(out, "looks like a connection pooler") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	env, _ := os.ReadFile(filepath.Join(filepath.Dir(appYAML(dir)), config.SecretsFile))
	if strings.Contains(string(env), "_URL=") || strings.Contains(string(env), ":pw@") {
		t.Errorf("secrets.env holds the dropped instance's URL:\n%s", env)
	}
}

// BR-016 item 5: an instance name is checked where it is typed: the
// app.yaml rule, and no variable shared with another secret (db-1 and db_1;
// heartbeat and the heartbeat URL).
func TestBR016InstanceNameCheckedAtPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption,
		[]string{"1", "_db", "main", ""}, drillOff, schedDefault)...)
	cfg, _ := config.Load(appYAML(dir))
	if code != 0 || cfg == nil || cfg.Instances[0].Name != "main" || !strings.Contains(out, "Name must start with a lowercase letter or digit") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	dir = t.TempDir()
	t.Setenv("HOME", dir)
	gated, _ := localWorld("pw")
	code, out, errOut = runWorld(t, dir, &world{sh: gated}, answers(s3Answers, noEncryption,
		[]string{"1", "heartbeat", "pg", "", "pw", ""}, drillOff, sched, []string{"https://hc.example/abc"})...)
	cfg, _ = config.Load(appYAML(dir))
	if code != 0 || cfg == nil || cfg.Instances[0].Name != "pg" || !strings.Contains(out, "heartbeat would keep its secret in BRACER_SHOP_HEARTBEAT_URL, as the heartbeat URL does") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	dir = t.TempDir()
	t.Setenv("HOME", dir)
	gated, _ = containerAndTCP()
	fp := newFakePair()
	fp.react = webAll
	code, out, errOut = runWorld(t, dir, &world{sh: gated, args: webArgs, pair: fp}, answers(s3Answers, noEncryption,
		[]string{"y", "db-1"},
		[]string{"db_1", "heartbeat", "", "pw"}, // --web has no heartbeat: the name is free
		drillOff, sched, []string{"y"})...)
	cfg, _ = config.Load(appYAML(dir))
	if code != 0 || cfg == nil || len(cfg.Instances) != 2 || cfg.Instances[1].Name != "heartbeat" || !strings.Contains(out, "db_1 would keep its secret in BRACER_SHOP_DB_1_*, as instance db-1 does") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	// A container keeps a password, never a URL: heartbeat takes nothing.
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut = runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption,
		[]string{"1", "heartbeat", ""}, drillOff, schedDefault)...)
	if cfg, _ = config.Load(appYAML(dir)); code != 0 || cfg == nil || cfg.Instances[0].Name != "heartbeat" {
		t.Errorf("container heartbeat: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

// BR-016 item 6: a failed check sends the web a SQL hint without a password
// literal and a reason with credentials redacted: the server refuses any
// body that looks like a credential, which stopped init --web every time.
func TestBR016WebCheckHasNoCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	gated, sh := localWorld("pw")
	psql17 := "docker run --rm -i --network host postgres:17-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -0 -c "
	sh.out[psql17+"select current_user"] = "app\x1ff\x1ff\x1ff\n" // cannot dump: the hint is shown
	delete(sh.out, "docker run --rm -i --network host postgres:17-alpine pg_dump --schema-only")
	sh.fail["docker run --rm -i --network host postgres:17-alpine pg_dump --schema-only"] = `pg_dump: error: connection to "postgres://app:hunter2@db.internal/app" failed: permission denied for table orders`
	fp := newFakePair()
	fp.react = func(state map[string]any) []string {
		if state["step"] == "checks" {
			checks := state["checks"].([]any)
			last := checks[len(checks)-1].(map[string]any)
			return []string{`{"type":"check_decision","instance_id":"` + last["instance_id"].(string) + `","database":"app","action":"skip_instance"}`}
		}
		return webAll(state)
	}
	runWorld(t, dir, &world{sh: gated, args: webArgs, pair: fp}, answers(s3Answers, noEncryption,
		[]string{"y", "", "", "pw"})...)
	var sent []map[string]any
	for _, s := range fp.states {
		if s["step"] == "checks" {
			sent = append(sent, s)
		}
	}
	if len(sent) == 0 {
		t.Fatalf("no checks sent: %s", fp.steps())
	}
	body, _ := json.Marshal(sent)
	check := sent[0]["checks"].([]any)[0].(map[string]any)
	hint, _ := check["sql_hint"].(string)
	if looksLikeCredentials(string(body)) || strings.Contains(string(body), "hunter2") || !strings.Contains(hint, `\password bracer_backup`) {
		t.Errorf("sent to the web:\n%s", body)
	}
}

// BR-016 item 7: --server is checked before anything else: https, or
// http:// only to this machine.
func TestBR016ServerCheckedFirst(t *testing.T) {
	for _, srv := range []string{"http://app.example.com", "app.bracer.dev", "https://"} {
		dir := t.TempDir()
		fp := newFakePair()
		w := &world{sh: containerWorld(), args: []string{"shop", "--web", "--server", srv}, pair: fp}
		code, out, errOut := runWorld(t, dir, w)
		if code != exitUsage || out != "" || !strings.Contains(errOut, "bracer init: --server") || w.tty != nil || fp.opened != 0 {
			t.Errorf("%s: exit %d tty %q\nstdout:\n%s\nstderr:\n%s", srv, code, w.tty, out, errOut)
		}
	}
	// A server on this machine may be plain http.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: []string{"shop", "--web", "--server", "http://127.0.0.1:8080"}, pair: fp}
	if code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...); code != 0 {
		t.Errorf("loopback: exit %d\n%s\n%s", code, out, errOut)
	}
}
