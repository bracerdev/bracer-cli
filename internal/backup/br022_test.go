package backup

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// BR-022: pg_dumpall --globals-only runs in a pinned session, so a role's
// VALID UNTIL is written in ISO and UTC whatever the server's DateStyle
// and TimeZone; restore reads the target the same way. Only that command:
// pg_dump and psql keep the session the server gives them.
func TestBR022GlobalsDumpPinsDateStyleAndTimeZone(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "PGDMP-shop"}}
	res, err := Backup(context.Background(), baseCfg(r.dbs...), deps(st, r))
	if err != nil || !res.OK {
		t.Fatalf("backup: %v %+v", err, res)
	}
	for _, kv := range []string{"PGDATESTYLE=ISO", "PGTZ=UTC"} {
		if !slices.Contains(r.globalsEnv, kv) {
			t.Errorf("pg_dumpall env lacks %s: %v", kv, r.globalsEnv)
		}
	}
	var dumps int
	for call, env := range r.otherEnvs {
		if strings.HasPrefix(call, "pg_dump ") {
			dumps++
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "PGDATESTYLE=") || strings.HasPrefix(kv, "PGTZ=") {
				t.Errorf("%s runs with %s", call, kv)
			}
		}
	}
	if dumps == 0 || len(r.otherEnvs) < 2 {
		t.Fatalf("the fake saw no pg_dump or psql: %v", r.otherEnvs)
	}
}
