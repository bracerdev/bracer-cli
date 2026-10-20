package drill

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/restore"
	"bracer.dev/bracer/internal/storage"
)

// Check is one line of `drill --dry-run`.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// DryRunResult is the outcome of `drill --dry-run`.
type DryRunResult struct {
	Kind   string  `json:"kind"`
	App    string  `json:"app"`
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// DryRun checks what a drill needs without creating anything (spec drill AC
// "--dry-run"): Docker answers, the age key matches age_recipient, the manifest
// is readable. No backup yet is not a failure: init runs this before the first
// backup. drill.runtime: none → UsageError, like Run.
func DryRun(ctx context.Context, cfg *config.App, d Deps) (*DryRunResult, error) {
	d = withDefaults(d)
	if cfg.DrillOff() && !d.ForceDocker {
		return nil, OffError(cfg.App)
	}
	res := &DryRunResult{Kind: "drill-dry-run", App: cfg.App}
	add := func(c Check) { res.Checks = append(res.Checks, c) }

	if err := d.Tools.Docker().Info(ctx); err != nil {
		add(Check{Name: "docker", Detail: firstLine(err.Error()), Fix: "bracer check"})
	} else {
		add(Check{Name: "docker", OK: true, Detail: "docker info answers"})
	}

	if cfg.Encryption == "age" {
		add(identityCheck(cfg, d))
	}

	key := storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json")
	m, err := backup.LoadManifest(ctx, d.Store, key, cfg.App)
	if err != nil {
		add(Check{Name: "bucket", Detail: fmt.Sprintf("cannot read %s: %v", key, firstLine(err.Error())), Fix: "check storage in app.yaml and the S3 keys in secrets_file, then bracer backup --dry-run"})
	} else {
		for _, in := range cfg.Instances {
			if d.Instance != "" && in.Name != d.Instance {
				continue
			}
			add(manifestCheck(in, m.Instances[in.Name]))
		}
	}

	res.OK = true
	for _, c := range res.Checks {
		if !c.OK {
			res.OK = false
		}
	}
	return res, nil
}

func identityCheck(cfg *config.App, d Deps) Check {
	c := Check{Name: "age key"}
	path, err := config.ExpandHome(cfg.AgeIdentityFile)
	var ids *restore.Identities
	if err != nil {
		ids = &restore.Identities{Err: err}
	} else {
		ids = restore.ReadIdentities(path, d.ReadIdentities)
	}
	switch {
	case ids.Err != nil:
		c.Detail = fmt.Sprintf("age_identity_file: %v", ids.Err)
		c.Fix = "set age_identity_file in app.yaml to the key file printed by bracer init (absolute path, readable by the user running bracer)"
	case !ids.Has(cfg.AgeRecipient):
		c.Detail = fmt.Sprintf("%s has no key for age_recipient %s", cfg.AgeIdentityFile, cfg.AgeRecipient)
		c.Fix = "point age_identity_file at the key matching age_recipient (after rotate-key, keep the old keys in the file too)"
	default:
		c.OK, c.Detail = true, fmt.Sprintf("%s has the key for %s", cfg.AgeIdentityFile, cfg.AgeRecipient)
	}
	return c
}

func manifestCheck(in config.Instance, im *backup.InstanceManifest) Check {
	c := Check{Name: "backup " + in.Name, OK: true}
	if im == nil || len(im.Databases) == 0 {
		c.Detail = "no backup yet"
		return c
	}
	var parts []string
	dbs := make([]string, 0, len(im.Databases))
	for db := range im.Databases {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	for _, db := range dbs {
		dm := im.Databases[db]
		if latest(dm.Dumps, dm.Latest) == nil {
			parts = append(parts, db+" (no backup yet)")
			continue
		}
		parts = append(parts, db+" "+dm.Latest)
	}
	c.Detail = "latest: " + strings.Join(parts, ", ")
	if im.Env.Latest != "" {
		c.Detail += "; latest run " + im.Env.Latest + ": environment record yes"
	} else {
		c.OK = false
		c.Detail += "; environment record no (backup made before ADR-0015)"
		c.Fix = "run bracer backup with this version"
	}
	return c
}
