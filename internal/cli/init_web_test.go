package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// fakePair is the hosted server and the person on the web: react answers
// every state the CLI posts with the web's choices, in order.
type fakePair struct {
	opened    int
	events    []json.RawMessage // what Next hands out next
	states    []map[string]any  // every state posted
	finished  []string          // "true shop" / "false shop"
	openErr   error
	nextErr   error
	finishErr error
	// react returns the events the web sends after seeing a state.
	react func(state map[string]any) []string
}

func newFakePair() *fakePair {
	return &fakePair{events: []json.RawMessage{json.RawMessage(`{"type":"claimed","account":"k***@example.com"}`)}}
}

func (f *fakePair) push(events ...string) {
	for _, e := range events {
		f.events = append(f.events, json.RawMessage(e))
	}
}

func (f *fakePair) Open(_ context.Context, cliVersion, hostname string) (*pairOpened, error) {
	f.opened++
	if f.openErr != nil {
		return nil, f.openErr
	}
	if hostname != "vps-1" || cliVersion == "" {
		return nil, fmt.Errorf("open: %s %s", cliVersion, hostname)
	}
	return &pairOpened{Secret: "s3cret", UserCode: "ABCDEFGH", VerifyURL: "http://app.test/pair", ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
}

func (f *fakePair) Next(_ context.Context, secret string, _ int) (json.RawMessage, error) {
	if secret != "s3cret" {
		return nil, errors.New("bad secret")
	}
	if f.nextErr != nil {
		return nil, f.nextErr
	}
	if len(f.events) == 0 {
		return nil, errors.New("fake web: the CLI waits for an event nobody sends")
	}
	ev := f.events[0]
	f.events = f.events[1:]
	return ev, nil
}

func (f *fakePair) State(_ context.Context, secret string, body any) error {
	if secret != "s3cret" {
		return errors.New("bad secret")
	}
	data, _ := json.Marshal(body)
	var m map[string]any
	json.Unmarshal(data, &m)
	f.states = append(f.states, m)
	if f.react != nil {
		f.push(f.react(m)...)
	}
	return nil
}

func (f *fakePair) Finish(_ context.Context, secret string, confirmed bool, app string) (*pairFinished, error) {
	if secret != "s3cret" {
		return nil, errors.New("bad secret")
	}
	f.finished = append(f.finished, fmt.Sprintf("%v %s", confirmed, app))
	if f.finishErr != nil {
		return nil, f.finishErr
	}
	if !confirmed {
		return nil, nil
	}
	return &pairFinished{AppID: "app-1", Token: "brc_test"}, nil
}

// steps lists the step of every state posted.
func (f *fakePair) steps() string {
	var out []string
	for _, s := range f.states {
		step, _ := s["step"].(string)
		if r, ok := s["rejected"]; ok {
			step += "!" + r.(map[string]any)["reason"].(string)
		}
		out = append(out, step)
	}
	return strings.Join(out, " ")
}

// webHappy is the person on the web picking the container and shop only.
func webHappy(state map[string]any) []string {
	switch state["step"] {
	case "instances":
		if _, rejected := state["rejected"]; rejected {
			return nil
		}
		return []string{`{"type":"select_instances","instances":["container:shop-db"]}`}
	case "databases":
		return []string{`{"type":"select_databases","instance_id":"container:shop-db","databases":["shop"]}`}
	}
	return nil
}

var webArgs = []string{"shop", "--web", "--server", "http://app.test/"}

// webAnswers: claim yes, instance name, no drill, schedule, confirm. No
// heartbeat question: hosted is the monitor.
var webAnswers = answers([]string{"y", ""}, drillOff, sched, []string{"y"})

func TestInitWebFullFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	fp.react = webHappy
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if w.server != "http://app.test" || fp.opened != 1 {
		t.Errorf("server %q opened %d", w.server, fp.opened)
	}
	if strings.Contains(out, "Heartbeat URL") {
		t.Error("init --web asked for a heartbeat URL; hosted is the monitor")
	}
	if !strings.Contains(out, "Open http://app.test/pair and enter this code:\n\n  ABCD-EFGH\n") || !strings.Contains(out, "Claimed by k***@example.com on the web. Continue with that account? [y/N]") {
		t.Errorf("code and claim:\n%s", out)
	}
	if strings.Contains(out, "Have a Bracer account?") {
		t.Errorf("--web must not advertise itself:\n%s", out)
	}
	// The bucket is asked before the session opens (ADR-0022).
	if strings.Index(out, "bucket reachable") > strings.Index(out, "enter this code") {
		t.Errorf("bucket must come before the code:\n%s", out)
	}
	// instances → (name asked: terminal) → databases → checks → terminal (drill…) → confirm.
	if got := fp.steps(); got != "instances terminal databases checks terminal confirm" {
		t.Errorf("states: %s", got)
	}
	inst := fp.states[0]["instances"].([]any)[0].(map[string]any)
	if inst["id"] != "container:shop-db" || inst["kind"] != "container" || inst["image"] != "postgres:17-alpine" || inst["verified"] != true || inst["needs_password"] != false {
		t.Errorf("instance for the web: %v", inst)
	}
	dbs := fp.states[2]["databases"].(map[string]any)
	if dbs["instance_id"] != "container:shop-db" || fmt.Sprint(dbs["names"]) != "[postgres shop]" {
		t.Errorf("databases for the web: %v", dbs)
	}
	checks := fp.states[3]["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["database"] != "shop" || checks[0].(map[string]any)["ok"] != true {
		t.Errorf("checks for the web: %v", checks)
	}
	if strings.Join(fp.finished, ",") != "true shop" {
		t.Errorf("finish: %v", fp.finished)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hosted == nil || cfg.Hosted.Server != "http://app.test" || cfg.Hosted.TokenEnv != "BRACER_TOKEN" {
		t.Errorf("hosted: %+v", cfg.Hosted)
	}
	if got := strings.Join(cfg.Instances[0].Databases.Names(), ","); got != "shop" || cfg.Instances[0].Name != "shop-db" {
		t.Errorf("instance: %+v", cfg.Instances[0])
	}
	env, _ := os.ReadFile(cfg.SecretsFile)
	if !strings.Contains(string(env), "BRACER_TOKEN=brc_test\n") {
		t.Errorf("secrets.env:\n%s", env)
	}
	raw, _ := os.ReadFile(appYAML(dir))
	if strings.Contains(string(raw), "brc_test") || strings.Contains(out, "brc_test") {
		t.Errorf("token leaked:\n%s\n%s", raw, out)
	}
	if !strings.Contains(out, "About to write:") || !strings.Contains(out, "instance  shop-db: shop") || !w.installed() {
		t.Errorf("summary and install:\n%s", out)
	}
}

func TestInitWebRejectsBadChoices(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	fp := newFakePair()
	n := 0
	fp.react = func(state map[string]any) []string {
		switch state["step"] {
		case "instances":
			n++
			switch n {
			case 1:
				return []string{`{"type":"select_instances","instances":["container:nope"]}`}
			case 2:
				return []string{`{"type":"select_databases","instance_id":"x","databases":["shop"]}`}
			case 3:
				return []string{`{"type":"select_instances","instances":["container:shop-db"],"extra":1}`}
			case 4:
				return []string{`{"type":"select_instances","instances":["container:shop-db"]}`}
			}
		case "databases":
			if _, rejected := state["rejected"]; rejected {
				return []string{`{"type":"select_databases","instance_id":"container:shop-db","databases":["shop","postgres"]}`}
			}
			return []string{`{"type":"select_databases","instance_id":"container:shop-db","databases":["ignore"]}`}
		}
		return nil
	}
	w := &world{sh: containerWorld(), args: webArgs, pair: fp}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	want := "instances instances!unknown instance container:nope instances!expected select_instances, got select_databases instances!unexpected field: json: unknown field \"extra\" terminal databases databases!unknown database ignore checks terminal confirm"
	if got := fp.steps(); got != want {
		t.Errorf("states:\n got %s\nwant %s", got, want)
	}
	if strings.Count(errOut, "choice from the web rejected") != 4 {
		t.Errorf("warnings:\n%s", errOut)
	}
	cfg, _ := config.Load(appYAML(dir))
	// In the server's order, whatever the web sent.
	if got := strings.Join(cfg.Instances[0].Databases.Names(), ","); got != "postgres,shop" {
		t.Errorf("databases %s", got)
	}
}

func TestInitWebCheckDecision(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// pg_dump of postgres fails; the web skips that database.
	sh := &gateShell{inner: containerWorld(), gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "pg_dump --schema-only") && envOf(c.Env, "PGDATABASE") == "postgres" {
			return pg.Result{}, &pg.ExitError{Cmd: "pg_dump", Code: 1, Stderr: []byte("permission denied for table secrets")}, true
		}
		return pg.Result{}, nil, false
	}}
	fp := newFakePair()
	decisions := 0
	fp.react = func(state map[string]any) []string {
		switch state["step"] {
		case "instances":
			return []string{`{"type":"select_instances","instances":["container:shop-db"]}`}
		case "databases":
			return []string{`{"type":"select_databases","instance_id":"container:shop-db","databases":["postgres","shop"]}`}
		case "checks":
			checks := state["checks"].([]any)
			last := checks[len(checks)-1].(map[string]any)
			if last["ok"] == true {
				return nil
			}
			decisions++
			if decisions == 1 {
				return []string{`{"type":"check_decision","instance_id":"container:shop-db","database":"shop","action":"retry"}`} // wrong database: rejected
			}
			return []string{`{"type":"check_decision","instance_id":"container:shop-db","database":"postgres","action":"skip_database"}`}
		}
		return nil
	}
	w := &world{sh: sh, args: webArgs, pair: fp}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, webAnswers)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	want := "instances terminal databases checks checks!not the database that failed checks terminal confirm"
	if got := fp.steps(); got != want {
		t.Errorf("states:\n got %s\nwant %s", got, want)
	}
	first := fp.states[3]["checks"].([]any)[0].(map[string]any)
	if first["database"] != "postgres" || first["ok"] != false || !strings.Contains(first["reason"].(string), "permission denied for table secrets") || first["instance_id"] != "container:shop-db" {
		t.Errorf("failed check: %v", first)
	}
	cfg, _ := config.Load(appYAML(dir))
	if got := strings.Join(cfg.Instances[0].Databases.Names(), ","); got != "shop" {
		t.Errorf("databases %s", got)
	}
}

func TestInitWebStops(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(fp *fakePair)
		answers []string
		noTTY   bool
		code    int
		stdout  string
		finish  string
		opened  int
	}{
		{"declined at claim", func(fp *fakePair) {}, answers(s3Answers, noEncryption, []string{"n"}), false, 1, "No app.yaml written.", "false shop", 1},
		{"cancelled on the web", func(fp *fakePair) {
			fp.react = func(state map[string]any) []string {
				if state["step"] == "instances" {
					return []string{`{"type":"cancelled"}`}
				}
				return nil
			}
		}, answers(s3Answers, noEncryption, []string{"y"}), false, 1, "The setup was cancelled on the web. No app.yaml written.", "", 1},
		{"trial ended", func(fp *fakePair) {
			fp.events = []json.RawMessage{json.RawMessage(`{"type":"cancelled","reason":"trial_ended"}`)}
		}, answers(s3Answers, noEncryption), false, 1, "Your trial on the hosted server has ended", "", 1},
		{"expired", func(fp *fakePair) { fp.nextErr = errPairClosed }, answers(s3Answers, noEncryption), false, 1, "pairing expired; run `bracer init shop --web` again, or `bracer init shop`", "", 1},
		{"pairing off", func(fp *fakePair) { fp.openErr = errPairingOff }, answers(s3Answers, noEncryption), false, 1, "Pairing is switched off on the server right now. No app.yaml written.\nSet the app up in the terminal: bracer init shop", "", 1},
		{"app exists", func(fp *fakePair) {
			fp.react = webHappy
			fp.finishErr = errAppExists
		}, answers(s3Answers, noEncryption, webAnswers), false, 1, "Your account already has an app named shop.", "true shop,false shop", 1},
		{"declined at summary", func(fp *fakePair) { fp.react = webHappy }, answers(s3Answers, noEncryption, []string{"y", ""}, drillOff, sched, []string{"n"}), false, 1, "No app.yaml written.", "false shop", 1},
		{"no tty", func(fp *fakePair) {}, nil, true, 2, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			fp := newFakePair()
			tc.setup(fp)
			w := &world{sh: containerWorld(), args: webArgs, pair: fp, noTTY: tc.noTTY}
			code, out, errOut := runWorld(t, dir, w, tc.answers...)
			if code != tc.code {
				t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
			}
			if !strings.Contains(out, tc.stdout) {
				t.Errorf("stdout:\n%s\nstderr:\n%s", out, errOut)
			}
			if got := strings.Join(fp.finished, ","); got != tc.finish || fp.opened != tc.opened {
				t.Errorf("finish %q opened %d", got, fp.opened)
			}
			if _, err := os.Stat(appYAML(dir)); err == nil {
				t.Error("app.yaml written")
			}
			if tc.noTTY && !strings.Contains(errOut, "--web needs a terminal") {
				t.Errorf("stderr:\n%s", errOut)
			}
		})
	}
}
