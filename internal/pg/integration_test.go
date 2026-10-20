//go:build integration

package pg

import (
	"context"
	"os"
	"testing"
	"time"
)

// Needs Docker and a Postgres container named BRACER_TEST_CONTAINER (default
// bracer-test-pg, image postgres:17-alpine, POSTGRES_PASSWORD=pw, POSTGRES_DB=shop).
func TestIntegrationContainer(t *testing.T) {
	name := os.Getenv("BRACER_TEST_CONTAINER")
	if name == "" {
		name = "bracer-test-pg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sh := ExecShell{}
	d := Docker{Shell: sh}
	if err := d.Version(ctx); err != nil {
		t.Skip("docker not running:", err)
	}
	cs, err := d.ListPostgresContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ct *Container
	for _, c := range cs {
		if c.Name == name {
			ct = c
		}
	}
	if ct == nil {
		t.Skipf("container %s not found among %d Postgres containers", name, len(cs))
	}
	if !ct.HasPgProc || len(ct.HostPorts) == 0 {
		t.Errorf("container: %+v", ct)
	}
	hs := ScanHost(ctx, sh)
	for _, c := range Discover(ctx, cs, hs, ProbeSSLRequest).Candidates {
		t.Log(c.Label())
		if c.Container == nil && c.Port == ct.HostPorts[0] {
			t.Error("a mapped port must merge into the container, not become its own instance")
		}
	}
	if ok, err := ProbeSSLRequest(ctx, "127.0.0.1", ct.HostPorts[0]); !ok || err != nil {
		t.Errorf("SSLRequest through the mapped port: %v %v", ok, err)
	}

	// Runner 1: docker exec, trust over the socket, no password.
	cl := &Client{Runner: ExecRunner{Shell: sh, Container: ct.Name}, Conn: Conn{User: ct.DefaultUser(), Database: "postgres"}}
	if err := cl.Ping(ctx); err != nil {
		t.Fatal("docker exec without password:", err)
	}
	num, major, err := cl.ServerVersionNum(ctx)
	if err != nil || major < 10 {
		t.Fatal(num, major, err)
	}
	dbs, err := cl.ListDatabases(ctx)
	if err != nil || len(dbs) == 0 {
		t.Fatal(dbs, err)
	}
	p, err := cl.Privileges(ctx)
	if err != nil || !p.Superuser {
		t.Errorf("postgres must be a superuser: %+v %v", p, err)
	}
	if err := cl.WithDB(dbs[0]).SchemaOnlyDump(ctx); err != nil {
		t.Error("schema-only dump:", err)
	}
	if ok, why := cl.DetectPooler(ctx); ok {
		t.Error("not a pooler, yet reported:", why)
	}

	// Runner 2: client image on the container's network, loopback trust.
	img := ImageRunner{Shell: sh, Image: ImageForMajor(major), Network: "container:" + ct.Name}
	cl2 := &Client{Runner: img, Conn: Conn{User: ct.DefaultUser(), Database: "postgres"}}
	if _, m2, err := cl2.ServerVersionNum(ctx); err != nil || m2 != major {
		t.Error("network container:", m2, err)
	}

	// Runner 3: through the mapped port on the host network → needs a password.
	host := NewURLRunner(sh, ImageForMajor(major))
	cl3 := &Client{Runner: host, Conn: Conn{Host: "localhost", Port: ct.HostPorts[0], User: ct.DefaultUser(), Database: "postgres"}}
	err = cl3.Ping(ctx)
	if !IsAuthError(err) {
		t.Errorf("mapped port without password must be an authentication error, got: %v", err)
	}
	cl3.Conn.Password = ct.Env["POSTGRES_PASSWORD"]
	if err := cl3.Ping(ctx); err != nil {
		t.Error("mapped port with password:", err)
	}
}
