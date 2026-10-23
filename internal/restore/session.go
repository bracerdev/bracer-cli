package restore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// A restore is one attended session (ADR-0022 § 4d): the CLI asks, the steps
// here do the work. Nothing is stored between sessions.

// Deps is everything a restore reaches outside; tests swap in fakes.
type Deps struct {
	Shell          pg.Shell
	Store          storage.Store
	Tools          *pg.Tools // nil: built from Shell
	Now            func() time.Time
	Log            *slog.Logger
	ReadIdentities func(path string) ([]age.Identity, []string, error)
}

func withDefaults(d Deps) Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ReadIdentities == nil {
		d.ReadIdentities = crypto.ReadIdentities
	}
	if d.Tools == nil {
		d.Tools = pg.NewTools(d.Shell)
	}
	return d
}

// Folder is one app's folder in the bucket: bracer/<name>/, with the
// newest run.json in it (ADR-0023, ADR-0026, ADR-0027).
type Folder struct {
	Name   string
	Root   string      // bracer/<name>/
	Newest *backup.Run // nil when Err
	Err    error
}

// ListFolders finds every app folder under bracer/: a folder with at least
// one <ts>/run.json, read from one listing. The newest run.json of each is
// loaded; one that does not read leaves the folder with Err.
func ListFolders(ctx context.Context, st storage.Store) ([]Folder, error) {
	root := config.BucketPrefix + "/"
	objs, err := st.List(ctx, root)
	if err != nil {
		return nil, err
	}
	newest := map[string]string{}
	for _, o := range objs {
		name, rest, ok := strings.Cut(strings.TrimPrefix(o.Key, root), "/")
		if !ok {
			continue
		}
		ts, file, ok := strings.Cut(rest, "/")
		if !ok || file != backup.RunFile {
			continue
		}
		if _, err := time.Parse(tsLayout, ts); err != nil {
			continue
		}
		if ts > newest[name] {
			newest[name] = ts
		}
	}
	var out []Folder
	for name, ts := range newest {
		f := Folder{Name: name, Root: root + name + "/"}
		f.Newest, f.Err = backup.LoadRun(ctx, st, f.Root, ts)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Session holds what one restore has chosen so far.
type Session struct {
	d        Deps
	Folder   Folder
	Instance string
	IDs      *Identities // empty when nothing is encrypted
	Run      *RunPick
	runs     map[string]*backup.Run // every run of the folder, by ts (ListRuns)
	run      *backup.Run            // the run picked
	Target   *Target
	Plan     *Plan

	src      *SourceRun   // read once per session
	state    *TargetState // the target as the last comparison read it
	toolsOK  bool         // Target's runner has a pg_restore that reads the run
	upFiles  [2]string    // the last env-up/env-down pair written
	lastUp   []string
	lastDown []string
}

// NewSession starts a restore of folder.
func NewSession(d Deps, f Folder) *Session {
	return &Session{d: withDefaults(d), Folder: f, IDs: &Identities{}}
}

// Newest is the newest run.json of the folder: the state of the app.
func (s *Session) Newest() *backup.Run { return s.Folder.Newest }

// ListRuns reads run.json of every run of the folder and lists those
// restore can load for the instance, newest first.
func (s *Session) ListRuns(ctx context.Context) ([]RunInfo, error) {
	runs, err := LoadRuns(ctx, s.d.Store, s.Folder.Root)
	if err != nil {
		return nil, err
	}
	s.runs = map[string]*backup.Run{}
	for _, r := range runs {
		s.runs[r.TS] = r
	}
	return Runs(runs, s.Instance), nil
}

// UseKey reads the age key at path; it must open the latest recipient.
func (s *Session) UseKey(path, recipient string) error {
	ids := ReadIdentities(path, s.d.ReadIdentities)
	if ids.Err != nil {
		return ids.Err
	}
	if !ids.Has(recipient) {
		return fmt.Errorf("%s has no key for %s (keys for: %s)", path, orUnknown(recipient), strings.Join(ids.Recipients, ", "))
	}
	s.IDs = ids
	return nil
}

// PickRun takes the run ts of the instance: every database of the run's
// app.yaml, with the globals and environment file of the same ts. A run that
// lacks any of them is refused, and the customer picks another (ADR-0016).
func (s *Session) PickRun(ctx context.Context, ts string) error {
	r := s.runs[ts]
	if r == nil || r.Instances[s.Instance] == nil {
		return fmt.Errorf("no run %s of %s in the list", ts, s.Instance)
	}
	ri := r.Instances[s.Instance]
	runCfg, err := ReadRunConfig(ctx, s.d.Store, r, s.IDs)
	if err != nil {
		return err
	}
	expected, known := Expected(runCfg, s.Instance)
	if !known {
		return fmt.Errorf("run %s has no app.yaml naming %s; pick another", ts, s.Instance)
	}
	if missing := RunMissing(ri, expected); len(missing) > 0 {
		return fmt.Errorf("run %s is missing %s; pick another", ts, strings.Join(missing, ", "))
	}
	rp, err := PickRun(ri, s.Instance, ts, expected)
	if err != nil {
		return err
	}
	s.Run, s.run, s.src = rp, r, nil
	return nil
}

// Drilled is how long the drill of the picked run took to load the
// instance, when it passed; 0 otherwise.
func (s *Session) Drilled() float64 {
	if s.run == nil {
		return 0
	}
	if ri := s.run.Instances[s.Instance]; ri != nil && ri.Drill != nil && ri.Drill.Result == backup.DrillPassed {
		return ri.Drill.TookSeconds
	}
	return 0
}

// Target is the server a restore loads into: asked for, never stored.
type Target struct {
	Spec      TargetSpec
	Conn      pg.Conn
	Runner    pg.Runner
	disk      pg.Runner // measures free space; the first runner chosen
	Label     string
	User      string
	Superuser bool
	Version   string
	Major     int
	Pooler    string // a connection pooler answered, as it described itself
}

// Admin is a client on the maintenance database.
func (t *Target) Admin() *pg.Client { return &pg.Client{Runner: t.Runner, Conn: t.Conn} }

// TargetChoice is what the customer picked: a container, a local cluster
// (socket) or a URL.
type TargetChoice struct {
	Container string
	Local     *pg.Local
	URL       string
	Password  string // for a socket that asked for one
}

// ErrNeedPassword: the socket asks for a password; ask it and connect again.
var ErrNeedPassword = errors.New("the target asks for a password")

// passwordPrompt is how psql says the socket wants a password.
var passwordPrompt = []string{"no password supplied", "password authentication failed", "Password for user"}

// Connect connects to the target the customer chose (spec restore § target):
// container → its superuser (POSTGRES_USER) through docker exec; local →
// postgres over the socket; URL → as given.
func (s *Session) Connect(ctx context.Context, c TargetChoice) error {
	t := &Target{}
	var where pg.Where
	switch {
	case c.Container != "":
		user, err := s.d.Tools.Docker().ContainerUser(ctx, c.Container)
		if err != nil {
			return err
		}
		t.Conn = pg.Conn{User: user, Database: "postgres", Password: c.Password}
		where = pg.Where{Container: c.Container}
		t.Spec = TargetSpec{Kind: "container", Container: c.Container, Shell: s.d.Shell, Password: c.Password != ""}
		t.Label = "container " + c.Container
	case c.Local != nil:
		l := *c.Local
		t.Conn = pg.LocalConn(l, "postgres", c.Password)
		where = pg.Where{Local: &l}
		t.Spec = TargetSpec{Kind: "local", Local: &l, Shell: s.d.Shell, Password: c.Password != ""}
		t.Label = fmt.Sprintf("local Postgres on port %d", l.Port)
	default:
		conn, err := pg.ParseURL(c.URL)
		if err != nil {
			return fmt.Errorf("target URL: %w", err)
		}
		if conn.Database == "" {
			conn.Database = "postgres"
		}
		t.Conn = conn
		t.Spec = TargetSpec{Kind: "url"}
		t.Label = hostPort(conn)
	}
	fail := func(err error) error {
		if c.URL == "" && c.Password == "" && containsAny(err.Error(), passwordPrompt) {
			return ErrNeedPassword
		}
		return fmt.Errorf("connect to %s: %w", t.Label, err)
	}
	runner, major, err := s.d.Tools.Resolve(ctx, where, t.Conn, 0)
	if err != nil {
		return fail(err)
	}
	t.Runner, t.disk, t.Major = runner, runner, major
	admin := t.Admin()
	if t.Version, err = admin.ServerVersion(ctx); err != nil {
		return fail(err)
	}
	ap, err := admin.AdminPrivileges(ctx)
	if err != nil {
		return fail(err)
	}
	t.User, t.Superuser = ap.Role, ap.Superuser
	if pooler, info := admin.DetectPooler(ctx); pooler {
		t.Pooler = info
	}
	s.Target, s.Plan, s.toolsOK = t, nil, false
	return nil
}

// Mode is how the run restores: full (users, settings, data) or data (a URL
// source); "" before the first comparison.
func (s *Session) Mode() string {
	if s.src == nil {
		return ""
	}
	return s.src.Mode()
}

// ErrPickTarget: this target cannot take the run; the customer picks another.
type ErrPickTarget struct{ Msg string }

func (e *ErrPickTarget) Error() string { return e.Msg }

// Compare reads the target and compares it with the run (ADR-0015 § 3).
// Nothing on the target changes.
func (s *Session) Compare(ctx context.Context) (*Plan, error) {
	t := s.Target
	if err := s.checkTools(ctx); err != nil {
		return nil, err
	}
	if s.src == nil {
		src, err := LoadSource(ctx, s.d.Store, s.Run, s.IDs, t.Runner, t.Conn)
		if err != nil {
			return nil, err
		}
		s.src = src
	}
	mode := s.src.Mode()
	if mode == ModeFull && !t.Superuser {
		return nil, &ErrPickTarget{Msg: fmt.Sprintf("a full backup restores only into a target where you connect as a superuser; %s is not one", t.User)}
	}
	tgt, err := ReadTarget(ctx, t.Admin(), t.Spec, mode == ModeFull, nil, s.src.Wants())
	if err != nil {
		return nil, fmt.Errorf("read the target: %w", err)
	}
	plan := MakePlan(PlanInput{Src: s.src, Tgt: tgt, Mode: mode, Now: s.d.Now()})
	s.checkDisk(ctx, plan)
	if t.Spec.Password && len(plan.Up) > 0 {
		plan.Notes = append(plan.Notes, "the target asks for a password: set PGPASSWORD to it in your shell before applying the files")
	}
	if t.Pooler != "" {
		plan.Notes = append(plan.Notes, fmt.Sprintf("the target looks like a connection pooler (%s); pg_restore needs a direct connection to the server", t.Pooler))
	}
	s.Plan, s.state = plan, tgt
	return plan, nil
}

// TargetHas: the target had database db at the last comparison.
func (s *Session) TargetHas(db string) bool { return s.state != nil && s.state.Names[db] }

// checkTools: an archive from pg_dump N is read only by pg_restore ≥ N; a
// target whose own is older is reached through postgres:<N>.
func (s *Session) checkTools(ctx context.Context) error {
	if s.toolsOK {
		return nil
	}
	t := s.Target
	need := 0
	for _, dp := range s.Run.DBs {
		need = max(need, pg.MajorOf(dp.ToolVersion))
	}
	if need > 0 {
		tv, have, err := t.Admin().ToolVersion(ctx)
		if err != nil {
			return fmt.Errorf("check pg_restore for %s: %w", t.Label, err)
		}
		if have < need {
			var alt pg.Runner
			switch {
			case t.Spec.Container != "":
				alt = pg.ImageRunner{Shell: s.d.Shell, Image: pg.ImageForMajor(need), Network: "container:" + t.Spec.Container}
			default:
				alt, err = s.d.Tools.Pick(ctx, pg.Where{Local: t.Spec.Local}, need)
				if err != nil {
					return fmt.Errorf("dumps were made with pg_dump %d but pg_restore %s cannot read them: %w", need, tv, err)
				}
			}
			t.Runner = alt
		}
	}
	s.toolsOK = true
	return nil
}

// checkDisk adds the disk check: the target holds 5 × the dumps (ADR-0022).
// A URL target's disk is the business of whoever runs that server.
func (s *Session) checkDisk(ctx context.Context, plan *Plan) {
	var need int64
	for _, dp := range s.Run.DBs {
		need += dp.Bytes
	}
	if s.Target.Spec.Kind == "url" {
		plan.Notes = append(plan.Notes, "the target's disk is not checked (URL target); a full disk makes the load roll back")
		return
	}
	dc := CheckDisk(ctx, s.Target.disk, &pg.Client{Runner: s.Target.disk, Conn: s.Target.Conn}, need)
	switch {
	case !dc.Checked:
		plan.Checks = append(plan.Checks, Check{Area: "disk", Name: "free space", Detail: dc.Error, Fix: FixManual, Manual: "cannot check free disk space on the target"})
	case !dc.Enough():
		plan.Checks = append(plan.Checks, Check{Area: "disk", Name: "free space",
			Detail: fmt.Sprintf("%s free at %s; the load needs about %s (%d × the dumps)", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes), DiskFactor),
			Fix:    FixManual, Manual: "free up " + humanBytes(dc.NeedBytes-dc.FreeBytes) + " on the target"})
	default:
		plan.Checks = append(plan.Checks, Check{Area: "disk", Name: "free space", OK: true,
			Detail: fmt.Sprintf("%s free at %s, about %s needed", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))})
	}
}

// SQLDir is ./bracer-restore-<app>-<instance>-<ts>.
func (s *Session) SQLDir() string {
	return "bracer-restore-" + s.run.App + "-" + s.Instance + "-" + s.Run.TS
}

// Apply is how the customer applies a file to this target, with <file>.
func (s *Session) Apply() string {
	return applyCommand(s.Target.Spec, s.Target.Conn, s.Target.Major)
}

// WriteFixes writes the plan's env-up and env-down as the next NN pair of
// the SQL directory, unless they are the pair this session wrote last; it
// returns the pair's paths and whether they are new (spec restore § SQL).
func (s *Session) WriteFixes() (up, down string, fresh bool, err error) {
	plan := s.Plan
	if s.upFiles[0] != "" && slices.Equal(plan.Up, s.lastUp) && slices.Equal(plan.Down, s.lastDown) {
		return s.upFiles[0], s.upFiles[1], false, nil
	}
	head := func(what string) []string {
		h := []string{
			fmt.Sprintf("bracer restore: %s for the backup run %s of %s, on %s, %s", what, s.Run.TS, s.Instance, s.Target.Label, s.d.Now().UTC().Format("2006-01-02")),
			"Apply with: " + s.Apply(),
		}
		if what == "env-up" {
			h = append(h, "Read it first. It may hold password hashes: delete this directory when done.",
				"Then press Enter in bracer restore to check again.")
		} else {
			h = append(h, "Puts the target back as it was before env-up (safe to run twice).",
				"Drop the databases restore created first: a user that owns one cannot be dropped.")
		}
		if plan.Restart {
			h = append(h, "After it: "+plan.RestartCmd)
		}
		return h
	}
	dir := s.SQLDir()
	paths, err := WriteSQLFiles(dir, SQLFile{Suffix: "env-up", Header: head("env-up"), Statements: plan.Up},
		SQLFile{Suffix: "env-down", Header: head("env-down"), Statements: plan.Down})
	if err != nil {
		return "", "", false, err
	}
	s.upFiles = [2]string{absPath(paths[0]), absPath(paths[1])}
	s.lastUp, s.lastDown = plan.Up, plan.Down
	return s.upFiles[0], s.upFiles[1], true, nil
}

// EnvDowns are the env-down files this session wrote, newest first.
func (s *Session) EnvDowns() []string {
	dir := s.SQLDir()
	files, _ := filepath.Glob(filepath.Join(dir, "*-env-down.sql"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	for i, f := range files {
		files[i] = absPath(f)
	}
	return files
}

// DBResult is one loaded database.
type DBResult struct {
	Source         string
	Target         string
	Bytes          int64
	OK             bool
	Seconds        float64
	TablesExpected int
	TablesFound    int
	MissingTables  []string
	RowMismatches  []RowMismatch
	// Kept: the database holds data although something failed; the customer
	// drops it (Drop).
	Kept  bool
	Drop  string
	Error string
}

// Outcome is what the load and its proof found.
type Outcome struct {
	DBs    []*DBResult
	Verify []Check // the comparison after the load, what no longer matches
	App    *AppCheck
	OK     bool
}

// Load creates and loads each database in one transaction, then proves the
// restore: the environment again, the tables, the database attributes and
// the app's URL (spec restore § Load).
func (s *Session) Load(ctx context.Context, appURL string) *Outcome {
	t, plan, mode := s.Target, s.Plan, s.src.Mode()
	out := &Outcome{OK: true}
	loaded := map[string]string{}
	var targets []string
	for i, dp := range plan.DBs {
		dump := s.Run.DBs[i]
		r := &DBResult{Source: dp.Source, Target: dp.Target, Bytes: dump.Bytes, TablesExpected: len(dump.Tables), Drop: DropDB(dp.Target, t.Major)}
		out.DBs = append(out.DBs, r)
		if ctx.Err() != nil {
			r.Error = "not loaded: interrupted"
			out.OK = false
			continue
		}
		start := s.d.Now()
		err := LoadNew(ctx, s.d.Store, dump, dp, s.IDs, t.Runner, t.Conn, mode, t.Major, config.DrillTimeout)
		r.Seconds = s.d.Now().Sub(start).Seconds()
		var committed *ErrCommitted
		var after *ErrAfterLoad
		var left *ErrLeft
		var notCreated *ErrNotCreated
		switch {
		case err == nil:
			// Loaded: on the target from now on, whatever happens to the
			// next database, so the undo lists it.
			r.Kept = true
			loaded[dp.Source] = dp.Target
			targets = append(targets, dp.Target)
			s.d.Log.Info("restored", "db", dp.Target, "seconds", fmt.Sprintf("%.1f", r.Seconds))
			continue
		case errors.As(err, &notCreated):
			r.Error = err.Error() + "; nothing was created"
		case errors.As(err, &committed):
			r.Kept, r.Error = true, dp.Target+" holds what was loaded from a dump that does not match its checksum ("+committed.Err.Error()+")"
		case errors.As(err, &after):
			r.Kept, r.Error = true, dp.Target+" holds the loaded data, but "+after.Err.Error()
		case errors.As(err, &left):
			r.Kept, r.Error = true, left.Err.Error()+"; "+dp.Target+" could not be dropped"
		case ctx.Err() != nil:
			r.Error = "interrupted; " + dp.Target + " was dropped"
		default:
			r.Error = err.Error() + "; the load rolled back and " + dp.Target + " was dropped"
		}
		out.OK = false
	}
	if !out.OK {
		for _, r := range out.DBs {
			if r.Kept && r.Error == "" {
				r.Error = "loaded, not verified: another database failed"
			}
		}
		return out
	}
	after, err := ReadTarget(ctx, t.Admin(), t.Spec, mode == ModeFull, targets, s.src.Wants())
	if err != nil {
		out.Verify, out.OK = []Check{{Area: "target", Name: "read", Detail: err.Error()}}, false
		return out
	}
	if check := MakePlan(PlanInput{Src: s.src, Tgt: after, Mode: mode, Loaded: loaded, Now: s.d.Now()}); !check.OK() {
		out.Verify, out.OK = check.Failed(), false
	}
	dumps := map[string]*backup.Dump{}
	renamed := map[string]string{}
	for i, r := range out.DBs {
		dump := s.Run.DBs[i]
		r.Kept = true
		cmp, err := Compare(ctx, &pg.Client{Runner: t.Runner, Conn: t.Conn.WithDatabase(r.Target)}, dump.Tables)
		if err != nil {
			r.Error, out.OK = fmt.Sprintf("compare tables: %v", err), false
			continue
		}
		r.TablesFound, r.MissingTables, r.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
		r.OK = cmp.OK()
		out.OK = out.OK && r.OK
		dumps[r.Target] = dump
		renamed[r.Source] = r.Target
	}
	out.App = CheckApp(ctx, s.d.Shell, pg.ImageForMajor(t.Major), appNetwork(after), appURL, dumps, renamed)
	out.OK = out.OK && out.App.OK
	return out
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// applyCommand is how the customer applies a file to this target. When the
// target asked for a password, psql takes it from PGPASSWORD in their shell.
func applyCommand(spec TargetSpec, conn pg.Conn, major int) string {
	switch spec.Kind {
	case "container":
		env := ""
		if spec.Password {
			env = " -e PGPASSWORD"
		}
		return fmt.Sprintf("docker exec -i%s %s psql -U %s -d postgres -v ON_ERROR_STOP=1 < <file>", env, spec.Container, conn.User)
	case "local":
		h, env := "", ""
		if spec.Local.Dir != config.DefaultSocketDir {
			h = " -h " + spec.Local.Dir
		}
		if spec.Password {
			env = " --preserve-env=PGPASSWORD"
		}
		return fmt.Sprintf("sudo -u postgres%s psql%s -p %d -d postgres -v ON_ERROR_STOP=1 < <file>", env, h, spec.Local.Port)
	}
	return fmt.Sprintf(`PGURL='<target URL>' docker run --rm -i --network host -e PGURL %s sh -c 'psql "$PGURL" -v ON_ERROR_STOP=1' < <file>`, pg.ImageForMajor(major))
}

// appNetwork: the app of a container target reaches it on the container's
// network (db:5432 resolves as the app sees it); others from the host.
func appNetwork(t *TargetState) string {
	if t.Kind == "container" && t.Env.Container != nil && len(t.Env.Container.Networks) > 0 {
		return t.Env.Container.Networks[0]
	}
	return "host"
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func hostPort(c pg.Conn) string {
	if c.Host == "" {
		return ""
	}
	if c.Port != 0 {
		return c.Host + ":" + strconv.Itoa(c.Port)
	}
	return c.Host
}

// humanBytes is as in the CLI, used for the disk message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// pgdmpMagic is the first 5 bytes of a custom archive (pg_dump -Fc).
const pgdmpMagic = "PGDMP"

// checksumErr returns the source stream's SHA-256 error if one was detected.
func checksumErr(r interface{}) error {
	if c, ok := r.(interface{ ChecksumErr() error }); ok {
		return c.ChecksumErr()
	}
	return nil
}
