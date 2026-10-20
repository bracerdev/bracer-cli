package restore

import (
	"io"
	"strings"
	"testing"
)

// What pg_restore --create -n <none> prints for a database with an owner,
// settings, a comment and privileges (PostgreSQL 17 layout).
const shopAttrSQL = `--
-- PostgreSQL database dump
--

\restrict abc123

SET statement_timeout = 0;
SET client_encoding = 'UTF8';
SELECT pg_catalog.set_config('search_path', '', false);

--
-- Name: shop; Type: DATABASE; Schema: -; Owner: app
--

CREATE DATABASE shop WITH TEMPLATE = template0 ENCODING = 'UTF8' LOCALE_PROVIDER = libc LOCALE = 'en_US.utf8';


ALTER DATABASE shop OWNER TO app;

\connect shop

SET statement_timeout = 0;

--
-- Name: DATABASE shop; Type: COMMENT; Schema: -; Owner: app
--

COMMENT ON DATABASE shop IS 'Shop; production';


--
-- Name: shop; Type: DATABASE PROPERTIES; Schema: -; Owner: app
--

ALTER DATABASE shop IS_TEMPLATE = true;
ALTER DATABASE shop CONNECTION LIMIT = 50;
ALTER DATABASE shop SET "TimeZone" TO 'Asia/Ho_Chi_Minh';
ALTER DATABASE shop SET app.settings.jwt_secret TO 'it''s';
ALTER ROLE app IN DATABASE shop SET search_path TO '$user', 'public', 'app';
ALTER ROLE ghost IN DATABASE shop SET work_mem TO '64MB';


\connect shop

--
-- Name: DATABASE shop; Type: ACL; Schema: -; Owner: app
--

REVOKE ALL ON DATABASE shop FROM PUBLIC;
GRANT CONNECT ON DATABASE shop TO PUBLIC;
GRANT ALL ON DATABASE shop TO app;
GRANT CONNECT ON DATABASE shop TO ghost;
SECURITY LABEL FOR selinux ON DATABASE shop IS 'system_u:object_r:sepgsql_db_t:s0';

\unrestrict abc123
`

// spec fetch-restore § Flow 5b: keep only the database's attributes, rebuilt
// for the target name; drop what names a missing user, the template/connection
// switches, and security labels, each with a warning.
func TestParseDatabaseSQL(t *testing.T) {
	a, err := ParseDatabaseSQL([]byte(shopAttrSQL), "shop_restored", map[string]bool{"postgres": true, "app": true})
	if err != nil {
		t.Fatal(err)
	}
	want := `ALTER DATABASE "shop_restored" OWNER TO app
COMMENT ON DATABASE "shop_restored" IS 'Shop; production'
ALTER DATABASE "shop_restored" CONNECTION LIMIT = 50
ALTER DATABASE "shop_restored" SET "TimeZone" TO 'Asia/Ho_Chi_Minh'
ALTER DATABASE "shop_restored" SET app.settings.jwt_secret TO 'it''s'
ALTER ROLE app IN DATABASE "shop_restored" SET search_path TO '$user', 'public', 'app'
REVOKE ALL ON DATABASE "shop_restored" FROM PUBLIC
GRANT CONNECT ON DATABASE "shop_restored" TO PUBLIC
GRANT ALL ON DATABASE "shop_restored" TO app`
	if got := strings.Join(a.Statements, "\n"); got != want {
		t.Errorf("statements:\n%s\nwant:\n%s", got, want)
	}
	if a.SourceDB != "shop" || a.Owner != "app" || !a.ACL {
		t.Errorf("attributes: %+v", a)
	}
	if len(a.Settings) != 3 || a.Settings[1].Name != "app.settings.jwt_secret" || a.Settings[1].Value != "it's" ||
		a.Settings[2].Role != "app" || a.Settings[2].Value != "$user,public,app" {
		t.Errorf("settings: %+v", a.Settings)
	}
	warn := strings.Join(a.Warnings, "\n")
	for _, w := range []string{`not applied, ALTER DATABASE "shop_restored" IS_TEMPLATE = true:`, "ALTER ROLE ghost IN DATABASE", "user ghost does not exist", "GRANT CONNECT ON DATABASE", "SECURITY LABEL FOR selinux ON DATABASE shop IS …"} {
		if !strings.Contains(warn, w) {
			t.Errorf("warnings miss %q:\n%s", w, warn)
		}
	}
	if len(a.Warnings) != 4 {
		t.Errorf("want 4 warnings, got:\n%s", warn)
	}
}

// spec fetch-restore AC: the owner is missing on the target → no OWNER TO, a
// warning with Fix naming the target database.
func TestParseDatabaseSQLMissingOwner(t *testing.T) {
	a, err := ParseDatabaseSQL([]byte(shopAttrSQL), "shop_restored", map[string]bool{"postgres": true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Owner != "" || strings.Contains(strings.Join(a.Statements, "\n"), "OWNER TO") {
		t.Errorf("owner must be dropped: %+v", a.Statements)
	}
	warn := strings.Join(a.Warnings, "\n")
	if !strings.Contains(warn, "database owner app does not exist on the target") || !strings.Contains(warn, "schema public") ||
		!strings.Contains(warn, `Fix: create the user, then: ALTER DATABASE "shop_restored" OWNER TO "app";`) {
		t.Errorf("warnings:\n%s", warn)
	}
}

func TestParseDatabaseSQLNoCreate(t *testing.T) {
	if _, err := ParseDatabaseSQL([]byte("SET x = 1;\n"), "t", nil); err == nil {
		t.Error("an archive without CREATE DATABASE must be an error")
	}
	if _, err := ParseDatabaseSQL([]byte("CREATE DATABASE x"), "t", nil); err == nil {
		t.Error("an unterminated statement must be an error")
	}
}

// The privileges are replayed on acldefault: owner all three, PUBLIC CONNECT+TEMPORARY.
func TestExpectedACL(t *testing.T) {
	a, _ := ParseDatabaseSQL([]byte(shopAttrSQL), "s", map[string]bool{"app": true})
	got := privList(a.expectedACL("app"))
	if got != "PUBLIC: CONNECT; app: CONNECT, CREATE, TEMPORARY" {
		t.Errorf("acl = %s", got)
	}
}

func TestNormSetting(t *testing.T) {
	if normSetting(`"$user", public`) != normSetting("$user,public") {
		t.Error("list settings must compare equal")
	}
	if normSetting("Asia/Ho_Chi_Minh") == normSetting("UTC") {
		t.Error("different values must differ")
	}
}

func TestHeadReplays(t *testing.T) {
	head, all, err := Head(strings.NewReader("PGDMP-shop"))
	if err != nil || string(head) != "PGDMP-shop" {
		t.Fatalf("head %q, err %v", head, err)
	}
	var b strings.Builder
	if _, err := io.Copy(&b, all); err != nil || b.String() != "PGDMP-shop" {
		t.Errorf("replay %q, err %v", b.String(), err)
	}
}

// Security NFR: a warning about a dropped statement never shows its values.
func TestAttributeWarningsMaskValues(t *testing.T) {
	sql := "CREATE DATABASE shop;\nALTER ROLE ghost IN DATABASE shop SET app.jwt TO 'hunter2';\n"
	a, err := ParseDatabaseSQL([]byte(sql), "shop", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(a.Warnings, "\n"); strings.Contains(w, "hunter2") || !strings.Contains(w, "ghost") {
		t.Errorf("warnings: %s", w)
	}
}
