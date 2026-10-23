package cli

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// Steps 4–10 of init (spec init § Flow): find a Postgres, connect as the
// right user, read its version, pick databases, check what a backup needs.
// target is how the picked instance is reached while init talks to it.
type target struct {
	where pg.Where
	conn  pg.Conn
	// host:port of Postgres on this machine (or typed by hand): init asks a user
	// and password and builds the URL itself (spec init § Flow 4).
	hostPort string
	// fromURL: the user pasted a URL.
	fromURL bool
	// ct is the chosen container; pwFrom names the variable its superuser
	// password was taken from (Bitnami, ADR-0017).
	ct     *pg.Container
	pwFrom string
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

// pickInstance is steps 4–10: discover, pick, connect, version, databases,
// permissions, pooler. Returns nil when this instance is not written.
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
			r.p.printf("Container %s is stopped; it cannot be tested now. Start it and run %s again.\n", ct.Name, r.again)
			return nil, nil
		}
		inst.Container = ct.Name
		inst.User = ct.DefaultUser()
		t.conn.User = inst.User
		t.where = pg.Where{Container: ct.Name}
		t.ct = ct
	case cand != nil && cand.Local != nil:
		r.useLocal(&inst, &t, *cand.Local)
	case cand != nil:
		t.hostPort = net.JoinHostPort(strings.Trim(cand.URLHost(), "[]"), strconv.Itoa(cand.Port))
	default:
		// Typed by hand: always a URL source, even 127.0.0.1 with a socket
		// here; local: is only the line init found (ADR-0022).
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
	}
	if t.isURL() {
		inst.URLEnv = "pending" // named once the instance name is known
		r.p.printf("URL source: bracer backs up the databases only, not users, privileges or server settings.\n" +
			"To back up everything, pick the local Postgres or container line from the list, or run bracer on the machine where Postgres runs.\n")
	}
	// Runner per ADR-0013, major not known yet; re-picked after asking the version.
	runner, err := r.tools.Pick(r.ctx, t.where, 0)
	if err != nil {
		r.p.printf("Cannot reach this Postgres from here: %v\n", err)
		return nil, nil
	}
	if ir, ok := runner.(pg.ImageRunner); ok && t.where.Container != "" {
		r.p.printf("Container %s has no pg_dump; using %s on its network.\n", t.where.Container, ir.Image)
	}

	// Instance name.
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
	if inst.URLEnv == "pending" {
		inst.URLEnv = config.EnvVarName(r.cfg.App, inst.Name, "url")
	}
	if t.fromURL {
		r.secrets[inst.URLEnv] = r.secrets["__url__"]
		delete(r.secrets, "__url__")
	}

	// 7. Test connection; host:port → user + password → URL; auth error → ask.
	client := &pg.Client{Runner: runner, Conn: t.conn}
	if t.hostPort != "" {
		r.p.printf("\nPostgres on %s is reached over TCP with a user and a password.\n"+
			"If you only ever use `sudo -u postgres psql`, create a backup user first:\n\n  %s\n%s\n\n", t.hostPort, sudoPsql(t.hostPort), indent(pg.FixRoleSQL("bracer_backup", 0), "  "))
		if err := r.askHostLogin(client, &inst, t.hostPort, "bracer_backup"); err != nil {
			return nil, err
		}
	}
	// Bitnami asks for a password even on its socket: try the one its
	// environment gave postgres before asking (ADR-0017).
	if t.ct != nil && t.ct.IsBitnami() {
		pw, from, err := r.docker.SuperuserPassword(r.ctx, t.ct)
		if err != nil {
			r.p.printf("  ! %v\n", err)
		}
		if pw != "" {
			client.Conn.Password, t.pwFrom = pw, from
			r.setPassword(&inst, pw)
		}
	}
	if err := r.connectLoop(client, &inst, t); err != nil {
		if errors.Is(err, errSkipInstance) {
			r.forgetPassword(inst)
			return nil, nil
		}
		return nil, err
	}
	// A full backup needs the superuser (ADR-0017).
	if !t.isURL() {
		ok, err := r.ensureSuperuser(client, &inst)
		if err != nil {
			return nil, err
		}
		if !ok {
			r.forgetPassword(inst)
			return nil, nil
		}
	}
	// 6. Version → pg_major and system_id; pull postgres:N; re-pick the runner for the right major.
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
		// Managed servers refuse pg_control_system(): written as none (ADR-0022).
		inst.SystemID = config.SystemIDNone
		r.p.printf("system_identifier is not readable here (%s); system_id: none, so backup will not check that this stays the same cluster.\n", firstLine(err.Error()))
	}

	// 10. Pooler: pg_dump through a transaction-mode pooler fails (ADR-0019).
	if ok, why := client.DetectPooler(r.ctx); ok {
		r.p.printf("✗ this looks like a connection pooler (%s); the instance is not written.\n  Fix: point bracer at Postgres directly (the server's own host and port), then run: %s\n", why, r.again)
		r.forgetPassword(inst)
		return nil, nil
	}

	// 8. Databases: always written as a list of what was picked (ADR-0016, ADR-0024).
	selected, err := r.chooseDatabases(client, &inst)
	if err != nil {
		return nil, err
	}
	// 9. Permissions
	kept, err := r.checkPermissions(client, &inst, selected)
	if err != nil {
		return nil, err
	}
	if !kept {
		return nil, nil
	}
	return &inst, nil
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

// setPassword keeps the superuser password of a container or local instance
// for secrets_file.
func (r *initRun) setPassword(inst *config.Instance, pw string) {
	inst.PasswordEnv = config.EnvVarName(r.cfg.App, inst.Name, "password")
	r.secrets[inst.PasswordEnv] = pw
}

// forgetPassword drops the password of an instance that is not written.
func (r *initRun) forgetPassword(inst config.Instance) {
	if inst.PasswordEnv != "" {
		delete(r.secrets, inst.PasswordEnv)
	}
}

// ensureSuperuser: a container or local instance is backed up as its
// superuser only (ADR-0017). A connected user that is not one is replaced by
// the bootstrap superuser (oid 10) when that connects with the same
// credentials; otherwise the instance is not supported.
func (r *initRun) ensureSuperuser(client *pg.Client, inst *config.Instance) (bool, error) {
	priv, err := client.Privileges(r.ctx)
	if err != nil {
		return false, fmt.Errorf("check user: %w", err)
	}
	if priv.Superuser {
		return true, nil
	}
	if boot, err := client.BootstrapSuperuser(r.ctx); err == nil && boot != priv.Role {
		prev := client.Conn.User
		client.Conn.User = boot
		if client.Ping(r.ctx) == nil {
			if p2, err := client.Privileges(r.ctx); err == nil && p2.Superuser {
				r.p.printf("%s is not a superuser; using the superuser %s.\n", priv.Role, boot)
				inst.User = boot
				return true, nil
			}
		}
		client.Conn.User = prev
	}
	r.p.printf("✗ %s: not supported: a full backup (users, settings) needs the superuser, and %s is not one.\n", inst.Name, priv.Role)
	return false, nil
}

// errSkipInstance: the user cannot go on with this instance; skip it, write nothing.
var errSkipInstance = errors.New("skip instance")

var reHbaHost = regexp.MustCompile(`no pg_hba\.conf entry for host "([^"]+)"`)

// connectLoop tries to connect; on an auth failure it asks (user and password
// for host:port, password for a container or pasted URL), other errors offer a retry.
func (r *initRun) connectLoop(client *pg.Client, inst *config.Instance, t target) error {
	bitnamiNoPassword := t.ct != nil && t.ct.IsBitnami() && t.pwFrom == ""
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
			full := inst.URLEnv == ""
			switch {
			case t.pwFrom != "":
				r.p.printf("%s of %s is not the password %s uses now (Bitnami applies it only when the volume is created).\n", t.pwFrom, t.ct.Name, client.Conn.User)
				t.pwFrom = ""
			case bitnamiNoPassword:
				r.p.printf("%s sets no password for its superuser %s; enter it if you set one yourself (Enter to skip this instance).\n", t.ct.Name, client.Conn.User)
				bitnamiNoPassword = false
			case full:
				r.p.printf("Authentication required: password of the superuser %s (Enter to skip this instance).\n", client.Conn.User)
			default:
				r.p.printf("Authentication required.\n")
			}
			pw, err := r.p.askSecret("Password")
			if err != nil {
				return err
			}
			if full && pw == "" {
				return errSkipInstance
			}
			client.Conn.Password = pw
			if full {
				r.setPassword(inst, pw)
			} else {
				// URL without a password: store the full URL.
				r.secrets[inst.URLEnv] = withPassword(r.secrets[inst.URLEnv], pw)
			}
			continue
		}
		if isNoSuchRole(err) && t.hostPort == "" && inst.URLEnv == "" {
			// Container or local: never ask for a user (ADR-0017). A POSTGRES_USER
			// changed after the volume was created names no role; try postgres.
			if client.Conn.User != "postgres" {
				r.p.printf("User %s does not exist on this server; trying postgres.\n", client.Conn.User)
				client.Conn.User, inst.User = "postgres", "postgres"
				continue
			}
			if t.ct != nil {
				r.p.printf("✗ %s: cannot find its superuser: POSTGRES_USER=%s does not exist on this server (it was probably changed after the data volume was created) and there is no role postgres.\n  Fix: set POSTGRES_USER back to the user the volume was created with, then run: %s\n", t.ct.Name, orDefault(t.ct.Env["POSTGRES_USER"], "postgres"), r.again)
			} else {
				r.p.printf("✗ this Postgres has no role postgres: not supported (a full backup connects as postgres over the socket).\n")
			}
			return errSkipInstance
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

// checkPermissions is step 9 for each database. Returns kept=false if the user drops the instance.
func (r *initRun) checkPermissions(client *pg.Client, inst *config.Instance, dbs []string) (bool, error) {
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
			keep, err := r.acceptExternal(c, inst, db)
			if err != nil {
				return false, err
			}
			if !keep {
				skipped = append(skipped, db)
			}
			continue
		}
		r.p.printf("  ✗ %s: %s\n", db, problem)
		fix := ""
		if !priv.CanDump() {
			fix = fmt.Sprintf("Run this as a superuser to create a backup user, then use it here:\n\n  %s\n%s\n", r.psqlCommand(*inst, client.Conn, db), indent(pg.FixRoleSQL("bracer_backup", inst.PgMajor), "  "))
			r.p.printf("\n%s\n", fix)
		}
		_, key, err := r.p.choose("[r] retry  [s] skip this database  [d] drop this instance  [q] quit", 0, "r", "s", "d", "q")
		if err != nil {
			return false, err
		}
		switch key {
		case "r":
			i--
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
		var keep []config.Database
		for _, d := range inst.Databases {
			if !containsStr(skipped, d.Name) {
				keep = append(keep, d)
			}
		}
		inst.Databases = keep
		if len(inst.Databases) == 0 {
			r.p.printf("No database left for this instance; skipping it.\n")
			return false, nil
		}
	}
	return true, nil
}

// acceptExternal shows what in db depends on something outside Postgres
// (external, read like backup does) and has the customer accept it or skip
// the database (ADR-0019, ADR-0029). Accepted items are written under the
// database. keep is false for [s].
func (r *initRun) acceptExternal(c *pg.Client, inst *config.Instance, db string) (keep bool, err error) {
	ext, err := c.External(r.ctx)
	if err != nil {
		return false, fmt.Errorf("read %s for what depends on something outside Postgres: %w", db, err)
	}
	var entry *config.Database
	for i := range inst.Databases {
		if inst.Databases[i].Name == db {
			entry = &inst.Databases[i]
		}
	}
	if entry == nil {
		return true, nil
	}
	if len(ext) > 0 {
		r.p.printf("    %s depends on something outside Postgres; restore will bring these back but cannot promise they work:\n", db)
		for _, x := range ext {
			r.p.printf("      - %s\n", x)
		}
		_, key, err := r.p.choose("[a] accept: back it up, restore will not guarantee these  [s] skip this database  [q] quit", 0, "a", "s", "q")
		if err != nil {
			return false, err
		}
		switch key {
		case "s":
			return false, nil
		case "q":
			return false, errQuit
		}
	}
	entry.AcceptedExternal = ext
	return true, nil
}

// chooseDatabases is step 8: list the server's candidate databases, let the
// customer pick (default all), and write them as a list. Only what is picked
// is written (ADR-0016, ADR-0024).
func (r *initRun) chooseDatabases(client *pg.Client, inst *config.Instance) ([]string, error) {
	dbs, err := client.ListCandidates(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	if len(dbs) == 0 {
		return nil, fmt.Errorf("no databases found")
	}
	r.p.printf("\nDatabases:\n")
	for i, db := range dbs {
		r.p.printf("  %d) %s\n", i+1, db)
	}
	all, idx, err := r.p.chooseMany("Databases to back up (numbers separated by commas, or all)", len(dbs))
	if err != nil {
		return nil, err
	}
	var selected []string
	if all {
		selected = dbs
	} else {
		for _, i := range idx {
			selected = append(selected, dbs[i])
		}
	}
	inst.Databases = nil
	for _, n := range selected {
		inst.Databases = append(inst.Databases, config.Database{Name: n})
	}
	return selected, nil
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
