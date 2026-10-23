package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// envDeps is what the environment check reads; tests replace every field.
type envDeps struct {
	shell      pg.Shell
	lookPath   func(string) (string, error)
	getenv     func(string) string
	userName   func() string
	hasSystemd func() bool
}

func defaultEnvDeps(sh pg.Shell) envDeps {
	return envDeps{shell: sh, lookPath: exec.LookPath, getenv: os.Getenv, userName: currentUser, hasSystemd: hasSystemd}
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// envItem is one line of the environment table (spec init § Flow 0).
type envItem struct {
	Name     string
	OK       bool
	Blocking bool     // a ✗ that stops init
	Detail   string   // one line
	Fix      []string // commands or steps, in order
}

// envReport is the outcome of step 0: the table and, when nothing blocks, the
// runtime: block init writes.
type envReport struct {
	Items   []envItem
	Runtime *config.Runtime
}

func (r envReport) Blocked() bool {
	for _, it := range r.Items {
		if !it.OK && it.Blocking {
			return true
		}
	}
	return false
}

// defaultPath is appended to the docker directory in runtime.path.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// checkEnv is step 0 of init: read-only, no network beyond the
// local Docker daemon. → ADR-0013, ADR-0022
func checkEnv(ctx context.Context, d envDeps) envReport {
	var rep envReport
	add := func(it envItem) { rep.Items = append(rep.Items, it) }
	who := d.userName()
	docker := pg.Docker{Shell: d.shell}

	it, dockerPath, lerr := checkDocker(ctx, d)
	add(it)

	endpoint := ""
	if lerr == nil {
		endpoint, _ = docker.Endpoint(ctx, d.getenv("DOCKER_HOST"))
	}
	if strings.HasPrefix(endpoint, "unix:///run/user/") {
		res, err := d.shell.Run(ctx, pg.Cmd{Name: "loginctl", Args: []string{"show-user", who, "-p", "Linger"}})
		switch {
		case err != nil:
			add(envItem{Name: "rootless docker", Blocking: true, Detail: fmt.Sprintf("Docker runs rootless (%s) and lingering for %s could not be read: %s", endpoint, who, firstLine(err.Error())),
				Fix: []string{"sudo loginctl enable-linger " + who}})
		case strings.TrimSpace(string(res.Stdout)) != "Linger=yes":
			add(envItem{Name: "rootless docker", Blocking: true, Detail: fmt.Sprintf("Docker runs rootless (%s) but %s has no lingering: the daemon stops at logout and the nightly jobs would fail", endpoint, who),
				Fix: []string{"sudo loginctl enable-linger " + who}})
		default:
			add(envItem{Name: "rootless docker", OK: true, Detail: "lingering is on for " + who})
		}
	}

	if d.hasSystemd() {
		add(envItem{Name: "systemd", OK: true, Detail: "the nightly jobs run as systemd timers"})
	} else {
		add(envItem{Name: "systemd", Blocking: true, Detail: "not found; bracer runs its nightly jobs with systemd timers",
			Fix: []string{"run bracer on a Linux machine where systemd is PID 1"}})
	}

	if rep.Blocked() {
		return rep
	}
	rep.Runtime = &config.Runtime{User: who, Path: joinPath(filepath.Dir(dockerPath), defaultPath), Docker: dockerPath, DockerHost: endpoint}
	return rep
}

// checkDocker is the docker line of step 0: the docker command is
// installed and answers as this user. restore checks only this.
func checkDocker(ctx context.Context, d envDeps) (envItem, string, error) {
	who := d.userName()
	dockerPath, lerr := d.lookPath("docker")
	if lerr != nil {
		return envItem{Name: "docker", Blocking: true, Detail: "the docker command is not installed (bracer runs every Postgres tool through Docker)",
			Fix: []string{"install Docker Engine: https://docs.docker.com/engine/install/"}}, "", lerr
	}
	if abs, err := filepath.Abs(dockerPath); err == nil {
		dockerPath = abs
	}
	if err := (pg.Docker{Shell: d.shell}).Info(ctx); err != nil {
		return dockerInfoItem(err, who), dockerPath, nil
	}
	return envItem{Name: "docker", OK: true, Detail: "docker info answers as " + who + " (" + dockerPath + ")"}, dockerPath, nil
}

// dockerInfoItem explains why `docker info` failed and what to run.
func dockerInfoItem(err error, who string) envItem {
	msg := err.Error()
	it := envItem{Name: "docker", Blocking: true, Detail: "docker info failed: " + firstLine(msg)}
	switch {
	case strings.Contains(msg, "permission denied"):
		it.Detail = who + " may not use Docker (permission denied on the Docker socket)"
		it.Fix = []string{
			"sudo usermod -aG docker " + who + "   # the docker group is equivalent to root",
			"log out and back in (or run: newgrp docker)",
		}
	default:
		it.Fix = []string{"sudo systemctl enable --now docker"}
	}
	return it
}

// joinPath puts dir in front of list, each directory once.
func joinPath(dir, list string) string {
	seen := map[string]bool{}
	var out []string
	for _, d := range append([]string{dir}, strings.Split(list, ":")...) {
		if d != "" && d != "." && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, ":")
}

// printEnv prints the table; when something blocks, the fixes in order and,
// last, the command to run again.
func printEnv(w io.Writer, rep envReport, again string) {
	for _, it := range rep.Items {
		mark := "✓"
		if !it.OK {
			mark = "✗"
		}
		fmt.Fprintf(w, "  %s %s: %s\n", mark, it.Name, it.Detail)
	}
	if !rep.Blocked() {
		return
	}
	fmt.Fprintf(w, "\nFix, in this order, then run the command again:\n")
	n := 1
	for _, it := range rep.Items {
		if it.OK || !it.Blocking {
			continue
		}
		for _, f := range it.Fix {
			fmt.Fprintf(w, "  %d. %s\n", n, f)
			n++
		}
	}
	fmt.Fprintf(w, "  %d. %s\n", n, again)
}

// commandLine rebuilds what the user typed, for the last line of the fixes.
func commandLine(name string, args []string) string {
	parts := []string{"bracer", name}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"$\\") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// errEnvBlocked: step 0 found something to fix; the table is already printed.
var errEnvBlocked = errors.New("environment check failed")
