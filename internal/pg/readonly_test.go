package pg

import (
	"context"
	"io"
	"strings"
	"testing"
)

// readLog records every psql -c statement and answers the few that the
// readers need to go on; anything else returns no rows.
type readLog struct{ sql []string }

func (r *readLog) Run(_ context.Context, _ Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	if tool != "psql" || stdin != nil {
		r.sql = append(r.sql, tool+" "+strings.Join(args, " "))
		return nil
	}
	sql := args[len(args)-1]
	r.sql = append(r.sql, sql)
	switch {
	case sql == "show server_version_num":
		io.WriteString(stdout, "170006\x00")
	case sql == "show server_version":
		io.WriteString(stdout, "17.6\x00")
	case strings.HasPrefix(sql, "select (select rolname from pg_roles where oid = 10)"):
		io.WriteString(stdout, "postgres\x1fpostgres\x1ft\x1foff\x1f100\x1f\x00")
	case strings.HasPrefix(sql, "select current_user, rolsuper"):
		io.WriteString(stdout, "postgres\x1ft\x1ft\x1ft\x00")
	}
	return nil
}
func (r *readLog) Describe() string { return "read log" }

// BR-011: what the CLI asks the source server is reads only; it never writes
// to a customer's database.
func TestBR011SourceReadsOnly(t *testing.T) {
	ctx := context.Background()
	log := &readLog{}
	c := &Client{Runner: log, Conn: Conn{Database: "postgres"}}
	if _, _, err := CollectEnv(ctx, c, EnvSource{Kind: "url"}, []string{"shop"}); err != nil {
		t.Fatal(err)
	}
	c.Ping(ctx)
	c.SystemIdentifier(ctx)
	c.ListCandidates(ctx)
	TemplateInUse(ctx, c)
	c.Privileges(ctx)
	c.BootstrapSuperuser(ctx)
	c.AdminPrivileges(ctx)
	c.RLSTables(ctx)
	c.DetectPooler(ctx)
	c.PreloadLibraries(ctx)
	c.Tables(ctx)
	c.Extensions(ctx)
	c.AvailableExtensions(ctx)
	c.AutoConfSettings(ctx)
	c.DatabaseLocale(ctx, "shop", 17)
	c.RestoreNeeds(ctx, 17)
	c.LockObjects(ctx)
	c.LockCapacity(ctx)
	c.UserObjects(ctx)
	c.External(ctx)
	if len(log.sql) < 20 {
		t.Fatalf("only %d statements: %v", len(log.sql), log.sql)
	}
	writes := []string{"insert", "update", "delete", "grant", "revoke", "alter", "create", "drop", "truncate", "comment", "copy", "vacuum", "reindex", "cluster", "set "}
	for _, s := range log.sql {
		low := strings.ToLower(strings.TrimSpace(s))
		for _, w := range writes {
			if strings.HasPrefix(low, w) {
				t.Errorf("writes to the source: %s", s)
			}
		}
	}
}
