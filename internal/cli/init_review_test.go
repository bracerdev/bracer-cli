package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// BR-016 review D1: under nohup SIGHUP stays ignored, so closing the SSH
// session does not stop `nohup bracer backup`. The test binary runs itself
// under nohup; the child sends itself SIGHUP and must not be cancelled.
func TestBR016NohupKeepsSIGHUPIgnored(t *testing.T) {
	if os.Getenv("BR016_NOHUP_CHILD") == "1" {
		if !signal.Ignored(syscall.SIGHUP) {
			os.Exit(3) // not started with SIGHUP ignored: the test proves nothing
		}
		ctx, stop := signalContext()
		defer stop()
		syscall.Kill(os.Getpid(), syscall.SIGHUP)
		select {
		case <-ctx.Done():
			os.Exit(1)
		case <-time.After(300 * time.Millisecond):
			os.Exit(0)
		}
	}
	nohup, err := exec.LookPath("nohup")
	if err != nil {
		t.Skip("no nohup on this machine")
	}
	cmd := exec.Command(nohup, os.Args[0], "-test.run=^TestBR016NohupKeepsSIGHUPIgnored$")
	cmd.Env = append(os.Environ(), "BR016_NOHUP_CHILD=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() == 3:
		t.Fatalf("nohup did not ignore SIGHUP:\n%s", out.String())
	default:
		t.Errorf("SIGHUP under nohup cancelled the command (%v):\n%s", err, out.String())
	}
}

// runAsync runs f and fails the test when it is still running after d:
// a prompt that waits for Enter after an interrupt.
func runAsync(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("still waiting at the prompt after the interrupt")
	}
}

// BR-016 review D2: remove interrupted at its prompt stops at once and
// removes nothing.
func TestBR016RemoveInterruptedAtPrompt(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	d, out, errb := testDeps(t, &fakeSystemctl{}, "")
	pr, pw := io.Pipe()
	defer pw.Close()
	d.stdin, d.tty = pr, true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	var code int
	runAsync(t, 3*time.Second, func() { code = runRemove(ctx, []string{"shop"}, d) })
	if code != exitError || !strings.Contains(errb.String(), "bracer remove: interrupted; nothing removed") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("app.yaml must stay: %v", err)
	}
}

// BR-016 review D2: restore interrupted at a question stops at once.
func TestBR016RestoreInterruptedAtPrompt(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	var code int
	runAsync(t, 3*time.Second, func() {
		code = restoreSession(ctx, backupDeps{stdin: pr, stdout: &out, stderr: &errb}, newLogger(&errb, false))
	})
	if code != exitError || errb.String() != "bracer restore: interrupted; nothing was loaded\n" {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
}

// BR-016 review D6 (a, b): a proof run cut short by the interrupt says
// "interrupted", not "the check failed" or "the real run failed".
func TestBR016InterruptedProofSaysInterrupted(t *testing.T) {
	for _, dry := range []bool{true, false} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		ctx, cancel := context.WithCancel(context.Background())
		w := &world{sh: containerWorld(), ctx: ctx, selfRun: func(args []string) int {
			if hasArg(args, "--dry-run") == dry {
				cancel()
				return 1
			}
			return 0
		}}
		code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
		if code != exitError || !strings.Contains(errOut, "interrupted. No app.yaml written.") || strings.Contains(out, "failed (exit") {
			t.Errorf("dry-run %v: exit %d\nstdout:\n%s\nstderr:\n%s", dry, code, out, errOut)
		}
		cancel()
	}
}

// BR-016 review D6 (c): a proof run that ignores SIGINT is killed after
// childGrace, so init never hangs; the user is told it is stopping.
func TestBR016ChildIgnoringInterruptIsKilled(t *testing.T) {
	defer func(d time.Duration) { childGrace = d }(childGrace)
	childGrace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	var out, errb lockedBuffer // the child's copy and the message share stderr
	var code int
	runAsync(t, 5*time.Second, func() {
		code = runChild(ctx, "/bin/sh", []string{"-c", `trap '' INT; while :; do sleep 0.1; done`}, []string{"PATH=/usr/bin:/bin"}, &out, &errb)
	})
	if code == exitOK || !strings.Contains(errb.String(), "Stopping the running backup; it cleans up first (up to 300ms).") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

// lockedBuffer is a writer two goroutines may share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// BR-016 review D6 (d): with --web, a proof that fails after the token was
// saved removes secrets.env and does not print the token.
func TestBR016WebFailedProofHidesToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: webArgs, pair: fp, selfRun: func(args []string) int {
		if hasArg(args, "--dry-run") {
			return 0
		}
		return 1
	}}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != exitError || strings.Contains(out+errOut, "brc_test") || !strings.Contains(out, "the real run failed") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(appYAML(dir)), config.SecretsFile)); !os.IsNotExist(err) {
		t.Errorf("secrets.env must be removed: %v", err)
	}
}

// BR-016 review D6 (e), D7 (b): an interrupt during a check (bucket,
// connection, permissions, disk) is not reported as that check's failure:
// no ✗, no retry menu, only "interrupted".
func TestBR016InterruptDuringCheckStopsClean(t *testing.T) {
	cut := func(cancel context.CancelFunc, match func(pg.Cmd) bool) pg.Shell {
		return &gateShell{inner: containerWorld(), gate: func(c pg.Cmd) (pg.Result, error, bool) {
			if !match(c) {
				return pg.Result{}, nil, false
			}
			cancel()
			return pg.Result{}, &pg.ExitError{Cmd: c.Name, Code: -1, Stderr: []byte("signal: killed")}, true
		}}
	}
	args := func(c pg.Cmd) string { return c.Name + " " + strings.Join(c.Args, " ") }
	for _, tc := range []struct {
		name  string
		setup func(w *world, cancel context.CancelFunc)
	}{
		{"bucket", func(w *world, cancel context.CancelFunc) {
			w.onStore, w.store = cancel, context.Canceled
		}},
		{"connect", func(w *world, cancel context.CancelFunc) {
			w.sh = cut(cancel, func(c pg.Cmd) bool { return len(c.Args) > 0 && c.Args[len(c.Args)-1] == "select 1" })
		}},
		{"permissions", func(w *world, cancel context.CancelFunc) {
			w.sh = cut(cancel, func(c pg.Cmd) bool { return strings.Contains(args(c), "pg_dump --schema-only") })
		}},
		// The error comes back wrapped in init's own words: still an interrupt.
		{"external", func(w *world, cancel context.CancelFunc) {
			w.sh = cut(cancel, func(c pg.Cmd) bool { return strings.Contains(args(c), "select 'function '") })
		}},
		{"disk", func(w *world, cancel context.CancelFunc) {
			w.sh = cut(cancel, func(c pg.Cmd) bool { return c.Name == "df" })
		}},
	} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		ctx, cancel := context.WithCancel(context.Background())
		w := &world{sh: containerWorld(), ctx: ctx}
		tc.setup(w, cancel)
		code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
		if code != exitError || !strings.Contains(errOut, "interrupted. No app.yaml written.") || strings.Contains(out, "✗") || strings.Contains(out, "[r]") || strings.Contains(out, "Could not connect") {
			t.Errorf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", tc.name, code, out, errOut)
		}
		cancel()
	}
}

// BR-016 review D6 (f): after the interrupt no question tells the web
// "finish in your terminal".
func TestBR016HookSkippedAfterInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := newPrompter(ctx, strings.NewReader("x\n"), io.Discard)
	calls := 0
	p.before = func() { calls++ }
	if _, err := p.ask("Bucket", ""); err != context.Canceled || calls != 0 {
		t.Errorf("err %v, hook ran %d times", err, calls)
	}
}
