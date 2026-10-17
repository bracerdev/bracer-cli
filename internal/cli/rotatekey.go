package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"filippo.io/age"
	"golang.org/x/term"
)

// rotateResult is the --json body; it never contains the private key.
type rotateResult struct {
	Kind          string   `json:"kind"`
	App           string   `json:"app"`
	KeyFile       string   `json:"key_file"`
	NewRecipient  string   `json:"new_recipient"`
	OldRecipients []string `json:"old_recipients"`
	OK            bool     `json:"ok"`
}

// runRotateKey is `bracer rotate-key -c app.yaml [--yes] [--json]`
// (spec backup/rotate-key, ADR-0008 option B): the new key is appended to the
// key file, app.yaml points at the new key, old backups stay as they are. Offline, does not touch the bucket.
func runRotateKey(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(stderr, "bracer rotate-key: "+format+"\n", a...)
		return code
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "unexpected argument %q", fs.Arg(0))
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if cfg.Encryption != "age" {
		return fail(exitUsage, "encryption is not enabled for this app; run `bracer init -c %s` to turn it on", *cfgPath)
	}
	keyPath, err := config.ExpandHome(cfg.AgeIdentityFile)
	if err != nil {
		return fail(exitUsage, "age_identity_file: %v", err)
	}

	// Can only ask when stdin is a terminal (tests passing a plain reader still get asked).
	if f, ok := stdin.(*os.File); ok && !*yes && !term.IsTerminal(int(f.Fd())) {
		return fail(exitUsage, "stdin is not a terminal; pass --yes to rotate without confirmation")
	}
	p := newPrompter(stdin, stderr)
	confirm := func(question string, def bool) (bool, error) {
		if *yes {
			return true, nil
		}
		ok, err := p.yesNo(question, def)
		if errors.Is(err, ErrNoInput) {
			return false, fmt.Errorf("no answer on stdin; pass --yes to rotate without confirmation")
		}
		return ok, err
	}

	// 2. State of the key file.
	var oldRecips []string
	exists := true
	_, recips, rerr := crypto.ReadIdentities(keyPath)
	switch {
	case rerr == nil:
		oldRecips = recips
		if !containsStr(recips, cfg.AgeRecipient) {
			fmt.Fprintf(stderr, "warning: %s does not hold the private key for the current recipient %s; backups made with it already cannot be decrypted\n", cfg.AgeIdentityFile, cfg.AgeRecipient)
		}
	case errors.Is(rerr, os.ErrNotExist):
		exists = false
		fmt.Fprintf(stderr, "warning: the key file %s is missing: backups made with %s cannot be decrypted until it is restored\n", cfg.AgeIdentityFile, cfg.AgeRecipient)
		ok, err := confirm("Continue and create a new key file with only the new key?", false)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		if !ok {
			return fail(exitError, "not rotated")
		}
	case errors.Is(rerr, os.ErrPermission):
		return fail(exitUsage, "cannot read %s: %v (run with sudo, or chown the file to the user running bracer)", cfg.AgeIdentityFile, rerr)
	default:
		return fail(exitUsage, "cannot read %s: %v", cfg.AgeIdentityFile, rerr)
	}

	// 3. Confirmation.
	if exists {
		ok, err := confirm(fmt.Sprintf("Rotate the age key for app %q? New backups will use a new key; the current key (%s) stays in %s so old backups still open.", cfg.App, cfg.AgeRecipient, cfg.AgeIdentityFile), false)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		if !ok {
			return fail(exitError, "not rotated")
		}
	}

	// 4. New key into the file (key first, app.yaml after).
	kp, err := crypto.Generate()
	if err != nil {
		return fail(exitError, "%v", err)
	}
	if exists {
		err = crypto.AppendIdentity(keyPath, kp)
	} else {
		err = crypto.WriteIdentityFile(keyPath, kp)
	}
	if err != nil {
		return fail(exitError, "cannot write %s: %v", cfg.AgeIdentityFile, err)
	}

	// 5. Self-test: the new key reads back and can encrypt/decrypt.
	ids, recips, err := crypto.ReadIdentities(keyPath)
	if err != nil || !containsStr(recips, kp.Recipient) {
		return fail(exitError, "the new key was written to %s but cannot be read back (%v); app.yaml left unchanged", cfg.AgeIdentityFile, err)
	}
	if err := roundTrip(kp.Recipient, ids); err != nil {
		return fail(exitError, "self-test with the new key failed: %v; app.yaml left unchanged", err)
	}

	// 6. app.yaml points at the new key.
	oldRecipient := cfg.AgeRecipient
	cfg.AgeRecipient = kp.Recipient
	if err := config.Save(*cfgPath, cfg); err != nil {
		return fail(exitError, "cannot write %s: %v\nThe key file now holds one extra, unused key (harmless); app.yaml still points at %s and backups keep using it. Fix the problem and run rotate-key again.", *cfgPath, err, oldRecipient)
	}

	// 7. Result.
	res := rotateResult{Kind: "rotate-key", App: cfg.App, KeyFile: cfg.AgeIdentityFile, NewRecipient: kp.Recipient, OldRecipients: oldRecips, OK: true}
	out := stdout
	if *asJSON {
		out = stderr // stdout is JSON; the key and the advice go to stderr
	}
	fmt.Fprintf(out, "Rotated the age key for app %q.\n\n  New public key:  %s\n  Key file:        %s (%d older key(s) kept, so old backups still open)\n\nNew private key, shown once, never logged:\n\n  %s\n\nBack it up in a password manager now, or back up the whole key file: it holds every key this app has used.\nAlso rotate the bucket access key if this rotation is because something leaked; a leaked age key alone does not expose data, a leaked bucket key with it does.\nThe next `bracer backup` report shows how many dumps still use old keys; retention removes them over time.\n",
		cfg.App, kp.Recipient, cfg.AgeIdentityFile, len(oldRecips), kp.Identity)
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
	return exitOK
}

// roundTrip encrypts a string in memory for recipient, then decrypts it with ids.
func roundTrip(recipient string, ids []age.Identity) error {
	var buf bytes.Buffer
	w, err := crypto.Encrypt(&buf, recipient)
	if err != nil {
		return err
	}
	const probe = "bracer rotate-key self-test"
	if _, err := io.WriteString(w, probe); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	r, err := crypto.Decrypt(&buf, ids)
	if err != nil {
		return err
	}
	got, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if string(got) != probe {
		return fmt.Errorf("decrypted text differs")
	}
	return nil
}
