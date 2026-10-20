package pg

import (
	"context"
	"strings"
	"testing"
)

func TestParseToolVersion(t *testing.T) {
	for in, want := range map[string]struct {
		ver   string
		major int
	}{
		"pg_dump (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)\n": {"17.11", 17},
		"pg_dump (PostgreSQL) 18.1\n":                            {"18.1", 18},
		"pg_dump (PostgreSQL) 9.6.24\n":                          {"9.6", 9},
	} {
		ver, major, ok := ParseToolVersion(in)
		if !ok || ver != want.ver || major != want.major {
			t.Errorf("%q → %q %d %v", in, ver, major, ok)
		}
	}
	if _, _, ok := ParseToolVersion("psql: command not found"); ok {
		t.Error("garbage must not parse")
	}
}

func TestToolsPickFollowsADR0013(t *testing.T) {
	ctx := context.Background()
	// Container has pg_dump → docker exec; without it → image on the same network.
	sh := &fakeShell{out: map[string]string{"docker version": "29\n", "docker exec shop-db sh -c command -v pg_dump": "/usr/local/bin/pg_dump\n",
		"docker inspect --format {{.State.Status}} slim": "running\n", "docker inspect --format {{.State.Status}} stopped": "exited\n"},
		fail: map[string]string{"docker exec slim sh": "not found", "docker exec stopped sh": "is not running", "docker exec gone sh": "No such container",
			"docker inspect --format {{.State.Status}} gone": "Error: No such object: gone"}}
	tl := &Tools{Shell: sh}
	if r, err := tl.Pick(ctx, Where{Container: "shop-db"}, 17); err != nil || r.Describe() != "docker exec shop-db" {
		t.Errorf("container: %v %v", r, err)
	}
	if r, err := tl.Pick(ctx, Where{Container: "slim"}, 17); err != nil || r.Describe() != "docker run postgres:17-alpine --network container:slim" {
		t.Errorf("container without pg_dump: %v %v", r, err)
	}
	// docker exec fails because the container is stopped or gone → say so, with Fix.
	if _, err := tl.Pick(ctx, Where{Container: "stopped"}, 17); err == nil || err.Error() != "container stopped is not running (exited)\nFix: docker start stopped" {
		t.Errorf("stopped: %v", err)
	}
	if _, err := tl.Pick(ctx, Where{Container: "gone"}, 17); err == nil || !strings.Contains(err.Error(), "container gone does not exist\nFix: docker ps -a") {
		t.Errorf("gone: %v", err)
	}
	// URL: always the image of the server's major; unknown major → the client image.
	if r, err := tl.Pick(ctx, Where{}, 16); err != nil || r.Describe() != "docker run postgres:16-alpine --network host" {
		t.Errorf("url: %v %v", r, err)
	}
	if r, err := tl.Pick(ctx, Where{}, 0); err != nil || r.Describe() != "docker run "+ClientImage+" --network host" {
		t.Errorf("url, unknown major: %v %v", r, err)
	}
	// No Docker → error; probes are cached.
	noDocker := &fakeShell{fail: map[string]string{"docker version": "Cannot connect to the Docker daemon"}}
	if _, err := (&Tools{Shell: noDocker}).Pick(ctx, Where{}, 16); err == nil || !strings.Contains(err.Error(), "Docker is not available") {
		t.Errorf("no docker: %v", err)
	}
	n := 0
	for _, c := range sh.calls {
		if strings.HasPrefix(strings.Join(c.Args, " "), "version") || strings.Contains(strings.Join(c.Args, " "), "command -v") {
			n++
		}
	}
	if n != 5 {
		t.Errorf("probe results must be cached: %d commands", n)
	}
}

func TestToolsResolveAsksVersionThenRepicks(t *testing.T) {
	psql := "docker run --rm -i --network host"
	sh := &fakeShell{out: map[string]string{"docker version": "29\n", psql: "170011\n"}}
	tl := &Tools{Shell: sh}
	r, major, err := tl.Resolve(context.Background(), Where{}, Conn{Host: "127.0.0.1", Port: 15432, User: "postgres", Database: "postgres"}, 0)
	if err != nil || major != 17 || r.Describe() != "docker run postgres:17-alpine --network host" {
		t.Errorf("resolve: %v %d %v", r, major, err)
	}
}

func TestDiscoverHidesCriDockerdAndListsSockets(t *testing.T) {
	k8s := &Container{Name: "k8s_postgres_postgres-0_ns_u_0", Image: "postgres:17", Running: true, Kubernetes: true}
	hs := HostScan{Sockets: map[int]string{5433: "/var/run/postgresql/.s.PGSQL.5433"},
		Listeners: []Listener{{Addr: "127.0.0.1", Port: 5433, Process: "postgres"}, {Addr: "127.0.0.1", Port: 5440, Process: "postgres"}}}
	d := Discover(context.Background(), []*Container{k8s}, hs, nil)
	if len(d.Kubernetes) != 1 || len(d.Candidates) != 2 {
		t.Fatalf("discovery: %+v", d)
	}
	// The socket is the cluster; its TCP port is folded in, not listed again.
	if l := d.Candidates[0].Local; l == nil || l.Port != 5433 || l.Dir != "/var/run/postgresql" ||
		d.Candidates[0].Label() != "local Postgres on port 5433 (socket /var/run/postgresql)" || d.Candidates[0].SuggestName() != "local-5433" {
		t.Errorf("local candidate: %+v", d.Candidates[0])
	}
	if c := d.Candidates[1]; c.Local != nil || c.Port != 5440 {
		t.Errorf("tcp candidate: %+v", c)
	}
}

func TestCandidateURLHost(t *testing.T) {
	for host, want := range map[string]string{"0.0.0.0": "127.0.0.1", "127.0.0.1": "127.0.0.1", "::": "127.0.0.1", "::1": "[::1]", "10.0.0.5": "10.0.0.5", "fd00::5": "[fd00::5]"} {
		if got := (Candidate{Host: host}).URLHost(); got != want {
			t.Errorf("%s → %s, want %s", host, got, want)
		}
	}
}
