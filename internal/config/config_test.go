package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tail is the part of app.yaml every test file shares: storage, encryption,
// runtime, as init writes them.
const schedBlock = "schedule:\n  every: daily\n  at: [\"03:00\"]\n  days: []\n  keep: 7\n"

const tail = `secrets_file: /home/khanh/.config/bracer/shop/secrets.env
storage:
  endpoint: https://x.r2.cloudflarestorage.com
  bucket: backups
  folder: shop-7f3k2q
  access_key_env: BRACER_SHOP_S3_ACCESS_KEY
  secret_key_env: BRACER_SHOP_S3_SECRET_KEY
encryption: age
age_recipient: age1qxyabc
age_identity_file: /home/khanh/.config/bracer/shop/age.key
schedule:
  every: daily
  at: ["03:00"]
  days: []
  keep: 7
runtime:
  user: khanh
  path: /usr/bin:/bin
  docker: /usr/bin/docker
  docker_host: unix:///var/run/docker.sock
`

const sample = `
app: shop
instances:
  - name: main
    connection:
      container: shop-db
      user: postgres
      password_env: BRACER_SHOP_MAIN_PASSWORD
      pg_major: 17
      system_id: "7698217040907026472"
    databases: [{name: shop}, {name: auth}]
  - name: analytics
    connection:
      url_env: BRACER_SHOP_ANALYTICS_URL
      pg_major: 16
      system_id: "7698216964553203752"
    databases:
      - name: analytics
      - name: events
        accepted_external: ["function f() (language plpython3u)"]
  - name: prod-cloud
    connection:
      url_env: BRACER_SHOP_PROD_CLOUD_URL
      pg_major: 16
      system_id: none
    databases: [{name: postgres}]
drill:
  memory: 2g
` + tail

// one is a file with one container instance; extra lines go under the instance.
func one(instanceExtra, appExtra string) string {
	return "app: shop\ninstances:\n  - name: a\n    connection:\n      container: x\n      pg_major: 17\n      system_id: \"1\"\n    databases: [{name: shop}]\n" + instanceExtra + appExtra + tail
}

func TestParseSample(t *testing.T) {
	a, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Instances) != 3 {
		t.Fatalf("instances = %d", len(a.Instances))
	}
	if got := strings.Join(a.Instances[0].Databases.Names(), ","); got != "shop,auth" {
		t.Errorf("main.databases = %s", got)
	}
	if a.Instances[0].Kind() != KindContainer || a.Instances[1].Kind() != KindURL || a.Instances[2].Kind() != KindURL {
		t.Error("wrong Kind")
	}
	dbs := a.Instances[1].Databases
	if len(dbs) != 2 || dbs[0].Name != "analytics" || dbs[1].Name != "events" || len(dbs[1].AcceptedExternal) != 1 {
		t.Errorf("analytics.databases = %+v", dbs)
	}
	if a.Instances[2].SystemID != SystemIDNone || a.Storage.Folder != "shop-7f3k2q" || a.DrillMemory() != "2g" {
		t.Errorf("system_id = %q, folder = %q, memory = %q", a.Instances[2].SystemID, a.Storage.Folder, a.DrillMemory())
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
	if !strings.Contains(string(out), "- name: analytics\n") || !strings.Contains(string(out), "folder: shop-7f3k2q") {
		t.Errorf("written:\n%s", out)
	}
	if len(b.Instances) != 3 || len(b.Instances[1].Databases[1].AcceptedExternal) != 1 {
		t.Errorf("round trip lost data:\n%s", out)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"app: shop\ninstances: []\n" + tail:                                                               "instances: at least one",
		one("    ignore: [scratch]\n", ""):                                                                "ignore", // ADR-0024
		strings.Replace(one("", ""), "app: shop", "app: Shop", 1):                                         "app: lowercase",
		strings.Replace(one("", ""), "      container: x\n", "", 1):                                       "exactly one of",
		strings.Replace(one("", ""), "      container: x\n", "      container: x\n      url_env: U\n", 1): "exactly one of",
		strings.Replace(one("", ""), "[{name: shop}]", "[]", 1):                                           "databases: expected",
		strings.Replace(one("", ""), "[{name: shop}]", "all", 1):                                          "sequence is expected",
		strings.Replace(one("", ""), "[{name: shop}]", "[shop]", 1):                                       "mapping is expected",
		one("    password: s3cret\n", ""):                                                                 "secrets must not be written",
		one("    bogus: 1\n", ""):                                                                         "bogus",
		one("    pod: ns/pg\n", ""):                                                                       "pod",
		one("    drill:\n      image: bitnamilegacy/postgresql:16\n", ""):                                 "not supported as the drill image",
		one("    drill:\n      runtime: docker\n", ""):                                                    "drill.runtime: must be none",
		one("    drill:\n      runtime: none\n      image: postgres:16\n", ""):                            "runtime is none but image is set",
		one("", "drill:\n  image: postgres:16\n"):                                                         "image",
		one("", "drill:\n  timeout: 1h\n"):                                                                "timeout",
		one("", "drill:\n  memory: lots\n"):                                                               "drill.memory",
		one("", "row_counts: exact\n"):                                                                    "row_counts",
		one("", "backup:\n  timeout: 1h\n"):                                                               "backup",
		one("", "retention: { keep: 7 }\n"):                                                               "retention",
		strings.Replace(one("", ""), schedBlock, "", 1):                                                   "schedule: required",
		strings.Replace(one("", ""), "  every: daily\n", "", 1):                                           "schedule.every: must be daily, weekly or monthly",
		strings.Replace(one("", ""), "  keep: 7\n", "  keep: 0\n", 1):                                     "schedule.keep: must be ≥ 1",
		strings.Replace(one("", ""), `at: ["03:00"]`, "at: []", 1):                                        "schedule.at: at least one time of day",
		strings.Replace(one("", ""), `at: ["03:00"]`, `at: ["3:00"]`, 1):                                  `schedule.at: "3:00" is not HH:MM`,
		strings.Replace(one("", ""), `at: ["03:00"]`, `at: ["03:00", "03:00"]`, 1):                        "schedule.at: 03:00 twice",
		strings.Replace(one("", ""), "days: []", "days: [mon]", 1):                                        "schedule.days: must be empty for a daily schedule",
		strings.Replace(one("", ""), "every: daily", "every: weekly", 1):                                  "schedule.days: at least one day of the week (mon…sun)",
		strings.Replace(one("", ""), "every: daily\n  at: [\"03:00\"]\n  days: []", "every: weekly\n  at: [\"03:00\"]\n  days: [mon, Fri]", 1):                         `schedule.days: "Fri" is not a day of the week`,
		strings.Replace(one("", ""), "every: daily\n  at: [\"03:00\"]\n  days: []", "every: monthly\n  at: [\"03:00\"]\n  days: [1, 29]", 1):                           `schedule.days: "29" is not a day of the month (1–28 or last)`,
		strings.Replace(one("", ""), "every: daily\n  at: [\"03:00\"]\n  days: []", "every: monthly\n  at: [\"03:00\"]\n  days: [last, last]", 1):                      "schedule.days: last twice",
		strings.Replace(one("", ""), "  every: daily\n", "  every: daily\n  backup: \"03:00\"\n", 1):                                                                   "backup", // ADR-0029: no old form
		strings.Replace(one("", ""), "  every: daily\n", "  every: daily\n  drill: \"04:00\"\n", 1):                                                                    "drill",  // ADR-0026: the drill runs inside backup
		strings.Replace(one("", ""), "      system_id: \"1\"\n", "", 1):                                                                                                "system_id: required",
		strings.Replace(one("", ""), "system_id: \"1\"", "system_id: abc", 1):                                                                                          "system_id: expected",
		strings.Replace(one("", ""), "      pg_major: 17\n", "", 1):                                                                                                    "pg_major: required",
		strings.Replace(one("", ""), "  folder: shop-7f3k2q\n", "", 1):                                                                                                 "storage.folder",
		strings.Replace(one("", ""), "folder: shop-7f3k2q", "folder: shop", 1):                                                                                         "storage.folder",
		strings.Replace(one("", ""), "encryption: age\nage_recipient: age1qxyabc\nage_identity_file: /home/khanh/.config/bracer/shop/age.key\n", "", 1):                "encryption: required",
		strings.Replace(one("", ""), "age_identity_file: /home/khanh/.config/bracer/shop/age.key\n", "", 1):                                                            "age_identity_file: required",
		strings.Replace(one("", ""), "secrets_file: /home/khanh/.config/bracer/shop/secrets.env", "secrets_file: ~/shop.env", 1):                                       "secrets_file: must be an absolute path",
		strings.Replace(one("", ""), "runtime:\n  user: khanh\n  path: /usr/bin:/bin\n  docker: /usr/bin/docker\n  docker_host: unix:///var/run/docker.sock\n", "", 1): "runtime: required",
		strings.Replace(one("", ""), "docker: /usr/bin/docker", "docker: docker", 1):                                                                                   "runtime.docker",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("input:\n%s\nwant an error containing %q, got: %v", in, want, err)
		}
	}
	two := "app: shop\ninstances:\n  - name: a\n    connection:\n      url_env: U\n      pg_major: 17\n      system_id: none\n    databases: [{name: shop}]\n  - name: a\n    connection:\n      url_env: U\n      pg_major: 17\n      system_id: none\n    databases: [{name: shop}]\n" + tail
	_, err := Parse([]byte(two))
	if err == nil || !strings.Contains(err.Error(), "duplicates instances[0]") || !strings.Contains(err.Error(), "two instances cannot share") {
		t.Errorf("two: %v", err)
	}
}

func TestValidateReportsAllErrorsAtOnce(t *testing.T) {
	in := "app: ''\ninstances:\n  - name: ''\n    databases: [{name: shop}]\n"
	_, err := Parse([]byte(in))
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	if len(ve.Errs) < 3 {
		t.Errorf("want ≥3 errors at once, got %d: %v", len(ve.Errs), ve.Errs)
	}
}

// A file that does not read is made again: every error of Load ends with
// remove, then init (ADR-0022, ADR-0023).
func TestLoadFix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop", "app.yaml")
	os.MkdirAll(filepath.Dir(path), 0o700)
	for _, body := range []string{one("    bogus: 1\n", ""), "app: [\n", strings.Replace(one("", ""), "      pg_major: 17\n", "", 1)} {
		_ = os.WriteFile(path, []byte(body), 0o644)
		if _, err := Load(path); err == nil || !strings.HasSuffix(err.Error(), "\nFix: bracer remove shop, then bracer init shop") {
			t.Errorf("Load(%q): %v", body, err)
		}
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

func TestDrillPerInstance(t *testing.T) {
	a, err := Parse([]byte(one("    drill:\n      image: postgis/postgis:16-3.4\n", "")))
	if err != nil {
		t.Fatal(err)
	}
	if a.Instances[0].DrillImage() != "postgis/postgis:16-3.4" || a.DrillOff() {
		t.Errorf("drill: %+v", a.Instances[0].Drill)
	}
	a.Instances[0].SetDrillOff(true)
	if !a.DrillOff() || a.Instances[0].DrillImage() != "" {
		t.Errorf("off: %+v", a.Instances[0].Drill)
	}
	a.Instances[0].SetDrillOff(false)
	if a.Instances[0].Drill != nil {
		t.Errorf("drill back on must leave no block: %+v", a.Instances[0].Drill)
	}
}

// ADR-0029: the schedule lives in app.yaml (one rhythm, times, days, keep),
// written by init, and survives a save/load with the times quoted
// and the days of the month as strings.
func TestScheduleInAppYAML(t *testing.T) {
	if !reflect.DeepEqual(DefaultSchedule, Schedule{Every: "daily", At: []string{"03:00"}, Days: []string{}, Keep: 7}) || DefaultKeep("weekly") != 4 || DefaultKeep("monthly") != 3 || DefaultKeep("daily") != 7 {
		t.Errorf("DefaultSchedule = %+v", DefaultSchedule)
	}
	a, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if a.Schedule == nil || !reflect.DeepEqual(*a.Schedule, DefaultSchedule) {
		t.Fatalf("schedule = %+v", a.Schedule)
	}
	monthly := Schedule{Every: "monthly", At: []string{"02:30", "14:00"}, Days: []string{"1", "15", "last"}, Keep: 3}
	a.Schedule = &monthly
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "- \"02:30\"\n") || !strings.Contains(string(raw), "- \"1\"\n") {
		t.Errorf("times and days must be strings in YAML:\n%s", raw)
	}
	b, err := Load(path)
	if err != nil || !reflect.DeepEqual(*b.Schedule, monthly) {
		t.Errorf("round trip: %+v %v", b.Schedule, err)
	}
	if monthly.Period() != "month" || DefaultSchedule.Period() != "day" || (Schedule{Every: "weekly"}).Period() != "week" {
		t.Error("Period")
	}
	for _, s := range []Schedule{
		{Every: "weekly", At: []string{"03:00"}, Days: []string{"mon", "sun"}, Keep: 1},
		{Every: "monthly", At: []string{"00:00", "23:59"}, Days: []string{"28", "last"}, Keep: 1},
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	if err := (Schedule{Every: "hourly", At: []string{"03:00"}, Days: []string{"mon"}, Keep: 0}).Validate(); err == nil || !strings.Contains(err.Error(), "every: must be daily, weekly or monthly; keep: must be ≥ 1") {
		t.Errorf("validate: %v", err)
	}
}
