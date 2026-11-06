package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// `bracer init <app> --web` (spec backup/init-web): the same init, with the
// three decisions (instances, databases, failed checks) made on the web by
// the paired account. Bucket, encryption, names, passwords, drill, schedule
// and the final confirmation stay in the terminal; no secret leaves it.

const (
	// hostedTokenEnv is the variable in secrets.env that holds the app token.
	hostedTokenEnv = "BRACER_TOKEN"
	// pairWait is one long-poll; pairRetry is how long the CLI keeps trying
	// through network errors before giving up as expired.
	pairWait  = 30
	pairRetry = 2 * time.Minute
)

// errPairStopped: the session ended before the files were written; what
// happened and what to run next are already printed.
var errPairStopped = errors.New("pairing stopped")

// webSession is one pairing session, and the chooser of init --web.
type webSession struct {
	r      *initRun
	api    pairAPI
	server string // the URL paired through, written to hosted.server
	secret string
	// cur is the id of the instance being set up, as the web knows it.
	cur string
	// last is the state last sent, re-sent with a rejection.
	last map[string]any
	// atTerminal: the web already shows "finish in your terminal".
	atTerminal bool
	// continues: the web confirmed continuing an app already in the
	// account, not creating one (ADR-0033).
	continues bool
	// connected: finish gave the token; the app is in the account.
	connected bool
	// stopKeep ends keepAlive; nil when it does not run.
	stopKeep func()
	// webCancelled is the reason the session was closed on the web while
	// the proof ran ("" when it was not); keepAlive sets it, then stops the
	// proof. Read only after stopKeepAlive.
	webCancelled string
	// proofSince is the draft's schedule.since the proof ran under.
	proofSince string
}

// newWebSession pairs through api at server, the URL the nightly job will
// report to: the one this machine reached, not one the server names.
func newWebSession(r *initRun, api pairAPI, server string) *webSession {
	return &webSession{r: r, api: api, server: server}
}

// webInstance is one found Postgres as the web lists it (contract init-web).
type webInstance struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"` // container | local | url (ADR-0013)
	Label         string   `json:"label"`
	Image         string   `json:"image,omitempty"`
	PgVersion     string   `json:"pg_version,omitempty"`
	Verified      bool     `json:"verified"`
	NeedsPassword bool     `json:"needs_password"`
	Warnings      []string `json:"warnings"`
}

// describeCandidate is a candidate with its id for the web.
func describeCandidate(c pg.Candidate) webInstance {
	w := webInstance{Label: c.Label(), Warnings: []string{}}
	switch {
	case c.Container != nil:
		ct := c.Container
		w.ID, w.Kind, w.Label, w.Image = "container:"+ct.Name, "container", ct.Name, ct.Image
		w.Verified = ct.Running && !ct.Unverified
		if !ct.Running {
			w.Warnings = append(w.Warnings, "stopped: "+ct.Status+"; it cannot be tested now")
		}
		if ct.Unverified {
			w.Warnings = append(w.Warnings, "not verified: docker top failed")
		}
	case c.Local != nil:
		w.ID, w.Kind = fmt.Sprintf("local:%d", c.Local.Port), "local"
		w.NeedsPassword = true
	default:
		w.ID, w.Kind = fmt.Sprintf("tcp:%s:%d", c.URLHost(), c.Port), "url"
		w.NeedsPassword = true
		if c.Confirmed != nil && !*c.Confirmed {
			w.Warnings = append(w.Warnings, "did not answer like Postgres")
		}
	}
	return w
}

// runInitWeb is the --web flow after step 0: bucket and encryption in the
// terminal, then the session, then the same steps as init.
func (r *initRun) runInitWeb() error {
	r.cfg = &config.App{App: r.appName, Runtime: r.runtime}
	r.p.printf("\nNew app %s: its files go to %s.\n", r.appName, filepath.Dir(r.path))
	if err := r.setupStorage(); err != nil {
		return err
	}
	if err := r.setupEncryption(); err != nil {
		return err
	}
	w := r.web
	if err := w.open(); err != nil {
		return w.stop(err)
	}
	ok, err := w.waitClaimed()
	if err != nil {
		return w.stop(err)
	}
	if !ok {
		return w.stop(w.cancel())
	}
	// From here every terminal question tells the web so.
	r.p.before = w.terminalHook
	insts, err := w.pickInstances()
	if err != nil {
		return w.stop(err)
	}
	if len(insts) == 0 {
		return w.stop(fmt.Errorf("no instance to back up; nothing was written. When it is fixed, run: %s", r.again))
	}
	r.cfg.Instances = append(r.cfg.Instances, insts...)
	if err := r.askDrill(); err != nil {
		return w.stop(err)
	}
	if err := r.askSchedule(); err != nil {
		return w.stop(err)
	}
	// No heartbeat question: the hosted dashboard is the monitor.
	if err := w.state(map[string]any{"step": "confirm"}); err != nil {
		return w.stop(err)
	}
	yes, err := w.summary()
	if err != nil {
		return w.stop(err)
	}
	if !yes {
		return w.stop(w.cancel())
	}
	// The proof runs on a draft without hosted:; connect, between the proof
	// and the rename, creates or continues the app (ADR-0033). It runs
	// under a context of its own: a cancel on the web ends it as Ctrl-C
	// does (the child gets SIGINT and cleans up), ADR-0033 § 10.
	outer := r.ctx
	proofCtx, stopProof := context.WithCancel(outer)
	r.ctx = proofCtx
	w.keepAlive(proofCtx, stopProof)
	err = r.finish()
	w.stopKeepAlive()
	r.ctx = outer
	stopProof()
	if err == nil {
		return nil
	}
	if w.connected {
		if !r.written {
			r.p.printf("\nApp %s is in your account, but this machine was not set up to report to it.\nFix: run bracer init %s --web again: it continues the app with a new token.\n", r.appName, r.appName)
		}
		return err
	}
	if w.webCancelled != "" && outer.Err() == nil {
		// Closed on the web: nothing to close here, the files are gone.
		if w.webCancelled == "app_changed" {
			r.p.printf("\nApp %s changed or was deleted on the web while the first backup ran, so nothing was added or changed.\n", r.appName)
		} else {
			r.p.printf("\nThe setup was cancelled on the web while the first backup ran, so app %s was not added or changed.\n", r.appName)
		}
		return errPairStopped
	}
	return w.stop(err)
}

// pairKeepAlive is how often the CLI re-posts its step while the proof
// runs, well inside the session's idle lifetime on the server.
var pairKeepAlive = 5 * time.Minute

// keepAlive re-posts the confirm step every pairKeepAlive until
// stopKeepAlive, so a proof longer than the session's idle lifetime does
// not close it. The session closed on the web meanwhile (410), it stops
// posting, keeps the reason in webCancelled and calls stopProof. It calls
// the api directly: w.last and w.atTerminal stay as the main goroutine
// left them.
func (w *webSession) keepAlive(ctx context.Context, stopProof func()) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	w.stopKeep = func() {
		close(done)
		<-stopped
	}
	body := map[string]any{"step": "confirm"}
	go func() {
		defer close(stopped)
		t := time.NewTicker(pairKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				err := w.api.State(ctx, w.secret, body)
				if errors.Is(err, errPairClosed) {
					w.webCancelled = "user"
					// The reason, once: user, or app_changed for a delete.
					if ev, err := w.api.Next(ctx, w.secret, 0); err == nil {
						var e struct {
							Type, Reason string
						}
						if json.Unmarshal(ev, &e) == nil && e.Type == "cancelled" && e.Reason != "" {
							w.webCancelled = e.Reason
						}
					}
					stopProof()
					return
				}
				if err != nil && !errors.Is(err, context.Canceled) {
					w.r.warn("web: %v", err)
				}
			}
		}
	}()
}

// stopKeepAlive ends keepAlive and waits for it; twice is fine.
func (w *webSession) stopKeepAlive() {
	if w.stopKeep != nil {
		w.stopKeep()
		w.stopKeep = nil
	}
}

// connect is the end of the proof under init --web: the real run passed,
// so finish creates or continues the app; its token goes to secrets.env
// and hosted: to the draft. Refused, nothing is in the account and the
// caller removes the draft and secrets.env as for any stop before the
// rename (spec init-web § BR-021).
func (w *webSession) connect(draft string) error {
	w.stopKeepAlive()
	r := w.r
	if w.webCancelled != "" {
		// Closed on the web as the proof ended: nothing to finish.
		return context.Canceled
	}
	// The proof's period, before prove stamps app.yaml's later one: the
	// proof run is reported with it (BR-023).
	w.proofSince = r.cfg.Schedule.Since
	var done *pairFinished
	lost := false
	err := w.retry(func() error {
		var err error
		done, err = w.api.Finish(r.ctx, w.secret, true)
		lost = lost || isNetError(err)
		return err
	})
	if err != nil && r.ctx.Err() != nil {
		// Interrupted with the request out: the server may have committed
		// (on a continue the old token is revoked then), so do not say
		// nothing happened (ADR-0033 § 7).
		r.p.printf("\nThe backup passed, but init was interrupted while the server was answering: app %s may be in your account (created or continued).\nFix: run bracer init %s --web again: it continues the app with a new token.\n", r.appName, r.appName)
		w.closeQuietly()
		return errPairStopped
	}
	if err != nil {
		return w.notAdded(err, lost)
	}
	w.connected = true
	r.secrets[hostedTokenEnv] = done.Token
	if err := r.storeSecrets(); err != nil {
		return err
	}
	r.cfg.Hosted = &config.Hosted{Server: w.server, TokenEnv: hostedTokenEnv}
	if err := config.SaveDraft(draft, r.cfg); err != nil {
		return err
	}
	if done.Continued {
		r.p.printf("\nApp %s continued in your account on %s; the token of its earlier setup no longer works.\n", r.appName, w.server)
	} else {
		r.p.printf("\nApp %s created in your account on %s.\n", r.appName, w.server)
	}
	return nil
}

// notAdded says why a passed proof did not become an app in the account.
// Other errors (an interrupt) pass through to stop.
func (w *webSession) notAdded(err error, lost bool) error {
	r := w.r
	var why, fix string
	var pe *pairError
	switch {
	case errors.Is(err, errAppChanged):
		why = "another setup created, continued or deleted it since you confirmed on the web"
	case errors.Is(err, errAppLimit):
		why = "your plan's app limit is reached"
		fix = "delete an app on the web, then run: " + r.again
	case errors.Is(err, errPairClosed) && lost:
		// The answer may be what was lost: then the app is in the account
		// without this machine reporting to it, and a rerun continues it.
		why = "no answer from the server (if it added the app, running again continues it)"
	case errors.Is(err, errPairClosed):
		why = "the session was cancelled on the web or expired"
	case errors.As(err, &pe):
		// The server did not close the session: close it, or the web waits
		// on "Finish in your terminal" until it expires.
		why = "the server refused: " + pe.Error()
		w.closeQuietly()
	default:
		return err
	}
	if fix == "" {
		fix = "run again: " + r.again
	}
	r.p.printf("\nThe backup passed, but app %s was not added to your account: %s.\nFix: %s\n", r.appName, why, fix)
	return errPairStopped
}

// reportProof sends the proof run, which the child wrote next to the draft
// since it reported nothing (no hosted: yet), now that this machine has
// the token: the Backups page shows it at once. Not delivered only warns.
func (w *webSession) reportProof(path string) {
	r := w.r
	noRun := fmt.Sprintf("app %s shows no run on the web until its next backup reports", r.appName)
	notReported := func(err error) {
		r.warn("the first backup was not reported: %v; %s\nFix: bracer backup %s", err, noRun, r.appName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		notReported(err)
		return
	}
	var res backup.Result
	if err := json.Unmarshal(data, &res); err != nil {
		notReported(err)
		return
	}
	secrets, _, err := config.LoadSecrets(r.cfg)
	if err != nil {
		notReported(err)
		return
	}
	// The report's schedule is the proof's, with the draft's since, not
	// app.yaml's later one: the proof run belongs to the proof period.
	cfg := *r.cfg
	sched := *cfg.Schedule
	sched.Since = w.proofSince
	cfg.Schedule = &sched
	line := report(r.ctx, &cfg, secrets, backupDeps{http: r.reportHTTP}, newLogger(r.stderr, false), &res)
	if strings.HasPrefix(line, "✗") {
		// What it means on the web, before report's own Fix line if any.
		head, fix, hasFix := strings.Cut(line, "\nFix: ")
		line = head + "; " + noRun
		if hasFix {
			line += "\nFix: " + fix
		}
	}
	if line != "" {
		r.p.printf("%s\n", line)
	}
}

// open is POST /pair and the code on the terminal.
func (w *webSession) open() error {
	host, err := w.r.hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	var opened *pairOpened
	err = w.retry(func() error {
		var err error
		opened, err = w.api.Open(w.r.ctx, Version, host, w.r.appName)
		return err
	})
	if err != nil {
		return err
	}
	w.secret = opened.Secret
	w.r.p.printf("\nOpen %s and enter this code:\n\n  %s\n\nThe code expires in %s. Waiting...\n",
		opened.VerifyURL, maskedCode(opened.UserCode), untilText(opened.ExpiresAt))
	return nil
}

func untilText(t time.Time) string {
	d := time.Until(t).Round(time.Minute)
	if d < time.Minute {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

// waitClaimed waits for the code to be entered, then asks whether to go on
// with that account. false: the customer said no.
func (w *webSession) waitClaimed() (bool, error) {
	for {
		ev, err := w.next()
		if err != nil {
			return false, err
		}
		var e struct {
			Type    string `json:"type"`
			Account string `json:"account"`
			Reason  string `json:"reason"`
			App     struct {
				Name         string     `json:"name"`
				Continues    bool       `json:"continues"`
				LastReportAt *time.Time `json:"last_report_at"`
				Hostname     string     `json:"hostname"`
			} `json:"app"`
		}
		json.Unmarshal(ev, &e)
		switch e.Type {
		case "claimed":
			if e.App.Name != w.r.appName {
				// The server names the app this session opened with; anything
				// else is not what the web confirmed for this command.
				return false, fmt.Errorf("the web confirmed app %q, not %s", e.App.Name, w.r.appName)
			}
			w.continues = e.App.Continues
			what := fmt.Sprintf("to create app %s", e.App.Name)
			if e.App.Continues {
				last := "no report yet"
				if e.App.LastReportAt != nil {
					last = "last report " + e.App.LastReportAt.Local().Format("2006-01-02 15:04")
					if e.App.Hostname != "" {
						last += " from " + e.App.Hostname
					}
				}
				what = fmt.Sprintf("to continue app %s (%s): its history stays; the token of its earlier setup stops working once this init passes", e.App.Name, last)
			}
			return w.r.p.yesNo(fmt.Sprintf("\nClaimed by %s on the web, %s. Continue with that account?", orDefault(e.Account, "an account"), what), false)
		case "cancelled":
			return false, w.cancelled(e.Reason)
		default:
			w.r.warn("ignored %s from the web while waiting for the code", orDefault(e.Type, "an event"))
		}
	}
}

// next is one long-poll that only returns with an event.
func (w *webSession) next() (json.RawMessage, error) {
	for {
		var ev json.RawMessage
		err := w.retry(func() error {
			var err error
			ev, err = w.api.Next(w.r.ctx, w.secret, pairWait)
			return err
		})
		if err != nil {
			return nil, err
		}
		if ev != nil {
			return ev, nil
		}
	}
}

// retry runs call again through network errors for up to pairRetry.
func (w *webSession) retry(call func() error) error {
	deadline := time.Now().Add(pairRetry)
	delay := 2 * time.Second
	for {
		err := call()
		if err == nil || !isNetError(err) || w.r.ctx.Err() != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: no answer from the server for %s (%v)", errPairClosed, pairRetry, err)
		}
		w.r.warn("cannot reach the server (%v); trying again in %s", err, delay)
		select {
		case <-w.r.ctx.Done():
			return w.r.ctx.Err()
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
}

// state is POST /pair/state; the web switches screens on it.
func (w *webSession) state(body map[string]any) error {
	w.last = body
	w.atTerminal = body["step"] == "terminal" || body["step"] == "confirm"
	return w.retry(func() error { return w.api.State(w.r.ctx, w.secret, body) })
}

// terminalHook runs before every terminal question: the web shows
// "finish in your terminal" until the next state.
func (w *webSession) terminalHook() {
	if w.atTerminal || w.secret == "" {
		return
	}
	if err := w.state(map[string]any{"step": "terminal"}); err != nil {
		w.r.warn("web: %v", err)
	}
}

// reject re-sends the current step with why the choice was not taken.
func (w *webSession) reject(typ, reason string) error {
	w.r.warn("choice from the web rejected: %s", reason)
	body := map[string]any{}
	for k, v := range w.last {
		body[k] = v
	}
	delete(body, "rejected")
	body["rejected"] = map[string]string{"type": typ, "reason": reason}
	return w.state(body)
}

// choice waits for the web's answer of type want and decodes it into out
// (strictly: an unknown field is a rejection). Anything else is rejected
// and waited out again; cancelled stops.
func (w *webSession) choice(want string, out any, check func() string) error {
	for {
		ev, err := w.next()
		if err != nil {
			return err
		}
		var head struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ev, &head); err != nil || head.Type == "" {
			if err := w.reject(want, "not a choice"); err != nil {
				return err
			}
			continue
		}
		if head.Type == "cancelled" {
			return w.cancelled(head.Reason)
		}
		reason := ""
		if head.Type != want {
			reason = fmt.Sprintf("expected %s, got %s", want, head.Type)
		} else {
			dec := json.NewDecoder(strings.NewReader(string(ev)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(out); err != nil {
				reason = "unexpected field: " + firstLine(err.Error())
			} else {
				reason = check()
			}
		}
		if reason == "" {
			return nil
		}
		if err := w.reject(head.Type, reason); err != nil {
			return err
		}
	}
}

// pickInstances is steps 4–10 for every instance the web picks.
func (w *webSession) pickInstances() ([]config.Instance, error) {
	r := w.r
	disc, err := r.discover()
	if err != nil {
		return nil, err
	}
	var list []webInstance
	byID := map[string]pg.Candidate{}
	for _, c := range disc.Candidates {
		wi := describeCandidate(c)
		list = append(list, wi)
		byID[wi.ID] = c
	}
	if list == nil {
		list = []webInstance{}
	}
	if len(disc.Candidates) == 0 {
		r.p.printf("  No Postgres found on this machine; the web shows the same. Cancel there and run bracer init %s to enter an address by hand.\n", r.appName)
	} else {
		r.p.printf("  Found %d; choose on the web.\n", len(disc.Candidates))
	}
	if err := w.state(map[string]any{"step": "instances", "instances": list}); err != nil {
		return nil, err
	}
	var sel struct {
		Type      string   `json:"type"`
		Instances []string `json:"instances"`
	}
	err = w.choice("select_instances", &sel, func() string {
		if len(sel.Instances) == 0 {
			return "no instance picked"
		}
		seen := map[string]bool{}
		for _, id := range sel.Instances {
			if _, ok := byID[id]; !ok {
				return "unknown instance " + id
			}
			if seen[id] {
				return "instance " + id + " twice"
			}
			seen[id] = true
		}
		return ""
	})
	if err != nil {
		return nil, err
	}
	var out []config.Instance
	for _, id := range sel.Instances {
		cand := byID[id]
		w.cur = id
		r.p.printf("\nSetting up %s\n", cand.Label())
		inst, err := r.setupInstance(&cand)
		if err != nil {
			return nil, err
		}
		if inst == nil {
			r.p.printf("  %s is not written.\n", cand.Label())
			continue
		}
		out = append(out, *inst)
	}
	w.cur = ""
	return out, nil
}

// databases is the chooser's step 8 on the web.
func (w *webSession) databases(inst *config.Instance, dbs []string) ([]string, error) {
	w.r.p.printf("  Tick the databases on the web; this terminal waits.\n")
	if err := w.state(map[string]any{"step": "databases", "databases": map[string]any{"instance_id": w.cur, "names": dbs}}); err != nil {
		return nil, err
	}
	var sel struct {
		Type       string   `json:"type"`
		InstanceID string   `json:"instance_id"`
		Databases  []string `json:"databases"`
	}
	err := w.choice("select_databases", &sel, func() string {
		if sel.InstanceID != w.cur {
			return "not the instance being set up"
		}
		if len(sel.Databases) == 0 {
			return "no database picked"
		}
		for _, d := range sel.Databases {
			if !containsStr(dbs, d) {
				return "unknown database " + d
			}
		}
		return ""
	})
	if err != nil {
		return nil, err
	}
	// In the server's order, once each.
	var out []string
	for _, d := range dbs {
		if containsStr(sel.Databases, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// decision is the chooser's step 9 on the web: the checks so far, then
// retry / skip the database / skip the instance for the one that failed.
func (w *webSession) decision(inst *config.Instance, checks []webCheck) (string, error) {
	w.forWeb(checks)
	failed := checks[len(checks)-1]
	w.r.p.printf("  Decide on the web: retry, skip %s, or skip this instance; this terminal waits.\n", failed.Database)
	if err := w.state(map[string]any{"step": "checks", "checks": checks}); err != nil {
		return "", err
	}
	var d struct {
		Type       string `json:"type"`
		InstanceID string `json:"instance_id"`
		Database   string `json:"database"`
		Action     string `json:"action"`
	}
	actions := map[string]string{"retry": "r", "skip_database": "s", "skip_instance": "d"}
	err := w.choice("check_decision", &d, func() string {
		if d.InstanceID != w.cur || d.Database != failed.Database {
			return "not the database that failed"
		}
		if actions[d.Action] == "" {
			return "unknown action " + d.Action
		}
		return ""
	})
	if err != nil {
		return "", err
	}
	return actions[d.Action], nil
}

// checked shows the web the whole list once every database is settled.
func (w *webSession) checked(_ *config.Instance, checks []webCheck) error {
	w.forWeb(checks)
	return w.state(map[string]any{"step": "checks", "checks": checks})
}

// forWeb readies checks for /pair/state: the instance's id, and reasons
// with what a report redacts taken out (psql and pg_dump errors can quote
// a URL or a password; the server refuses a body that looks like one).
func (w *webSession) forWeb(checks []webCheck) {
	secrets := config.NewSecrets(w.r.secrets, "init")
	for i := range checks {
		checks[i].InstanceID = w.cur
		checks[i].Reason = secrets.Redact(checks[i].Reason)
	}
}

// summary prints what will be written and asks the last question.
func (w *webSession) summary() (bool, error) {
	r := w.r
	cfg := r.cfg
	r.p.printf("\nAbout to write:\n")
	for _, in := range cfg.Instances {
		r.p.printf("  instance  %s: %s\n", in.Name, strings.Join(in.Databases.Names(), ", "))
	}
	r.p.printf("  bucket    %s\n", storage.Join(cfg.Storage.Bucket, config.BucketPrefix, cfg.Storage.Folder))
	enc := "none"
	if cfg.Encryption == "age" {
		enc = "age, key in " + cfg.AgeIdentityFile
	}
	r.p.printf("  encryption %s\n", enc)
	if cfg.Schedule != nil {
		r.p.printf("  schedule  %s\n", describeSchedule(*cfg.Schedule))
	}
	path, _ := r.configPath(config.SecretsFile)
	r.p.printf("  secrets   %s (S3 keys, passwords and the app token; never sent anywhere)\n", path)
	what := "create"
	if w.continues {
		what = "continue"
	}
	return r.p.yesNo(fmt.Sprintf("Write these files and, once the first backup passes, %s app %s in your account?", what, r.appName), false)
}

// cancel is finish(false): the session closes, nothing is written.
func (w *webSession) cancel() error {
	if _, err := w.api.Finish(w.r.ctx, w.secret, false); err != nil && !errors.Is(err, errPairClosed) {
		w.r.warn("could not close the session: %v", err)
	}
	w.r.p.printf("\n%s Run again when ready:\n  %s\n", w.r.unchanged(), w.r.again)
	return errPairStopped
}

// cancelled is the web's cancel, or the server refusing the account.
func (w *webSession) cancelled(reason string) error {
	// The plan's limit never closes a session before finish (ADR-0033 § 3),
	// so app_limit does not come here.
	switch reason {
	case "app_changed":
		w.r.p.printf("\nApp %s changed or was deleted on the web, so the setup stopped. %s Run again when ready:\n  %s\n", w.r.appName, w.r.unchanged(), w.r.again)
	default:
		w.r.p.printf("\nThe setup was cancelled on the web. %s Run again when ready:\n  %s\n", w.r.unchanged(), w.r.again)
	}
	return errPairStopped
}

// stop turns a pairing error into the message and errPairStopped; other
// errors pass through.
func (w *webSession) stop(err error) error {
	r := w.r
	switch {
	case err == nil || errors.Is(err, errPairStopped):
		return err
	case errors.Is(err, errPairClosed):
		r.p.printf("\npairing expired; run `bracer init %s --web` again, or `bracer init %s` to set up in the terminal. %s\n", r.appName, r.appName, r.unchanged())
		return errPairStopped
	case errors.Is(err, errPairingOff):
		r.p.printf("\nPairing is switched off on the server right now. %s\nSet the app up in the terminal: bracer init %s\n", r.unchanged(), r.appName)
		return errPairStopped
	case errors.Is(err, context.Canceled), errors.Is(err, errQuit), errors.Is(err, ErrNoInput):
		w.closeQuietly()
		return err
	}
	var pe *pairError
	if errors.As(err, &pe) {
		r.p.printf("\nThe server refused: %v. %s Run again when ready:\n  %s\n", err, r.unchanged(), r.again)
		w.closeQuietly()
		return errPairStopped
	}
	w.closeQuietly()
	return err
}

// closeQuietly is finish(false), best effort, with a fresh context in
// case the first one is already cancelled (Ctrl-C).
func (w *webSession) closeQuietly() {
	if w.secret == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.api.Finish(ctx, w.secret, false)
}
