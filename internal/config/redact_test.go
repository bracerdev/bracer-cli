package config

import (
	"testing"

	"bracer.dev/bracer/internal/hostedfilter"
)

// br014Case is one text and what Redact makes of it.
type br014Case struct{ name, in, want string }

// checkRedact runs cases through Redact with secrets: each gives want, is
// stable when redacted again, and is never refused by the hosted server.
func checkRedact(t *testing.T, s *Secrets, cases []br014Case) {
	t.Helper()
	for _, c := range cases {
		got := s.Redact(c.in)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if twice := s.Redact(got); twice != got {
			t.Errorf("%s: not idempotent: %q then %q", c.name, got, twice)
		}
		if hostedfilter.Refuses(got) {
			t.Errorf("%s: the server would refuse %q", c.name, got)
		}
	}
}

// BR-014: the credential shapes the BR-008 patterns missed are redacted
// with no tail of a password left, every key shape the server refuses is
// caught, and a URL whose @ comes after a / is not userinfo.
func TestBR014Redact(t *testing.T) {
	checkRedact(t, nil, []br014Case{
		{"quoted with blanks", "host=h password='a b c' dbname=x", "host=h password=*** dbname=x"},
		{"double quoted", `password="a b" x`, "password=*** x"},
		{"blanks around =", "host=h password = hunter2 port=5432", "host=h password = *** port=5432"},
		{"PGPASSWORD", "env PGPASSWORD=hunter2 psql", "env PGPASSWORD=*** psql"},
		{"docker -e", "docker run -e PGPASSWORD=hunter2", "docker run -e PGPASSWORD=***"},
		{"POSTGRES_PASSWORD", "Env: POSTGRES_PASSWORD=hunter2 POSTGRES_DB=shop", "Env: POSTGRES_PASSWORD=*** POSTGRES_DB=shop"},
		{"DB_PASSWORD", "DB_PASSWORD=hunter2", "DB_PASSWORD=***"},
		{"APP_PASSWD", "x;APP_PASSWD=hunter2", "x;APP_PASSWD=***"},
		{"MYSQL_PWD", "MYSQL_PWD=hunter2 mysql", "MYSQL_PWD=*** mysql"},
		{"pwd", "?pwd=hunter2", "?pwd=***"},
		{"oldpassword", "&oldpassword=hunter2", "&oldpassword=***"},
		{"& and # in value", "?password=ab&cd#ef", "?password=***"},
		{"quote in value", "password=it's-secret now", "password=*** now"},
		{"SQL E string", `ALTER ROLE app PASSWORD E'sec\'ret' VALID`, "ALTER ROLE app PASSWORD *** VALID"},
		{"SQL dollar", "CREATE ROLE app PASSWORD $$secret$$ LOGIN", "CREATE ROLE app PASSWORD *** LOGIN"},
		{"SQL dollar tag", "PASSWORD $pw$secret$pw$;", "PASSWORD ***;"},
		{"SQL doubled quote", "PASSWORD 'it''s-secret' LOGIN", "PASSWORD *** LOGIN"},
		{"SQL cut short", "LINE 1: ALTER ROLE app PASSWORD 'SCRAM-SHA-256$4096:ab\nHINT: x", "LINE 1: ALTER ROLE app PASSWORD ***\nHINT: x"},
		{"URL password with @", "postgres://u:p@ss@h/db", "postgres://***@h/db"},
		{"URL password with ?", "postgres://u:pa?ss@h/db", "postgres://***@h/db"},
		{"URL password with #", "postgres://u:pa#ss@h:5432/db", "postgres://***@h:5432/db"},
		{"https userinfo", "GET https://key:secret@minio:9000/b", "GET https://***@minio:9000/b"},
		{"port then @ in path", "PUT https://minio.local:9000/b/key/a@b.dump failed", "PUT https://minio.local:9000/b/key/a@b.dump failed"},
		{"image digest", "pull docker://registry:5000/pg@sha256:0123abcd", "pull docker://registry:5000/pg@sha256:0123abcd"},
		{"slash before @", "https://AKID:12/secretpart@minio/b", "https://AKID:12/secretpart@minio/b"},
		{"user and port, no password", "postgres://app@db:5432/shop", "postgres://app@db:5432/shop"},
		{"aws key", "key AKIAIOSFODNN7EXAMPLE", "key ***"},
		{"aws session key", "key ASIAIOSFODNN7EXAMPLE", "key ***"},
		{"age key", "AGE-SECRET-KEY-1QYQSZQGPQYQSZQ is wrong", "*** is wrong"},
		// What must stay readable.
		{"auth failed", `FATAL: password authentication failed for user "app"`, `FATAL: password authentication failed for user "app"`},
		{"sslmode", "postgres://app@db/shop?sslmode=require", "postgres://app@db/shop?sslmode=require"},
		{"accepted parameters", "unsupported URL parameter (accepted: host, port, user, password, dbname, sslmode)", "unsupported URL parameter (accepted: host, port, user, password, dbname, sslmode)"},
		{"plain", "pg_dump: error: connection refused\nFix: start it", "pg_dump: error: connection refused\nFix: start it"},
	})
}

// BR-014: no dump content leaves the machine: the row values Postgres
// quotes in a message are masked, the rest of the line stays.
func TestBR014RedactRowValues(t *testing.T) {
	checkRedact(t, nil, []br014Case{
		{"unique violation", "ERROR:  duplicate key value violates unique constraint \"users_email_key\"\nDETAIL:  Key (email)=(ann@example.com) already exists.",
			"ERROR:  duplicate key value violates unique constraint \"users_email_key\"\nDETAIL:  Key (email)=(***) already exists."},
		{"expression key", "DETAIL:  Key (lower(email::text))=(ann@example.com) already exists.", "DETAIL:  Key (lower(email::text))=(***) already exists."},
		{"two columns", "DETAIL:  Key (tenant, email)=(7, ann@example.com) already exists.", "DETAIL:  Key (tenant, email)=(***) already exists."},
		{"foreign key", "DETAIL:  Key (user_id)=(42) is not present in table \"users\".", "DETAIL:  Key (user_id)=(***) is not present in table \"users\"."},
		{"exclusion", "DETAIL:  Key (room, during)=(1, [10,12)) conflicts with existing key (room, during)=(1, [11,13)).",
			"DETAIL:  Key (room, during)=(***) conflicts with existing key (room, during)=(***)."},
		{"not-null violation", "ERROR:  null value in column \"email\" of relation \"users\" violates not-null constraint\nDETAIL:  Failing row contains (5, Ann, null, secret-note).",
			"ERROR:  null value in column \"email\" of relation \"users\" violates not-null constraint\nDETAIL:  Failing row contains (***)."},
		{"check violation", "DETAIL:  Failing row contains (5, -3).\nFix: x", "DETAIL:  Failing row contains (***).\nFix: x"},
		{"COPY line", "CONTEXT:  COPY users, line 5: \"5\tAnn\tann@example.com\"", "CONTEXT:  COPY users, line 5: ***"},
		{"COPY column", "ERROR:  invalid input syntax for type integer: \"x\"\nCONTEXT:  COPY users, line 5, column age: \"abc\"\npg_restore: error",
			"ERROR:  invalid input syntax for type integer: \"x\"\nCONTEXT:  COPY users, line 5, column age: ***\npg_restore: error"},
	})
}

// BR-014: the values of secrets_file go first, then the patterns, so a URL
// whose password is a secret value reads postgres://***@; a value shorter
// than 4 characters stays.
func TestBR014RedactValuesThenPatterns(t *testing.T) {
	s := NewSecrets(map[string]string{"PW": "hunter2-long", "SHORT": "abc"}, "test")
	checkRedact(t, s, []br014Case{{"value in URL", "postgres://app:hunter2-long@db/shop abc hunter2-long", "postgres://***@db/shop abc ***"}})
}
