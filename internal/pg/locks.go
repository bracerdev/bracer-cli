package pg

import (
	"context"
	"fmt"
	"strconv"
)

// LockObjects counts what a single-transaction load locks until it commits
// (spec restore § locks).
type LockObjects struct {
	Relations   int `json:"relations"`
	ForeignKeys int `json:"foreign_keys"`
}

// Locks estimates the lock table entries a pg_restore --single-transaction of
// the database holds. Measured on PG17: a table with a primary key and a text
// column takes ~2.6 locks per relation, a partition with a primary key ~3.5,
// and each foreign key ~3 more.
func (o *LockObjects) Locks() int {
	if o == nil {
		return 0
	}
	return (7*o.Relations+1)/2 + 3*o.ForeignKeys + 100
}

// LocksPer is the max_locks_per_transaction that gives room for locks over
// slots, rounded up to a multiple of 64.
func LocksPer(locks, slots int) int {
	if slots <= 0 {
		slots = 1
	}
	n := (locks + slots - 1) / slots
	return (n + 63) / 64 * 64
}

// lockObjectsSQL: relations created after initdb (user tables, their indexes,
// sequences, views, TOAST) and foreign keys.
const lockObjectsSQL = "select r.n, f.n from (select count(*) n from pg_class where oid >= 16384) r, (select count(*) n from pg_constraint where contype = 'f' and oid >= 16384) f"

// LockObjects counts the connected database's lock objects.
func (c *Client) LockObjects(ctx context.Context) (*LockObjects, error) {
	rows, err := c.Query(ctx, lockObjectsSQL)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("unexpected row %q", rows)
	}
	r, err1 := strconv.Atoi(rows[0][0])
	f, err2 := strconv.Atoi(rows[0][1])
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("unexpected row %q", rows[0])
	}
	return &LockObjects{Relations: r, ForeignKeys: f}, nil
}

// LockCapacity is the lock table size Postgres guarantees:
// max_locks_per_transaction × (MaxBackends + max_prepared_transactions).
type LockCapacity struct {
	PerTransaction int
	Slots          int
	Source         string // pg_settings.source of max_locks_per_transaction
}

// Capacity is PerTransaction × Slots.
func (l *LockCapacity) Capacity() int { return l.PerTransaction * l.Slots }

// lockSettingsSQL reads what sizes the lock table. autovacuum_worker_slots
// (PG18) replaces autovacuum_max_workers in MaxBackends.
const lockSettingsSQL = "select name, setting, source from pg_settings where name in ('max_locks_per_transaction', 'max_connections', 'autovacuum_max_workers', 'autovacuum_worker_slots', 'max_worker_processes', 'max_wal_senders', 'max_prepared_transactions')"

// LockCapacity reads the server's lock table size.
func (c *Client) LockCapacity(ctx context.Context) (*LockCapacity, error) {
	rows, err := c.Query(ctx, lockSettingsSQL)
	if err != nil {
		return nil, err
	}
	v := map[string]int{}
	l := &LockCapacity{}
	for _, r := range rows {
		if len(r) != 3 {
			return nil, fmt.Errorf("unexpected pg_settings row %q", r)
		}
		n, err := strconv.Atoi(r[1])
		if err != nil {
			return nil, fmt.Errorf("%s = %q: %w", r[0], r[1], err)
		}
		v[r[0]] = n
		if r[0] == "max_locks_per_transaction" {
			l.Source = r[2]
		}
	}
	if _, ok := v["max_locks_per_transaction"]; !ok {
		return nil, fmt.Errorf("max_locks_per_transaction is not readable")
	}
	av := v["autovacuum_max_workers"]
	if s, ok := v["autovacuum_worker_slots"]; ok {
		av = s
	}
	l.PerTransaction = v["max_locks_per_transaction"]
	l.Slots = v["max_connections"] + av + 1 + v["max_worker_processes"] + v["max_wal_senders"] + v["max_prepared_transactions"]
	return l, nil
}
