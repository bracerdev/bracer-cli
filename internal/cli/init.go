package cli

import (
	"context"

	"bracer.dev/bracer/internal/backup"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

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
	goos     string
	lookPath func(string) (string, error) // nil: exec.LookPath; tests block kubectl/host binaries
	glob     func(string) ([]string, error)
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
}

// initRun holds the state of one init run.
type initRun struct {
	initDeps
	ctx     context.Context
	p       *prompter
	tools   *pg.Tools
	docker  pg.Docker
	cfg     *config.App
	path    string
	fresh   bool              // app.yaml does not exist yet
	secrets map[string]string // variable name → collected value, not written anywhere yet
	verbose bool
	// srcImage/srcKind: image of the chosen container or pod, used to fill drill.image (step 11b).
	srcImage string
	srcKind  string
}

// runInit is `bracer init` (spec backup/init § Flow).
func runInit(ctx context.Context, args []string, d initDeps) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	path := fs.String("c", "app.yaml", "path of app.yaml to create or extend")
	verbose := fs.Bool("verbose", false, "print every docker, kubectl and pg_* command")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = *verbose
		d.shell = sh
	}
	r := &initRun{initDeps: d, ctx: ctx, p: newPrompter(d.stdin, d.stdout), path: *path, verbose: *verbose,
		docker: pg.Docker{Shell: d.shell}, secrets: map[string]string{}}
	r.tools = &pg.Tools{Shell: d.shell, GOOS: d.goos, LookPath: d.lookPath, Glob: d.glob}
	err := r.run()
	switch {
	case err == nil:
		return exitOK
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

// usageError is a usage error (exit 2): missing Docker, broken app.yaml…
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usageErrorf(format string, args ...interface{}) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

func (r *initRun) warn(format string, args ...interface{}) {
	fmt.Fprintf(r.stderr, "warning: "+format+"\n", args...)
}

func (r *initRun) run() error {
	if os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
		r.warn("running under sudo: key and env files will belong to root; a cron job running as %s will not be able to read them", os.Getenv("SUDO_USER"))
	}
	if err := r.loadOrCreate(); err != nil {
		return err
	}
	// 1. Docker: optional (ADR-0009); without it containers are not listed and drill does not run on this machine.
	if !r.tools.DockerOK(r.ctx) {
		r.warn("Docker is not available (`docker version` failed): Docker containers will not be listed, and `bracer drill` will not run on this machine")
	}
	if !r.fresh {
		r.recheckExisting()
	}
	// 2–8. Instance
	inst, err := r.pickInstance()
	if err != nil {
		return err
	}
	if inst != nil {
		r.addInstance(*inst)
	}
	// 9. Bucket
	if r.cfg.Storage == nil {
		if err := r.setupStorage(); err != nil {
			return err
		}
	}
	// 10. Encryption
	if r.cfg.Encryption == "" {
		if err := r.setupEncryption(); err != nil {
			return err
		}
	}
	// 11. Secrets
	if err := r.storeSecrets(); err != nil {
		return err
	}
	// 12. Write
	if err := config.Save(r.path, r.cfg); err != nil {
		return err
	}
	r.p.printf("\nWrote %s.\n\nNext steps:\n  bracer backup --dry-run -c %s\n  bracer cron-example -c %s\n", r.path, r.path, r.path)
	return nil
}

// loadOrCreate reads an existing app.yaml (merge) or asks for the app name.
func (r *initRun) loadOrCreate() error {
	cfg, err := config.Load(r.path)
	switch {
	case err == nil:
		r.cfg = cfg
		r.p.printf("Found %s (app %q, %d instance(s)); new choices will be added to it.\n", r.path, cfg.App, len(cfg.Instances))
		return nil
	case os.IsNotExist(err):
		r.fresh = true
		wd, _ := os.Getwd()
		def := sanitizeName(filepath.Base(wd))
		for {
			name, err := r.p.ask("App name (lowercase, used in bucket paths and variable names)", def)
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
	secrets, warns, err := config.LoadSecrets(r.cfg, "")
	for _, w := range warns {
		r.warn("%s", w)
	}
	if err != nil {
		r.warn("could not load secrets to re-test existing instances: %v", err)
		return
	}
	for _, in := range r.cfg.Instances {
		conn, runner, err := backup.Connect(r.ctx, r.tools, in, secrets)
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

// pickInstance is steps 2–8: discover, pick, connect, version, databases, permissions, pooler.
// Returns nil if the user skips this instance.
func (r *initRun) pickInstance() (*config.Instance, error) {
	r.p.printf("\nLooking for Postgres (Docker containers, Kubernetes pods, local ports, sockets)...\n")
	var containers []*pg.Container
	if r.tools.DockerOK(r.ctx) {
		var err error
		containers, err = r.docker.ListPostgresContainers(r.ctx)
		if err != nil {
			return nil, err
		}
	}
	k8s := 0
	for _, c := range containers {
		if c.Kubernetes {
			k8s++
		}
	}
	var pods []*pg.Pod
	kube := r.tools.Kubectl()
	if kube != nil {
		var err error
		pods, err = kube.ListPostgresPods(r.ctx)
		if err != nil {
			if hint := pg.KubeHint(err); hint != "" {
				r.warn("could not list Kubernetes pods: %v\n  %s", err, hint)
			} else {
				r.warn("could not list Kubernetes pods: %v", err)
			}
		}
	}
	// A pod with only env hints (app pods connecting to a DB have them too) whose
	// process could not be checked: no conclusion → not listed, only noted in one line.
	var unchecked []string
	pods = slices.DeleteFunc(pods, func(p *pg.Pod) bool {
		if p.Unverified && p.EnvOnly {
			unchecked = append(unchecked, p.Ref.String())
			return true
		}
		return false
	})
	hs := pg.ScanHost(r.ctx, r.shell, r.tools.GOOS)
	shown, warns := pg.Discover(r.ctx, containers, pods, hs, r.probe)
	for _, w := range warns {
		r.warn("%s", w)
	}
	r.p.printf("\n")
	for i, c := range shown {
		r.p.printf("  %d) %s\n", i+1, c.Label())
	}
	if k8s > 0 && kube == nil {
		r.p.printf("  -  %d Kubernetes container(s) seen through Docker; install kubectl (or run on a node that has k3s) so bracer can list the pods\n", k8s)
	}
	if len(unchecked) > 0 {
		r.p.printf("  -  %d pod(s) have Postgres-like variables but could not be checked for a postgres process (no shell in the container): %s; if one of them is your server, enter it as namespace/pod/container\n", len(unchecked), strings.Join(unchecked, ", "))
	}
	if len(shown) == 0 {
		r.p.printf("  No Postgres found.\n")
	}
	r.p.printf("  m) enter host:port, a postgres:// URL, or namespace/pod[/container] manually\n")
	var cand *pg.Candidate
	idx, key, err := r.p.choose("Pick an instance (number or m)", len(shown), "m")
	if err != nil {
		return nil, err
	}
	if key == "" {
		cand = &shown[idx]
	}

	inst := config.Instance{}
	conn := pg.Conn{Database: "postgres"}
	var where pg.Where
	switch {
	case cand != nil && cand.Container != nil:
		ct := cand.Container
		if !ct.Running {
			r.p.printf("Container %s is stopped; it cannot be tested now. Start it and run init again.\n", ct.Name)
			return nil, nil
		}
		inst.Container = ct.Name
		r.srcImage, r.srcKind = ct.Image, "container"
		inst.User = ct.DefaultUser()
		conn.User = inst.User
		where = pg.Where{Container: ct.Name}
	case cand != nil && cand.Pod != nil:
		p := cand.Pod
		if !p.Ready {
			r.p.printf("Pod %s/%s is not ready; it cannot be tested now. Wait for it and run init again.\n", p.Ref.Namespace, p.Ref.Name)
			return nil, nil
		}
		inst.Pod = p.Ref.String()
		r.srcImage, r.srcKind = p.Image, "pod"
		inst.User = r.podUser(p)
		conn.User = inst.User
		where = pg.Where{Pod: p.Ref}
	case cand != nil && cand.Host == "":
		dir := filepath.Dir(cand.Socket)
		inst.Socket = dir
		if cand.Port != 5432 {
			inst.Port = cand.Port
		}
		conn.Host, conn.Port = dir, cand.Port
		inst.User, conn.User = "postgres", "postgres"
		where = pg.Where{SocketDir: dir}
	case cand != nil:
		host := cand.Host
		switch host {
		case "0.0.0.0", "::", "::1", "127.0.0.1":
			host = "localhost"
		}
		inst.Host, inst.Port = host, cand.Port
		conn.Host, conn.Port = host, cand.Port
		inst.User = "postgres"
		conn.User = inst.User
	default:
		s, err := r.p.askRequired("host:port, postgres://user:password@host:port/db?sslmode=…, or namespace/pod[/container]")
		if err != nil {
			return nil, err
		}
		if isPodRef(s) {
			// The image gave no sign to list it automatically: a postgres process is still required.
			p, err := r.verifyPod(s)
			if err != nil {
				r.p.printf("%v\n", err)
				return nil, nil
			}
			inst.Pod = p.Ref.String()
			r.srcImage, r.srcKind = p.Image, "pod"
			inst.User = r.podUser(p)
			conn.User = inst.User
			where = pg.Where{Pod: p.Ref}
		} else if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
			c, err := pg.ParseURL(s)
			if err != nil {
				return nil, err
			}
			conn = c
			inst.URLEnv = "pending" // named once the instance name is known
			r.secrets["__url__"] = s
		} else {
			host, port, ok := splitHostPort(s)
			if !ok {
				return nil, fmt.Errorf("expected host:port, got %q", s)
			}
			inst.Host, inst.Port = host, port
			conn.Host, conn.Port = host, port
			inst.User, conn.User = "postgres", "postgres" // asked again when the server requires authentication
		}
	}
	// 4. Runner per ADR-0009, major not known yet; re-picked after asking the version.
	runner, err := r.tools.Pick(r.ctx, where, 0)
	if err != nil {
		r.p.printf("Cannot reach this Postgres from here: %v\n", err)
		return nil, nil
	}
	if ir, ok := runner.(pg.ImageRunner); ok && where.Container != "" {
		r.p.printf("Container %s has no pg_dump; using %s on its network.\n", where.Container, ir.Image)
	}

	// Instance name; same target as an existing instance → add databases to it.
	var existing *config.Instance
	for i := range r.cfg.Instances {
		if sameTarget(r.cfg.Instances[i], inst) {
			existing = &r.cfg.Instances[i]
		}
	}
	if existing != nil {
		ok, err := r.p.yesNo(fmt.Sprintf("Instance %q in %s already points here. Add databases to it?", existing.Name, r.path), true)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		inst.Name = existing.Name
	} else {
		def := "local"
		if cand != nil {
			def = cand.SuggestName()
		} else if inst.Host != "" {
			def = sanitizeName(fmt.Sprintf("%s-%d", inst.Host, inst.Port))
		} else {
			def = "cloud"
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
		r.secrets[inst.URLEnv] = r.secrets["__url__"]
		delete(r.secrets, "__url__")
		inst.User = ""
	}

	// 5. Test connection; auth error → ask for user + password.
	client := &pg.Client{Runner: runner, Conn: conn}
	if err := r.connectLoop(client, &inst); err != nil {
		if errors.Is(err, errSkipInstance) {
			return nil, nil
		}
		return nil, err
	}
	// 4. Version → pg_major; re-pick the runner for the right major (host binary too old → image).
	_, major, err := client.ServerVersionNum(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("show server_version_num: %w", err)
	}
	inst.PgMajor = major
	if client.Runner, err = r.tools.Pick(r.ctx, where, major); err != nil {
		r.p.printf("Cannot back up PostgreSQL %d from here: %v\n", major, err)
		return nil, nil
	}
	r.p.printf("Connected: PostgreSQL %d via %s\n", major, client.Runner.Describe())

	// 8. Pooler
	if ok, why := client.DetectPooler(r.ctx); ok {
		r.warn("this looks like a connection pooler (%s). pg_dump through a transaction-mode pooler fails; point bracer at Postgres directly.", why)
	}

	// 6. Database
	dbs, err := client.ListDatabases(r.ctx)
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
	all, dbIdx, err := r.p.chooseMany("Databases to back up (numbers separated by commas, or all)", len(dbs))
	if err != nil {
		return nil, err
	}
	var selected []string
	if all {
		inst.Databases = config.Databases{All: true}
		selected = dbs
	} else {
		for _, i := range dbIdx {
			selected = append(selected, dbs[i])
			inst.Databases.List = append(inst.Databases.List, config.Database{Name: dbs[i]})
		}
	}

	// 7. Permissions
	kept, err := r.checkPermissions(client, &inst, selected)
	if err != nil {
		return nil, err
	}
	if !kept {
		return nil, nil
	}
	// 11b. Drill image
	if err := r.chooseDrillImage(client, &inst, selected); err != nil {
		return nil, err
	}

	if existing != nil {
		mergeDatabases(existing, inst)
		existing.PgMajor = inst.PgMajor
		if inst.PasswordEnv != "" {
			existing.PasswordEnv = inst.PasswordEnv
		}
		return nil, nil
	}
	return &inst, nil
}

// podUser picks the first user to try for a pod: env in the spec, env resolved
// inside the container or the OS user of the postgres process, then the default
// for the image (supabase_admin/postgres). If wrong, connectLoop asks when the
// server reports the user does not exist; no further guessing.
func (r *initRun) podUser(p *pg.Pod) string {
	if p.Env["POSTGRES_USER"] == "" && p.Env["POSTGRESQL_USERNAME"] == "" {
		if k := r.tools.Kubectl(); k != nil {
			if u := k.ContainerUser(r.ctx, p.Ref, p.PgPID); u != "" {
				return u
			}
		}
	}
	return p.DefaultUser()
}

// verifyPod takes a manually entered ns/pod[/container] and checks for a postgres process.
func (r *initRun) verifyPod(s string) (*pg.Pod, error) {
	ref, err := pg.ParsePodRef(s)
	if err != nil {
		return nil, err
	}
	k := r.tools.Kubectl()
	if k == nil {
		return nil, fmt.Errorf("kubectl (or k3s) was not found on this machine, so pod %s cannot be reached", s)
	}
	pid, err := k.PostgresProcess(r.ctx, ref)
	if err != nil {
		// The user named the pod themselves: do not block, but say clearly it is unverified.
		r.warn("could not look for a postgres process in %s (%v%s); continuing because you named it", s, err, hintSuffix(pg.KubeHint(err)))
		return &pg.Pod{Ref: ref, Ready: true, Unverified: true, Env: map[string]string{}}, nil
	}
	if pid == "" {
		return nil, fmt.Errorf("no postgres process is running in %s; pick the container that runs the server", s)
	}
	p := &pg.Pod{Ref: ref, Ready: true, HasPgProc: true, PgPID: pid, Env: map[string]string{}}
	// Image to fill drill.image (step 11b); if it cannot be read leave it empty, drill picks on its own.
	if img, err := k.PodImage(r.ctx, ref); err == nil {
		p.Image = img
	} else {
		r.warn("could not read the image of %s (%v); drill will pick an image from the extensions in use", s, err)
	}
	return p, nil
}

// isPodRef: ns/pod[/container], not a URL or host:port.
func isPodRef(s string) bool {
	return strings.Contains(s, "/") && !strings.Contains(s, "://") && !strings.Contains(s, ":")
}

func hintSuffix(h string) string {
	if h == "" {
		return ""
	}
	return " (" + h + ")"
}

// errSkipInstance: the user cannot go on with this instance; skip it, write nothing.
var errSkipInstance = errors.New("skip instance")

// connectLoop tries to connect without a password; on an auth failure it asks,
// other errors offer a retry.
func (r *initRun) connectLoop(client *pg.Client, inst *config.Instance) error {
	for {
		err := client.Ping(r.ctx)
		if err == nil {
			return nil
		}
		if isNoSuchDB(err) && client.Conn.Database == "postgres" {
			client.Conn.Database = "template1"
			continue
		}
		if pg.IsAuthError(err) && inst.Pod != "" {
			r.p.printf("Authentication required, but a password cannot be passed through kubectl exec (%v).\nPoint bracer at the service with a postgres:// URL instead (option m).\n", err)
			return errSkipInstance
		}
		if pg.IsAuthError(err) && inst.Socket != "" && strings.Contains(err.Error(), "Peer authentication failed") {
			r.p.printf("Peer authentication failed: over this socket Postgres only accepts the OS user of the same name.\nRun `sudo -u postgres bracer init` (key and env files will then belong to postgres, as should the cron job), or use a user with a password over TCP.\n")
		}
		if pg.IsAuthError(err) {
			r.p.printf("Authentication required.\n")
			if inst.URLEnv == "" {
				u, err := r.p.ask("User", client.Conn.User)
				if err != nil {
					return err
				}
				client.Conn.User, inst.User = u, u
			}
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
		if isNoSuchRole(err) && inst.URLEnv == "" {
			// The image runs with a different POSTGRES_USER that init could not read (Secret).
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
			return fmt.Errorf("connection failed")
		}
	}
}

// checkPermissions is step 7 for each database. Returns kept=false if the user drops the instance.
func (r *initRun) checkPermissions(client *pg.Client, inst *config.Instance, dbs []string) (bool, error) {
	priv, err := client.Privileges(r.ctx)
	if err != nil {
		return false, fmt.Errorf("check user: %w", err)
	}
	r.p.printf("\nUser %s: superuser=%v pg_read_all_data=%v bypassrls=%v\n", priv.Role, priv.Superuser, priv.ReadAllData, priv.BypassRLS)
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
		if !priv.CanDump() {
			r.p.printf("\nRun this as a superuser to create a backup user, then use it here:\n\n%s\n\n", pg.FixRoleSQL("bracer_backup", inst.PgMajor))
		}
		_, key, err := r.p.choose("[r] retry  [w] write anyway  [s] skip this database  [q] drop this instance", 0, "r", "w", "s", "q")
		if err != nil {
			return false, err
		}
		switch key {
		case "r":
			i--
		case "w":
		case "s":
			skipped = append(skipped, db)
		case "q":
			return false, nil
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

func (r *initRun) addInstance(in config.Instance) {
	r.cfg.Instances = append(r.cfg.Instances, in)
}

// setupStorage is step 9.
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
		_, key, err := r.p.choose("[r] fix and retry  [s] skip the bucket for now (backup will not run until storage: is set)", 0, "r", "s")
		if err != nil {
			return err
		}
		if key == "s" {
			return nil
		}
	}
}

// setupEncryption is step 10.
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
	defPath := r.configPath(r.cfg.App + ".key")
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
			full, err := config.ExpandHome(path)
			if err != nil {
				return err
			}
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
			return nil
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
	if full, err := config.ExpandHome(path); err == nil {
		if _, recips, rerr := crypto.ReadIdentities(full); rerr != nil {
			r.warn("%s is not readable (%v); drill will not run until the private key is there", path, rerr)
		} else if !containsStr(recips, recip) {
			r.warn("%s does not contain the private key for %s; drill will not be able to decrypt", path, recip)
		}
	}
	r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", recip, path
	return nil
}

// storeSecrets is step 11.
func (r *initRun) storeSecrets() error {
	if len(r.secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.secrets))
	for k := range r.secrets {
		names = append(names, k)
	}
	sort.Strings(names)
	defPath := r.cfg.SecretsFile
	if defPath == "" {
		defPath = r.configPath(r.cfg.App + ".env")
	}
	r.p.printf("\nSecrets collected: %s\nThey are never written to %s.\n", strings.Join(names, ", "), r.path)
	r.p.printf("  1) bracer keeps them in %s (mode 600) and loads it automatically\n  2) I manage them myself (pass --env-file or environment variables)\n", defPath)
	idx, _, err := r.p.choose("Where should secrets live? (1 or 2)", 2)
	if err != nil {
		return err
	}
	if idx == 1 {
		r.cfg.SecretsFile = ""
		r.p.printf("\nSet these before running bracer:\n")
		for _, n := range names {
			r.p.printf("  %s\n", n)
		}
		r.p.printf("\nEither:\n  bracer backup -c %s --env-file /path/to/%s.env\nor:\n  %s=... bracer backup -c %s\n", r.path, r.cfg.App, strings.Join(names, "=... "), r.path)
		return nil
	}
	path, err := r.p.ask("Env file path", defPath)
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
	r.cfg.SecretsFile = path
	r.p.printf("  ✓ wrote %s\n", path)
	return nil
}

// configPath returns ~/.config/bracer/<name> in its `~` form so app.yaml does
// not contain the absolute path of one machine.
func (r *initRun) configPath(name string) string {
	return filepath.Join("~", ".config", "bracer", name)
}

func sameTarget(a, b config.Instance) bool {
	switch {
	case a.Container != "" || b.Container != "":
		return a.Container == b.Container
	case a.Pod != "" || b.Pod != "":
		return a.Pod == b.Pod
	case a.Socket != "" || b.Socket != "":
		ad, ap := a.SocketDir()
		bd, bp := b.SocketDir()
		return ad == bd && ap == bp
	case a.Host != "" || b.Host != "":
		return a.Host == b.Host && a.Port == b.Port
	}
	return false
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

// chooseDrillImage fills drill.image (spec init § Flow 11b) so the user does not
// have to know this field: container → the running image if it is of the official
// family (evidence: docker image inspect); pod → the image in the pod spec except
// Bitnami; host → drill picks by the extensions in the manifest, asking only when an
// extension is outside the mapping table (it may be contrib; drill checks with pg_available_extensions).
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
	switch r.srcKind {
	case "container":
		official, err := r.docker.ImageOfficialFamily(r.ctx, r.srcImage)
		if err != nil {
			r.warn("cannot inspect image %s (%v); drill will pick an image from the extensions in use", r.srcImage, err)
			return nil
		}
		if official {
			set(r.srcImage, "the same image as this Postgres")
		} else {
			r.p.printf("Image %s is not built on the official postgres image; drill will pick an image from the extensions in use.\n", r.srcImage)
		}
		return nil
	case "pod":
		if r.srcImage != "" && !config.IsBitnamiImage(r.srcImage) {
			set(r.srcImage, "the same image as this pod")
		}
		return nil
	}
	// Host/socket/URL: no image to copy. If mappable (postgis…) drill picks on its
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
	if !r.tools.DockerOK(r.ctx) {
		return nil // drill does not run on this machine (warned in step 1)
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
