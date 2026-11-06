package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Every file of an app lives in ~/.config/bracer/<app>/ (ADR-0021): app.yaml,
// its schedule file, secrets.env and age.key. Commands name the app, and an
// app is nowhere else.
const (
	AppFile     = "app.yaml"
	SecretsFile = "secrets.env"
	KeyFile     = "age.key"
)

// AppsDir is ~/.config/bracer of home.
func AppsDir(home string) string { return filepath.Join(home, ".config", "bracer") }

// AppDir is the directory of one app.
func AppDir(home, name string) string { return filepath.Join(AppsDir(home), name) }

// AppPath is the app.yaml of one app.
func AppPath(home, name string) string { return filepath.Join(AppDir(home, name), AppFile) }

// MaxAppName is the longest app name: the name is also in the unit names
// (bracer-<app>.service, at most 256 characters for systemd) and in the
// timer's stamp file (stamp-bracer-<app>.timer, at most 255).
const MaxAppName = 64

// ValidAppName is the rule of `app:`, which is also the directory name.
func ValidAppName(name string) bool { return AppNameProblem(name) == "" }

// AppNameProblem says what is wrong with an app name, "" when nothing is.
func AppNameProblem(name string) string {
	switch {
	case !reName.MatchString(name):
		return "lowercase letters, digits, `-`, `_` only"
	case len(name) > MaxAppName:
		return fmt.Sprintf("at most %d characters (it is part of the systemd unit names)", MaxAppName)
	}
	return ""
}

// ValidInstanceName is the rule of instances[].name, the same as `app:`.
func ValidInstanceName(name string) bool { return reName.MatchString(name) }

// KnownApps lists, sorted, the apps of home: directories of ~/.config/bracer
// that hold an app.yaml.
func KnownApps(home string) []string {
	entries, err := os.ReadDir(AppsDir(home))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !ValidAppName(e.Name()) {
			continue
		}
		if fi, err := os.Stat(AppPath(home, e.Name())); err == nil && fi.Mode().IsRegular() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// AppOf is the app name of an app.yaml: the name of its app directory.
func AppOf(path string) string {
	return filepath.Base(filepath.Dir(path))
}
