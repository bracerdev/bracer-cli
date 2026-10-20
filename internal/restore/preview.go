package restore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// runPreview restores data to look at only (--preview, --file): each
// database under <db>_preview_<ts>, without owners, privileges or users.
// CREATEDB is enough; nothing is guaranteed (ADR-0014, kept by ADR-0015).
func runPreview(ctx context.Context, cfg *config.App, o Options, d Deps) (*Result, error) {
	res := &Result{Kind: "restore", App: cfg.App, Mode: ModePreview, DryRun: o.DryRun, Databases: []*DBResult{}}
	var im *backup.InstanceManifest
	if o.File == "" {
		manifest, err := backup.LoadManifest(ctx, d.Store, storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json"), cfg.App)
		if err != nil {
			return nil, fmt.Errorf("load manifest: %w", err)
		}
		inst, err := ResolveInstance(manifest, cfg, o.Instance)
		if err != nil {
			return nil, err
		}
		im = manifest.Instances[inst]
		res.Source = Source{Instance: inst, TS: o.TS, PgVersion: im.PgVersion, Kind: im.Kind}
		dbs := o.Databases
		if len(dbs) == 0 {
			for name, dm := range im.Databases {
				if len(dm.Dumps) > 0 {
					dbs = append(dbs, name)
				}
			}
			sort.Strings(dbs)
		}
		if len(dbs) == 0 {
			return nil, usagef("no database backup for instance %s", inst)
		}
		for _, db := range dbs {
			sel, err := Select(manifest, cfg, inst, db, o.TS, false)
			if err != nil {
				return nil, err
			}
			dp := sel.Dump
			res.Databases = append(res.Databases, &DBResult{SourceDB: db, TS: dp.TS, Key: dp.Key, Bytes: dp.Bytes, Encrypted: dp.Encrypted, dump: dp})
			res.Source.Encrypted = res.Source.Encrypted || dp.Encrypted
			if dp.SHA256 == "" {
				res.warn("%s: manifest has no sha256 for this dump; its integrity cannot be checked", db)
			}
			if !dp.Encrypted && earlierEncrypted(manifest, sel) {
				res.warn("%s: this dump is not encrypted although an earlier one was; check that encryption was turned off on purpose", db)
			}
		}
	} else {
		if len(o.Databases) > 0 {
			return nil, usagef("--file restores the database in that file; leave out --database")
		}
		res.Source = Source{File: o.File}
		res.Databases = append(res.Databases, &DBResult{})
	}

	conn, runner, _, err := connectTarget(ctx, o, d, res)
	if err != nil {
		return nil, err
	}
	admin := &pg.Client{Runner: runner, Conn: conn}
	if res.Target.PgVersion, err = admin.ServerVersion(ctx); err != nil {
		return nil, fmt.Errorf("connect to target: %w", err)
	}
	tmajor := major(res.Target.PgVersion)
	ap, err := admin.AdminPrivileges(ctx)
	if err != nil {
		return nil, fmt.Errorf("check target user: %w", err)
	}
	res.Target.User, res.Target.Superuser = ap.Role, ap.Superuser
	if !ap.Superuser && !ap.CreateDB {
		return nil, usagef("user %s on %s cannot create databases; --preview needs CREATEDB or a superuser", ap.Role, targetName(res.Target))
	}
	rp := &RunPick{}
	for _, db := range res.Databases {
		if db.dump != nil {
			rp.DBs = append(rp.DBs, db.dump)
		}
	}
	if runner, err = pickRestoreRunner(ctx, d, o, rp, runner, conn, res); err != nil {
		return nil, err
	}
	admin = &pg.Client{Runner: runner, Conn: conn}
	for _, db := range res.Databases {
		if db.dump == nil {
			res.warn("source locale unknown (--file): the database is created with the server's default locale")
			continue
		}
		if loc := db.dump.Locale; loc != nil {
			if _, err := loc.CreateOptions(tmajor); err != nil {
				return nil, usagef("cannot create %s on %s: %v", db.SourceDB, targetName(res.Target), err)
			}
		}
		if len(db.dump.Extensions) == 0 {
			continue
		}
		avail, err := admin.AvailableExtensions(ctx)
		if err != nil {
			return nil, fmt.Errorf("check extensions on target: %w", err)
		}
		missing, differ := pg.MissingExtensions(db.dump.Extensions, avail)
		if len(missing) > 0 {
			return nil, usagef("target %s does not have extension %s that database %s uses; install it on the target first", targetName(res.Target), joinExtensions(missing), db.SourceDB)
		}
		for _, e := range differ {
			res.warn("target has extension %s %s, database %s used %s %s; pg_restore installs the target's version", e.Name, e.Version, db.SourceDB, e.Name, versionOf(db.dump.Extensions, e.Name))
		}
	}
	var dumpBytes int64
	for _, db := range res.Databases {
		dumpBytes += db.Bytes
	}
	if o.File != "" {
		if fi, err := os.Stat(o.File); err == nil {
			dumpBytes = fi.Size()
		}
	}
	dc := CheckDisk(ctx, runner, admin, dumpBytes)
	res.Disk = &dc
	if dc.Checked && !dc.Enough() {
		res.warn("only %s free at %s, about %s may be needed", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	}
	var ids *Identities
	if res.Source.Encrypted || o.File != "" {
		ids = identities(cfg, d)
	}
	for _, db := range res.Databases {
		info, err := ReadArchive(ctx, runner, conn, opener(ctx, d, o, db, ids))
		if err != nil {
			return res, fmt.Errorf("read %s: %w; nothing was created", sourceLabel(o, db), err)
		}
		db.info = info
		if db.SourceDB == "" {
			db.SourceDB = info.SourceDB
		}
		res.Warnings = append(res.Warnings, info.Warnings...)
	}
	pf := &Preflight{OK: true}
	if !ap.Superuser {
		trusted, err := TrustedExtensions(ctx, admin, tmajor)
		if err != nil {
			return res, fmt.Errorf("check extensions on target: %w", err)
		}
		for _, db := range res.Databases {
			for _, obj := range SuperuserObjects(db.info.Entries, trusted) {
				pf.NeedsSuperuser = append(pf.NeedsSuperuser, db.SourceDB+": "+obj)
			}
		}
		pf.OK = len(pf.NeedsSuperuser) == 0
	}
	res.Preflight = pf
	if !pf.OK {
		return res, ErrPreflight
	}
	exists := func(name string) (bool, error) { return DatabaseExists(ctx, admin, name) }
	for _, db := range res.Databases {
		if db.TargetDB, err = SideName(db.SourceDB, "preview", backupTime(db.TS, d.Now()), exists); err != nil {
			return res, err
		}
	}
	if o.DryRun {
		return res, nil
	}
	if d.Started != nil {
		d.Started(res)
	}
	res.OK = true
	for _, db := range res.Databases {
		restoreOne(ctx, d, o, runner, conn, admin, res, db, ids, tmajor)
		res.OK = res.OK && db.OK
	}
	return res, nil
}

// opener opens a database's source from the start (bucket or --file).
func opener(ctx context.Context, d Deps, o Options, db *DBResult, ids *Identities) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		if o.File != "" {
			rc, _, err := OpenFile(o.File, ids)
			return rc, err
		}
		return OpenDump(ctx, d.Store, db.dump, ids)
	}
}

// restoreOne creates one database, loads the data, applies its attributes and
// compares it with the manifest. Failures stay in db; the database is left in
// place for inspection.
func restoreOne(ctx context.Context, d Deps, o Options, runner pg.Runner, conn pg.Conn, admin *pg.Client, res *Result, db *DBResult, ids *Identities, tmajor int) {
	open := opener(ctx, d, o, db, ids)
	src, err := open()
	if err != nil {
		db.Error = err.Error() + "; nothing was created for it"
		return
	}
	defer src.Close()
	if o.File != "" {
		_, db.Encrypted, _ = fileEncrypted(o.File)
		res.Source.Encrypted = db.Encrypted
	}
	body := bufio.NewReader(src)
	if head, _ := body.Peek(len(pgdmpMagic)); !bytes.Equal(head, []byte(pgdmpMagic)) {
		db.Error = fmt.Sprintf("%s is not a pg_dump custom-format archive (expected it to start with %q); nothing was created", sourceLabel(o, db), pgdmpMagic)
		return
	}
	var loc *pg.Locale
	if db.dump != nil {
		loc = db.dump.Locale
	}
	intoExisting := false
	if !intoExisting {
		if err := CreateDatabase(ctx, admin, db.TargetDB, loc, tmajor); err != nil {
			db.Error = fmt.Sprintf("create database %s: %v", db.TargetDB, err)
			return
		}
	}
	leftInPlace := fmt.Sprintf("database %s was left in place for inspection; drop it with: DROP DATABASE %s;", db.TargetDB, pg.QuoteIdent(db.TargetDB))
	if intoExisting {
		leftInPlace = "database postgres holds what was restored so far"
	}
	var extra []string
	if res.Mode == ModePreview {
		extra = []string{"--no-owner", "--no-acl", "--no-publications", "--no-subscriptions", "--no-security-labels"}
	}
	start := d.Now()
	rerr := Database(ctx, runner, conn, db.TargetDB, extra, body, d.Timeout)
	db.RestoreSeconds = d.Now().Sub(start).Seconds()
	// A SHA-256 mismatch takes priority: for an encrypted dump, age reports the error before handing over
	// the last chunk, so pg_restore only sees a truncated stream ("could not read input file").
	if ce := checksumErr(src); ce != nil {
		rerr = ce
	}
	if rerr != nil {
		db.Error = rerr.Error()
		if ctx.Err() != nil {
			db.Error = "pg_restore interrupted"
		}
		db.Error += "\n" + leftInPlace
		return
	}
	d.Log.Info("restored", "db", db.TargetDB, "seconds", fmt.Sprintf("%.1f", db.RestoreSeconds))

	target := &pg.Client{Runner: runner, Conn: conn.WithDatabase(db.TargetDB)}
	if db.dump != nil {
		db.TablesExpected = len(db.dump.Tables)
		cmp, err := Compare(ctx, target, db.dump.Tables)
		if err != nil {
			db.Error = fmt.Sprintf("compare tables: %v", err)
			return
		}
		db.TablesFound, db.MissingTables, db.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
		db.OK = cmp.OK()
	} else {
		tables, err := target.Tables(ctx)
		if err != nil {
			db.Error = fmt.Sprintf("list tables: %v", err)
			return
		}
		db.TablesFound = len(tables)
		db.OK = true
	}
	// Attributes after the data (spec § Flow 8), against the users now on the target.
	names, err := ListRoles(ctx, admin)
	if err != nil {
		res.warn("%s: database attributes not applied: list users on target: %v", db.TargetDB, err)
		return
	}
	roles := map[string]bool{}
	for _, n := range names {
		roles[n] = true
	}
	attrs, err := ParseDatabaseSQL(db.info.AttrSQL, db.TargetDB, roles)
	if err != nil {
		res.warn("%s: database attributes not applied: %v", db.TargetDB, err)
		return
	}
	if res.Mode == ModePreview {
		attrs.SettingsOnly()
	} else {
		res.Warnings = append(res.Warnings, attrs.Warnings...)
	}
	ar, aw := attrs.Finish(ctx, runner, conn)
	db.Attributes = ar
	res.Warnings = append(res.Warnings, aw...)
	db.OK = db.OK && ar.OK()
}

func fileEncrypted(path string) (io.ReadCloser, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	head := make([]byte, len(ageHeader))
	n, _ := io.ReadFull(f, head)
	return nil, string(head[:n]) == ageHeader, nil
}

// sourceLabel names a database's source for error messages.
func sourceLabel(o Options, db *DBResult) string {
	if o.File != "" {
		return o.File
	}
	return db.Key
}

// earlierEncrypted: an older dump of the same database was encrypted while the chosen one is not.
func earlierEncrypted(m *backup.Manifest, sel *Selection) bool {
	im := m.Instances[sel.Instance]
	if im == nil || im.Databases[sel.Database] == nil {
		return false
	}
	for _, dp := range im.Databases[sel.Database].Dumps {
		if dp.Encrypted && dp.UploadedAt.Before(sel.Dump.UploadedAt) {
			return true
		}
	}
	return false
}

// joinExtensions: "postgis 3.4.2, pg_partman 5.0.1".
func joinExtensions(exts []pg.Extension) string {
	parts := make([]string, len(exts))
	for i, e := range exts {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}

// versionOf returns the version of extension name in the manifest list.
func versionOf(exts []pg.Extension, name string) string {
	for _, e := range exts {
		if e.Name == name {
			return e.Version
		}
	}
	return ""
}
