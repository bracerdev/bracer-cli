package pg

import (
	"strings"
	"testing"
)

func TestImageForExtensions(t *testing.T) {
	cases := []struct {
		exts []Extension
		want string
	}{
		{nil, "postgres:16-alpine"},
		{[]Extension{{"uuid-ossp", "1.1"}, {"pg_trgm", "1.6"}}, "postgres:16-alpine"},
		{[]Extension{{"postgis", "3.4.2"}}, "postgis/postgis:16-3.4"},
		{[]Extension{{"postgis", "3.4.2"}, {"postgis_topology", "3.4.2"}, {"uuid-ossp", "1.1"}}, "postgis/postgis:16-3.4"},
		{[]Extension{{"timescaledb", "2.17.2"}}, "timescale/timescaledb:2.17.2-pg16"},
		{[]Extension{{"vector", "0.8.0"}}, "pgvector/pgvector:pg16"},
		{[]Extension{{"pg_partman", "5.0.1"}}, "postgres:16-alpine"}, // outside the table: let pg_available_extensions decide
	}
	for _, c := range cases {
		got, err := ImageForExtensions(16, c.exts)
		if err != nil || got != c.want {
			t.Errorf("%v: got %q, %v; want %q", c.exts, got, err, c.want)
		}
	}
	if _, err := ImageForExtensions(16, []Extension{{"postgis", "3.4.2"}, {"timescaledb", "2.17.2"}}); err == nil || !strings.Contains(err.Error(), "both postgis and timescaledb") || !strings.Contains(err.Error(), "drill.image") {
		t.Errorf("two images: %v", err)
	}
}

func TestMissingExtensions(t *testing.T) {
	need := []Extension{{"postgis", "3.4.2"}, {"uuid-ossp", "1.1"}, {"pg_partman", "5.0.1"}}
	avail := map[string]string{"postgis": "3.5.0", "uuid-ossp": "1.1", "plpgsql": "1.0"}
	missing, differ := MissingExtensions(need, avail)
	if len(missing) != 1 || missing[0].Name != "pg_partman" {
		t.Errorf("missing = %v", missing)
	}
	if len(differ) != 1 || differ[0] != (Extension{"postgis", "3.5.0"}) {
		t.Errorf("differ = %v", differ)
	}
}
