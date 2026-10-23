package backup

import (
	"fmt"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
)

// What a run checks beyond dumping (ADR-0019, ADR-0029): the age key that
// must open it, what depends on something outside Postgres, and whether a
// database lost most of its rows; the last two are warnings.

// keyMismatch says why age_identity_file cannot open what age_recipient
// encrypts; "" when it can.
func keyMismatch(cfg *config.App) string {
	_, recips, err := crypto.ReadIdentities(cfg.AgeIdentityFile)
	if err != nil {
		return fmt.Sprintf("age_identity_file %s is not readable (%v)", cfg.AgeIdentityFile, firstLine(err.Error()))
	}
	if !contains(recips, cfg.AgeRecipient) {
		return fmt.Sprintf("age_identity_file %s does not hold the private key for %s", cfg.AgeIdentityFile, cfg.AgeRecipient)
	}
	return ""
}

// accepted is accepted_external of database db of the instance.
func accepted(in config.Instance, db string) []string {
	for _, d := range in.Databases {
		if d.Name == db {
			return d.AcceptedExternal
		}
	}
	return nil
}

// needNames names what a restore of a database needs on the target server:
// library and text search files and collations, without version.
func needNames(n *pg.RestoreNeeds) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, f := range n.Libraries {
		out = append(out, "library "+f.Name)
	}
	for _, f := range n.TSFiles {
		out = append(out, "text search file "+f.Name)
	}
	for _, c := range n.Collations {
		if !c.Created {
			out = append(out, "collation "+c.Schema+"."+c.Name)
		}
	}
	return out
}

// A database that drops below dropShare of its rows in the last run that
// dumped it, from at least dropMinRows, needs a person (ADR-0019,
// ADR-0028): the run says so, nothing is held.
const (
	dropMinRows = 1000
	dropShare   = 2 // "less than half"
)

// totalRows is TotalRows without the estimate flag.
func totalRows(d *Dump) int64 {
	n, _ := TotalRows(d)
	return n
}

// TotalRows sums the rows of a dump: counted rows, else the estimate (est
// then true); tables of extensions and partitioned parents are left out.
func TotalRows(d *Dump) (n int64, est bool) {
	for _, t := range d.Tables {
		if t.Extension != "" || t.Kind == "p" {
			continue
		}
		switch {
		case t.Rows != nil:
			n += *t.Rows
		case t.RowsEst != nil && *t.RowsEst > 0:
			n += *t.RowsEst
			est = true
		}
	}
	return n, est
}

// checkRowDrops compares every database of this run with the last run that
// dumped it (walking back past runs that lack it, so a failed night in
// between hides nothing) and warns when it lost more than half of its rows
// (ADR-0028, ADR-0029); the run it names lives as long as the retention
// window keeps it.
func checkRowDrops(insts []config.Instance, run *Run, runs *runCache, res *Result) {
	for _, in := range insts {
		ri := run.Instances[in.Name]
		if ri == nil {
			continue
		}
		for _, name := range in.Databases.Names() {
			cur := ri.Databases[name]
			if cur == nil {
				continue
			}
			last, before := runs.lastDump(in.Name, name)
			if before == nil {
				continue
			}
			was, now := totalRows(before), totalRows(cur)
			if was < dropMinRows || now*dropShare >= was {
				continue
			}
			res.warn("%s/%s dropped from %d to %d rows since the run %s; if that was not on purpose, restore from it while retention keeps it", in.Name, name, was, now, last.TS)
		}
	}
}
