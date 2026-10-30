package restore

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/pg"
)

const seedTS = "2026-10-16T030000Z"

func TestSideName(t *testing.T) {
	at := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	taken := map[string]bool{"shop_restored_20261016_0300Z": true}
	name, err := SideName("shop", "restored", at, func(n string) (bool, error) { return taken[n], nil })
	if err != nil || name != "shop_restored_20261016_0300Z_2" {
		t.Errorf("name = %q, %v", name, err)
	}
	long := strings.Repeat("é", 40) // 80 bytes
	name, _ = SideName(long, "restored", at, func(string) (bool, error) { return false, nil })
	if len(name) > 63 || !strings.HasSuffix(name, "_restored_20261016_0300Z") || !bytes.HasPrefix([]byte(name), []byte("éé")) || strings.ContainsRune(name, '�') {
		t.Errorf("long name = %q (%d bytes)", name, len(name))
	}
}

// The command the customer applies a file with, by target kind. A socket
// that asked for a password (Bitnami) takes it from PGPASSWORD in their shell.
func TestApplyCommand(t *testing.T) {
	conn := pg.Conn{User: "postgres"}
	if got := applyCommand(TargetSpec{Kind: "container", Container: "db", Password: true}, conn, 17); !strings.HasPrefix(got, "docker exec -i -e PGPASSWORD db psql -U postgres") {
		t.Errorf("container: %s", got)
	}
	if got := applyCommand(TargetSpec{Kind: "container", Container: "db"}, conn, 17); !strings.HasPrefix(got, "docker exec -i db psql -U postgres -d postgres -v ON_ERROR_STOP=1 < ") {
		t.Errorf("container without password: %s", got)
	}
	local := &pg.Local{Dir: "/tmp", Port: 5433}
	if got := applyCommand(TargetSpec{Kind: "local", Local: local, Password: true}, conn, 17); !strings.HasPrefix(got, "sudo -u postgres --preserve-env=PGPASSWORD psql -h /tmp -p 5433 -d postgres") {
		t.Errorf("local: %s", got)
	}
	if got := applyCommand(TargetSpec{Kind: "url"}, conn, 17); !strings.Contains(got, "-e PGURL "+pg.ImageForMajor(17)+" sh -c") || strings.Contains(got, "postgres://") {
		t.Errorf("url: %s", got)
	}
}
