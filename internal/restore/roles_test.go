package restore

import (
	"context"
	"io"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/pg"
)

// catalogRunner answers psql -c by the first marker the SQL contains: rows
// joined with \x1f, each ended by NUL, as psql -A -t -0 prints them.
type catalogRunner struct {
	answers [][2]string // marker, rows ("\n" between rows)
}

func (r *catalogRunner) Run(_ context.Context, _ pg.Conn, _ string, args []string, _ io.Reader, stdout io.Writer) error {
	sql := args[len(args)-1]
	for _, a := range r.answers {
		if strings.Contains(sql, a[0]) {
			if a[1] != "" {
				io.WriteString(stdout, strings.ReplaceAll(a[1], "\n", "\x00")+"\x00")
			}
			return nil
		}
	}
	return nil
}
func (r *catalogRunner) Describe() string { return "fake" }

// readRoles runs ReadTargetRoles (PostgreSQL 17) on a target whose pg_roles
// holds roles ("name|validuntil", superuser postgres first) and whose
// pg_parameter_acl holds acl ("param|privilege|grantee|grantable").
func readRoles(t *testing.T, roles, acl []string) *TargetRoles {
	t.Helper()
	var rows []string
	for i, r := range roles {
		name, vu, _ := strings.Cut(r, "|")
		f := "f\x1ft\x1ff\x1ff\x1ft\x1ff\x1ff"
		if i == 0 {
			f = "t\x1ft\x1ft\x1ft\x1ft\x1ft\x1ft"
		}
		rows = append(rows, name+"\x1f"+f+"\x1f-1\x1f"+vu)
	}
	run := &catalogRunner{answers: [][2]string{
		{"rolvaliduntil", strings.Join(rows, "\n")},
		{"pg_parameter_acl", strings.ReplaceAll(strings.Join(acl, "\n"), "|", "\x1f")},
	}}
	tr, err := ReadTargetRoles(context.Background(), &pg.Client{Runner: run}, 17)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// BR-011 B6: infinity is no expiry; times are compared as instants whatever
// the time zone each server prints them in.
func TestBR011SameTime(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"infinity", "infinity", true},
		{"infinity", "", true},
		{"", "infinity", true},
		{" Infinity ", "", true},
		{"", "", true},
		{"-infinity", "-infinity", true},
		{"-infinity", "", false},
		{"-infinity", "infinity", false},
		{"infinity", "2030-01-01 03:00:00+00", false},
		{"2030-01-01 10:00:00+07", "2030-01-01 10:00:00+07", true},
		{"2030-01-01 10:00:00+07", "2030-01-01 03:00:00+00", true},
		{"2030-01-01 10:00:00+07:00", "2030-01-01 03:00:00+00", true},
		{"2030-01-01 10:00:00.5+07", "2030-01-01 03:00:00.5+00", true},
		{"2030-01-01 10:00:00.5+07", "2030-01-01 03:00:00+00", false},
		{"2030-01-01 08:30:00+05:30", "2030-01-01 03:00:00+00", true},
		{"2030-01-01 08:30:45+05:30:45", "2030-01-01 03:00:00+00", true},
		{"2029-12-31 22:00:00-05", "2030-01-01 03:00:00+00", true},
		{"2030-01-01T10:00:00+07", "2030-01-01 03:00:00 +00", true},
		{"2030-01-01 10:00:00+07", "2030-01-01 04:00:00+00", false},
		{"2020-01-01 00:00:00+00", "2020-01-01 00:00:00+00", true},
		{"2020-01-01 00:00:00+00", "2020-01-02 00:00:00+00", false},
		// Text that does not parse is compared as before: +00 is dropped.
		{"2030-01-01 03:00:00 BC+00", "2030-01-01 03:00:00 BC", true},
		{"tomorrow", "yesterday", false},
	}
	for _, c := range cases {
		if got := sameTime(c.a, c.b); got != c.want {
			t.Errorf("sameTime(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// BR-011 B6: alterRole asks for VALID UNTIL only when the instants differ,
// with the backup's value up and the target's down.
func TestBR011AlterRoleValidUntil(t *testing.T) {
	cases := []struct{ spec, tgt, up, down string }{
		{"infinity", "infinity", "", ""},
		{"infinity", "", "", ""},
		{"", "infinity", "", ""},
		{"", "", "", ""},
		{"infinity", "2030-01-01 03:00:00+00", "VALID UNTIL 'infinity'", "VALID UNTIL '2030-01-01 03:00:00+00'"},
		{"2030-01-01 10:00:00+07", "2030-01-01 10:00:00+07", "", ""},
		{"2030-01-01 10:00:00+07", "2030-01-01 03:00:00+00", "", ""},
		{"2030-01-01 10:00:00+07", "2030-01-01 04:00:00+00", "VALID UNTIL '2030-01-01 10:00:00+07'", "VALID UNTIL '2030-01-01 04:00:00+00'"},
		{"2030-01-01 10:00:00+07", "infinity", "VALID UNTIL '2030-01-01 10:00:00+07'", "VALID UNTIL 'infinity'"},
		{"2020-01-01 00:00:00+00", "2020-01-01 00:00:00+00", "", ""},
		{"2020-01-01 00:00:00+00", "2021-01-01 00:00:00+00", "VALID UNTIL '2020-01-01 00:00:00+00'", "VALID UNTIL '2021-01-01 00:00:00+00'"},
	}
	for _, c := range cases {
		spec, tgt := newRole("app"), newRole("app")
		spec.ValidUntil, tgt.ValidUntil = c.spec, c.tgt
		if up, down := alterRole(spec, tgt); up != c.up || down != c.down {
			t.Errorf("spec %q target %q: up %q down %q", c.spec, c.tgt, up, down)
		}
	}
}

// BR-011 B6: the target read keeps infinity (and every other value) as pg_roles prints it.
func TestBR011ReadTargetRolesValidUntil(t *testing.T) {
	tr := readRoles(t, []string{"postgres|", "inf|infinity", "neg|-infinity", "soon|2030-01-01 03:00:00+00"}, nil)
	for n, want := range map[string]string{"postgres": "", "inf": "infinity", "neg": "-infinity", "soon": "2030-01-01 03:00:00+00"} {
		if r := tr.Roles[n]; r == nil || r.ValidUntil != want {
			t.Errorf("%s: %+v", n, r)
		}
	}
}

// roleTarget is freshTarget with planSource's settings, so only users differ.
func roleTarget(t *testing.T) *TargetState {
	tgt := freshTarget(t)
	pre := "pg_stat_statements"
	tgt.Env.PreloadLibraries = &pre
	tgt.Env.Settings[0].Value = "ISO, DMY"
	return tgt
}

func validUntilPlan(t *testing.T, globals string, roles []string) *Plan {
	t.Helper()
	src, tgt := planSource(t), roleTarget(t)
	src.Globals = mustGlobals(t, superGlobals+globals)
	tgt.Roles = readRoles(t, roles, nil)
	return MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
}

func roleCheck(p *Plan, name string) *Check {
	for i, c := range p.Checks {
		if c.Area == "role" && c.Name == name {
			return &p.Checks[i]
		}
	}
	return nil
}

// BR-011 B6 through the compare a drill or restore runs: env-up creates the
// role with infinity; read back, it matches and nothing is left to do.
func TestBR011PlanValidUntilInfinity(t *testing.T) {
	const app = "CREATE ROLE app;\nALTER ROLE app WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS VALID UNTIL 'infinity';\n"
	p := validUntilPlan(t, app, []string{"postgres|"})
	if !has(p.Up, `CREATE ROLE "app" WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS VALID UNTIL 'infinity';`) {
		t.Errorf("env-up:\n%s", strings.Join(p.Up, "\n"))
	}
	for _, vu := range []string{"infinity", ""} {
		p = validUntilPlan(t, app, []string{"postgres|", "app|" + vu})
		if c := roleCheck(p, "app"); c == nil || !c.OK || c.Detail != "same or more" || strings.Contains(strings.Join(p.Up, "\n"), "VALID UNTIL") {
			t.Errorf("target %q: %+v\n%s", vu, c, strings.Join(p.Up, "\n"))
		}
	}
	// A real expiry on the target is a real difference.
	p = validUntilPlan(t, app, []string{"postgres|", "app|2030-01-01 03:00:00+00"})
	if c := roleCheck(p, "app"); c == nil || c.OK || c.Detail != "VALID UNTIL 'infinity'" || !has(p.Up, `ALTER ROLE "app" WITH VALID UNTIL 'infinity';`) ||
		!has(p.Down, `ALTER ROLE "app" WITH VALID UNTIL '2030-01-01 03:00:00+00';`) {
		t.Errorf("%+v\nup:\n%s\ndown:\n%s", c, strings.Join(p.Up, "\n"), strings.Join(p.Down, "\n"))
	}
}

// BR-011 B6: a source in +07 and a UTC target print the same expiry
// differently; another instant still fails with the backup's value.
func TestBR011PlanValidUntilTimeZone(t *testing.T) {
	const app = "CREATE ROLE app;\nALTER ROLE app WITH LOGIN VALID UNTIL '2030-01-01 10:00:00+07';\n"
	p := validUntilPlan(t, app, []string{"postgres|", "app|2030-01-01 03:00:00+00"})
	if c := roleCheck(p, "app"); c == nil || !c.OK || len(p.Up) != 0 {
		t.Errorf("same instant: %+v\n%s", c, strings.Join(p.Up, "\n"))
	}
	p = validUntilPlan(t, app, []string{"postgres|", "app|2030-01-01 04:00:00+00"})
	if c := roleCheck(p, "app"); c == nil || c.OK || c.Fix != FixSQL || !has(p.Up, `ALTER ROLE "app" WITH VALID UNTIL '2030-01-01 10:00:00+07';`) {
		t.Errorf("other instant: %+v\n%s", c, strings.Join(p.Up, "\n"))
	}
	// Past expiry: same is ok, different is a difference.
	const old = "CREATE ROLE app;\nALTER ROLE app WITH LOGIN VALID UNTIL '2020-01-01 00:00:00+00';\n"
	if c := roleCheck(validUntilPlan(t, old, []string{"postgres|", "app|2020-01-01 00:00:00+00"}), "app"); c == nil || !c.OK {
		t.Errorf("past same: %+v", c)
	}
	if c := roleCheck(validUntilPlan(t, old, []string{"postgres|", "app|2020-06-01 00:00:00+00"}), "app"); c == nil || c.OK {
		t.Errorf("past different: %+v", c)
	}
	// None on both sides.
	if c := roleCheck(validUntilPlan(t, "CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\n", []string{"postgres|", "app|"}), "app"); c == nil || !c.OK {
		t.Errorf("none: %+v", c)
	}
}

// BR-011 B7: PUBLIC is one grantee however the globals spell it. Since BR-015
// it is held as publicGrantee and the target read names it so too.
func TestBR011ParseParamGrantPublic(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"GRANT SET ON PARAMETER work_mem TO PUBLIC;", "work_mem SET public false"},
		{`GRANT SET ON PARAMETER work_mem TO "public";`, "work_mem SET public false"},
		{"GRANT SET ON PARAMETER work_mem TO app;", "work_mem SET app false"},
		{"GRANT SET ON PARAMETER work_mem TO PUBLIC, app;", "work_mem SET public false; work_mem SET app false"},
		{"GRANT SET ON PARAMETER work_mem TO PUBLIC;\nGRANT SET ON PARAMETER work_mem TO app;", "work_mem SET public false; work_mem SET app false"},
		{"GRANT ALTER SYSTEM ON PARAMETER Work_Mem TO PUBLIC;", "work_mem ALTER SYSTEM public false"},
		// PostgreSQL gives grant options to roles only, never to PUBLIC.
		{"GRANT ALTER SYSTEM ON PARAMETER work_mem TO app WITH GRANT OPTION;", "work_mem ALTER SYSTEM app true"},
		// A quoted mixed-case name is a role, not PUBLIC.
		{`GRANT SET ON PARAMETER work_mem TO "Public";`, "work_mem SET Public false"},
	}
	for _, c := range cases {
		var got []string
		for _, g := range mustGlobals(t, c.sql).ParamGrants {
			got = append(got, strings.Join([]string{g.Param, g.Privilege, g.Grantee, map[bool]string{true: "true", false: "false"}[g.GrantOption]}, " "))
		}
		if strings.Join(got, "; ") != c.want {
			t.Errorf("%s: %v", c.sql, got)
		}
	}
	// The parsed grant and the target's pg_parameter_acl row are one key.
	parsed := mustGlobals(t, "GRANT SET ON PARAMETER work_mem TO PUBLIC;").ParamGrants[0]
	tr := readRoles(t, []string{"postgres|"}, []string{"work_mem|SET|public|f"})
	if _, ok := tr.ParamGrants[parsed.key()]; !ok {
		t.Errorf("key %q not in %v", parsed.key(), tr.ParamGrants)
	}
	if (ParamGrant{Param: "work_mem", Privilege: "SET", Grantee: publicGrantee}).key() != parsed.key() {
		t.Error("a grant built with publicGrantee must have the PUBLIC key")
	}
}

func paramPlan(t *testing.T, globals string, acl []string) *Plan {
	t.Helper()
	src, tgt := planSource(t), roleTarget(t)
	src.Globals = mustGlobals(t, superGlobals+"CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\n"+globals)
	tgt.Roles = readRoles(t, []string{"postgres|", "app|"}, acl)
	return MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
}

func paramLines(stmts []string) string {
	var out []string
	for _, s := range stmts {
		if strings.Contains(s, " ON PARAMETER ") {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// BR-011 B7: a parameter grant to PUBLIC is written with the keyword PUBLIC,
// up and down, and a target that has it needs nothing.
func TestBR011PlanParamGrantPublic(t *testing.T) {
	const grant = "GRANT SET ON PARAMETER work_mem TO PUBLIC;\n"
	p := paramPlan(t, grant, []string{"work_mem|SET|public|f"})
	if c := roleCheck(p, "PUBLIC"); c != nil || len(p.Up) != 0 {
		t.Errorf("target has it: %+v\n%s", c, strings.Join(p.Up, "\n"))
	}
	p = paramPlan(t, grant, nil)
	if up, down := paramLines(p.Up), paramLines(p.Down); up != "GRANT SET ON PARAMETER work_mem TO PUBLIC;" || down != "REVOKE SET ON PARAMETER work_mem FROM PUBLIC;" {
		t.Errorf("up:\n%s\ndown:\n%s", up, down)
	}
	// A missing grant is still a difference, with the drill's message.
	if c := roleCheck(p, "PUBLIC"); c == nil || c.OK || c.Fix != FixSQL || c.Detail != "missing SET on parameter work_mem" {
		t.Errorf("check: %+v", c)
	}

	// PUBLIC and a named role: only the one the target lacks.
	const both = "GRANT SET ON PARAMETER work_mem TO PUBLIC, app;\n"
	p = paramPlan(t, both, []string{"work_mem|SET|public|f"})
	if up, down := paramLines(p.Up), paramLines(p.Down); up != `GRANT SET ON PARAMETER work_mem TO "app";` || down != `REVOKE SET ON PARAMETER work_mem FROM "app";` {
		t.Errorf("app missing:\n%s\n%s", up, down)
	}
	p = paramPlan(t, both, []string{"work_mem|SET|app|f"})
	if up := paramLines(p.Up); up != "GRANT SET ON PARAMETER work_mem TO PUBLIC;" {
		t.Errorf("PUBLIC missing:\n%s", up)
	}
	if p = paramPlan(t, both, []string{"work_mem|SET|public|f", "work_mem|SET|app|f"}); len(p.Up) != 0 || !p.OK() {
		t.Errorf("both there: %v\n%s", failed(p), strings.Join(p.Up, "\n"))
	}

	// ALTER SYSTEM, a parameter name in another case.
	const sys = "GRANT ALTER SYSTEM ON PARAMETER Work_Mem TO PUBLIC;\n"
	p = paramPlan(t, sys, []string{"work_mem|SET|public|f"})
	if up := paramLines(p.Up); up != "GRANT ALTER SYSTEM ON PARAMETER work_mem TO PUBLIC;" {
		t.Errorf("alter system:\n%s", up)
	}
	if p = paramPlan(t, sys, []string{"work_mem|ALTER SYSTEM|public|f"}); len(p.Up) != 0 {
		t.Errorf("alter system there:\n%s", strings.Join(p.Up, "\n"))
	}
	// WITH GRANT OPTION exists for roles only (PostgreSQL refuses it for PUBLIC).
	const opt = "GRANT ALTER SYSTEM ON PARAMETER Work_Mem TO app WITH GRANT OPTION;\n"
	if p = paramPlan(t, opt, []string{"work_mem|ALTER SYSTEM|app|t"}); len(p.Up) != 0 {
		t.Errorf("grant option there:\n%s", strings.Join(p.Up, "\n"))
	}
	// The target has it without the grant option: still a difference.
	if p = paramPlan(t, opt, []string{"work_mem|ALTER SYSTEM|app|f"}); paramLines(p.Up) != `GRANT ALTER SYSTEM ON PARAMETER work_mem TO "app" WITH GRANT OPTION;` {
		t.Errorf("grant option missing must be granted:\n%s", paramLines(p.Up))
	}
}

// recordRunner is catalogRunner that keeps every SQL it was asked.
type recordRunner struct {
	catalogRunner
	sql []string
}

func (r *recordRunner) Run(ctx context.Context, c pg.Conn, name string, args []string, in io.Reader, out io.Writer) error {
	r.sql = append(r.sql, args[len(args)-1])
	return r.catalogRunner.Run(ctx, c, name, args, in, out)
}

// BR-015 #2: a role named "PUBLIC" (only lower-case public is reserved) is not
// the PUBLIC pseudo-role: parsed, read from the target, keyed and written as
// itself, so its grant is never given to everyone nor taken as PUBLIC's.
func TestBR015ParamGrantRoleNamedPUBLIC(t *testing.T) {
	g := mustGlobals(t, `GRANT SET ON PARAMETER work_mem TO "PUBLIC", PUBLIC;`)
	if len(g.ParamGrants) != 2 || g.ParamGrants[0].Grantee != "PUBLIC" || g.ParamGrants[1].Grantee != publicGrantee || g.ParamGrants[0].key() == g.ParamGrants[1].key() {
		t.Fatalf("parsed: %+v", g.ParamGrants)
	}
	// The target read names grantee 0 public; pg_get_userbyid gives the role's own name.
	run := &recordRunner{catalogRunner: catalogRunner{answers: [][2]string{{"pg_parameter_acl", "work_mem\x1fSET\x1fPUBLIC\x1ff"}}}}
	tr, err := ReadTargetRoles(context.Background(), &pg.Client{Runner: run}, 17)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.ParamGrants[g.ParamGrants[0].key()]; !ok || len(tr.ParamGrants) != 1 {
		t.Errorf("the role's grant: %v", tr.ParamGrants)
	}
	for _, q := range run.sql {
		if strings.Contains(q, "pg_parameter_acl") && !strings.Contains(q, "a.grantee = 0 then 'public'") {
			t.Errorf("pseudo-role spelling: %s", q)
		}
	}

	plan := func(grant string, acl []string) *Plan {
		src, tgt := planSource(t), roleTarget(t)
		src.Globals = mustGlobals(t, superGlobals+"CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\nCREATE ROLE \"PUBLIC\";\nALTER ROLE \"PUBLIC\" WITH LOGIN;\n"+grant)
		tgt.Roles = readRoles(t, []string{"postgres|", "app|", "PUBLIC|"}, acl)
		return MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	}
	// The backup grants the role; the target has only PUBLIC's grant.
	p := plan(`GRANT SET ON PARAMETER work_mem TO "PUBLIC";`+"\n", []string{"work_mem|SET|public|f"})
	if up, down := paramLines(p.Up), paramLines(p.Down); up != `GRANT SET ON PARAMETER work_mem TO "PUBLIC";` || down != `REVOKE SET ON PARAMETER work_mem FROM "PUBLIC";` {
		t.Errorf("role missing:\nup:\n%s\ndown:\n%s", up, down)
	}
	// The backup grants PUBLIC; the target has only the role's grant.
	p = plan("GRANT SET ON PARAMETER work_mem TO PUBLIC;\n", []string{"work_mem|SET|PUBLIC|f"})
	if up, down := paramLines(p.Up), paramLines(p.Down); up != "GRANT SET ON PARAMETER work_mem TO PUBLIC;" || down != "REVOKE SET ON PARAMETER work_mem FROM PUBLIC;" {
		t.Errorf("PUBLIC missing:\nup:\n%s\ndown:\n%s", up, down)
	}
	// Both there: nothing to do.
	if p = plan(`GRANT SET ON PARAMETER work_mem TO "PUBLIC", PUBLIC;`+"\n", []string{"work_mem|SET|public|f", "work_mem|SET|PUBLIC|f"}); paramLines(p.Up) != "" || !p.OK() {
		t.Errorf("both there: %v\n%s", failed(p), strings.Join(p.Up, "\n"))
	}
}

// BR-015 #3: when the target has the privilege without the grant option,
// env-up adds the option and env-down takes back only the option.
func TestBR015ParamGrantOptionDown(t *testing.T) {
	const grant = "GRANT SET ON PARAMETER work_mem TO app WITH GRANT OPTION;\n"
	p := paramPlan(t, grant, []string{"work_mem|SET|app|f"})
	if up, down := paramLines(p.Up), paramLines(p.Down); up != `GRANT SET ON PARAMETER work_mem TO "app" WITH GRANT OPTION;` ||
		down != `REVOKE GRANT OPTION FOR SET ON PARAMETER work_mem FROM "app";` {
		t.Errorf("option only:\nup:\n%s\ndown:\n%s", up, down)
	}
	if c := failed(p)["role app"]; c.Fix != FixSQL || c.Detail != "missing the grant option of SET on parameter work_mem" {
		t.Errorf("check: %+v", c)
	}
	// Without the privilege at all, env-down revokes it whole.
	p = paramPlan(t, grant, nil)
	if down := paramLines(p.Down); down != `REVOKE SET ON PARAMETER work_mem FROM "app";` {
		t.Errorf("missing: %s", down)
	}
}

// settingRoles reads a target whose app has the role setting search_path=v.
func settingRoles(t *testing.T, v string) *TargetRoles {
	t.Helper()
	rows := "postgres\x1ft\x1ft\x1ft\x1ft\x1ft\x1ft\x1ft\x1f-1\x1f\napp\x1ff\x1ft\x1ff\x1ff\x1ft\x1ff\x1ff\x1f-1\x1f"
	run := &catalogRunner{answers: [][2]string{{"rolvaliduntil", rows}, {"pg_db_role_setting", "app\x1fsearch_path=" + v}}}
	tr, err := ReadTargetRoles(context.Background(), &pg.Client{Runner: run}, 17)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// BR-015 #4: a role's search_path compares element by element with case and
// quoting kept: "MySchema" on the source is not myschema on the target.
func TestBR015RoleSettingKeepsCase(t *testing.T) {
	const app = "CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\nALTER ROLE app SET search_path TO 'MySchema', 'public';\n"
	plan := func(globals, have string) *Plan {
		src, tgt := planSource(t), roleTarget(t)
		src.Globals = mustGlobals(t, superGlobals+globals)
		tgt.Roles = settingRoles(t, have)
		return MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	}
	if p := plan(app, `"MySchema", public`); len(p.Up) != 0 {
		t.Errorf("same:\n%s", strings.Join(p.Up, "\n"))
	}
	p := plan(app, "myschema, public")
	if c := roleCheck(p, "app"); c == nil || c.OK || c.Detail != "setting search_path" {
		t.Errorf("check: %+v", c)
	}
	if !has(p.Up, `ALTER ROLE "app" SET search_path TO 'MySchema', 'public';`) || !has(p.Down, `ALTER ROLE "app" SET search_path = 'myschema', 'public';`) {
		t.Errorf("up:\n%s\ndown:\n%s", strings.Join(p.Up, "\n"), strings.Join(p.Down, "\n"))
	}
	// An empty search_path is stored as "" and dumped as ''.
	if p := plan("CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\nALTER ROLE app SET search_path TO '';\n", `""`); len(p.Up) != 0 {
		t.Errorf("empty:\n%s", strings.Join(p.Up, "\n"))
	}
	// A database setting compares the same way; other settings as before.
	if sameDumped("search_path", "myschema, public", "MySchema,public", "'MySchema', 'public'") || !sameDumped("work_mem", "64MB", "64MB", "'64MB'") {
		t.Error("sameDumped")
	}
}

// BR-015 #5: the session that reads pg_roles prints VALID UNTIL in ISO and
// UTC whatever the target's defaults, so sameTime parses it as an instant.
func TestBR015ReadTargetRolesPinsSession(t *testing.T) {
	run := &recordRunner{}
	if _, err := ReadTargetRoles(context.Background(), &pg.Client{Runner: run}, 17); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range run.sql {
		if strings.Contains(q, "rolvaliduntil") {
			found = true
			if !strings.HasPrefix(q, "set datestyle = 'ISO'; set timezone = 'UTC'; select ") {
				t.Errorf("roles query: %s", q)
			}
		}
	}
	if !found {
		t.Error("no roles query")
	}
}

// BR-015 review #5: an empty element of temp_tablespaces is the database's
// default tablespace, so it counts; a list of only that is the empty list.
// In search_path an empty element names no schema and is skipped.
func TestBR015ListEmptyElements(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"temp_tablespaces", `fast, ""`, "fast", false},
		{"temp_tablespaces", `"", fast`, `"", fast`, true},
		{"temp_tablespaces", `""`, "", true},
		{"search_path", `""`, "", true},
		{"search_path", `"", public`, "public", true},
	}
	for _, c := range cases {
		if got := sameValue(c.name, "string", c.a, c.b); got != c.want {
			t.Errorf("sameValue(%s, %q, %q) = %v", c.name, c.a, c.b, got)
		}
	}
	if got := valueSQL("temp_tablespaces", `fast, ""`); got != "'fast', ''" {
		t.Errorf("valueSQL: %s", got)
	}
	if same, ok := sameDumpedList("temp_tablespaces", `fast, ""`, "'fast'"); !ok || same {
		t.Errorf("sameDumpedList: %v %v", same, ok)
	}
}

// loadedShop is the plan after the load of planSource's shop, whose archive
// sets search_path to 'MySchema', 'public', into a target database that holds have.
func loadedShop(t *testing.T, have string) *Plan {
	src, tgt := planSource(t), freshTarget(t)
	src.DBs[0].AttrSQL = append(src.DBs[0].AttrSQL, "ALTER DATABASE shop SET search_path TO 'MySchema', 'public';\n"...)
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): ""}
	pre := "pg_stat_statements"
	tgt.Env.PreloadLibraries = &pre
	tgt.Env.Settings[0].Value = "ISO, DMY"
	tgt.Roles = &TargetRoles{Roles: src.Globals.Roles, ParamGrants: map[string]ParamGrant{}, HasGrantOptions: true}
	tgt.Names[restoredShop] = true
	tgt.DBs[restoredShop] = &TargetDB{Name: restoredShop, Exists: true, Objects: 5, Owner: "app", Locale: src.DBs[0].Dump.Locale,
		Settings: map[string]string{"\x00search_path": have}, SettingName: map[string]string{"\x00search_path": "search_path"},
		ACL: map[string]bool{"public CONNECT": true, "public TEMPORARY": true, "app CONNECT": true, "app CREATE": true, "app TEMPORARY": true, "Report Reader CONNECT": true}}
	return MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: map[string]string{"shop": restoredShop}})
}

// BR-015 review #1: after the load, a database setting compares element by
// element with case kept: "MySchema" is not myschema.
func TestBR015DatabaseSettingKeepsCase(t *testing.T) {
	if p := loadedShop(t, `"MySchema", public`); !p.OK() {
		t.Errorf("same: %v", failed(p))
	}
	c, ok := failed(loadedShop(t, "myschema, public"))["database "+restoredShop+" attributes"]
	if !ok || c.Detail != "setting search_path" {
		t.Errorf("differs: %+v", c)
	}
}

// dataPlan is a URL run whose shop has search_path backup in the backup, on a
// target where it is target before the load and loaded after it; attr is an
// extra database entry of the archive.
func dataPlan(t *testing.T, backup, target, loaded, attr string) (before, after *Plan) {
	src, tgt := planSource(t), freshTarget(t)
	src.Kind, src.Env.SourceKind, src.Globals = "url", "url", nil
	src.Env.DBSettings = map[string][]pg.Setting{"shop": {{Name: "search_path", Value: backup, Vartype: "string"}}}
	src.DBs[0].AttrSQL = append(src.DBs[0].AttrSQL, attr...)
	tgt.Roles = nil
	tgt.CreateDB = true
	tgt.Env.CurrentUser, tgt.Env.Superuser = "deployer", false
	tgt.Env.PreloadLibraries = src.Env.PreloadLibraries
	tgt.Avail["pg_stat_statements"] = ExtAvail{Default: "1.11", Trusted: true}
	tgt.Effective = []pg.Setting{{Name: "search_path", Value: target, Vartype: "string"}}
	before = MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData})
	tgt.DBs[restoredShop] = &TargetDB{Name: restoredShop, Exists: true, Owner: "deployer", Settings: map[string]string{}, SettingName: map[string]string{},
		Effective: []pg.Setting{{Name: "search_path", Value: loaded, Vartype: "string"}}}
	after = MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData, Loaded: map[string]string{"shop": restoredShop}})
	return before, after
}

// searchPaths are the statements of create that set search_path.
func searchPaths(create []string) []string {
	var out []string
	for _, s := range create {
		if strings.Contains(s, "search_path") {
			out = append(out, s)
		}
	}
	return out
}

// BR-015 review #1: a URL run compares the settings in effect as the list the
// server uses: $user, public (postgresql.conf) equals "$user", public.
func TestBR015DataSettingsList(t *testing.T) {
	before, after := dataPlan(t, "$user, public", `"$user", public`, `"$user", public`, "")
	if got := searchPaths(before.DBs[0].Create); len(got) != 0 || !before.OK() {
		t.Errorf("before the load: %v %v", got, failed(before))
	}
	if !after.OK() {
		t.Errorf("after the load: %v", failed(after))
	}
	// A real difference is still one, set when the database is created.
	before, after = dataPlan(t, `"App", public`, "app, public", "app, public", "")
	if got := searchPaths(before.DBs[0].Create); len(got) != 1 || got[0] != `ALTER DATABASE "`+restoredShop+`" SET search_path = 'App', 'public';` {
		t.Errorf("before the load: %v", got)
	}
	if _, ok := failed(after)["setting shop: search_path"]; !ok {
		t.Errorf("after the load: %v", failed(after))
	}
}

// BR-015 review #6: the archive's own search_path with a quoted mixed-case
// schema equals the backup's, so the new database gets it once.
func TestBR015DataSettingsArchiveOnce(t *testing.T) {
	before, _ := dataPlan(t, `"MySchema", public`, "public", `"MySchema", public`, "ALTER DATABASE shop SET search_path TO 'MySchema', 'public';\n")
	if got := searchPaths(before.DBs[0].Create); len(got) != 1 {
		t.Errorf("create:\n%s", strings.Join(before.DBs[0].Create, "\n"))
	}
}

// BR-015 review #3: a database privilege of a role named "PUBLIC" is that
// role's, not the PUBLIC pseudo-role's: when the load did not bring it back,
// PUBLIC's default CONNECT does not hide it.
func TestBR015DatabaseACLRoleNamedPUBLIC(t *testing.T) {
	const grant = "GRANT CONNECT ON DATABASE shop TO \"PUBLIC\";\nGRANT CONNECT ON DATABASE shop TO PUBLIC;\n"
	a, err := ParseDatabaseSQL([]byte("CREATE DATABASE shop;\n"+grant), "s", map[string]bool{"PUBLIC": true})
	if err != nil {
		t.Fatal(err)
	}
	acl := a.expectedACL("app")
	if !acl["PUBLIC CONNECT"] || !acl[publicGrantee+" CONNECT"] || privList(acl) != `"PUBLIC": CONNECT; PUBLIC: CONNECT, TEMPORARY; app: CONNECT, CREATE, TEMPORARY` {
		t.Errorf("expected: %v (%s)", acl, privList(acl))
	}
	if got := aclDiff("s", map[string]bool{"public CONNECT": true}, map[string]bool{"public CONNECT": true, "PUBLIC CONNECT": true}); len(got) != 1 || got[0] != `GRANT CONNECT ON DATABASE "s" TO "PUBLIC";` {
		t.Errorf("aclDiff: %v", got)
	}

	// The target read names grantee 0 public; pg_get_userbyid gives the role's own name.
	run := &recordRunner{catalogRunner: catalogRunner{answers: [][2]string{
		{"datacl is null", "app\x1ff\x1f\x01\x1ff"},
		{"pg_encoding_to_char", "UTF8\x1fC\x1fC\x1fc\x1f\x1f\x1f"},
		{"aclexplode", "public\x1fCONNECT\nPUBLIC\x1fCONNECT"},
	}}}
	td, err := readTargetDB(context.Background(), &pg.Client{Runner: run}, "s", true, 17, false)
	if err != nil {
		t.Fatal(err)
	}
	if !td.ACL["public CONNECT"] || !td.ACL["PUBLIC CONNECT"] {
		t.Errorf("read: %v", td.ACL)
	}
	for _, q := range run.sql {
		if strings.Contains(q, "aclexplode") && !strings.Contains(q, "a.grantee = 0 then 'public'") {
			t.Errorf("pseudo-role spelling: %s", q)
		}
	}

	// Through the check after the load: the role's CONNECT is missing.
	src, tgt := planSource(t), freshTarget(t)
	src.Globals = mustGlobals(t, superGlobals+"CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\nCREATE ROLE \"Report Reader\";\nALTER ROLE \"Report Reader\" WITH LOGIN;\nCREATE ROLE \"PUBLIC\";\nALTER ROLE \"PUBLIC\" WITH LOGIN;\n")
	src.DBs[0].AttrSQL = append(src.DBs[0].AttrSQL, "GRANT CONNECT ON DATABASE shop TO \"PUBLIC\";\n"...)
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): ""}
	tgt.Roles = &TargetRoles{Roles: src.Globals.Roles, ParamGrants: map[string]ParamGrant{}, HasGrantOptions: true}
	tgt.Names[restoredShop] = true
	tgt.DBs[restoredShop] = &TargetDB{Name: restoredShop, Exists: true, Objects: 5, Owner: "app", Locale: src.DBs[0].Dump.Locale,
		Settings: map[string]string{}, SettingName: map[string]string{},
		ACL: map[string]bool{"public CONNECT": true, "public TEMPORARY": true, "app CONNECT": true, "app CREATE": true, "app TEMPORARY": true, "Report Reader CONNECT": true}}
	loaded := map[string]string{"shop": restoredShop}
	if c, ok := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: loaded}))["database "+restoredShop+" attributes"]; !ok || c.Detail != "privileges" {
		t.Errorf("missing: %+v", c)
	}
	tgt.DBs[restoredShop].ACL["PUBLIC CONNECT"] = true
	if c, ok := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: loaded}))["database "+restoredShop+" attributes"]; ok {
		t.Errorf("there: %+v", c)
	}
}

// BR-015 review #4: the check of a role named "PUBLIC" is not the PUBLIC pseudo-role's.
func TestBR015GranteeName(t *testing.T) {
	for g, want := range map[string]string{publicGrantee: "PUBLIC", "PUBLIC": `"PUBLIC"`, "Public": `"Public"`, "app": "app"} {
		if got := granteeName(g); got != want {
			t.Errorf("granteeName(%q) = %s, want %s", g, got, want)
		}
	}
	src, tgt := planSource(t), roleTarget(t)
	src.Globals = mustGlobals(t, superGlobals+"CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\nCREATE ROLE \"PUBLIC\";\nALTER ROLE \"PUBLIC\" WITH LOGIN;\nGRANT SET ON PARAMETER work_mem TO \"PUBLIC\", PUBLIC;\n")
	tgt.Roles = readRoles(t, []string{"postgres|", "app|", "PUBLIC|"}, nil)
	f := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}))
	if f[`role "PUBLIC"`].Detail != "missing SET on parameter work_mem" || f["role PUBLIC"].Detail != "missing SET on parameter work_mem" {
		t.Errorf("checks: %v", f)
	}
}
