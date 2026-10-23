package pg

import "testing"

func TestLocks(t *testing.T) {
	// 1600 tables with a serial primary key and a text column: 8000 relations.
	o := &LockObjects{Relations: 8000}
	if got := o.Locks(); got != 28100 {
		t.Errorf("locks: %d", got)
	}
	if got := (&LockObjects{Relations: 101, ForeignKeys: 100}).Locks(); got != 754 {
		t.Errorf("with foreign keys: %d", got)
	}
	if (*LockObjects)(nil).Locks() != 0 {
		t.Error("nil needs none")
	}
	// PG17 defaults: 100 + 3 + 1 + 8 + 10 + 0 slots.
	if got := LocksPer(28100, 122); got != 256 {
		t.Errorf("per transaction: %d", got)
	}
	if got := LocksPer(100, 122); got != 64 {
		t.Errorf("small: %d", got)
	}
}
