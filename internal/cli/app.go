package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"bracer.dev/bracer/internal/config"
)

// errNoApp: the app named is not on this machine. restore then reads the
// configuration from the bucket.
var errNoApp = errors.New("no app")

// parseArgs parses flags placed before or after the app name and returns
// the positional arguments (`bracer backup shop --dry-run`).
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// appArg is parseArgs for commands taking at most one app name.
func appArg(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parseArgs(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) > 1 {
		return "", fmt.Errorf("one app at a time; got %s", strings.Join(pos, " "))
	}
	if len(pos) == 1 {
		return pos[0], nil
	}
	return "", nil
}

// resolveApp is the app.yaml a command works on (ADR-0021):
// ~/.config/bracer/<name>/app.yaml of home, and nowhere else. The name is
// required even with one app on the machine: a command never guesses its
// app. A missing app wraps errNoApp.
func resolveApp(home, name, cmd string) (string, error) {
	apps := config.KnownApps(home)
	if name == "" {
		return "", noName(apps, cmd)
	}
	if !config.ValidAppName(name) {
		return "", fmt.Errorf("%q is not an app name: lowercase letters, digits, `-`, `_` only", name)
	}
	p := config.AppPath(home, name)
	if _, err := os.Stat(p); err != nil {
		here := ""
		if len(apps) > 0 {
			here = " (apps here: " + strings.Join(apps, ", ") + ")"
		}
		return "", fmt.Errorf("%w %s on this machine%s\nFix: bracer init %s", errNoApp, name, here, name)
	}
	return p, nil
}

// noName is the error of a command run without an app name (ADR-0021).
func noName(apps []string, cmd string) error {
	if len(apps) == 0 {
		return fmt.Errorf("name the app: bracer %s <app> (no app on this machine yet)\nFix: bracer init <name>", cmd)
	}
	return fmt.Errorf("name the app: bracer %s <app> (apps here: %s)", cmd, strings.Join(apps, ", "))
}

// userHome is $HOME, or "" when it cannot be told (resolveApp then finds no app).
func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// selectApp parses args and returns the app.yaml of the app named. A flag
// error is already printed (exitUsage, nil); a resolution error is returned
// to print as "bracer <cmd>: …".
func selectApp(fs *flag.FlagSet, args []string, home, cmd string) (string, int, error) {
	return selectAppOrDraft(fs, args, home, cmd, nil)
}

// selectAppOrDraft is selectApp for backup and drill: once the flags are
// parsed and *draft is set (--init-draft), the app's draft must exist
// instead of its app.yaml, which a new app does not have yet.
func selectAppOrDraft(fs *flag.FlagSet, args []string, home, cmd string, draft *bool) (string, int, error) {
	name, err := appArg(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) || !strings.HasPrefix(err.Error(), "one app at a time") {
			return "", exitUsage, nil
		}
		return "", exitUsage, err
	}
	if draft != nil && *draft {
		if !config.ValidAppName(name) {
			return "", exitUsage, fmt.Errorf("--init-draft needs the app's name")
		}
		p := config.AppPath(home, name)
		if _, err := os.Stat(draftPath(p)); err != nil {
			return "", exitUsage, fmt.Errorf("no draft of app %s: %v", name, err)
		}
		return p, exitOK, nil
	}
	p, err := resolveApp(home, name, cmd)
	if err != nil {
		return "", exitUsage, err
	}
	return p, exitOK, nil
}
