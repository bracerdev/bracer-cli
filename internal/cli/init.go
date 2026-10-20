package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// initDeps is everything init touches outside the process, so tests can run
// the whole flow without Docker or network.
type initDeps struct {
	shell    pg.Shell
	probe    pg.Prober
	newStore func(storage.S3Config) (storage.Store, error)
	env      envDeps // step 0; its shell is replaced by shell
	home     func() (string, error)
	// runSelf runs this binary with args and exactly env (step 14); returns the exit code.
	runSelf func(ctx context.Context, args, env []string, stdout, stderr io.Writer) int
	// runTTY runs a command attached to the terminal (step 15: sudo bracer schedule install).
	runTTY func(ctx context.Context, name string, args ...string) error
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// initRun holds the state of one init run.
type initRun struct {
	initDeps
	ctx     context.Context
	p       *prompter
	tools   *pg.Tools
	docker  pg.Docker
	cfg     *config.App
	path    string // as given with -c
	abs     string // absolute path of app.yaml
	again   string // the command line typed, printed last when init stops for a fix
	fresh   bool   // app.yaml does not exist yet
	noDrill bool
	// legacy are instances from before ADR-0013, to be picked again (spec init AC).
	legacy []config.Instance
	// loaded are the secrets of the existing app.yaml, to recognise an instance picked again.
	loaded  *config.Secrets
	secrets map[string]string // variable name → collected value, not written anywhere yet
	// srcImage: image of the chosen container, used to fill drill.image (step 11).
	srcImage string
}

// runInit is `bracer init` (spec backup/init § Flow).
func runInit(ctx context.Context, args []string, d initDeps) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	path := fs.String("c", "app.yaml", "path of app.yaml to create or extend")
	noDrill := fs.Bool("no-drill", false, "do not test-restore backups on this machine (drill.runtime: none)")
	verbose := fs.Bool("verbose", false, "print every docker and pg_* command")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = *verbose
		d.shell = sh
	}
	d.env.shell = d.shell
	r := &initRun{initDeps: d, ctx: ctx, p: newPrompter(d.stdin, d.stdout), path: *path, abs: absPath(*path),
		again: commandLine("init", args), noDrill: *noDrill, docker: pg.Docker{Shell: d.shell}, secrets: map[string]string{}}
	r.tools = pg.NewTools(d.shell)
	err := r.run()
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errEnvBlocked), errors.Is(err, errSelfCheck):
		return exitError
	case errors.Is(err, errQuit):
		fmt.Fprintf(d.stdout, "\nNothing written. Run again when ready:\n  %s\n", r.again)
		return exitError
	case errors.Is(err, ErrNoInput), errors.As(err, new(*usageError)):
		fmt.Fprintln(d.stderr, "bracer init:", err)
		return exitUsage
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(d.stderr, "\nbracer init: interrupted, nothing written")
		return exitError
	default:
		fmt.Fprintln(d.stderr, "bracer init:", err)
		return exitError
	}
}

// usageError is a usage error (exit 2): broken app.yaml…
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usageErrorf(format string, args ...interface{}) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

var (
	// errQuit: the user chose [q]; what to run is already printed.
	errQuit = errors.New("quit")
	// errSelfCheck: step 14 failed; the child's own error is already printed.
	errSelfCheck = errors.New("self-check failed")
)

func (r *initRun) warn(format string, args ...interface{}) {
	fmt.Fprintf(r.stderr, "warning: "+format+"\n", args...)
}

func (r *initRun) run() error {
	// 0. Environment, before any question (same function as `bracer check`).
	r.p.printf("Checking this machine:\n")
	rep := checkEnv(r.ctx, r.env)
	printEnv(r.stdout, rep, r.again)
	if rep.Blocked() {
		r.p.printf("\nNothing written.\n")
		return errEnvBlocked
	}
	if rep.Runtime.User == "root" {
		r.p.printf("Running as root: the key and env files go to root's home and the jobs will run as root.\n")
	}
	if err := r.loadOrCreate(); err != nil {
		return err
	}
	r.cfg.Runtime = rep.Runtime
	if !r.fresh {
		r.recheckExisting()
	}
	// 1–7. Instance
	inst, err := r.pickInstance()
	if err != nil {
		return err
	}
	if inst != nil {
		r.cfg.Instances = append(r.cfg.Instances, *inst)
	}
	for _, in := range r.legacy {
		r.warn("instance %s (%s, before ADR-0013) was not picked again and is left out of %s", in.Name, in.Legacy(), r.path)
	}
	// 8. Bucket
	if r.cfg.Storage == nil {
		if err := r.setupStorage(); err != nil {
			return err
		}
	}
	// 9. Encryption
	if r.cfg.Encryption == "" {
		if err := r.setupEncryption(); err != nil {
			return err
		}
	}
	// 12. Drill runtime
	if r.noDrill {
		if r.cfg.Drill == nil {
			r.cfg.Drill = &config.Drill{}
		}
		r.cfg.Drill.Runtime = config.DrillRuntimeNone
		r.cfg.Drill.Image = ""
		r.p.printf("drill.runtime: none — backups on this machine will not be test-restored; every report will say so.\n")
	}
	// 10. Secrets
	if err := r.storeSecrets(); err != nil {
		return err
	}
	// 13. Write
	if err := config.Save(r.path, r.cfg); err != nil {
		return err
	}
	r.p.printf("\nWrote %s.\n", r.abs)
	// 14–15.
	if err := r.selfCheck(); err != nil {
		return err
	}
	return r.offerSchedule()
}

// loadOrCreate reads an existing app.yaml (merge) or asks for the app name.
func (r *initRun) loadOrCreate() error {
	cfg, legacy, err := config.LoadForInit(r.path)
	switch {
	case err == nil:
		r.cfg, r.legacy = cfg, legacy
		r.p.printf("\nFound %s (app %q, %d instance(s)); new choices will be added to it.\n", r.path, cfg.App, len(cfg.Instances))
		for _, in := range legacy {
			r.p.printf("  -  %s (%s) needs to be picked again: bracer no longer reaches Postgres that way (ADR-0013)\n", in.Name, in.Legacy())
		}
		if s, warns, err := config.LoadSecrets(cfg, ""); err == nil {
			r.loaded = s
			for _, w := range warns {
				r.warn("%s", w)
			}
		}
		return nil
	case os.IsNotExist(err):
		r.fresh = true
		wd, _ := os.Getwd()
		def := sanitizeName(filepath.Base(wd))
		for {
			name, err := r.p.ask("\nApp name (lowercase, used in bucket paths and variable names)", def)
			if err != nil {
				return err
			}
			probe := &config.App{App: name, Instances: []config.Instance{{Name: "x", Container: "x", Databases: config.Databases{All: true}}}}
			if verr := probe.Validate(); verr == nil {
				r.cfg = &config.App{App: name}
				return nil
			}
			r.p.printf("Use lowercase letters, digits, - or _ only.\n")
		}
	default:
		return usageErrorf("%v", err)
	}
}

// recheckExisting re-tests the connection to every existing instance and reports the broken ones (last AC).
func (r *initRun) recheckExisting() {
	if r.loaded == nil || len(r.cfg.Instances) == 0 {
		return
	}
	for _, in := range r.cfg.Instances {
		conn, runner, err := backup.Connect(r.ctx, r.tools, in, r.loaded)
		if err == nil {
			err = (&pg.Client{Runner: runner, Conn: conn}).Ping(r.ctx)
		}
		if err != nil {
			r.p.printf("  ✗ %s: %v\n", in.Name, err)
		} else {
			r.p.printf("  ✓ %s\n", in.Name)
		}
	}
}

// target is how the picked instance is reached while init talks to it.
type target struct {
	where pg.Where
	conn  pg.Conn
	// host:port of Postgres on this machine (or typed by hand): init asks a user
	// and password and builds the URL itself (spec init § Flow 4).
	hostPort string
	// fromURL: the user pasted a URL.
	fromURL bool
}

// sudoPsql is the command that opens psql as the postgres OS user on this
// machine; a cluster on another port (apt numbers a second one 5433) needs -p.
func sudoPsql(hostPort string) string {
	if _, port, err := net.SplitHostPort(hostPort); err == nil && port != "5432" {
		return "sudo -u postgres psql -p " + port
	}
	return "sudo -u postgres psql"
}

func (t target) isURL() bool { return t.hostPort != "" || t.fromURL }

// useLocal points the instance at the local cluster's socket: user postgres, peer.
func (r *initRun) useLocal(inst *config.Instance, t *target, l pg.Local) {
	inst.Local = &config.Local{Port: l.Port}
	if l.Dir != config.DefaultSocketDir {
		inst.Local.SocketDir = l.Dir
	}
	inst.User = "postgres"
	t.conn.User = "postgres"
	t.where = pg.Where{Local: &l}
}

// localSocket: the typed address is this machine and a socket has its port.
func (r *initRun) localSocket(hs pg.HostScan, t target) (int, string) {
	hp := t.hostPort
	if t.fromURL {
		hp = net.JoinHostPort(t.conn.Host, strconv.Itoa(t.conn.Port))
	}
	host, ps, err := net.SplitHostPort(hp)
	if err != nil {
		return 0, ""
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "":
	default:
		return 0, ""
	}
	port, _ := strconv.Atoi(ps)
	return port, hs.Sockets[port]
}

// pickInstance is steps 1–7: discover, pick, connect, version, databases, permissions, pooler.
// Returns nil if the user skips this instance.
func (r *initRun) pickInstance() (*config.Instance, error) {
	r.p.printf("\nLooking for Postgres (Docker containers, sockets and TCP ports on this machine)...\n")
	containers, err := r.docker.ListPostgresContainers(r.ctx)
	if err != nil {
		return nil, err
	}
	hs := pg.ScanHost(r.ctx, r.shell)
	disc := pg.Discover(r.ctx, containers, hs, r.probe)
	disc.Kubernetes = append(disc.Kubernetes, pg.KubernetesSigns()...)
	for _, w := range disc.Warnings {
		r.warn("%s", w)
	}
	shown := disc.Candidates
	r.p.printf("\n")
	for i, c := range shown {
		r.p.printf("  %d) %s\n", i+1, c.Label())
	}
	if len(disc.Kubernetes) > 0 {
		r.p.printf("  -  Kubernetes is not supported (seen: %s).\n"+
			"     bracer does not reach pods. Run Postgres in Docker on this machine, or enter a\n"+
			"     postgres:// URL to a Service or NodePort this machine can reach (m).\n", strings.Join(disc.Kubernetes, ", "))
	}
	if len(shown) == 0 {
		r.p.printf("  No Postgres found.\n")
	}
	r.p.printf("  m) enter host:port or a postgres:// URL manually\n")
	var cand *pg.Candidate
	idx, key, err := r.p.choose("Pick an instance (number or m)", len(shown), "m")
	if err != nil {
		return nil, err
	}
	if key == "" {
		cand = &shown[idx]
	}

	inst := config.Instance{}
	t := target{conn: pg.Conn{Database: "postgres"}}
	switch {
	case cand != nil && cand.Container != nil:
		ct := cand.Container
		if !ct.Running {
			r.p.printf("Container %s is stopped; it cannot be tested now. Start it and run init again.\n", ct.Name)
			return nil, nil
		}
		inst.Container = ct.Name
		r.srcImage = ct.Image
		inst.User = ct.DefaultUser()
		t.conn.User = inst.User
		t.where = pg.Where{Container: ct.Name}
	case cand != nil && cand.Local != nil:
		r.useLocal(&inst, &t, *cand.Local)
	case cand != nil:
		t.hostPort = net.JoinHostPort(strings.Trim(cand.URLHost(), "[]"), strconv.Itoa(cand.Port))
	default:
		s, err := r.p.askRequired("host:port, or postgres://user:password@host:port/db?sslmode=…")
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
			c, err := pg.ParseURL(s)
			if err != nil {
				return nil, fmt.Errorf("%v (characters such as @ : / %% in the user or password must be percent-encoded)", err)
			}
			t.conn, t.fromURL = c, true
			r.secrets["__url__"] = s
		} else {
			host, port, ok := splitHostPort(s)
			if !ok {
				return nil, fmt.Errorf("expected host:port, got %q", s)
			}
			t.hostPort = net.JoinHostPort(host, strconv.Itoa(port))
		}
		// A local address whose port has a socket here is the local cluster:
		// offer the full backup through the socket (ADR-0015).
		if port, sock := r.localSocket(hs, t); sock != "" {
			ok, err := r.p.yesNo(fmt.Sprintf("This is the local Postgres on port %d. Back it up through its socket instead (full backup: users, privileges, settings)?", port), true)
			if err != nil {
				return nil, err
			}
			if ok {
				delete(r.secrets, "__url__")
				t = target{conn: pg.Conn{Database: "postgres"}}
				r.useLocal(&inst, &t, pg.Local{Dir: filepath.Dir(sock), Port: port})
			}
		}
	}
	if t.isURL() {
		inst.URLEnv = "pending" // named once the instance name is known
		r.p.printf("URL source: bracer backs up the databases only, not users, privileges or server settings.\n" +
			"To back up everything, run bracer on the machine where Postgres runs.\n")
	}
	// 3. Runner per ADR-0013, major not known yet; re-picked after asking the version.
	runner, err := r.tools.Pick(r.ctx, t.where, 0)
	if err != nil {
		r.p.printf("Cannot reach this Postgres from here: %v\n", err)
		return nil, nil
	}
	if ir, ok := runner.(pg.ImageRunner); ok && t.where.Container != "" {
		r.p.printf("Container %s has no pg_dump; using %s on its network.\n", t.where.Container, ir.Image)
	}

	// Instance name; same target as an existing instance → add databases to it;
	// an instance from before ADR-0013 → keep its name and databases.
	existing := r.findSame(inst, t)
	var replaced *config.Instance
	if existing != nil {
		ok, err := r.p.yesNo(fmt.Sprintf("Instance %q in %s already points here. Add databases to it?", existing.Name, r.path), true)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		inst.Name = existing.Name
		if t.isURL() {
			inst.URLEnv = existing.URLEnv
		}
	} else if replaced, err = r.pickLegacy(); err != nil {
		return nil, err
	} else if replaced != nil {
		inst.Name = replaced.Name
		inst.Databases = replaced.Databases
	} else {
		def := "cloud"
		switch {
		case inst.Local != nil:
			def = fmt.Sprintf("local-%d", inst.Local.Port)
		case cand != nil:
			def = cand.SuggestName()
		case t.hostPort != "":
			def = sanitizeName(strings.NewReplacer(":", "-", "[", "", "]", "").Replace(t.hostPort))
		}
		for {
			name, err := r.p.ask("Instance name", def)
			if err != nil {
				return nil, err
			}
			name = sanitizeName(name)
			if name != "" && r.cfg.FindInstance(name) == nil {
				inst.Name = name
				break
			}
			r.p.printf("Name must be unique in %s and use lowercase letters, digits, - or _.\n", r.path)
		}
	}
	if inst.URLEnv == "pending" {
		inst.URLEnv = config.EnvVarName(r.cfg.App, inst.Name, "url")
	}
	if t.fromURL {
		r.secrets[inst.URLEnv] = r.secrets["__url__"]
		delete(r.secrets, "__url__")
	}

	// 4. Test connection; host:port → user + password → URL; auth error → ask.
	client := &pg.Client{Runner: runner, Conn: t.conn}
	if t.hostPort != "" {
		r.p.printf("\nPostgres on %s is reached over TCP with a user and a password.\n"+
			"If you only ever use `sudo -u postgres psql`, create a backup user first:\n\n  %s\n%s\n\n", t.hostPort, sudoPsql(t.hostPort), indent(pg.FixRoleSQL("bracer_backup", 0), "  "))
		if err := r.askHostLogin(client, &inst, t.hostPort, "bracer_backup"); err != nil {
			return nil, err
		}
	}
	if err := r.connectLoop(client, &inst, t); err != nil {
		if errors.Is(err, errSkipInstance) {
			return nil, nil
		}
		return nil, err
	}
	// 3. Version → pg_major and system_id; pull postgres:N; re-pick the runner for the right major.
	_, major, err := client.ServerVersionNum(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("show server_version_num: %w", err)
	}
	inst.PgMajor = major
	if client.Runner, err = r.tools.Pick(r.ctx, t.where, major); err != nil {
		r.p.printf("Cannot back up PostgreSQL %d from here: %v\n", major, err)
		return nil, nil
	}
	if ir, ok := client.Runner.(pg.ImageRunner); ok {
		r.p.printf("Pulling %s (backup runs pg_dump from it)...\n", ir.Image)
		if err := r.docker.Pull(r.ctx, ir.Image); err != nil {
			r.p.printf("  ✗ %v\n  Fix: check this machine reaches the registry (docker pull %s), then run: %s\n", err, ir.Image, r.again)
			return nil, nil
		}
	}
	r.p.printf("Connected: PostgreSQL %d via %s\n", major, client.Runner.Describe())
	if id, err := client.SystemIdentifier(r.ctx); err == nil {
		inst.SystemID = id
	} else {
		r.p.printf("system_identifier is not readable here (%s); backup will not check that this stays the same cluster.\n", firstLine(err.Error()))
	}

	// 7. Pooler
	if ok, why := client.DetectPooler(r.ctx); ok {
		r.warn("this looks like a connection pooler (%s). pg_dump through a transaction-mode pooler fails; point bracer at Postgres directly.", why)
	}

	// 5. Databases (an instance picked again keeps its list).
	var selected []string
	if replaced != nil && !inst.Databases.All {
		for _, db := range inst.Databases.List {
			selected = append(selected, db.Name)
		}
		r.p.printf("Keeping the databases of %s: %s\n", inst.Name, strings.Join(selected, ", "))
	} else {
		dbs, err := client.ListDatabases(r.ctx)
		if err != nil {
			return nil, fmt.Errorf("list databases: %w", err)
		}
		if len(dbs) == 0 {
			return nil, fmt.Errorf("no databases found")
		}
		if replaced != nil {
			selected = dbs
			r.p.printf("Keeping databases: all of %s (%s)\n", inst.Name, strings.Join(dbs, ", "))
		} else {
			r.p.printf("\nDatabases:\n")
			for i, db := range dbs {
				r.p.printf("  %d) %s\n", i+1, db)
			}
			all, dbIdx, err := r.p.chooseMany("Databases to back up (numbers separated by commas, or all)", len(dbs))
			if err != nil {
				return nil, err
			}
			if all {
				inst.Databases = config.Databases{All: true}
				selected = dbs
			} else {
				for _, i := range dbIdx {
					selected = append(selected, dbs[i])
					inst.Databases.List = append(inst.Databases.List, config.Database{Name: dbs[i]})
				}
			}
		}
	}

	// 6. Permissions
	kept, err := r.checkPermissions(client, &inst, t, selected)
	if err != nil {
		return nil, err
	}
	if !kept {
		return nil, nil
	}
	if replaced != nil {
		r.dropLegacy(replaced.Name)
	}
	// 11. Drill image
	if !r.noDrill {
		if err := r.chooseDrillImage(client, &inst, selected); err != nil {
			return nil, err
		}
	}

	if existing != nil {
		mergeDatabases(existing, inst)
		existing.PgMajor, existing.SystemID = inst.PgMajor, inst.SystemID
		if inst.PasswordEnv != "" {
			existing.PasswordEnv = inst.PasswordEnv
		}
		return nil, nil
	}
	return &inst, nil
}

// findSame returns the instance of app.yaml that points at the same Postgres:
// same container, or a URL to the same host:port.
func (r *initRun) findSame(inst config.Instance, t target) *config.Instance {
	for i := range r.cfg.Instances {
		ex := &r.cfg.Instances[i]
		switch {
		case inst.Container != "" && ex.Container == inst.Container:
			return ex
		case inst.Local != nil && ex.Local != nil && ex.Local.Port == inst.Local.Port && ex.Local.Dir() == inst.Local.Dir():
			return ex
		case t.isURL() && ex.URLEnv != "" && r.loaded != nil:
			v, err := r.loaded.Require(ex.URLEnv)
			if err != nil {
				continue
			}
			c, err := pg.ParseURL(v)
			if err != nil {
				continue
			}
			hp := t.hostPort
			if t.fromURL {
				hp = net.JoinHostPort(t.conn.Host, strconv.Itoa(t.conn.Port))
			}
			if net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) == hp {
				return ex
			}
		}
	}
	return nil
}

// pickLegacy asks whether the picked Postgres replaces an instance from before ADR-0013.
func (r *initRun) pickLegacy() (*config.Instance, error) {
	if len(r.legacy) == 0 {
		return nil, nil
	}
	r.p.printf("\nIs this the new way to reach an instance that needs to be picked again? It keeps its name, databases and retention.\n")
	for i, in := range r.legacy {
		r.p.printf("  %d) %s (%s)\n", i+1, in.Name, in.Legacy())
	}
	r.p.printf("  n) no, a new instance\n")
	idx, key, err := r.p.choose("Pick (number or n)", len(r.legacy), "n")
	if err != nil || key == "n" {
		return nil, err
	}
	in := r.legacy[idx]
	return &in, nil
}

func (r *initRun) dropLegacy(name string) {
	for i, in := range r.legacy {
		if in.Name == name {
			r.legacy = append(r.legacy[:i], r.legacy[i+1:]...)
			return
		}
	}
}

// askHostLogin asks user and password for Postgres at hostPort, builds the URL
// (percent-encoded) into the instance's secret and the client's Conn.
func (r *initRun) askHostLogin(client *pg.Client, inst *config.Instance, hostPort, defUser string) error {
	u, err := r.p.ask("User", defUser)
	if err != nil {
		return err
	}
	pw, err := r.p.askSecret("Password")
	if err != nil {
		return err
	}
	raw := buildURL(u, pw, hostPort, "postgres")
	c, err := pg.ParseURL(raw)
	if err != nil {
		return err
	}
	client.Conn = c
	r.secrets[inst.URLEnv] = raw
	return nil
}

// buildURL is postgres://user:password@host:port/db with user and password percent-encoded.
func buildURL(user, password, hostPort, db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: hostPort, Path: "/" + db}
	return u.String()
}

// errSkipInstance: the user cannot go on with this instance; skip it, write nothing.
var errSkipInstance = errors.New("skip instance")

var reHbaHost = regexp.MustCompile(`no pg_hba\.conf entry for host "([^"]+)"`)

// connectLoop tries to connect; on an auth failure it asks (user and password
// for host:port, password for a container or pasted URL), other errors offer a retry.
func (r *initRun) connectLoop(client *pg.Client, inst *config.Instance, t target) error {
	for {
		err := client.Ping(r.ctx)
		if err == nil {
			return nil
		}
		if isNoSuchDB(err) && client.Conn.Database == "postgres" {
			client.Conn.Database = "template1"
			if t.isURL() {
				r.secrets[inst.URLEnv] = withDatabase(r.secrets[inst.URLEnv], "template1")
			}
			continue
		}
		if m := reHbaHost.FindStringSubmatch(err.Error()); m != nil {
			where := ""
			if t.hostPort != "" {
				where = fmt.Sprintf(" (find it with: %s -Atc 'show hba_file')", sudoPsql(t.hostPort))
			}
			r.p.printf("Postgres refused the connection: pg_hba.conf has no line for %s.\nAdd this line to pg_hba.conf%s, then reload Postgres (sudo systemctl reload postgresql, or SELECT pg_reload_conf()):\n\n  host all %s %s/32 scram-sha-256\n\n", m[1], where, orAll(client.Conn.User), m[1])
			_, key, err := r.p.choose("[r] retry  [q] quit", 0, "r", "q")
			if err != nil {
				return err
			}
			if key == "q" {
				return errQuit
			}
			continue
		}
		if pg.IsAuthError(err) && t.hostPort != "" {
			r.p.printf("Authentication failed: %s\nNo user with a password yet? Create one with `%s`:\n%s\n", firstLine(err.Error()), sudoPsql(t.hostPort), indent(pg.FixRoleSQL("bracer_backup", 0), "  "))
			if err := r.askHostLogin(client, inst, t.hostPort, client.Conn.User); err != nil {
				return err
			}
			continue
		}
		if pg.IsAuthError(err) {
			r.p.printf("Authentication required.\n")
			pw, err := r.p.askSecret("Password")
			if err != nil {
				return err
			}
			client.Conn.Password = pw
			if inst.URLEnv == "" {
				inst.PasswordEnv = config.EnvVarName(r.cfg.App, inst.Name, "password")
				r.secrets[inst.PasswordEnv] = pw
			} else {
				// URL without a password: store the full URL.
				r.secrets[inst.URLEnv] = withPassword(r.secrets[inst.URLEnv], pw)
			}
			continue
		}
		if isNoSuchRole(err) && t.hostPort == "" && inst.URLEnv == "" {
			// The image runs with a different POSTGRES_USER that init could not read.
			r.p.printf("User %s does not exist on this server; it was probably started with another POSTGRES_USER.\n", client.Conn.User)
			u, err := r.p.ask("User", client.Conn.User)
			if err != nil {
				return err
			}
			client.Conn.User, inst.User = u, u
			continue
		}
		r.p.printf("Could not connect: %v\n", err)
		again, aerr := r.p.yesNo("Try again?", true)
		if aerr != nil {
			return aerr
		}
		if !again {
			return errSkipInstance
		}
	}
}

func orAll(user string) string {
	if user == "" {
		return "all"
	}
	return user
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// psqlCommand opens psql on the instance for the user to run the SQL (spec init
// AC permissions): no secret in it, psql asks for the password.
func (r *initRun) psqlCommand(inst config.Instance, conn pg.Conn, db string) string {
	if inst.Container != "" {
		return fmt.Sprintf("docker exec -it %s psql -U %s %s", inst.Container, orDefault(conn.User, "postgres"), db)
	}
	return fmt.Sprintf("docker run --rm -it --network host %s psql -h %s -p %d -U %s %s", pg.ImageForMajor(inst.PgMajor), conn.Host, conn.Port, orDefault(conn.User, "postgres"), db)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// checkPermissions is step 6 for each database. Returns kept=false if the user drops the instance.
func (r *initRun) checkPermissions(client *pg.Client, inst *config.Instance, t target, dbs []string) (bool, error) {
	priv, err := client.Privileges(r.ctx)
	if err != nil {
		return false, fmt.Errorf("check user: %w", err)
	}
	r.p.printf("\nUser %s: superuser=%v pg_read_all_data=%v bypassrls=%v\n", priv.Role, priv.Superuser, priv.ReadAllData, priv.BypassRLS)
	// backup records shared_preload_libraries for restore to check the target
	// (ADR-0014); without pg_read_all_settings it cannot, which only warns.
	if !priv.Superuser {
		if _, err := client.PreloadLibraries(r.ctx); err != nil {
			r.p.printf("  ! %s cannot read shared_preload_libraries, so restore will not be able to check the libraries a target must load.\n    Fix (as a superuser): GRANT pg_read_all_settings TO %s;\n", priv.Role, pg.QuoteIdent(priv.Role))
		}
	}
	var skipped []string
	for i := 0; i < len(dbs); i++ {
		db := dbs[i]
		c := client.WithDB(db)
		var problem string
		if rls, err := c.RLSTables(r.ctx); err != nil {
			problem = fmt.Sprintf("cannot read pg_class: %v", err)
		} else if len(rls) > 0 && !priv.Superuser && !priv.BypassRLS {
			problem = fmt.Sprintf("%d table(s) have row-level security (e.g. %s) and user %s lacks BYPASSRLS; pg_dump would fail", len(rls), rls[0], priv.Role)
		} else if err := c.SchemaOnlyDump(r.ctx); err != nil {
			problem = fmt.Sprintf("pg_dump --schema-only failed: %v", err)
		}
		if problem == "" {
			r.p.printf("  ✓ %s\n", db)
			continue
		}
		r.p.printf("  ✗ %s: %s\n", db, problem)
		fix := ""
		if !priv.CanDump() {
			fix = fmt.Sprintf("Run this as a superuser to create a backup user, then use it here:\n\n  %s\n%s\n", r.psqlCommand(*inst, client.Conn, db), indent(pg.FixRoleSQL("bracer_backup", inst.PgMajor), "  "))
			r.p.printf("\n%s\n", fix)
		}
		_, key, err := r.p.choose("[r] retry  [w] write anyway  [s] skip this database  [d] drop this instance  [q] quit", 0, "r", "w", "s", "d", "q")
		if err != nil {
			return false, err
		}
		switch key {
		case "r":
			i--
		case "w":
		case "s":
			skipped = append(skipped, db)
		case "d":
			return false, nil
		case "q":
			if fix != "" {
				r.p.printf("\n%s", fix)
			}
			return false, errQuit
		}
	}
	if len(skipped) > 0 {
		if inst.Databases.All {
			inst.Databases = config.Databases{}
			for _, db := range dbs {
				if !containsStr(skipped, db) {
					inst.Databases.List = append(inst.Databases.List, config.Database{Name: db})
				}
			}
		} else {
			var keep []config.Database
			for _, d := range inst.Databases.List {
				if !containsStr(skipped, d.Name) {
					keep = append(keep, d)
				}
			}
			inst.Databases.List = keep
		}
		if len(inst.Databases.List) == 0 {
			r.p.printf("No database left for this instance; skipping it.\n")
			return false, nil
		}
	}
	return true, nil
}

// setupStorage is step 8.
func (r *initRun) setupStorage() error {
	r.p.printf("\nBackups go to an S3-compatible bucket you own (R2, B2, Wasabi, MinIO...).\n")
	for {
		endpoint, err := r.p.askRequired("S3 endpoint URL (e.g. https://<account>.r2.cloudflarestorage.com)")
		if err != nil {
			return err
		}
		bucket, err := r.p.askRequired("Bucket")
		if err != nil {
			return err
		}
		prefix, err := r.p.ask("Prefix inside the bucket", "bracer")
		if err != nil {
			return err
		}
		region, err := r.p.ask("Region", "auto")
		if err != nil {
			return err
		}
		pathStyle, err := r.p.yesNo("Path-style URLs (needed for MinIO/Garage)?", false)
		if err != nil {
			return err
		}
		akEnv := config.EnvVarName(r.cfg.App, "s3", "access_key")
		skEnv := config.EnvVarName(r.cfg.App, "s3", "secret_key")
		ak, err := r.p.askSecret("Access key")
		if err != nil {
			return err
		}
		sk, err := r.p.askSecret("Secret key")
		if err != nil {
			return err
		}
		st, err := r.newStore(storage.S3Config{Endpoint: endpoint, Bucket: bucket, Region: region, AccessKey: ak, SecretKey: sk, PathStyle: pathStyle})
		if err == nil {
			err = st.Check(r.ctx)
		}
		if err == nil {
			r.p.printf("  ✓ bucket reachable\n")
			r.cfg.Storage = &config.Storage{Endpoint: endpoint, Bucket: bucket, Prefix: prefix, Region: region, AccessKeyEnv: akEnv, SecretKeyEnv: skEnv, PathStyle: pathStyle}
			r.secrets[akEnv] = ak
			r.secrets[skEnv] = sk
			return nil
		}
		r.p.printf("  ✗ %v\n", err)
		// No skip: without a bucket backup has nowhere to go.
		_, key, err := r.p.choose("[r] fix and retry  [q] quit", 0, "r", "q")
		if err != nil {
			return err
		}
		if key == "q" {
			return errQuit
		}
	}
}

// setupEncryption is step 9.
func (r *initRun) setupEncryption() error {
	yes, err := r.p.yesNo("\nEncrypt backups with age? Protects the data from whoever can read the bucket (recommended)", true)
	if err != nil {
		return err
	}
	if !yes {
		r.cfg.Encryption = "none"
		r.p.printf("Backups will be stored unencrypted in the bucket.\n")
		return nil
	}
	pub, err := r.p.ask("Paste an age public key (age1...), or press Enter to generate a new keypair", "")
	if err != nil {
		return err
	}
	defPath, err := r.configPath(r.cfg.App + ".key")
	if err != nil {
		return err
	}
	if pub == "" {
		kp, err := crypto.Generate()
		if err != nil {
			return err
		}
		for {
			path, err := r.p.ask("Where to store the private key", defPath)
			if err != nil {
				return err
			}
			full, err := absHome(path)
			if err != nil {
				return err
			}
			path = full
			err = crypto.WriteIdentityFile(full, kp)
			if errors.Is(err, crypto.ErrExists) {
				_, recips, rerr := crypto.ReadIdentities(full)
				if rerr == nil && len(recips) > 0 {
					reuse, err := r.p.yesNo(fmt.Sprintf("%s already exists (public key %s). Use that key instead?", path, recips[len(recips)-1]), true)
					if err != nil {
						return err
					}
					if reuse {
						r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", recips[len(recips)-1], path
						return nil
					}
				} else {
					r.p.printf("%s exists but is not an age identity file.\n", path)
				}
				continue
			}
			if err != nil {
				return err
			}
			r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", kp.Recipient, path
			r.p.printf("\nPrivate key written to %s (mode 600). Shown once, never logged:\n\n  %s\n\nBack it up in a password manager now. If this file is lost, the backups cannot be decrypted.\n\n", path, kp.Identity)
			// Without the key off this machine, encrypted backups die with it (ADR-0015).
			for {
				ans, err := r.p.ask(fmt.Sprintf("Copy %s somewhere off this machine (password manager, another disk). Without it the encrypted backups cannot be opened if this machine is lost. Type copied when done", path), "")
				if err != nil {
					return err
				}
				if strings.TrimSpace(ans) == "copied" {
					return nil
				}
			}
		}
	}
	recip, err := crypto.ParseRecipient(pub)
	if err != nil {
		return err
	}
	path, err := r.p.ask("Path to the matching private key (drill needs it to restore)", defPath)
	if err != nil {
		return err
	}
	if full, err := absHome(path); err == nil {
		path = full
		if _, recips, rerr := crypto.ReadIdentities(full); rerr != nil {
			r.warn("%s is not readable (%v); drill will not run until the private key is there", path, rerr)
		} else if !containsStr(recips, recip) {
			r.warn("%s does not contain the private key for %s; drill will not be able to decrypt", path, recip)
		}
	}
	r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", recip, path
	return nil
}

// storeSecrets is step 10: no question, secrets_file is the one source init
// writes (absolute, chmod 600) and the one the timers read.
func (r *initRun) storeSecrets() error {
	if len(r.secrets) == 0 {
		return nil
	}
	path := r.cfg.SecretsFile
	if path == "" {
		var err error
		if path, err = r.configPath(r.cfg.App + ".env"); err != nil {
			return err
		}
	}
	path, err := absHome(path)
	if err != nil {
		return err
	}
	kept, err := config.MergeEnvFile(path, r.secrets, func(name string) bool {
		ok, err := r.p.yesNo(fmt.Sprintf("%s already has a different value in %s. Overwrite?", name, path), false)
		return err == nil && ok
	})
	if err != nil {
		return err
	}
	for _, k := range kept {
		r.warn("kept the existing value of %s in %s", k, path)
	}
	names := make([]string, 0, len(r.secrets))
	for k := range r.secrets {
		names = append(names, k)
	}
	sort.Strings(names)
	r.cfg.SecretsFile = path
	r.p.printf("Secrets saved to %s (mode 600): %s. They are never written to %s.\n", path, strings.Join(names, ", "), r.path)
	return nil
}

// configPath is <home>/.config/bracer/<name>, absolute: the timers run without
// a shell, so app.yaml must not depend on `~` (spec init AC secrets).
func (r *initRun) configPath(name string) (string, error) {
	home, err := r.home()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".config", "bracer", name), nil
}

// absHome expands `~` and makes the path absolute.
func absHome(p string) (string, error) {
	p, err := config.ExpandHome(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// selfCheck is step 14: backup --dry-run and drill --dry-run as the timer will
// run them, with only HOME and runtime.path in the environment.
func (r *initRun) selfCheck() error {
	home, err := r.home()
	if err != nil {
		return fmt.Errorf("home directory: %w", err)
	}
	env := []string{"HOME=" + home, "PATH=" + r.cfg.Runtime.Path}
	runs := [][]string{{"backup", "--dry-run", "-c", r.abs}}
	if !r.cfg.DrillOff() {
		runs = append(runs, []string{"drill", "--dry-run", "-c", r.abs})
	}
	for _, args := range runs {
		r.p.printf("\nChecking as the nightly job will run: bracer %s\n", strings.Join(args, " "))
		if code := r.runSelf(r.ctx, args, env, r.stdout, r.stderr); code != exitOK {
			r.p.printf("\nSelf-check failed (exit %d). %s is kept; fix the above, then run: %s\n", code, r.abs, r.again)
			return errSelfCheck
		}
	}
	return nil
}

// offerSchedule is step 15.
func (r *initRun) offerSchedule() error {
	bin := stableBinaryPath()
	if !r.env.hasSystemd() {
		r.p.printf("\nNo systemd on this machine. For a crontab with the nightly jobs run:\n  %s schedule print -c %s\n", bin, r.abs)
		return nil
	}
	cmd := []string{"sudo", bin, "schedule", "install", "-c", r.abs}
	yes, err := r.p.yesNo(fmt.Sprintf("\nInstall the nightly schedule now? It runs: %s", strings.Join(cmd, " ")), true)
	if err != nil {
		return err
	}
	if !yes {
		r.p.printf("To install it later:\n  %s\n", strings.Join(cmd, " "))
		return nil
	}
	if err := r.runTTY(r.ctx, cmd[0], cmd[1:]...); err != nil {
		r.p.printf("%s failed: %v\nRun it again when ready:\n  %s\n", cmd[0], err, strings.Join(cmd, " "))
		return errSelfCheck
	}
	return nil
}

// execSelf runs this binary for the self-check with exactly env.
func execSelf(ctx context.Context, args, env []string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "bracer init: cannot find this binary:", err)
		return exitError
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, "bracer init:", err)
		return exitError
	}
	return exitOK
}

// execTTY runs a command on the terminal (sudo asks its password there).
func execTTY(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func mergeDatabases(dst *config.Instance, src config.Instance) {
	if dst.Databases.All || src.Databases.All {
		dst.Databases = config.Databases{All: true}
		return
	}
	for _, d := range src.Databases.List {
		found := false
		for _, e := range dst.Databases.List {
			if e.Name == d.Name {
				found = true
			}
		}
		if !found {
			dst.Databases.List = append(dst.Databases.List, d)
		}
	}
}

func isNoSuchDB(err error) bool {
	var ee *pg.ExitError
	return errors.As(err, &ee) && strings.Contains(string(ee.Stderr), `database "`) && strings.Contains(string(ee.Stderr), "does not exist")
}

// isNoSuchRole: FATAL: role "x" does not exist.
func isNoSuchRole(err error) bool {
	var ee *pg.ExitError
	return errors.As(err, &ee) && strings.Contains(string(ee.Stderr), `role "`) && strings.Contains(string(ee.Stderr), "does not exist")
}

// withPassword inserts the password into the URL postgres://user@host → postgres://user:pw@host.
func withPassword(rawURL, pw string) string {
	c, err := pg.ParseURL(rawURL)
	if err != nil {
		return rawURL
	}
	scheme, rest, _ := strings.Cut(rawURL, "://")
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return fmt.Sprintf("%s://%s:%s@%s", scheme, c.User, escapeURLPart(pw), rest)
}

func escapeURLPart(s string) string {
	repl := strings.NewReplacer("%", "%25", "@", "%40", ":", "%3A", "/", "%2F", "?", "%3F", "#", "%23", " ", "%20")
	return repl.Replace(s)
}

func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", 0, false
	}
	var port int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return strings.Trim(s[:i], "[]"), port, true
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// withDatabase replaces the database in a postgres:// URL.
func withDatabase(rawURL, db string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Path = "/" + db
	return u.String()
}

// chooseDrillImage fills drill.image (spec init § Flow 11) so the user does not
// have to know this field: container → the running image if it is of the official
// family (evidence: docker image inspect); URL → drill picks by the extensions in
// the manifest, asking only when an extension is outside the mapping table and
// missing from the default image.
func (r *initRun) chooseDrillImage(client *pg.Client, inst *config.Instance, dbs []string) error {
	if r.cfg.Drill != nil && r.cfg.Drill.Image != "" {
		return nil
	}
	set := func(image, why string) {
		if r.cfg.Drill == nil {
			r.cfg.Drill = &config.Drill{}
		}
		r.cfg.Drill.Image = image
		r.p.printf("drill will use image %s (%s).\n", image, why)
	}
	if inst.Local != nil {
		r.p.printf("drill will build an image like this cluster (postgres:%d with its extension packages and locales).\n", inst.PgMajor)
		return nil
	}
	if inst.Container != "" {
		official, err := r.docker.ImageOfficialFamily(r.ctx, r.srcImage)
		if err != nil {
			r.warn("cannot inspect image %s (%v); drill will pick an image from the extensions in use", r.srcImage, err)
			return nil
		}
		if official {
			r.p.printf("drill will use the same image as this Postgres (%s).\n", r.srcImage)
		} else {
			r.p.printf("Image %s is not built on the official postgres image: drill cannot run it. Set drill.image in %s to an official-family image with the same extensions.\n", r.srcImage, r.path)
		}
		return nil
	}
	// URL: no image to copy. If mappable (postgis…) drill picks on its
	// own; otherwise check against evidence in the default image instead of asking
	// the user which extension is contrib.
	var exts []pg.Extension
	for _, db := range dbs {
		e, err := client.WithDB(db).Extensions(r.ctx)
		if err != nil {
			return fmt.Errorf("read extensions of %s: %w", db, err)
		}
		exts = append(exts, e...)
	}
	if len(exts) == 0 {
		return nil
	}
	def := pg.ImageForMajor(inst.PgMajor)
	image, err := pg.ImageForExtensions(inst.PgMajor, exts)
	if err != nil {
		r.p.printf("%v\n", err)
	} else if image != def {
		return nil // drill will pick the dedicated image from the manifest
	}
	have, err := r.docker.ImageExtensions(r.ctx, def)
	if err != nil {
		r.warn("cannot list extensions of %s (%v); drill checks them before restoring", def, err)
		return nil
	}
	seen := map[string]bool{}
	var missing []string
	for _, e := range exts {
		if !have[e.Name] && !seen[e.Name] {
			seen[e.Name] = true
			missing = append(missing, e.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	r.p.printf("\nDatabases use extensions %s, which %s does not have; drill needs an image that has them (it stops before restoring if one is missing).\n", strings.Join(missing, ", "), def)
	s, err := r.p.ask("Docker image with these extensions (empty: leave drill.image unset)", "")
	if err != nil {
		return err
	}
	if s = strings.TrimSpace(s); s != "" {
		set(s, "your choice")
	}
	return nil
}
