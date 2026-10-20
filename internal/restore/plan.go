package restore

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
)

// Restore modes (ADR-0015).
const (
	ModeFull    = "full"    // container or local source: users, settings, data
	ModeData    = "data"    // URL source: databases only, owned by the connecting user
	ModePreview = "preview" // --preview: data to look at, nothing guaranteed
)

// Database states on the target before a load.
const (
	StateAbsent  = "absent"
	StateEmpty   = "empty"
	StateHasData = "has_data"
)

// Fix kinds of a check that is not ok.
const (
	FixSQL    = "sql"    // env-up has it
	FixManual = "manual" // outside SQL: image, package, configuration, restart
	FixRerun  = "rerun"  // checked once env-up has run
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

// DBPlan is where one source database goes.
type DBPlan struct {
	Source string `json:"source_db"`
	Target string `json:"target_db"`
	State  string `json:"state"` // before env-up: absent | empty | has_data (of the source name)
	// Recreate: the empty database's locale differs; env-up drops and recreates it.
	Recreate bool `json:"recreate,omitempty"`
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
	up   [9][]string
	down [9][]string
	tgt  *TargetState
	src  *SourceRun
}

// SQL groups, in the order env-up runs them; env-down runs them backwards.
const (
	gCreateRole = iota
	gAlterRole
	gGrant
	gRoleSet
	gParam
	gCreateDB
	gDBAttr
	gSystem
	gReload
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
	if in.Mode == ModeFull {
		pl.serverSettings()
		pl.ssl()
		pl.tablespaces()
		pl.roles()
	} else {
		pl.dataSettings()
	}
	pl.dbAttributes()
	pl.outOfScope()
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
		return fmt.Sprintf("use an image with %s for container %s (image: in the compose file), recreate it, then run restore again", what, pl.tgt.Container)
	case "local":
		return fmt.Sprintf("install %s on this machine, then run restore again", what)
	}
	return fmt.Sprintf("use a server with %s, then run restore again", what)
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

// databases picks where each database goes and creates the absent ones.
func (pl *planner) databases() {
	at := backupTime(pl.src.TS, pl.in.Now)
	for _, sdb := range pl.src.DBs {
		dp := &DBPlan{Source: sdb.Name}
		pl.p.DBs = append(pl.p.DBs, dp)
		if t, ok := pl.in.Loaded[sdb.Name]; ok {
			dp.Target, dp.State = t, StateHasData
			continue
		}
		td := pl.tgt.DBs[sdb.Name]
		switch {
		case td == nil || !td.Exists:
			dp.State, dp.Target = StateAbsent, sdb.Name
		case td.Objects == 0:
			dp.State, dp.Target = StateEmpty, sdb.Name
		default:
			dp.State = StateHasData
			// <db>_restored_<ts>: reuse it when it is there and empty, else the next free one.
			base := "_restored_" + at.UTC().Format("20060102_1504")
			for n := 1; n < 100; n++ {
				suffix := base
				if n > 1 {
					suffix += "_" + strconv.Itoa(n)
				}
				name := cutBytes(sdb.Name, maxIdent-len(suffix)) + suffix
				side := pl.tgt.DBs[name]
				if side == nil || !side.Exists || side.Objects == 0 {
					dp.Target = name
					break
				}
			}
		}
		if dp.Target == "" {
			pl.manual("database", sdb.Name, "no free name for a database next to it", "drop the old "+sdb.Name+"_restored_* databases")
			continue
		}
		td = pl.tgt.DBs[dp.Target]
		switch {
		case td == nil || !td.Exists:
			pl.createDB(sdb, dp.Target)
			pl.sql("database", sdb.Name, fmt.Sprintf("%s does not exist; env-up creates it", dp.Target))
		default:
			if loc := sdb.Dump.Locale; loc != nil && td.Locale != nil && !sameLocale(loc, td.Locale) {
				dp.Recreate = true
				pl.up[gCreateDB] = append(pl.up[gCreateDB], fmt.Sprintf("-- %s is empty but has locale %s; the backup has %s", dp.Target, td.Locale, loc))
				pl.up[gCreateDB] = append(pl.up[gCreateDB], dropStmts(dp.Target, pl.targetMajor(), false)...)
				pl.createDB(sdb, dp.Target)
				pl.up[gCreateDB] = append(pl.up[gCreateDB], afterCreate(dp.Target)...)
				// env-down: drop what env-up created (createDB), then bring the old one back.
				// createDB's down already drops it: only build the old one again.
				pl.down[gCreateDB] = append(pl.down[gCreateDB], recreateDB(td, pl.targetMajor())[1+len(dropStmts(td.Name, pl.targetMajor(), true)):]...)
				pl.sql("database", sdb.Name, fmt.Sprintf("%s is empty but its locale is %s, the backup's is %s; env-up recreates it", dp.Target, td.Locale, loc))
			} else if dp.State == StateHasData {
				pl.ok("database", sdb.Name, fmt.Sprintf("%s has data; restoring into %s (empty) next to it", sdb.Name, dp.Target))
			} else {
				pl.ok("database", sdb.Name, dp.Target+" exists and is empty")
			}
			if len(td.Connections) > 0 {
				pl.manual("connections", dp.Target, "connected now: "+listFirst(td.Connections, 3), stopHint(pl.tgt))
			}
		}
	}
}

func stopHint(t *TargetState) string {
	if t.Kind == "container" {
		return "stop the app (for example: docker compose stop <app service>), then run restore again"
	}
	return "stop the app connected to it, then run restore again"
}

// createDB writes CREATE DATABASE with the source locale, owner and, in data
// mode, the connecting user as owner.
func (pl *planner) createDB(sdb *SourceDB, target string) {
	stmt := "CREATE DATABASE " + pg.QuoteIdent(target) + " TEMPLATE template0"
	if loc := sdb.Dump.Locale; loc != nil {
		if opts, err := loc.CreateOptions(pl.targetMajor()); err == nil {
			stmt += opts
		}
	}
	owner := pl.tgt.Env.CurrentUser
	if pl.in.Mode == ModeFull {
		if a := pl.attrs(sdb, target); a != nil && a.Owner != "" {
			owner = a.Owner
		}
	}
	stmt += " OWNER " + pg.QuoteIdent(owner) + ";"
	pl.up[gCreateDB] = append(pl.up[gCreateDB], stmt)
	pl.down[gCreateDB] = append(pl.down[gCreateDB], dropStmts(target, pl.targetMajor(), true)...)
}

// dropStmts drops a database, stepping out of it first when it is the one
// psql is connected to (postgres) and clearing IS_TEMPLATE (template1).
func dropStmts(name string, major int, ifExists bool) []string {
	q := pg.QuoteIdent(name)
	stmt := "DROP DATABASE "
	if ifExists {
		stmt += "IF EXISTS "
	}
	stmt += q
	if major >= 13 {
		stmt += " WITH (FORCE)"
	}
	stmt += ";"
	switch name {
	case "postgres":
		return []string{`\connect template1`, stmt}
	case "template1":
		return []string{"ALTER DATABASE template1 IS_TEMPLATE false;", stmt}
	}
	return []string{stmt}
}

// afterCreate undoes what dropStmts stepped around.
func afterCreate(name string) []string {
	switch name {
	case "postgres":
		return []string{`\connect postgres`}
	case "template1":
		return []string{"ALTER DATABASE template1 IS_TEMPLATE true;"}
	}
	return nil
}

// recreateDB writes the SQL that puts an empty database back as it was:
// locale, owner, settings, privileges, comment, extensions.
func recreateDB(td *TargetDB, major int) []string {
	q := pg.QuoteIdent(td.Name)
	out := []string{"-- " + td.Name}
	out = append(out, dropStmts(td.Name, major, true)...)
	stmt := "CREATE DATABASE " + q + " TEMPLATE template0"
	if td.Locale != nil {
		if opts, err := td.Locale.CreateOptions(major); err == nil {
			stmt += opts
		}
	}
	out = append(out, stmt+" OWNER "+pg.QuoteIdent(td.Owner)+";")
	out = append(out, afterCreate(td.Name)...)
	for _, k := range sortedKeys(td.Settings) {
		role, _, _ := strings.Cut(k, "\x00")
		out = append(out, setDBSQL(td.Name, role, td.SettingName[k], td.Settings[k]))
	}
	if !td.ACLNull {
		def := defaultDBACL(td.Owner)
		out = append(out, aclDiff(td.Name, def, td.ACL)...)
	}
	if td.Comment != nil {
		out = append(out, "COMMENT ON DATABASE "+q+" IS "+pg.QuoteLiteral(*td.Comment)+";")
	}
	if len(td.Extensions) > 0 {
		out = append(out, `\connect `+q)
		for _, e := range td.Extensions {
			s := "CREATE EXTENSION IF NOT EXISTS " + pg.QuoteIdent(e.Name)
			if e.Schema != "" {
				s += " SCHEMA " + pg.QuoteIdent(e.Schema)
			}
			out = append(out, s+" VERSION "+pg.QuoteLiteral(e.Version)+";")
		}
		out = append(out, `\connect postgres`)
	}
	return out
}

func sameLocale(a, b *pg.Locale) bool {
	n := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "-", "")) }
	pa, pb := a.Provider, b.Provider
	if pa == "" {
		pa = "libc"
	}
	if pb == "" {
		pb = "libc"
	}
	// For UTF8, collate C.UTF-8 sorts by code point, which is byte order: C.
	// Postgres on musl stores it as C.
	coll := func(l *pg.Locale) string {
		if c := n(l.Collate); c != "c.utf8" || !strings.EqualFold(l.Encoding, "UTF8") {
			return c
		}
		return "c"
	}
	return strings.EqualFold(a.Encoding, b.Encoding) && coll(a) == coll(b) && n(a.Ctype) == n(b.Ctype) &&
		pa == pb && n(a.Locale) == n(b.Locale) && a.ICURules == b.ICURules
}

// extensions: every extension the backup uses is available in the same or a
// newer version; one already installed in the target database must sit in
// the same schema.
func (pl *planner) extensions() {
	for i, sdb := range pl.src.DBs {
		dp := pl.p.DBs[i]
		var installed map[string]pg.Extension
		if td := pl.tgt.DBs[dp.Target]; td != nil && td.Exists && !dp.Recreate {
			installed = map[string]pg.Extension{}
			for _, e := range td.Extensions {
				installed[e.Name] = e
			}
		}
		for _, e := range sdb.Dump.Extensions {
			name := sdb.Name + ": " + e.Name + " " + e.Version
			if have, ok := installed[e.Name]; ok {
				switch {
				case e.Schema != "" && have.Schema != e.Schema:
					pl.manual("extension", name, fmt.Sprintf("%s has %s in schema %s, the backup in %s", dp.Target, e.Name, have.Schema, e.Schema),
						fmt.Sprintf("DROP EXTENSION %s in %s (it is empty), then run restore again", pg.QuoteIdent(e.Name), dp.Target))
				case cmpVersion(have.Version, e.Version) < 0:
					pl.manual("extension", name, fmt.Sprintf("%s has %s %s installed, older than the backup", dp.Target, e.Name, have.Version),
						fmt.Sprintf("ALTER EXTENSION %s UPDATE in %s, then run restore again", pg.QuoteIdent(e.Name), dp.Target))
				default:
					pl.ok("extension", name, "installed: "+have.Version)
				}
				continue
			}
			av, ok := pl.tgt.Avail[e.Name]
			switch {
			case !ok:
				pl.manual("extension", name, "the target server does not have it", pl.extensionHint(e))
			case av.Default != "" && cmpVersion(av.Default, e.Version) < 0:
				pl.manual("extension", name, fmt.Sprintf("the target server has %s %s, older than the backup", e.Name, av.Default), pl.extensionHint(e))
			case pl.in.Mode == ModeData && !pl.tgt.Env.Superuser && !av.Trusted:
				pl.manual("extension", name, fmt.Sprintf("%s is not trusted and %s is not a superuser", e.Name, pl.tgt.Env.CurrentUser),
					fmt.Sprintf("ask your provider's admin to run CREATE EXTENSION %s in %s, then run restore again", pg.QuoteIdent(e.Name), dp.Target))
			default:
				pl.ok("extension", name, "available: "+av.Default)
			}
		}
	}
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
			return "sudo apt install " + strings.Join(pkgs, " ") + ", then run restore again"
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
		pl.manual("preload", strings.Join(missing, ","), "shared_preload_libraries is set on the server's command line", fmt.Sprintf("add %s to -c shared_preload_libraries in the container's command, recreate it, then run restore again", strings.Join(missing, ",")))
		return
	}
	all := append(append([]string{}, cur...), missing...)
	pl.up[gSystem] = append(pl.up[gSystem], "ALTER SYSTEM SET shared_preload_libraries = "+pg.QuoteLiteral(strings.Join(all, ","))+";")
	pl.down[gSystem] = append(pl.down[gSystem], pl.systemRevert("shared_preload_libraries"))
	pl.p.Restart = true
	pl.add(Check{Area: "preload", Name: strings.Join(missing, ","), Detail: "not loaded; the library is installed", Fix: FixSQL,
		Manual: "after env-up: " + pl.restartCmd()})
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
			pl.warn("locale", sdb.Name, "the manifest has no locale for this dump")
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
				fix = fmt.Sprintf("sudo locale-gen %s && %s, then run restore again", l.Collate, pl.restartCmd())
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
			pl.manual("setting", s.Name, detail+" (set on the server's command line)", fmt.Sprintf("change -c %s in the container's command to %s, recreate it, then run restore again", s.Name, s.Value))
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
			pl.manual("setting", s.Name, detail+" (set on the server's command line)", fmt.Sprintf("change -c %s in the container's command to %s, recreate it, then run restore again", s.Name, s.Value))
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

// dataSettings (URL source): the values in effect in each database are equal;
// ALTER DATABASE … SET fixes them, since a managed target refuses ALTER SYSTEM.
func (pl *planner) dataSettings() {
	for i, sdb := range pl.src.DBs {
		dp := pl.p.DBs[i]
		want := pl.src.Env.DBSettings[sdb.Name]
		if len(want) == 0 {
			pl.warn("setting", sdb.Name, "the backup has no settings for this database")
			continue
		}
		td := pl.tgt.DBs[dp.Target]
		if td == nil || !td.Exists || dp.Recreate || td.Effective == nil {
			pl.add(Check{Area: "setting", Name: sdb.Name, Detail: "compared once " + dp.Target + " exists", Fix: FixRerun})
			continue
		}
		have := map[string]pg.Setting{}
		for _, s := range td.Effective {
			have[strings.ToLower(s.Name)] = s
		}
		a := pl.attrs(sdb, dp.Target)
		diffs := 0
		for _, s := range want {
			t, ok := have[strings.ToLower(s.Name)]
			if !ok || pg.SessionOnly(s.Name) {
				continue
			}
			// A database setting of the archive takes over the value once loaded.
			value := t.Value
			if a != nil {
				for _, as := range a.Settings {
					if as.Role == "" && strings.EqualFold(as.Name, s.Name) {
						value = pg.NormalizeValue(as.Value, s.Unit, s.Vartype)
					}
				}
			}
			if pg.SameValue(s.Name, s.Vartype, s.Value, value) {
				continue
			}
			diffs++
			k := "\x00" + strings.ToLower(s.Name)
			pl.up[gDBAttr] = append(pl.up[gDBAttr], setDBSQL(dp.Target, "", s.Name, s.Value))
			pl.down[gDBAttr] = append(pl.down[gDBAttr], revertDBSetting(td, dp.Target, k, s.Name))
			pl.sql("setting", sdb.Name+": "+s.Name, fmt.Sprintf("in %s %s, backup %s", dp.Target, value, s.Value))
		}
		if diffs == 0 {
			pl.ok("setting", sdb.Name, fmt.Sprintf("%d settings equal in %s", len(want), dp.Target))
		}
	}
}

func setDBSQL(db, role, name, value string) string {
	if role != "" {
		return fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET %s = %s;", pg.QuoteIdent(role), pg.QuoteIdent(db), settingName(name), valueSQL(name, value))
	}
	return fmt.Sprintf("ALTER DATABASE %s SET %s = %s;", pg.QuoteIdent(db), settingName(name), valueSQL(name, value))
}

func resetDBSQL(db, role, name string) string {
	if role != "" {
		return fmt.Sprintf("ALTER ROLE %s IN DATABASE %s RESET %s;", pg.QuoteIdent(role), pg.QuoteIdent(db), settingName(name))
	}
	return fmt.Sprintf("ALTER DATABASE %s RESET %s;", pg.QuoteIdent(db), settingName(name))
}

func revertDBSetting(td *TargetDB, db, key, name string) string {
	role, _, _ := strings.Cut(key, "\x00")
	if v, ok := td.Settings[key]; ok {
		return setDBSQL(db, role, td.SettingName[key], v)
	}
	return resetDBSQL(db, role, name)
}

func (pl *planner) ssl() {
	switch {
	case pl.src.Env.SSL && !pl.tgt.Env.SSL:
		pl.manual("ssl", "ssl", "on in the backup, off on the target: apps with sslmode=require cannot connect", "turn on ssl on the target (ssl = on with a certificate), then run restore again")
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
			fmt.Sprintf("create a directory owned by postgres on the target and run CREATE TABLESPACE %s LOCATION '<dir>', then run restore again", pg.QuoteIdent(t.Name)))
	}
}

// roles: the target has every user of the backup with the same or more.
func (pl *planner) roles() {
	g, tr := pl.src.Globals, pl.tgt.Roles
	if g == nil || tr == nil {
		pl.manual("role", "users", "the run has no globals", "use --preview")
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
						pl.manual("role", n, detail, fmt.Sprintf("%s is the target's initial superuser and cannot lose %s: recreate the target with POSTGRES_USER=%s (initdb -U %s), then run restore again", n, f, pl.src.Env.BootstrapSuperuser, pl.src.Env.BootstrapSuperuser))
					case pl.tgt.Env.CurrentUser == n:
						pl.manual("role", n, detail, "connect as another superuser, then run restore again")
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
		pl.p.Notes = append(pl.p.Notes, fmt.Sprintf("users %s already exist on the target and keep the target's password (its hash differs from the backup's; restore never changes it): an app that connects as one of them uses the target's password", strings.Join(existing, ", ")))
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
				fmt.Sprintf("change that pg_hba.conf line to md5 (it accepts both), or after env-up set a new password: ALTER ROLE %s PASSWORD '…'; then run restore again", pg.QuoteIdent(name)))
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

// dbAttributes: owner, settings, privileges and comment equal the archive's.
func (pl *planner) dbAttributes() {
	for i, sdb := range pl.src.DBs {
		dp := pl.p.DBs[i]
		a := pl.attrs(sdb, dp.Target)
		if a == nil {
			pl.manual("database", sdb.Name+" attributes", "could not read the database's entries from the archive", "use --preview")
			continue
		}
		if pl.in.Mode == ModeFull {
			for _, w := range a.Warnings {
				pl.p.Notes = append(pl.p.Notes, sdb.Name+": "+w)
			}
		}
		td := pl.tgt.DBs[dp.Target]
		if td == nil || !td.Exists || dp.Recreate {
			// env-up creates it: every attribute of the archive follows CREATE DATABASE.
			for _, st := range a.Statements {
				pl.up[gDBAttr] = append(pl.up[gDBAttr], st+";")
			}
			continue
		}
		q := pg.QuoteIdent(dp.Target)
		var diffs []string
		if pl.in.Mode == ModeData {
			if td.Owner != pl.tgt.Env.CurrentUser && !pl.tgt.Env.Superuser {
				pl.manual("database", dp.Target+" owner", fmt.Sprintf("owned by %s; %s cannot load into it", td.Owner, pl.tgt.Env.CurrentUser),
					fmt.Sprintf("connect as %s, or ask the admin: ALTER DATABASE %s OWNER TO %s; then run restore again", td.Owner, q, pg.QuoteIdent(pl.tgt.Env.CurrentUser)))
			}
		} else if a.Owner != "" && td.Owner != a.Owner {
			pl.up[gDBAttr] = append(pl.up[gDBAttr], "ALTER DATABASE "+q+" OWNER TO "+pg.QuoteIdent(a.Owner)+";")
			pl.down[gDBAttr] = append(pl.down[gDBAttr], "ALTER DATABASE "+q+" OWNER TO "+pg.QuoteIdent(td.Owner)+";")
			diffs = append(diffs, "owner "+td.Owner+" → "+a.Owner)
		}
		want := map[string]bool{}
		for _, s := range a.Settings {
			k := s.Role + "\x00" + strings.ToLower(s.Name)
			want[k] = true
			if got, ok := td.Settings[k]; ok && normSetting(got) == normSetting(s.Value) {
				continue
			}
			stmt := fmt.Sprintf("ALTER DATABASE %s SET %s TO %s;", q, settingName(s.Name), s.SQL)
			if s.Role != "" {
				stmt = fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET %s TO %s;", pg.QuoteIdent(s.Role), q, settingName(s.Name), s.SQL)
			}
			pl.up[gDBAttr] = append(pl.up[gDBAttr], stmt)
			pl.down[gDBAttr] = append(pl.down[gDBAttr], revertDBSetting(td, dp.Target, k, s.Name))
			diffs = append(diffs, "setting "+s.Name)
		}
		if pl.in.Mode == ModeFull {
			for _, k := range sortedKeys(td.Settings) {
				if want[k] {
					continue
				}
				role, _, _ := strings.Cut(k, "\x00")
				pl.up[gDBAttr] = append(pl.up[gDBAttr], resetDBSQL(dp.Target, role, td.SettingName[k]))
				pl.down[gDBAttr] = append(pl.down[gDBAttr], setDBSQL(dp.Target, role, td.SettingName[k], td.Settings[k]))
				diffs = append(diffs, "extra setting "+td.SettingName[k])
			}
			owner := a.Owner
			if owner == "" {
				owner = td.Owner
			}
			exp := a.expectedACL(owner)
			if up := aclDiff(dp.Target, td.ACL, exp); len(up) > 0 {
				pl.up[gDBAttr] = append(pl.up[gDBAttr], up...)
				pl.down[gDBAttr] = append(pl.down[gDBAttr], aclDiff(dp.Target, exp, td.ACL)...)
				diffs = append(diffs, "privileges")
			}
			switch {
			case a.Comment != nil && (td.Comment == nil || *td.Comment != *a.Comment):
				pl.up[gDBAttr] = append(pl.up[gDBAttr], "COMMENT ON DATABASE "+q+" IS "+pg.QuoteLiteral(*a.Comment)+";")
				pl.down[gDBAttr] = append(pl.down[gDBAttr], commentSQL(q, td.Comment))
				diffs = append(diffs, "comment")
			case a.Comment == nil && td.Comment != nil:
				pl.up[gDBAttr] = append(pl.up[gDBAttr], "COMMENT ON DATABASE "+q+" IS NULL;")
				pl.down[gDBAttr] = append(pl.down[gDBAttr], commentSQL(q, td.Comment))
				diffs = append(diffs, "comment")
			}
		}
		if len(diffs) > 0 {
			pl.sql("database", dp.Target+" attributes", strings.Join(diffs, "; "))
		} else {
			pl.ok("database", dp.Target+" attributes", "equal")
		}
	}
}

func commentSQL(q string, c *string) string {
	if c == nil {
		return "COMMENT ON DATABASE " + q + " IS NULL;"
	}
	return "COMMENT ON DATABASE " + q + " IS " + pg.QuoteLiteral(*c) + ";"
}

// defaultDBACL is acldefault('d', owner): the owner has everything, PUBLIC
// CONNECT and TEMPORARY.
func defaultDBACL(owner string) map[string]bool {
	acl := map[string]bool{"PUBLIC CONNECT": true, "PUBLIC TEMPORARY": true}
	for _, p := range dbPrivileges {
		acl[owner+" "+p] = true
	}
	return acl
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

func (pl *planner) outOfScope() {
	for _, sdb := range pl.src.DBs {
		for _, o := range sdb.Dump.OutOfScope {
			pl.warn("out_of_scope", sdb.Name+": "+o, "depends on files or processes outside Postgres; restore cannot guarantee it")
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

// DataDown writes the SQL that empties the loaded databases again: drop and
// recreate each as it was right before the load (ADR-0015).
func DataDown(st *TargetState, targets []string) []string {
	var out []string
	sort.Strings(targets)
	for _, t := range targets {
		td := st.DBs[t]
		if td == nil || !td.Exists {
			continue
		}
		out = append(out, recreateDB(td, st.Major)...)
	}
	return out
}
