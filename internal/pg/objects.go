package pg

import (
	"context"
	"fmt"
	"strconv"
)

// notExtensionMember is a pg_depend test that the object of catalog cls with
// oid expression oid does not belong to an extension.
func notExtensionMember(cls, oid string) string {
	return fmt.Sprintf("not exists (select 1 from pg_depend d where d.classid = '%s'::regclass and d.objid = %s and d.deptype = 'e')", cls, oid)
}

// userObjectsSQL counts what makes a database not empty (ADR-0015): relations,
// functions, types and schemas that do not belong to an extension. The public
// schema and extension members do not count. A schema that a script created
// next to an extension (postgis image: topology, tiger) counts: the dump
// creates it again, which would fail.
var userObjectsSQL = "select " +
	"(select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind in ('r','p','v','m','S','f','c') and n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^pg_toast' and " + notExtensionMember("pg_class", "c.oid") +
	// a composite type's membership is recorded on its pg_type row
	" and (c.relkind <> 'c' or " + notExtensionMember("pg_type", "c.reltype") + "))" +
	" + (select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace where n.nspname not in ('pg_catalog','information_schema') and " + notExtensionMember("pg_proc", "p.oid") + ")" +
	" + (select count(*) from pg_type t join pg_namespace n on n.oid = t.typnamespace where n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^pg_toast' and t.typtype in ('b','d','e','r','m') and t.typcategory <> 'A' and " + notExtensionMember("pg_type", "t.oid") + ")" +
	" + (select count(*) from pg_namespace n where n.nspname not in ('public','information_schema') and n.nspname !~ '^pg_' and " + notExtensionMember("pg_namespace", "n.oid") + ")"

// UserObjects counts the objects of the connected database that are not part
// of an extension: 0 means the database is empty for a restore.
func (c *Client) UserObjects(ctx context.Context) (int, error) {
	s, err := c.scalar(ctx, userObjectsSQL)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("unexpected object count %q", s)
	}
	return n, nil
}

// outOfScopeSQL lists what depends on files or processes outside Postgres,
// which a dump does not carry (ADR-0015 § 2): functions in untrusted
// languages, C functions outside $libdir that no extension owns, file_fdw servers.
var outOfScopeSQL = "select 'function ' || p.oid::regprocedure::text || ' (language ' || l.lanname || ')' from pg_proc p join pg_language l on l.oid = p.prolang join pg_namespace n on n.oid = p.pronamespace " +
	"where not l.lanpltrusted and l.lanname not in ('c', 'internal') and n.nspname not in ('pg_catalog','information_schema') and " + notExtensionMember("pg_proc", "p.oid") +
	" union all select 'function ' || p.oid::regprocedure::text || ' (C library ' || p.probin || ')' from pg_proc p join pg_language l on l.oid = p.prolang " +
	"where l.lanname = 'c' and p.probin is not null and p.probin not like '$libdir%' and " + notExtensionMember("pg_proc", "p.oid") +
	" union all select 'foreign server ' || s.srvname || ' (file_fdw)' from pg_foreign_server s join pg_foreign_data_wrapper w on w.oid = s.srvfdw where w.fdwname = 'file_fdw'" +
	" order by 1"

// OutOfScope lists what a restore of the connected database cannot
// guarantee. Subscriptions are read apart: pg_subscription may be closed to
// the user, and then they are not listed.
func (c *Client) OutOfScope(ctx context.Context) ([]string, error) {
	rows, err := c.Query(ctx, outOfScopeSQL)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		out = append(out, r[0])
	}
	if subs, err := c.Query(ctx, "select 'subscription ' || subname from pg_subscription where subdbid = (select oid from pg_database where datname = current_database()) order by 1"); err == nil {
		for _, r := range subs {
			out = append(out, r[0])
		}
	}
	return out, nil
}
