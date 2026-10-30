package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"path/filepath"

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
}

// newWebSession pairs through api at server, the URL the nightly job will
// report to: the one this machine reached, not one the server names.
func newWebSession(r *initRun, api pairAPI, server string) *webSession {
	return &webSession{r: r, api: api, server: server}
}

// webInstance is one found Postgres as the web lists it (contract init-web).
type webInstance struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"` // container | url (ADR-0013)
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
		w.ID, w.Kind = fmt.Sprintf("local:%d", c.Local.Port), "url"
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
	done, err := w.api.Finish(r.ctx, w.secret, true, r.appName)
	if err != nil {
		return w.stop(err)
	}
	r.p.before = nil
	r.cfg.Hosted = &config.Hosted{Server: w.server, TokenEnv: hostedTokenEnv}
	r.secrets[hostedTokenEnv] = done.Token
	r.p.printf("\nApp %s created on %s; its token goes to secrets.env as %s.\n", r.appName, w.server, hostedTokenEnv)
	if err := r.finish(); err != nil {
		if !r.tokenSaved() {
			// The token was issued once (spec init-web § Edge cases).
			r.p.printf("\nThe token could not be saved. Keep it: %s=%s\n", hostedTokenEnv, done.Token)
		}
		return err
	}
	return nil
}

// tokenSaved reports whether secrets.env holds the token.
func (r *initRun) tokenSaved() bool {
	if r.cfg.SecretsFile == "" {
		return false
	}
	secrets, _, err := config.LoadSecrets(r.cfg)
	if err != nil {
		return false
	}
	_, err = secrets.Require(hostedTokenEnv)
	return err == nil
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
		opened, err = w.api.Open(w.r.ctx, Version, host)
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
		}
		json.Unmarshal(ev, &e)
		switch e.Type {
		case "claimed":
			return w.r.p.yesNo(fmt.Sprintf("\nClaimed by %s on the web. Continue with that account?", orDefault(e.Account, "an account")), false)
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
	for i := range checks {
		checks[i].InstanceID = w.cur
	}
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
	for i := range checks {
		checks[i].InstanceID = w.cur
	}
	return w.state(map[string]any{"step": "checks", "checks": checks})
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
	return r.p.yesNo("Create the app in your account and write these files?", false)
}

// cancel is finish(false): the session closes, nothing is written.
func (w *webSession) cancel() error {
	if _, err := w.api.Finish(w.r.ctx, w.secret, false, w.r.appName); err != nil && !errors.Is(err, errPairClosed) {
		w.r.warn("could not close the session: %v", err)
	}
	w.r.p.printf("\n%s Run again when ready:\n  %s\n", w.r.unchanged(), w.r.again)
	return errPairStopped
}

// cancelled is the web's cancel, or the server refusing the account.
func (w *webSession) cancelled(reason string) error {
	switch reason {
	case "trial_ended":
		w.r.p.printf("\nYour trial on the hosted server has ended, so no app can be added there. %s\nTo set the app up in the terminal alone, run: bracer init %s\n", w.r.unchanged(), w.r.appName)
	case "app_limit":
		w.r.p.printf("\nYour plan's app limit is reached, so no app can be added. %s\nTo set the app up in the terminal alone, run: bracer init %s\n", w.r.unchanged(), w.r.appName)
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
	case errors.Is(err, errAppExists):
		r.p.printf("\nYour account already has an app named %s. %s\nFix: run bracer init <another-name> --web, or revoke that app's token on the web and remove it, then run: %s\n", r.appName, r.unchanged(), r.again)
		w.closeQuietly()
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
	w.api.Finish(ctx, w.secret, false, w.r.appName)
}
