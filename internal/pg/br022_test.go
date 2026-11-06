package pg

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

// BR-022: a pinned session reaches the process that runs the tool, for
// every runner, by name through -e as every other variable; no argument
// carries a value.
func TestBR022PinTimeReachesTheTool(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		r    func(Shell) Runner
	}{
		{"docker exec", func(sh Shell) Runner { return ExecRunner{Shell: sh, Container: "shop-db"} }},
		{"docker run, host network", func(sh Shell) Runner { return NewURLRunner(sh, "postgres:17-alpine") }},
		{"docker run, container network", func(sh Shell) Runner {
			return ImageRunner{Shell: sh, Image: "postgres:17-alpine", Network: "container:shop-db"}
		}},
		{"socket", func(sh Shell) Runner {
			return SocketRunner{Shell: sh, Image: "postgres:17-alpine", Local: Local{Dir: "/var/run/postgresql", Port: 5432}}
		}},
	} {
		for _, pin := range []bool{true, false} {
			sh := &fakeShell{out: map[string]string{"docker": ""}}
			r := tc.r(sh)
			if err := r.Run(ctx, Conn{User: "u", Password: "s3cret", PinTime: pin}, "pg_dumpall", []string{"--globals-only"}, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			c := sh.calls[0]
			for _, k := range []string{"PGDATESTYLE", "PGTZ"} {
				named := false
				for i, a := range c.Args {
					if a == "-e" && i+1 < len(c.Args) && c.Args[i+1] == k {
						named = true
					}
				}
				if named != pin {
					t.Errorf("%s pin=%v: -e %s passed = %v: %v", tc.name, pin, k, named, c.Args)
				}
			}
			if pin && (envOf(c.Env, "PGDATESTYLE") != "ISO" || envOf(c.Env, "PGTZ") != "UTC") {
				t.Errorf("%s: env %v", tc.name, c.Env)
			}
			if i := slices.IndexFunc(c.Args, func(a string) bool { return strings.Contains(a, "=") || strings.Contains(a, "s3cret") }); i >= 0 {
				t.Errorf("%s: an argument carries a value: %q", tc.name, c.Args[i])
			}
		}
	}
}
