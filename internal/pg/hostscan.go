package pg

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Listener is a TCP port listening on this host.
type Listener struct {
	Addr    string // "127.0.0.1", "0.0.0.0", "::1"…
	Port    int
	Process string // process name if visible
}

// HostScan is the result of scanning the host: ports and Unix sockets, with warnings when permissions are lacking.
type HostScan struct {
	Listeners []Listener
	Sockets   map[int]string // port → path of the .s.PGSQL.<port> socket
	Warnings  []string
}

// ScanHost uses ss (Linux) or lsof (macOS) plus a socket glob; goos picks the
// tool (runtime.GOOS in production, fixed in tests). Tool failures are not
// fatal: record a warning, return what is available.
func ScanHost(ctx context.Context, sh Shell, goos string) HostScan {
	hs := HostScan{Sockets: map[int]string{}}
	var res Result
	var err error
	if goos == "linux" {
		res, err = sh.Run(ctx, Cmd{Name: "ss", Args: []string{"-ltnpH"}})
		if err == nil {
			hs.Listeners = parseSS(string(res.Stdout))
		}
	} else {
		res, err = sh.Run(ctx, Cmd{Name: "lsof", Args: []string{"-nP", "-iTCP", "-sTCP:LISTEN"}})
		if err == nil {
			hs.Listeners = parseLsof(string(res.Stdout))
		}
	}
	if err != nil {
		hs.Warnings = append(hs.Warnings, fmt.Sprintf("could not list listening ports (%v); instances running as another user may be missing", err))
	} else if missingProcess(hs.Listeners) {
		hs.Warnings = append(hs.Warnings, "could not see the process behind some ports (permission denied); instances running as another user may be missing")
	}
	for _, dir := range []string{"/var/run/postgresql", "/tmp"} {
		matches, _ := filepath.Glob(filepath.Join(dir, ".s.PGSQL.*"))
		for _, m := range matches {
			if strings.HasSuffix(m, ".lock") {
				continue
			}
			if p, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), ".s.PGSQL.")); err == nil {
				if _, dup := hs.Sockets[p]; !dup {
					hs.Sockets[p] = m
				}
			}
		}
	}
	return hs
}

func missingProcess(ls []Listener) bool {
	for _, l := range ls {
		if l.Process == "" {
			return true
		}
	}
	return false
}

// parseSS parses `ss -ltnpH`: LISTEN 0 244 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=1,fd=7))
func parseSS(out string) []Listener {
	var ls []Listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		addr, port, ok := splitHostPort(f[3])
		if !ok {
			continue
		}
		proc := ""
		if i := strings.Index(line, `users:(("`); i >= 0 {
			rest := line[i+9:]
			if j := strings.Index(rest, `"`); j >= 0 {
				proc = rest[:j]
			}
		}
		ls = append(ls, Listener{Addr: addr, Port: port, Process: proc})
	}
	return ls
}

// parseLsof parses `lsof -nP -iTCP -sTCP:LISTEN`: postgres 123 u 7u IPv4 … TCP 127.0.0.1:5432 (LISTEN)
func parseLsof(out string) []Listener {
	var ls []Listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 9 || f[0] == "COMMAND" {
			continue
		}
		addr, port, ok := splitHostPort(f[8])
		if !ok {
			continue
		}
		ls = append(ls, Listener{Addr: addr, Port: port, Process: f[0]})
	}
	return ls
}

// splitHostPort splits "127.0.0.1:5432", "[::1]:5432", "*:5432".
func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	host := strings.Trim(s[:i], "[]")
	if host == "*" || host == "" {
		host = "0.0.0.0"
	}
	return host, port, true
}
