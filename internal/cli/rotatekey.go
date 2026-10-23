package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"filippo.io/age"
	"golang.org/x/term"
)

// runRotateKey is `bracer rotate-key <app>` (spec backup/rotate-key,
// ADR-0008 option B): the new key is appended to age.key, app.yaml points at
// the new key, old backups stay as they are. Offline, does not touch the
// bucket. Attended only: the new key must be copied off the machine.
func runRotateKey(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath, code, err := selectApp(fs, args, userHome(), "rotate-key")
	if code != exitOK {
		if err != nil {
			fmt.Fprintf(stderr, "bracer rotate-key: %v\n", err)
		}
		return code
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(stderr, "bracer rotate-key: "+format+"\n", a...)
		return code
	}
	app := config.AppOf(cfgPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if cfg.Encryption != "age" {
		return fail(exitUsage, "encryption is not enabled for this app\nFix: %s", backup.FixReinit(app))
	}
	// Stop before writing anything when nobody can confirm the copy (ADR-0022).
	// Tests passing a plain reader still get asked.
	if f, ok := stdin.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		return fail(exitUsage, "rotate-key must confirm the new key was copied; run it in a terminal")
	}
	keyPath := cfg.AgeIdentityFile
	copyBack := fmt.Sprintf("Fix: copy age.key back from the copy you made off this machine, then run bracer rotate-key %s again", app)

	// The current key must be there: rotating over a lost key would leave
	// the old backups unopenable without anyone noticing (ADR-0022).
	_, oldRecips, rerr := crypto.ReadIdentities(keyPath)
	switch {
	case errors.Is(rerr, os.ErrNotExist):
		return fail(exitError, "the key file %s is missing: backups made with %s cannot be opened without it\n%s", keyPath, cfg.AgeRecipient, copyBack)
	case errors.Is(rerr, os.ErrPermission):
		return fail(exitUsage, "cannot read %s: %v\nFix: run as the user that owns it (sudo -u), or chown it to the user running bracer", keyPath, rerr)
	case rerr != nil:
		return fail(exitUsage, "cannot read %s: %v", keyPath, rerr)
	case !containsStr(oldRecips, cfg.AgeRecipient):
		return fail(exitError, "%s does not hold the key for %s: backups made with it cannot be opened\n%s", keyPath, cfg.AgeRecipient, copyBack)
	}

	p := newPrompter(stdin, stdout)
	ok, err := p.yesNo(fmt.Sprintf("Rotate the age key for app %s? New backups will use a new key; the current key (%s) stays in %s so old backups still open.", cfg.App, cfg.AgeRecipient, keyPath), false)
	if err != nil || !ok {
		return fail(exitError, "not rotated")
	}

	// New key into the file first, app.yaml after.
	kp, err := crypto.Generate()
	if err != nil {
		return fail(exitError, "%v", err)
	}
	if err := crypto.AppendIdentity(keyPath, kp); err != nil {
		return fail(exitError, "cannot write %s: %v", keyPath, err)
	}
	// Self-test: the new key reads back and can encrypt and decrypt.
	ids, recips, err := crypto.ReadIdentities(keyPath)
	if err != nil || !containsStr(recips, kp.Recipient) {
		return fail(exitError, "the new key was written to %s but cannot be read back (%v); app.yaml left unchanged", keyPath, err)
	}
	if err := roundTrip(kp.Recipient, ids); err != nil {
		return fail(exitError, "self-test with the new key failed: %v; app.yaml left unchanged", err)
	}
	oldRecipient := cfg.AgeRecipient
	cfg.AgeRecipient = kp.Recipient
	if err := config.Save(cfgPath, cfg); err != nil {
		return fail(exitError, "cannot write %s: %v\nThe key file now holds one extra, unused key (harmless); app.yaml still points at %s and backups keep using it. Fix the problem and run rotate-key again.", cfgPath, err, oldRecipient)
	}

	fmt.Fprintf(stdout, "\nRotated the age key for app %s.\n\n  New public key:  %s\n  Key file:        %s (%d older key(s) kept, so old backups still open)\n\nIf this rotation is because something leaked, rotate the bucket access key too: a leaked age key alone does not expose data, a leaked bucket key with it does.\n",
		cfg.App, kp.Recipient, keyPath, len(oldRecips))
	// The private key is never printed: age.key is the rescue card (ADR-0022).
	for {
		ans, err := p.ask(fmt.Sprintf("Copy %s somewhere off this machine again: it now holds the new key as well as the old ones. Without the new copy the backups made from now on cannot be opened if this machine is lost. Type copied when done", keyPath), "")
		if err != nil {
			return fail(exitUsage, "%v; the new key is in use: copy %s off this machine", err, keyPath)
		}
		if strings.TrimSpace(ans) == "copied" {
			return exitOK
		}
	}
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
