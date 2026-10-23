package restore

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
)

// Restore modes (ADR-0015).
const (
	ModeFull = "full" // container or local source: users, settings, data
	ModeData = "data" // URL source: databases only, owned by the connecting user
)

// Fix kinds of a check that is not ok.
const (
	FixSQL    = "sql"    // env-up has it
	FixManual = "manual" // outside SQL: image, package, configuration, restart
)

// Check is one item compared between the backup run and the target.
type Check struct {
	Area   string `json:"area"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"` // ok, but worth knowing
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	Manual string `json:"manual,omitempty"`
}

// SourceRun is one backup run: everything restore reads from the bucket.
type SourceRun struct {
	Instance string
	TS       string
	Kind     string // container | local | url
	Env      *pg.Env
	Globals  *RoleGlobals // nil for a URL source
	DBs      []*SourceDB
}

// SourceDB is one database of the run with its archive's table of contents.
type SourceDB struct {
	Name    string
	Dump    *backup.Dump
	AttrSQL []byte
	Entries []TOCEntry
}

// RLS reports whether a table of the archive has row-level security.
func (s *SourceDB) RLS() bool {
	for _, e := range s.Entries {
		if e.Type == "ROW SECURITY" {
			return true
		}
	}
	return false
}

// DBPlan is where one source database goes: always a new database, which
// the load creates; never one the target already has (ADR-0022).
type DBPlan struct {
	Source string
	Target string
	// Create makes Target with the source's locale, owner and attributes; it
	// runs in the maintenance database right before the load.
	Create []string
}

// Plan is the comparison and the SQL that closes the gaps.
type Plan struct {
	Mode    string
	Checks  []Check
	Up      []string // env-up, in run order
	Down    []string // env-down, in run order
	Restart bool     // env-up has a setting that needs a server restart
	// RestartCmd restarts this target, for the customer to run.
	RestartCmd string
	DBs        []*DBPlan
	Notes      []string
}

// OK when nothing blocks the load.
func (p *Plan) OK() bool {
	for _, c := range p.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Failed lists the checks that are not ok.
func (p *Plan) Failed() []Check {
	var out []Check
	for _, c := range p.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// PlanInput is what Plan compares.
type PlanInput struct {
	Src  *SourceRun
	Tgt  *TargetState
	Mode string
	// Loaded maps a source database to the target database it was loaded
	// into: the check after the load, where those databases hold data.
	Loaded map[string]string
	// Now names a side database when no run ts can be read.
	Now time.Time
	// OwnUser is a user the caller created on the target itself (drill's
	// superuser): its password is the caller's, not worth a note.
	OwnUser string
}

// planner accumulates the checks and the SQL in the groups env-up runs in.
type planner struct {
	in   PlanInput
	p    *Plan
	up   [nGroups][]string
	down [nGroups][]string
	tgt  *TargetState
	src  *SourceRun
}

// SQL groups, in the order env-up runs them; env-down runs them backwards.
// env-up holds only what is server-wide: the new databases are the load's.
const (
	gCreateRole = iota
	gAlterRole
	gGrant
	gRoleSet
	gParam
	gSystem
	gReload
	nGroups
)

func (pl *planner) add(c Check) { pl.p.Checks = append(pl.p.Checks, c) }

func (pl *planner) ok(area, name, detail string) {
	pl.add(Check{Area: area, Name: name, OK: true, Detail: detail})
}

func (pl *planner) warn(area, name, detail string) {
	pl.add(Check{Area: area, Name: name, OK: true, Warn: true, Detail: detail})
}

func (pl *planner) manual(area, name, detail, fix string) {
	pl.add(Check{Area: area, Name: name, Detail: detail, Fix: FixManual, Manual: fix})
}

func (pl *planner) sql(area, name, detail string) {
	pl.add(Check{Area: area, Name: name, Detail: detail, Fix: FixSQL})
}

// MakePlan compares the run with the target (ADR-0015 § 3). It reads nothing:
// everything comes from in.
func MakePlan(in PlanInput) *Plan {
	pl := &planner{in: in, p: &Plan{Mode: in.Mode}, tgt: in.Tgt, src: in.Src}
	pl.p.RestartCmd = pl.restartCmd()
	pl.version()
	pl.databases()
	pl.extensions()
	pl.preload()
	pl.locales()
	pl.needs()
	pl.locks()
	if in.Mode == ModeFull {
		pl.serverSettings()
		pl.ssl()
		pl.tablespaces()
		pl.roles()
	}
	pl.dbAttributes()
	if in.Mode == ModeData {
		pl.dataSettings()
	}
	pl.external()
	if pl.p.Restart {
		pl.up[gReload] = nil
	} else if len(pl.up[gSystem]) > 0 {
		pl.up[gReload] = []string{"SELECT pg_reload_conf();"}
	}
	for g := 0; g < len(pl.up); g++ {
		pl.p.Up = append(pl.p.Up, pl.up[g]...)
	}
	for g := len(pl.down) - 1; g >= 0; g-- {
		pl.p.Down = append(pl.p.Down, pl.down[g]...)
	}
	// env-down resets the settings first; the reload must follow them.
	if len(pl.down[gSystem]) > 0 && !pl.p.Restart {
		pl.p.Down = append(pl.p.Down, "SELECT pg_reload_conf();")
	}
	return pl.p
}

func (pl *planner) targetMajor() int { return pl.tgt.Major }
func (pl *planner) sourceMajor() int { return pl.src.Env.ServerVersionNum / 10000 }

// imageHint names what to change for the target kind.
func (pl *planner) rebuildHint(what string) string {
	switch pl.tgt.Kind {
	case "container":
		return fmt.Sprintf("use an image with %s for container %s (image: in the compose file), recreate it", what, pl.tgt.Container)
	case "local":
		return fmt.Sprintf("install %s on this machine", what)
	}
	return fmt.Sprintf("use a server with %s", what)
}

func (pl *planner) restartCmd() string {
	switch pl.tgt.Kind {
	case "container":
		return "docker restart " + pl.tgt.Container
	case "local":
		return pg.RestartLocal(pl.tgt.Env)
	}
	return "restart the server (your provider's console)"
}

func (pl *planner) version() {
	sm, tm := pl.sourceMajor(), pl.targetMajor()
	name := fmt.Sprintf("PostgreSQL %d", sm)
	if tm < sm {
		pl.manual("version", name, fmt.Sprintf("target is PostgreSQL %s, the backup is from %s", pl.tgt.Env.ServerVersion, pl.src.Env.ServerVersion),
			pl.rebuildHint(fmt.Sprintf("PostgreSQL %d or newer", sm)))
		return
	}
	pl.ok("version", name, "target is "+pl.tgt.Env.ServerVersion)
}

// databases names the new database each source database loads into:
// <db>_restored_<ts>, then _2, _3… while the name is taken. The target's own
// databases are never touched (ADR-0022).
func (pl *planner) databases() {
	if pl.in.Mode == ModeData && !pl.tgt.Env.Superuser && !pl.tgt.CreateDB {
		u := pl.tgt.Env.CurrentUser
		pl.manual("database", "CREATEDB", u+" needs CREATEDB to create the database the restore loads into",
			fmt.Sprintf("ask an admin of the server: ALTER ROLE %s CREATEDB", pg.QuoteIdent(u)))
	}
	at := backupTime(pl.src.TS, pl.in.Now)
	names := map[string]bool{}
	for n := range pl.tgt.Names {
		names[n] = true
	}
	taken := func(n string) (bool, error) { return names[n], nil }
	for _, sdb := range pl.src.DBs {
		dp := &DBPlan{Source: sdb.Name}
		pl.p.DBs = append(pl.p.DBs, dp)
		if t, ok := pl.in.Loaded[sdb.Name]; ok {
			dp.Target = t
			continue
		}
		name, err := SideName(sdb.Name, "restored", at, taken)
		if err != nil {
			pl.manual("database", sdb.Name, err.Error(), "drop the old "+sdb.Name+"_restored_* databases")
			continue
		}
		names[name] = true
		dp.Target = name
		pl.createDB(sdb, dp)
		pl.ok("database", sdb.Name, "loads into a new database "+name)
	}
}

// createDB starts dp.Create: CREATE DATABASE with the source's locale and
// owner (data mode: the connecting user).
func (pl *planner) createDB(sdb *SourceDB, dp *DBPlan) {
	stmt := "CREATE DATABASE " + pg.QuoteIdent(dp.Target) + " TEMPLATE template0"
	if loc := sdb.Dump.Locale; loc != nil {
		if opts, err := loc.CreateOptions(pl.targetMajor()); err == nil {
			stmt += opts
		}
	}
	owner := pl.tgt.Env.CurrentUser
	if pl.in.Mode == ModeFull {
		if a := pl.attrs(sdb, dp.Target); a != nil && a.Owner != "" {
			owner = a.Owner
		}
	}
	dp.Create = append(dp.Create, stmt+" OWNER "+pg.QuoteIdent(owner)+";")
}

// DropDB drops a database restore created; FORCE ends sessions in it (PG13+).
func DropDB(name string, major int) string {
	stmt := "DROP DATABASE IF EXISTS " + pg.QuoteIdent(name)
	if major >= 13 {
		stmt += " WITH (FORCE)"
	}
	return stmt + ";"
}

// extensions: every extension the backup uses is available in the same or a
// newer version; the dump creates it in the new database.
func (pl *planner) extensions() {
	for _, sdb := range pl.src.DBs {
		for _, e := range sdb.Dump.Extensions {
			name := sdb.Name + ": " + e.Name + " " + e.Version
			av, ok := pl.tgt.Avail[e.Name]
			switch {
			case !ok:
				pl.manual("extension", name, "the target server does not have it", pl.extensionHint(e))
			case pl.in.Mode == ModeData && !pl.tgt.Env.Superuser && !av.Trusted:
				pl.manual("extension", name, fmt.Sprintf("%s is not trusted and %s is not a superuser", e.Name, pl.tgt.Env.CurrentUser),
					fmt.Sprintf("ask your provider's admin to let %s create extension %s", pl.tgt.Env.CurrentUser, e.Name))
			case e.Config:
				pl.configExtension(name, e, av)
			case av.Default != "" && cmpVersion(av.Default, e.Version) < 0:
				pl.manual("extension", name, fmt.Sprintf("the target server has %s %s, older than the backup", e.Name, av.Default), pl.extensionHint(e))
			default:
				pl.ok("extension", name, "available: "+av.Default)
			}
		}
	}
}

// configExtension: an extension with configuration tables must be installed
// at exactly the backup's version, since the dump holds those tables' rows as
// of it. The dump's CREATE EXTENSION installs the target's default, so the
// default must be that version (spec restore § extensions).
func (pl *planner) configExtension(name string, e pg.Extension, av ExtAvail) {
	if av.Default == e.Version {
		pl.ok("extension", name, "available: "+av.Default)
		return
	}
	detail := fmt.Sprintf("the target installs %s %s by default; the dump holds its configuration tables as of %s", e.Name, av.Default, e.Version)
	if c := pl.src.Env.Container; c != nil && c.Image != "" && pl.tgt.Kind == "container" {
		pl.manual("extension", name, detail, fmt.Sprintf("use image %s (the source's) for container %s, recreate it, restore, then run ALTER EXTENSION %s UPDATE in each database that has it",
			c.Image, pl.tgt.Container, pg.QuoteIdent(e.Name)))
		return
	}
	pl.manual("extension", name, detail, pl.rebuildHint(fmt.Sprintf("extension %s %s as the default version", e.Name, e.Version))+
		fmt.Sprintf("; after the restore, ALTER EXTENSION %s UPDATE updates it", pg.QuoteIdent(e.Name)))
}

func (pl *planner) extensionHint(e pg.Extension) string {
	if pl.tgt.Kind == "local" && pl.src.Env.Local != nil {
		var pkgs []string
		for _, p := range pl.src.Env.Local.Packages {
			if p.Extension == e.Name {
				pkgs = append(pkgs, retargetPackage(p.Package, pl.sourceMajor(), pl.targetMajor()))
			}
		}
		if len(pkgs) > 0 {
			return "sudo apt install " + strings.Join(pkgs, " ")
		}
	}
	return pl.rebuildHint(fmt.Sprintf("extension %s %s or newer", e.Name, e.Version))
}

// retargetPackage swaps the major in a Debian package name (postgresql-17-pgvector).
func retargetPackage(pkg string, from, to int) string {
	return strings.Replace(pkg, fmt.Sprintf("postgresql-%d-", from), fmt.Sprintf("postgresql-%d-", to), 1)
}

// preload: the libraries the extensions need at server start are loaded.
func (pl *planner) preload() {
	src := pl.src.Env.PreloadLibraries
	if src == nil {
		pl.warn("preload", "shared_preload_libraries", "the backup could not read it; not checked")
		return
	}
	var exts []pg.Extension
	for _, sdb := range pl.src.DBs {
		exts = append(exts, sdb.Dump.Extensions...)
	}
	needed, _ := pg.PreloadNeeds(*src, exts)
	if pl.tgt.Env.PreloadLibraries != nil {
		need, have := map[string]bool{}, map[string]bool{}
		for _, l := range needed {
			need[l] = true
		}
		for _, l := range pg.SplitLibraries(*pl.tgt.Env.PreloadLibraries) {
			have[l] = true
		}
		var other []string
		for _, l := range pg.SplitLibraries(*src) {
			if !need[l] && !have[l] {
				other = append(other, l)
			}
		}
		if len(other) > 0 {
			pl.p.Notes = append(pl.p.Notes, fmt.Sprintf("the source also preloads %s, which no database's extension needs (statistics, logging, auditing); restore does not load it: add it to shared_preload_libraries on the target if you rely on it", strings.Join(other, ", ")))
		}
	}
	if len(needed) == 0 {
		return
	}
	if pl.tgt.Env.PreloadLibraries == nil {
		pl.manual("preload", strings.Join(needed, ","), "cannot read shared_preload_libraries on the target", "connect as a superuser or a member of pg_read_all_settings")
		return
	}
	have := map[string]bool{}
	cur := pg.SplitLibraries(*pl.tgt.Env.PreloadLibraries)
	for _, l := range cur {
		have[l] = true
	}
	var missing []string
	for _, l := range needed {
		if have[l] {
			pl.ok("preload", l, "loaded")
		} else {
			missing = append(missing, l)
		}
	}
	if len(missing) == 0 {
		return
	}
	var absent []string
	for _, l := range missing {
		if pl.tgt.PkgLib == nil || !pl.tgt.PkgLib[l+".so"] {
			absent = append(absent, l)
		}
	}
	if len(absent) > 0 || pl.tgt.PkgLib == nil {
		pl.manual("preload", strings.Join(missing, ","), "the target server does not load it", pl.rebuildHint("library "+strings.Join(missing, ", ")+" loaded at start (shared_preload_libraries)"))
		return
	}
	if pl.cmdLineSetting("shared_preload_libraries") {
		pl.manual("preload", strings.Join(missing, ","), "shared_preload_libraries is set on the server's command line", fmt.Sprintf("add %s to -c shared_preload_libraries in the container's command, recreate it", strings.Join(missing, ",")))
		return
	}
	all := append(append([]string{}, cur...), missing...)
	pl.up[gSystem] = append(pl.up[gSystem], "ALTER SYSTEM SET shared_preload_libraries = "+pg.QuoteLiteral(strings.Join(all, ","))+";")
	pl.down[gSystem] = append(pl.down[gSystem], pl.systemRevert("shared_preload_libraries"))
	pl.p.Restart = true
	pl.add(Check{Area: "preload", Name: strings.Join(missing, ","), Detail: "not loaded; the library is installed", Fix: FixSQL,
		Manual: "after env-up: " + pl.restartCmd()})
}

// locks: the target's lock table holds what loading the largest database in
// one transaction locks (spec restore § locks).
func (pl *planner) locks() {
	need, biggest := 0, ""
	for _, sdb := range pl.src.DBs {
		if n := sdb.Dump.LockObjects.Locks(); n > need {
			need, biggest = n, sdb.Name
		}
	}
	if need == 0 {
		return
	}
	const name = "max_locks_per_transaction"
	l := pl.tgt.Locks
	if l == nil {
		pl.warn("locks", name, "cannot read the target's lock settings; not checked")
		return
	}
	if l.Capacity() >= need {
		pl.ok("locks", name, fmt.Sprintf("room for %d locks; %s needs about %d", l.Capacity(), biggest, need))
		return
	}
	want := pg.LocksPer(need, l.Slots)
	detail := fmt.Sprintf("%s needs about %d locks in one transaction; the target has room for %d (%d × %d)", biggest, need, l.Capacity(), l.PerTransaction, l.Slots)
	switch {
	case pl.in.Mode != ModeFull:
		pl.manual("locks", name, detail, fmt.Sprintf("set max_locks_per_transaction to at least %d on the target and restart it", want))
	case l.Source == "command line" || l.Source == "environment variable" || pl.cmdLineSetting(name):
		pl.manual("locks", name, detail+" (set on the server's command line)", fmt.Sprintf("change -c max_locks_per_transaction in the container's command to %d, recreate it", want))
	default:
		pl.up[gSystem] = append(pl.up[gSystem], fmt.Sprintf("ALTER SYSTEM SET max_locks_per_transaction = %d;", want))
		pl.down[gSystem] = append(pl.down[gSystem], pl.systemRevert(name))
		pl.p.Restart = true
		pl.add(Check{Area: "locks", Name: name, Detail: detail, Fix: FixSQL, Manual: "after env-up: " + pl.restartCmd()})
	}
}

// cmdLineSetting: the target's value comes from its command line, which ALTER SYSTEM cannot change.
func (pl *planner) cmdLineSetting(name string) bool {
	if pl.tgt.Env.Container != nil {
		if _, ok := pg.CmdSettings(pl.tgt.Env.Container.Cmd)[strings.ToLower(name)]; ok {
			return true
		}
	}
	if s := pl.tgt.Env.Setting(name); s != nil && (s.Source == "command line" || s.Source == "environment variable") {
		return true
	}
	return false
}

func (pl *planner) systemRevert(name string) string {
	if v, ok := pl.tgt.AutoConf[strings.ToLower(name)]; ok {
		return "ALTER SYSTEM SET " + name + " = " + valueSQL(name, v) + ";"
	}
	return "ALTER SYSTEM RESET " + name + ";"
}

// locales: the target can create a collation with each source locale.
func (pl *planner) locales() {
	seen := map[string]bool{}
	for _, sdb := range pl.src.DBs {
		l := sdb.Dump.Locale
		if l == nil {
			pl.warn("locale", sdb.Name, "run.json has no locale for this dump")
			continue
		}
		k := localeKey(l)
		if seen[k] {
			continue
		}
		seen[k] = true
		if err, ok := pl.tgt.LocaleOK[k]; ok && err != "" {
			fix := pl.rebuildHint("locale " + l.Collate)
			if pl.tgt.Kind == "local" {
				fix = fmt.Sprintf("sudo locale-gen %s && %s", l.Collate, pl.restartCmd())
			}
			pl.manual("locale", l.String(), err, fix)
			continue
		}
		detail := "available"
		pl.ok("locale", l.String(), detail)
		for i, s := range pl.src.DBs {
			td := pl.tgt.DBs[pl.p.DBs[i].Target]
			if s.Dump.Locale == l && td != nil && td.Locale != nil && l.CollVersion != "" && td.Locale.CollVersion != "" && td.Locale.CollVersion != l.CollVersion {
				pl.warn("locale", s.Name, fmt.Sprintf("collation version %s on the target, %s in the backup: text may sort differently", td.Locale.CollVersion, l.CollVersion))
			}
		}
	}
}

// needs: the target has what the databases need beyond their dumps: library
// and text search files, collations (spec restore § outside the dump).
func (pl *planner) needs() {
	libs, ts := map[string]*neededUse{}, map[string]*neededUse{}
	var colls []pg.Collation
	collUsers := map[string][]string{}
	for _, sdb := range pl.src.DBs {
		n := sdb.Dump.RestoreNeeds
		if n == nil {
			continue
		}
		addUses(libs, sdb.Name, n.Libraries)
		addUses(ts, sdb.Name, n.TSFiles)
		for _, c := range n.Collations {
			k := c.QualName()
			if _, seen := collUsers[k]; !seen {
				colls = append(colls, c)
				collUsers[k] = []string{}
			}
			for _, u := range c.UsedBy {
				collUsers[k] = append(collUsers[k], sdb.Name+": "+u)
			}
		}
	}
	pl.neededFiles("library", libs, pl.tgt.PkgLib)
	pl.neededFiles("text search file", ts, pl.tgt.TSData)
	for _, c := range colls {
		users := usedBy(collUsers[c.QualName()])
		what := "locale " + c.Collate
		if c.Locale.Locale != "" {
			what = "locale " + c.Locale.Locale
		}
		if c.Created {
			if err := pl.tgt.LocaleOK[localeKey(&c.Locale)]; err != "" {
				pl.manual("collation", c.QualName(), "the target cannot create it ("+err+")"+users, pl.rebuildHint(what))
				continue
			}
			pl.ok("collation", c.QualName(), "the target can create it")
			continue
		}
		v, ok := pl.tgt.Collations[c.Name]
		switch {
		case !ok:
			pl.manual("collation", c.Name, "the target does not have this collation"+users+"; a server gets its system collations when it is initialized", pl.rebuildHint(what))
		case v != "" && c.Version != "" && v != c.Version:
			pl.warn("collation", c.Name, fmt.Sprintf("version %s on the target, %s in the backup: text may sort differently", v, c.Version))
		default:
			pl.ok("collation", c.Name, "available")
		}
	}
}

// neededUse is who uses one file the databases need.
type neededUse struct {
	users []string
}

func addUses(m map[string]*neededUse, db string, files []pg.NeededFile) {
	for _, f := range files {
		u := m[f.Name]
		if u == nil {
			u = &neededUse{}
			m[f.Name] = u
		}
		for _, by := range f.UsedBy {
			u.users = append(u.users, db+": "+by)
		}
	}
}

// neededFiles: each file is in the target's directory (have, nil when it
// cannot be read). Its content is not compared: a load that misses a
// function fails and rolls back (ADR-0022).
func (pl *planner) neededFiles(area string, files map[string]*neededUse, have map[string]bool) {
	for _, name := range sortedKeys(files) {
		users := usedBy(files[name].users)
		switch {
		case have == nil:
			pl.manual(area, name, "cannot check that the target has it"+users+": its directory is not readable", "connect as a superuser")
		case !have[name]:
			pl.manual(area, name, "the target does not have it"+users, pl.rebuildHint(area+" "+name))
		default:
			pl.ok(area, name, "installed")
		}
	}
}

// usedBy is " (used by a, b)" or "".
func usedBy(users []string) string {
	if len(users) == 0 {
		return ""
	}
	if len(users) > 3 {
		users = append(users[:3:3], fmt.Sprintf("%d more", len(users)-3))
	}
	return " (used by " + strings.Join(users, ", ") + ")"
}

// serverSettings: the settings that change what queries do are equal at the
// server level; ALTER SYSTEM fixes them.
func (pl *planner) serverSettings() {
	for _, s := range pl.src.Env.Settings {
		if pg.SessionOnly(s.Name) {
			continue
		}
		t := pl.tgt.Env.Setting(s.Name)
		switch {
		case t != nil && pg.SameValue(s.Name, s.Vartype, s.Value, t.Value):
			continue
		case s.Custom && t == nil && !pl.tgt.Env.HasFileSettings:
			pl.manual("setting", s.Name, "custom setting; the target's configuration files are not readable", "connect as a superuser")
			continue
		case t == nil && !s.Custom:
			pl.warn("setting", s.Name, "the target server does not have this setting")
			continue
		}
		have := "not set"
		if t != nil {
			have = t.Value
		}
		detail := fmt.Sprintf("target %s, backup %s", have, s.Value)
		if strings.EqualFold(s.Name, "lc_messages") {
			// The language of server messages: no query result depends on it,
			// and '' (inherit from the environment) cannot be set back.
			pl.warn("setting", s.Name, detail+"; only the language of server messages")
			continue
		}
		if pl.cmdLineSetting(s.Name) {
			pl.manual("setting", s.Name, detail+" (set on the server's command line)", fmt.Sprintf("change -c %s in the container's command to %s, recreate it", s.Name, s.Value))
			continue
		}
		pl.up[gSystem] = append(pl.up[gSystem], "ALTER SYSTEM SET "+settingName(s.Name)+" = "+valueSQL(s.Name, s.Value)+";")
		pl.down[gSystem] = append(pl.down[gSystem], pl.systemRevert(settingName(s.Name)))
		if t != nil && t.Context == "postmaster" {
			pl.p.Restart = true
		}
		pl.sql("setting", s.Name, detail)
	}
	if n := len(pl.src.Env.Settings); n > 0 {
		pl.ok("setting", "server settings", fmt.Sprintf("%d compared", n))
	}
	pl.moduleSettings()
}

// moduleSettings: a loaded module's settings equal the backup's (pg_cron
// needs cron.database_name to create its extension). Only modules the target
// has loaded are compared; preload() loads the ones an extension needs, and
// the next run compares their settings.
func (pl *planner) moduleSettings() {
	for _, s := range pl.src.Env.Modules {
		t := pl.tgt.Env.Module(s.Name)
		if t == nil || pg.SameValue(s.Name, s.Vartype, s.Value, t.Value) {
			continue
		}
		detail := fmt.Sprintf("target %s, backup %s", t.Value, s.Value)
		if pl.cmdLineSetting(s.Name) {
			pl.manual("setting", s.Name, detail+" (set on the server's command line)", fmt.Sprintf("change -c %s in the container's command to %s, recreate it", s.Name, s.Value))
			continue
		}
		pl.up[gSystem] = append(pl.up[gSystem], "ALTER SYSTEM SET "+settingName(s.Name)+" = "+valueSQL(s.Name, s.Value)+";")
		pl.down[gSystem] = append(pl.down[gSystem], pl.systemRevert(settingName(s.Name)))
		if t.Context == "postmaster" {
			pl.p.Restart = true
		}
		pl.sql("setting", s.Name, detail)
	}
}

// settingName quotes a dotted custom setting part by part when needed.
func settingName(n string) string {
	if strings.Contains(n, ".") {
		parts := strings.Split(n, ".")
		for i, p := range parts {
			if !isPlainIdent(p) {
				parts[i] = pg.QuoteIdent(p)
			}
		}
		return strings.Join(parts, ".")
	}
	return n
}

func isPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// listSettings take their value as a list of names: written bare, not as one literal.
var listSettings = map[string]bool{"search_path": true, "temp_tablespaces": true, "session_preload_libraries": true, "local_preload_libraries": true}

// valueSQL writes a stored value back as SQL for SET/ALTER SYSTEM SET.
func valueSQL(name, v string) string {
	if listSettings[strings.ToLower(name)] && v != "" {
		return v
	}
	return pg.QuoteLiteral(v)
}

// dataSettings (URL source): the settings in effect for the connecting user
// equal the backup's. Before the load they are read in the maintenance
// database, and a difference becomes ALTER DATABASE … SET on the new database
// (a managed target refuses ALTER SYSTEM); after it they are read in the
// loaded database and must be equal.
func (pl *planner) dataSettings() {
	for i, sdb := range pl.src.DBs {
		dp := pl.p.DBs[i]
		want := pl.src.Env.DBSettings[sdb.Name]
		if len(want) == 0 {
			pl.warn("setting", sdb.Name, "the backup has no settings for this database")
			continue
		}
		_, loaded := pl.in.Loaded[sdb.Name]
		eff := pl.tgt.Effective
		if loaded {
			eff = nil
			if td := pl.tgt.DBs[dp.Target]; td != nil {
				eff = td.Effective
			}
		}
		if eff == nil {
			pl.warn("setting", sdb.Name, "cannot read the settings in effect on the target; not compared")
			continue
		}
		have := map[string]pg.Setting{}
		for _, s := range eff {
			have[strings.ToLower(s.Name)] = s
		}
		a := pl.attrs(sdb, dp.Target)
		var differ []string
		for _, s := range want {
			t, ok := have[strings.ToLower(s.Name)]
			if !ok || pg.SessionOnly(s.Name) {
				continue
			}
			// A database setting of the archive is set on the new database too.
			value := t.Value
			if a != nil && !loaded {
				for _, as := range a.Settings {
					if as.Role == "" && strings.EqualFold(as.Name, s.Name) {
						value = pg.NormalizeValue(as.Value, s.Unit, s.Vartype)
					}
				}
			}
			if pg.SameValue(s.Name, s.Vartype, s.Value, value) {
				continue
			}
			differ = append(differ, s.Name)
			if loaded {
				pl.add(Check{Area: "setting", Name: sdb.Name + ": " + s.Name, Detail: fmt.Sprintf("in %s %s, backup %s", dp.Target, value, s.Value)})
				continue
			}
			dp.Create = append(dp.Create, setDBSQL(dp.Target, "", s.Name, s.Value))
		}
		switch {
		case len(differ) == 0:
			pl.ok("setting", sdb.Name, fmt.Sprintf("%d settings equal", len(want)))
		case !loaded:
			pl.ok("setting", sdb.Name, fmt.Sprintf("%s set on %s when it is created", strings.Join(differ, ", "), dp.Target))
		}
	}
}

func setDBSQL(db, role, name, value string) string {
	if role != "" {
		return fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET %s = %s;", pg.QuoteIdent(role), pg.QuoteIdent(db), settingName(name), valueSQL(name, value))
	}
	return fmt.Sprintf("ALTER DATABASE %s SET %s = %s;", pg.QuoteIdent(db), settingName(name), valueSQL(name, value))
}

func (pl *planner) ssl() {
	switch {
	case pl.src.Env.SSL && !pl.tgt.Env.SSL:
		pl.manual("ssl", "ssl", "on in the backup, off on the target: apps with sslmode=require cannot connect", "turn on ssl on the target (ssl = on with a certificate)")
	case pl.src.Env.SSL:
		pl.ok("ssl", "ssl", "on")
	}
	if s, t := pl.src.Env.MaxConnections, pl.tgt.Env.MaxConnections; s > 0 && t > 0 && t < s {
		pl.warn("max_connections", "max_connections", fmt.Sprintf("%d on the target, %d in the backup", t, s))
	}
}

func (pl *planner) tablespaces() {
	have := map[string]bool{}
	for _, t := range pl.tgt.Env.Tablespaces {
		have[t.Name] = true
	}
	for _, t := range pl.src.Env.Tablespaces {
		if !t.HasObjects {
			continue
		}
		if have[t.Name] {
			pl.ok("tablespace", t.Name, "exists")
			continue
		}
		pl.manual("tablespace", t.Name, "objects of the backup live in it; the target does not have it",
			fmt.Sprintf("create a directory owned by postgres on the target and run CREATE TABLESPACE %s LOCATION '<dir>'", pg.QuoteIdent(t.Name)))
	}
}

// roles: the target has every user of the backup with the same or more.
func (pl *planner) roles() {
	g, tr := pl.src.Globals, pl.tgt.Roles
	if g == nil || tr == nil {
		pl.manual("role", "users", "the run has no globals", "pick another run")
		return
	}
	rls := false
	for _, sdb := range pl.src.DBs {
		rls = rls || sdb.RLS()
	}
	tmajor := pl.targetMajor()
	var existing []string
	for _, n := range g.Order {
		if strings.HasPrefix(n, "pg_") {
			continue
		}
		spec := g.Roles[n]
		q := pg.QuoteIdent(n)
		t := tr.Roles[n]
		if t == nil {
			stmt := "CREATE ROLE " + q
			if opts := roleOptions(spec, nil); opts != "" {
				stmt += " WITH " + opts
			}
			if spec.Password != "" {
				stmt += " PASSWORD " + spec.Password
			}
			pl.up[gCreateRole] = append(pl.up[gCreateRole], stmt+";")
			pl.down[gCreateRole] = append(pl.down[gCreateRole], "DROP ROLE IF EXISTS "+q+";")
			for _, m := range sortedKeys(spec.MemberOf) {
				pl.up[gGrant] = append(pl.up[gGrant], grantSQL(m, n, spec.MemberOf[m], spec, tmajor))
			}
			for _, k := range sortedKeys(spec.Settings) {
				s := spec.Settings[k]
				pl.up[gRoleSet] = append(pl.up[gRoleSet], fmt.Sprintf("ALTER ROLE %s SET %s TO %s;", q, settingName(s.Name), s.SQL))
			}
			if spec.Comment != nil {
				pl.up[gAlterRole] = append(pl.up[gAlterRole], roleCommentSQL(q, spec.Comment))
			}
			pl.sql("role", n, "missing; env-up creates it with the backup's attributes"+passwordNote(spec))
			pl.hba(n, spec)
			continue
		}
		if spec.Password != "" && n != pl.in.OwnUser && strings.Trim(t.Password, "'") != strings.Trim(spec.Password, "'") {
			existing = append(existing, n)
		}
		var diffs []string
		up, down := alterRole(spec, t)
		if up != "" {
			pl.up[gAlterRole] = append(pl.up[gAlterRole], "ALTER ROLE "+q+" WITH "+up+";")
			pl.down[gAlterRole] = append(pl.down[gAlterRole], "ALTER ROLE "+q+" WITH "+down+";")
			diffs = append(diffs, up)
		}
		if !sameComment(spec.Comment, t.Comment) {
			pl.up[gAlterRole] = append(pl.up[gAlterRole], roleCommentSQL(q, spec.Comment))
			pl.down[gAlterRole] = append(pl.down[gAlterRole], roleCommentSQL(q, t.Comment))
			diffs = append(diffs, "comment")
		}
		// More than the backup is fine, except bypassing row security when the data relies on it.
		if rls {
			for _, f := range []string{"SUPERUSER", "BYPASSRLS"} {
				if t.Flags[f] && !spec.Flags[f] {
					detail := fmt.Sprintf("%s on the target, not in the backup; tables use row-level security, which %s would bypass", f, n)
					switch {
					case pl.tgt.Env.BootstrapSuperuser == n:
						pl.manual("role", n, detail, fmt.Sprintf("%s is the target's initial superuser and cannot lose %s: recreate the target with POSTGRES_USER=%s (initdb -U %s)", n, f, pl.src.Env.BootstrapSuperuser, pl.src.Env.BootstrapSuperuser))
					case pl.tgt.Env.CurrentUser == n:
						pl.manual("role", n, detail, "connect as another superuser")
					default:
						pl.up[gAlterRole] = append(pl.up[gAlterRole], "ALTER ROLE "+q+" WITH NO"+f+";")
						pl.down[gAlterRole] = append(pl.down[gAlterRole], "ALTER ROLE "+q+" WITH "+f+";")
						pl.sql("role", n, detail)
					}
				}
			}
		}
		for _, m := range sortedKeys(spec.MemberOf) {
			want := spec.MemberOf[m]
			have, ok := t.MemberOf[m]
			if !ok {
				pl.up[gGrant] = append(pl.up[gGrant], grantSQL(m, n, want, spec, tmajor))
				pl.down[gGrant] = append(pl.down[gGrant], fmt.Sprintf("REVOKE %s FROM %s;", pg.QuoteIdent(m), q))
				diffs = append(diffs, "member of "+m)
				continue
			}
			var opts, undo []string
			if want.Admin && !have.Admin {
				opts = append(opts, "ADMIN")
				undo = append(undo, fmt.Sprintf("REVOKE ADMIN OPTION FOR %s FROM %s;", pg.QuoteIdent(m), q))
			}
			if tr.HasGrantOptions {
				if inheritOf(want, spec) && !inheritOf(have, t) {
					opts = append(opts, "INHERIT")
					undo = append(undo, fmt.Sprintf("REVOKE INHERIT OPTION FOR %s FROM %s;", pg.QuoteIdent(m), q))
				}
				if setOf(want) && !setOf(have) {
					opts = append(opts, "SET")
					undo = append(undo, fmt.Sprintf("REVOKE SET OPTION FOR %s FROM %s;", pg.QuoteIdent(m), q))
				}
			}
			if len(opts) > 0 {
				with := make([]string, len(opts))
				for i, o := range opts {
					with[i] = o + " TRUE"
				}
				if tmajor < 16 {
					with = []string{"ADMIN OPTION"}
				}
				pl.up[gGrant] = append(pl.up[gGrant], fmt.Sprintf("GRANT %s TO %s WITH %s;", pg.QuoteIdent(m), q, strings.Join(with, ", ")))
				pl.down[gGrant] = append(pl.down[gGrant], undo...)
				diffs = append(diffs, strings.Join(opts, "/")+" on "+m)
			}
		}
		for _, k := range sortedKeys(spec.Settings) {
			s := spec.Settings[k]
			if got, ok := t.Settings[k]; ok && normSetting(got.Value) == normSetting(s.Value) {
				continue
			}
			pl.up[gRoleSet] = append(pl.up[gRoleSet], fmt.Sprintf("ALTER ROLE %s SET %s TO %s;", q, settingName(s.Name), s.SQL))
			pl.down[gRoleSet] = append(pl.down[gRoleSet], revertRoleSetting(n, t, k, s.Name))
			diffs = append(diffs, "setting "+s.Name)
		}
		for _, k := range sortedKeys(t.Settings) {
			if _, ok := spec.Settings[k]; ok {
				continue
			}
			pl.up[gRoleSet] = append(pl.up[gRoleSet], fmt.Sprintf("ALTER ROLE %s RESET %s;", q, settingName(t.Settings[k].Name)))
			pl.down[gRoleSet] = append(pl.down[gRoleSet], revertRoleSetting(n, t, k, t.Settings[k].Name))
			diffs = append(diffs, "extra setting "+t.Settings[k].Name)
		}
		if len(diffs) > 0 {
			pl.sql("role", n, strings.Join(diffs, "; "))
		} else {
			pl.ok("role", n, "same or more")
		}
	}
	for _, pgr := range g.ParamGrants {
		if strings.HasPrefix(pgr.Grantee, "pg_") && tr.Roles[pgr.Grantee] == nil {
			continue
		}
		if have, ok := tr.ParamGrants[pgr.key()]; ok && (have.GrantOption || !pgr.GrantOption) {
			continue
		}
		stmt := fmt.Sprintf("GRANT %s ON PARAMETER %s TO %s", pgr.Privilege, settingName(pgr.Param), pg.QuoteIdent(pgr.Grantee))
		if pgr.GrantOption {
			stmt += " WITH GRANT OPTION"
		}
		pl.up[gParam] = append(pl.up[gParam], stmt+";")
		pl.down[gParam] = append(pl.down[gParam], fmt.Sprintf("REVOKE %s ON PARAMETER %s FROM %s;", pgr.Privilege, settingName(pgr.Param), pg.QuoteIdent(pgr.Grantee)))
		pl.sql("role", pgr.Grantee, fmt.Sprintf("missing %s on parameter %s", pgr.Privilege, pgr.Param))
	}
	if len(existing) > 0 {
		pl.p.Notes = append(pl.p.Notes, fmt.Sprintf("users %s already exist on the target with their own password (restore never changes it; a hash cannot tell whether it is the same password): only the app check proves the app's password works", strings.Join(existing, ", ")))
	}
	var extra []string
	for _, n := range sortedKeys(tr.Roles) {
		if _, ok := g.Roles[n]; !ok && !strings.HasPrefix(n, "pg_") {
			extra = append(extra, n)
		}
	}
	if len(extra) > 0 {
		pl.p.Notes = append(pl.p.Notes, "users on the target that the backup does not have (left as they are): "+listFirst(extra, 8))
	}
}

func roleCommentSQL(q string, c *string) string {
	if c == nil {
		return "COMMENT ON ROLE " + q + " IS NULL;"
	}
	return "COMMENT ON ROLE " + q + " IS " + pg.QuoteLiteral(*c) + ";"
}

func sameComment(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func passwordNote(spec *RoleSpec) string {
	if spec.Password == "" {
		return ""
	}
	return " and password hash"
}

// hba: a user created with an md5 hash cannot log in where pg_hba asks for
// scram-sha-256; the first rule that may match decides.
func (pl *planner) hba(name string, spec *RoleSpec) {
	if !strings.HasPrefix(strings.Trim(spec.Password, "'"), "md5") || !spec.Flags["LOGIN"] {
		return
	}
	if pl.tgt.HBA == nil {
		pl.warn("pg_hba", name, "md5 password hash; pg_hba.conf is not readable, not checked")
		return
	}
	for _, r := range pl.tgt.HBA {
		if !strings.HasPrefix(r.Type, "host") {
			continue
		}
		match := false
		for _, u := range r.Users {
			if u == "all" || u == name || strings.HasPrefix(u, "+") {
				match = true
			}
		}
		if !match {
			continue
		}
		if r.Method == "scram-sha-256" {
			pl.manual("pg_hba", name, "the backup has an md5 password hash and pg_hba.conf asks for scram-sha-256: the app could not log in",
				fmt.Sprintf("change that pg_hba.conf line to md5 (it accepts both), or after env-up set a new password: ALTER ROLE %s PASSWORD '…'", pg.QuoteIdent(name)))
		}
		return
	}
}

// roleOptions renders every attribute of spec for CREATE ROLE.
func roleOptions(spec *RoleSpec, _ *RoleSpec) string {
	var opts []string
	for _, f := range roleFlags {
		want, ok := spec.Flags[f]
		if !ok {
			continue
		}
		if want {
			opts = append(opts, f)
		} else {
			opts = append(opts, "NO"+f)
		}
	}
	if spec.ConnLimit != -1 {
		opts = append(opts, fmt.Sprintf("CONNECTION LIMIT %d", spec.ConnLimit))
	}
	if spec.ValidUntil != "" {
		opts = append(opts, "VALID UNTIL "+pg.QuoteLiteral(spec.ValidUntil))
	}
	return strings.Join(opts, " ")
}

// alterRole returns the attributes the target lacks (a flag the backup has,
// a different connection limit or expiry) and how to put them back.
func alterRole(spec, t *RoleSpec) (up, down string) {
	var u, d []string
	for _, f := range roleFlags {
		if spec.Flags[f] && !t.Flags[f] {
			u = append(u, f)
			d = append(d, "NO"+f)
		}
	}
	if spec.ConnLimit != t.ConnLimit {
		u = append(u, fmt.Sprintf("CONNECTION LIMIT %d", spec.ConnLimit))
		d = append(d, fmt.Sprintf("CONNECTION LIMIT %d", t.ConnLimit))
	}
	if !sameTime(spec.ValidUntil, t.ValidUntil) {
		vu := func(s string) string {
			if s == "" {
				return "VALID UNTIL 'infinity'"
			}
			return "VALID UNTIL " + pg.QuoteLiteral(s)
		}
		u = append(u, vu(spec.ValidUntil))
		d = append(d, vu(t.ValidUntil))
	}
	return strings.Join(u, " "), strings.Join(d, " ")
}

func grantSQL(role, member string, m Membership, spec *RoleSpec, tmajor int) string {
	s := fmt.Sprintf("GRANT %s TO %s", pg.QuoteIdent(role), pg.QuoteIdent(member))
	if tmajor >= 16 {
		s += fmt.Sprintf(" WITH ADMIN %s, INHERIT %s, SET %s", tf(m.Admin), tf(inheritOf(m, spec)), tf(setOf(m)))
	} else if m.Admin {
		s += " WITH ADMIN OPTION"
	}
	return s + ";"
}

func tf(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

func revertRoleSetting(role string, t *RoleSpec, key, name string) string {
	if s, ok := t.Settings[key]; ok {
		return fmt.Sprintf("ALTER ROLE %s SET %s = %s;", pg.QuoteIdent(role), settingName(s.Name), valueSQL(s.Name, s.Value))
	}
	return fmt.Sprintf("ALTER ROLE %s RESET %s;", pg.QuoteIdent(role), settingName(name))
}

// attrs parses the archive's database entries for target, keeping only
// what names users that exist on the target or env-up creates.
func (pl *planner) attrs(sdb *SourceDB, target string) *Attributes {
	roles := map[string]bool{}
	if pl.tgt.Roles != nil {
		for n := range pl.tgt.Roles.Roles {
			roles[n] = true
		}
	}
	if pl.src.Globals != nil {
		for n := range pl.src.Globals.Roles {
			roles[n] = true
		}
	}
	a, err := ParseDatabaseSQL(sdb.AttrSQL, target, roles)
	if err != nil {
		return nil
	}
	if pl.in.Mode != ModeFull {
		a.SettingsOnly()
	}
	return a
}

// dbAttributes: the new database gets the archive's owner, settings,
// privileges and comment when it is created; after the load they must be
// what the archive says.
func (pl *planner) dbAttributes() {
	for i, sdb := range pl.src.DBs {
		dp := pl.p.DBs[i]
		a := pl.attrs(sdb, dp.Target)
		if a == nil {
			pl.manual("database", sdb.Name+" attributes", "could not read the database's entries from the archive", "pick another run")
			continue
		}
		if _, loaded := pl.in.Loaded[sdb.Name]; !loaded {
			if pl.in.Mode == ModeFull {
				for _, w := range a.Warnings {
					pl.p.Notes = append(pl.p.Notes, sdb.Name+": "+w)
				}
			}
			for _, st := range a.Statements {
				dp.Create = append(dp.Create, st+";")
			}
			continue
		}
		td := pl.tgt.DBs[dp.Target]
		if td == nil || !td.Exists {
			pl.add(Check{Area: "database", Name: dp.Target, Detail: "not on the target after the load"})
			continue
		}
		owner := pl.tgt.Env.CurrentUser
		if pl.in.Mode == ModeFull {
			if owner = a.Owner; owner == "" {
				owner = td.Owner
			}
		}
		var diffs []string
		if td.Owner != owner {
			diffs = append(diffs, "owner "+td.Owner+", the backup's "+owner)
		}
		want := map[string]bool{}
		for _, s := range a.Settings {
			k := s.Role + "\x00" + strings.ToLower(s.Name)
			want[k] = true
			if got, ok := td.Settings[k]; !ok || normSetting(got) != normSetting(s.Value) {
				diffs = append(diffs, "setting "+s.Name)
			}
		}
		if pl.in.Mode == ModeFull {
			for _, k := range sortedKeys(td.Settings) {
				if !want[k] {
					diffs = append(diffs, "extra setting "+td.SettingName[k])
				}
			}
			if len(aclDiff(dp.Target, td.ACL, a.expectedACL(owner))) > 0 {
				diffs = append(diffs, "privileges")
			}
			if !sameComment(a.Comment, td.Comment) {
				diffs = append(diffs, "comment")
			}
		}
		if len(diffs) > 0 {
			pl.add(Check{Area: "database", Name: dp.Target + " attributes", Detail: strings.Join(diffs, "; ")})
		} else {
			pl.ok("database", dp.Target+" attributes", "equal")
		}
	}
}

// splitACL splits "grantee privilege"; a grantee may contain spaces.
func splitACL(k string) (grantee, privilege string) {
	i := strings.LastIndexByte(k, ' ')
	return k[:i], k[i+1:]
}

// aclDiff writes the GRANT/REVOKE that turn have into want.
func aclDiff(db string, have, want map[string]bool) []string {
	q := pg.QuoteIdent(db)
	var out []string
	who := func(g string) string {
		if g == "PUBLIC" {
			return "PUBLIC"
		}
		return pg.QuoteIdent(g)
	}
	for _, k := range sortedKeys(have) {
		if !want[k] {
			g, p := splitACL(k)
			out = append(out, fmt.Sprintf("REVOKE %s ON DATABASE %s FROM %s;", p, q, who(g)))
		}
	}
	for _, k := range sortedKeys(want) {
		if !have[k] {
			g, p := splitACL(k)
			out = append(out, fmt.Sprintf("GRANT %s ON DATABASE %s TO %s;", p, q, who(g)))
		}
	}
	return out
}

func (pl *planner) external() {
	for _, sdb := range pl.src.DBs {
		for _, o := range sdb.Dump.External {
			pl.warn("external", sdb.Name+": "+o, "depends on something outside Postgres; restore brings it back but cannot promise it works")
		}
	}
}

// cmpVersion compares dotted versions number by number ("3.4.10" > "3.4.9").
func cmpVersion(a, b string) int {
	split := func(s string) []int {
		var out []int
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
			n, _ := strconv.Atoi(f)
			out = append(out, n)
		}
		return out
	}
	x, y := split(a), split(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}
