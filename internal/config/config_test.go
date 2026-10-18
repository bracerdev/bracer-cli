package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
app: shop
secrets_file: ~/.config/bracer/shop.env
instances:
  - name: main
    container: shop-db
    user: postgres
    pg_major: 17
    databases: all
  - name: analytics
    host: localhost
    port: 5433
    user: bracer_backup
    password_env: BRACER_SHOP_ANALYTICS_PASSWORD
    pg_major: 16
    databases:
      - analytics
      - name: events
        retention: { keep: 30 }
  - name: prod-cloud
    url_env: BRACER_SHOP_PROD_CLOUD_URL
    databases: [postgres]
storage:
  endpoint: https://x.r2.cloudflarestorage.com
  bucket: backups
  access_key_env: BRACER_SHOP_S3_ACCESS_KEY
  secret_key_env: BRACER_SHOP_S3_SECRET_KEY
encryption: age
age_recipient: age1qxyabc
age_identity_file: ~/.config/bracer/shop.key
retention:
  keep: 7
`

func TestParseSample(t *testing.T) {
	a, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Instances) != 3 {
		t.Fatalf("instances = %d", len(a.Instances))
	}
	if !a.Instances[0].Databases.All {
		t.Error("main.databases must be all")
	}
	if a.Instances[0].Kind() != KindContainer || a.Instances[1].Kind() != KindHost || a.Instances[2].Kind() != KindURL {
		t.Error("Kind sai")
	}
	dbs := a.Instances[1].Databases.List
	if len(dbs) != 2 || dbs[0].Name != "analytics" || dbs[1].Name != "events" || dbs[1].Retention == nil || dbs[1].Retention.Keep != 30 {
		t.Errorf("analytics.databases = %+v", dbs)
	}
	if a.Instances[2].Databases.List[0].Name != "postgres" {
		t.Error("flow-style list not parsed")
	}
}

func TestRoundTrip(t *testing.T) {
	a, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "databases: all") {
		t.Errorf("all must be written in the short form:\n%s", out)
	}
	if !strings.Contains(string(out), "- analytics\n") {
		t.Errorf("a database without overrides must be written as a string:\n%s", out)
	}
	if len(b.Instances) != 3 || b.Instances[1].Databases.List[1].Retention.Keep != 30 {
		t.Errorf("round trip lost data:\n%s", out)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"app: shop\ninstances: []\n": "instances: at least one",
		"app: Shop\ninstances:\n  - name: a\n    container: x\n    databases: all\n":                                                                                                              "app: lowercase",
		"app: shop\ninstances:\n  - name: a\n    databases: all\n":                                                                                                                                "exactly one of",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    host: y\n    port: 1\n    databases: all\n":                                                                                    "exactly one of",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: []\n":                                                                                                               "databases: expected",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    password: s3cret\n    databases: all\n":                                                                                        "secrets must not be written",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\n  - name: a\n    container: y\n    databases: all\n":                                                           "duplicates instances[0]",
		"app: shop\ninstances:\n  - name: a\n    host: h\n    port: 1\n    password_env: P\n    databases: all\n  - name: b\n    host: h\n    port: 2\n    password_env: P\n    databases: all\n": "two instances cannot share",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\nencryption: age\n":                                                                                             "age_recipient",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\nencryption: rot13\n":                                                                                           "must be none | age",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\nstorage:\n  bucket: b\n":                                                                                       "storage.endpoint",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\ndrill:\n  image: bitnamilegacy/postgresql:16\n":                                                                "not supported as the drill image",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\ndrill:\n  runtime: podman\n":                                                                                   "drill.runtime: must be docker or kube",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\ndrill:\n  namespace: Bad_NS\n":                                                                                 "drill.namespace: must be a Kubernetes namespace name",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\nretention: { keep: 0 }\n":                                                                                      "retention.keep",
		"app: shop\ninstances:\n  - name: a\n    container: x\n    databases: all\n    bogus: 1\n":                                                                                                "bogus",
		"app: shop\ninstances:\n  - name: a\n    pod: onlyns\n    databases: all\n":                                                                                                               "namespace/pod",
		"app: shop\ninstances:\n  - name: a\n    pod: ns/pg\n    password_env: P\n    databases: all\n":                                                                                           "passwords cannot be passed",
		"app: shop\ninstances:\n  - name: a\n    socket: run/postgresql\n    databases: all\n":                                                                                                    "absolute path",
		"app: shop\ninstances:\n  - name: a\n    socket: /var/run/postgresql/.s.PGSQL.5433\n    port: 5432\n    databases: all\n":                                                                 "differs from the port",
		"app: shop\ninstances:\n  - name: a\n    pod: ns/pg\n    socket: /tmp\n    databases: all\n":                                                                                              "exactly one of",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("input:\n%s\nwant an error containing %q, got: %v", in, want, err)
		}
	}
}

func TestValidateReportsAllErrorsAtOnce(t *testing.T) {
	in := "app: ''\ninstances:\n  - name: ''\n    databases: all\n"
	_, err := Parse([]byte(in))
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	if len(ve.Errs) < 3 {
		t.Errorf("want ≥3 errors at once, got %d: %v", len(ve.Errs), ve.Errs)
	}
}

func TestEnvVarName(t *testing.T) {
	cases := map[string][]string{
		"BRACER_SHOP_ANALYTICS_PASSWORD": {"shop", "analytics", "password"},
		"BRACER_MY_APP_PROD_CLOUD_URL":   {"my-app", "prod.cloud", "url"},
		"BRACER_SHOP_S3_ACCESS_KEY":      {"shop", "s3", "access_key"},
	}
	for want, parts := range cases {
		if got := EnvVarName(parts...); got != want {
			t.Errorf("EnvVarName(%v) = %s, want %s", parts, got, want)
		}
	}
}

func TestSaveAtomicAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	a, _ := Parse([]byte(sample))
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestPodAndSocketInstances(t *testing.T) {
	app, err := Parse([]byte("app: shop\ninstances:\n  - name: k\n    pod: attendy-dev/postgres-0/postgres\n    user: attendy\n    databases: all\n  - name: s\n    socket: /var/run/postgresql\n    port: 5433\n    databases: all\n  - name: f\n    socket: /tmp/.s.PGSQL.5434\n    databases: all\n"))
	if err != nil {
		t.Fatal(err)
	}
	k, s, f := app.Instances[0], app.Instances[1], app.Instances[2]
	if k.Kind() != KindPod || s.Kind() != KindSocket || f.Kind() != KindSocket {
		t.Errorf("kinds: %v %v %v", k.Kind(), s.Kind(), f.Kind())
	}
	if ns, pod, ctr, ok := k.PodRef(); !ok || ns != "attendy-dev" || pod != "postgres-0" || ctr != "postgres" {
		t.Errorf("PodRef: %s %s %s %v", ns, pod, ctr, ok)
	}
	if dir, port := s.SocketDir(); dir != "/var/run/postgresql" || port != 5433 {
		t.Errorf("SocketDir: %s %d", dir, port)
	}
	if dir, port := f.SocketDir(); dir != "/tmp" || port != 5434 {
		t.Errorf("SocketDir from file: %s %d", dir, port)
	}
	out, _ := Marshal(app)
	if !strings.Contains(string(out), "pod: attendy-dev/postgres-0/postgres") || !strings.Contains(string(out), "socket: /var/run/postgresql") {
		t.Errorf("marshal:\n%s", out)
	}
}

func TestIsBitnamiImage(t *testing.T) {
	yes := []string{"bitnami/postgresql:16", "bitnamilegacy/postgresql:16.4.0", "docker.io/bitnami/postgresql", "registry.example.com:5000/bitnami/postgresql-repmgr:17@sha256:abc"}
	no := []string{"", "postgres:16-alpine", "postgis/postgis:16-3.4", "timescale/timescaledb:2.14-pg16", "mybitnami/postgresql", "bitnami/redis:7"}
	for _, s := range yes {
		if !IsBitnamiImage(s) {
			t.Errorf("%q: want true", s)
		}
	}
	for _, s := range no {
		if IsBitnamiImage(s) {
			t.Errorf("%q: want false", s)
		}
	}
}
