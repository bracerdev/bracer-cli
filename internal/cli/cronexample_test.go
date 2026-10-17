package cli

import (
	"bytes"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
)

func cronCfg() *config.App {
	return &config.App{
		App:             "shop",
		SecretsFile:     "shop.env",
		Instances:       []config.Instance{{Name: "main", Container: "shop-db", User: "postgres", PasswordEnv: "BRACER_SHOP_MAIN_PASSWORD", Databases: config.Databases{All: true}}},
		Storage:         &config.Storage{Endpoint: "https://s3", Bucket: "b", AccessKeyEnv: "BRACER_SHOP_S3_ACCESS_KEY", SecretKeyEnv: "BRACER_SHOP_S3_SECRET_KEY"},
		Hosted:          &config.Hosted{Server: "https://app.bracer.dev", TokenEnv: "BRACER_SHOP_TOKEN"},
		AgeIdentityFile: "~/.config/bracer/shop.key",
	}
}

func inputs(cfg *config.App) cronInputs {
	return cronInputs{cfg: cfg, cfgPath: "/srv/shop/app.yaml", binary: "/usr/local/bin/bracer", dockerDir: "/usr/bin", username: "deploy",
		backupAt: "0 3 * * *", drillAt: "0 4 * * 1", secretsAbs: "/srv/shop/shop.env"}
}

func TestRenderCron(t *testing.T) {
	var out bytes.Buffer
	renderCron(&out, inputs(cronCfg()))
	s := out.String()
	for _, want := range []string{
		"PATH=/usr/bin:",
		"0 3 * * * /usr/local/bin/bracer backup -c /srv/shop/app.yaml 2>&1 | logger -t bracer",
		"0 4 * * 1 /usr/local/bin/bracer drill  -c /srv/shop/app.yaml",
		"secrets_file (/srv/shop/shop.env)",
		"Results are reported to https://app.bracer.dev",
		"bracer ping <heartbeat-id> -- ",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--env-file") || strings.Contains(s, "BRACER_SHOP_MAIN_PASSWORD") {
		t.Error("with secrets_file, no env names or --env-file may appear")
	}

	// No secrets_file: list the variable names, without values.
	cfg := cronCfg()
	cfg.SecretsFile, cfg.Hosted = "", nil
	in := inputs(cfg)
	in.secretsAbs, in.dockerDir, in.binary = "", "", "bracer"
	out.Reset()
	renderCron(&out, in)
	s = out.String()
	for _, want := range []string{"BRACER_SHOP_MAIN_PASSWORD, BRACER_SHOP_S3_ACCESS_KEY, BRACER_SHOP_S3_SECRET_KEY", "docker was not found in PATH", "temporary path", "not reported anywhere"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestRenderSystemd(t *testing.T) {
	var out bytes.Buffer
	in := inputs(cronCfg())
	in.system = true
	renderSystemd(&out, in)
	s := out.String()
	for _, want := range []string{
		"# ---- bracer-backup-shop.service ----", "User=deploy", "ExecStart=/usr/local/bin/bracer backup -c /srv/shop/app.yaml",
		"OnCalendar=*-*-* 03:00:00", "OnCalendar=Mon *-*-* 04:00:00", "Persistent=true", "/etc/systemd/system/",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if got := cronToOnCalendar("*/5 * * * *"); !strings.Contains(got, "TODO") {
		t.Errorf("complex expression must be flagged: %s", got)
	}
}

func TestCronExampleUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCronExample(nil, []string{"--backup-at", "bad", "-c", "/nonexistent"}, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "5-field") {
		t.Errorf("code=%d err=%s", code, errb.String())
	}
	errb.Reset()
	if code := runCronExample(nil, []string{"--format", "github", "-c", "/nonexistent"}, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "cron or systemd") {
		t.Errorf("github must be rejected: code=%d err=%s", code, errb.String())
	}
	errb.Reset()
	if code := runCronExample(nil, []string{"-c", "/nonexistent/app.yaml"}, &out, &errb); code != exitUsage {
		t.Errorf("missing file: code=%d", code)
	}
}

func TestPingFlagOrder(t *testing.T) {
	// Flags after and before the id both work; bad server → exit 2 without a network call.
	var out, errb bytes.Buffer
	for _, args := range [][]string{{"hb", "--server", "nope"}, {"--server", "nope", "hb"}} {
		errb.Reset()
		if code := runPing(nil, args, nil, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "http(s)") {
			t.Errorf("%v: code=%d err=%s", args, code, errb.String())
		}
	}
	errb.Reset()
	if code := runPing(nil, []string{"hb", "extra", "--server", "http://x"}, nil, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "unexpected argument") {
		t.Errorf("code=%d err=%s", code, errb.String())
	}
}
