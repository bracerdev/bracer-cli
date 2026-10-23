package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// runRemove is `bracer remove <app>` (spec schedule § remove, ADR-0023):
// the timer goes (ADR-0027: by this command, through sudo), then every file
// of the app but age.key, which opens the backups and which the next init
// reuses (ADR-0030). The bucket is never touched: the backups stay there
// and restore still opens them. A broken app.yaml can be removed too; that
// is how it is fixed.
func runRemove(ctx context.Context, args []string, d scheduleDeps) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer remove: "+format+"\n", a...)
		return code
	}
	if !d.tty {
		return fail(exitUsage, "remove needs a terminal")
	}
	cfgPath, code, err := selectApp(fs, args, d.home(), "remove")
	if code != exitOK {
		if err != nil {
			return fail(code, "%v", err)
		}
		return code
	}
	app := config.AppOf(cfgPath)
	where := "unknown (app.yaml cannot be read)"
	cfg, err := config.Load(cfgPath)
	if err == nil {
		s := cfg.Storage
		where = storage.Join(s.Bucket, config.BucketPrefix, s.Folder)
		fmt.Fprintf(d.stdout, "app %s stores its backups at:\n  endpoint  %s\n  bucket    %s\n  folder    %s\n", app, s.Endpoint, s.Bucket, storage.Join(config.BucketPrefix, s.Folder))
		msg := "the backups in " + s.Folder + " stay in the bucket"
		if cfg.Encryption == "age" {
			msg += "; age.key stays on this machine and opens them"
		}
		fmt.Fprintln(d.stdout, msg)
	} else {
		fmt.Fprintf(d.stdout, "app %s: where it stores its backups is %s\n", app, where)
	}
	p := newPrompter(d.stdin, d.stdout)
	typed, err := p.ask("Type the app name to remove it", "")
	if err != nil || strings.TrimSpace(typed) != app {
		fmt.Fprintln(d.stdout, "nothing removed")
		return exitUsage
	}
	if err := uninstallUnits(ctx, app, d); err != nil {
		return fail(exitError, "%v\ntimer not removed; nothing deleted", err)
	}
	dir := filepath.Dir(cfgPath)
	keptKey, err := removeAppFiles(dir)
	if err != nil {
		return fail(exitError, "the timer is removed, but %s is not: %v", dir, err)
	}
	fmt.Fprintf(d.stdout, "app %s removed from this machine; its backups stay in %s\n", app, where)
	if keptKey {
		fmt.Fprintf(d.stdout, "kept %s: it opens the backups in %s; delete it yourself when you no longer need them\n", filepath.Join(dir, config.KeyFile), where)
	}
	return exitOK
}

// removeAppFiles deletes the app directory but age.key (ADR-0030); the
// directory itself goes when nothing is left. It reports whether a key stayed.
func removeAppFiles(dir string) (keptKey bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() == config.KeyFile {
			keptKey = true
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return keptKey, err
		}
	}
	if keptKey {
		return true, nil
	}
	return false, os.Remove(dir)
}
