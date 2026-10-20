package drill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// The temporary Postgres is built like the source (ADR-0015 § 4): the
// source container's own image, an image built for a local cluster, or an
// image chosen by extension for a URL source.

// localImageRepo is the repository of images built for local sources; kept
// between drills so the next one reuses them.
const localImageRepo = "bracer-drill/"

// chooseImage picks the image for the run's source kind and says how.
func chooseImage(ctx context.Context, rt *dockerRuntime, cfg *config.App, env *pg.Env, dumps []*backup.Dump, res *Result, instance string) (string, error) {
	if cfg.Drill != nil && cfg.Drill.Image != "" {
		return cfg.Drill.Image, nil
	}
	switch env.SourceKind {
	case "container":
		ce := env.Container
		if ce == nil || ce.Image == "" {
			return "", fmt.Errorf("the environment record has no image of the source container; set drill.image in app.yaml")
		}
		if ce.ImageID != "" {
			if _, err := rt.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"image", "inspect", "-f", "{{.Id}}", ce.ImageID}}); err == nil {
				return ce.ImageID, nil
			}
			res.warn("%s: the source image %s (%s) is not on this machine; using the tag %s, which may now be a different build", instance, ce.Image, shortID(ce.ImageID), ce.Image)
		}
		return ce.Image, nil
	case "local":
		return rt.buildLocalImage(ctx, cfg.App, env, dumps)
	}
	n := env.ServerVersionNum / 10000
	var exts []pg.Extension
	for _, dp := range dumps {
		n = max(n, pg.MajorOf(dp.ToolVersion))
		exts = append(exts, dp.Extensions...)
	}
	if n == 0 {
		return "", fmt.Errorf("cannot pick a Postgres image: the environment record has no server version; set drill.image in app.yaml")
	}
	return pg.ImageForExtensions(n, exts)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// presentLocales are in every official Debian postgres image.
var presentLocales = map[string]bool{"c": true, "posix": true, "c.utf8": true, "en_us.utf8": true}

func normLocale(s string) string { return strings.ToLower(strings.ReplaceAll(s, "-", "")) }

// LocalDockerfile is the image a local source's cluster is rebuilt in: the
// official image of its major, the extension packages it had, its locales.
func LocalDockerfile(env *pg.Env, dumps []*backup.Dump) (string, error) {
	major := env.ServerVersionNum / 10000
	for _, dp := range dumps {
		major = max(major, pg.MajorOf(dp.ToolVersion))
	}
	if major == 0 {
		return "", fmt.Errorf("the environment record has no server version")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FROM postgres:%d\n", major)
	var pkgs []string
	if env.Local != nil {
		seen := map[string]bool{}
		for _, p := range env.Local.Packages {
			name := strings.Replace(p.Package, fmt.Sprintf("postgresql-%d-", env.ServerVersionNum/10000), fmt.Sprintf("postgresql-%d-", major), 1)
			if !seen[name] {
				seen[name] = true
				pkgs = append(pkgs, name)
			}
		}
	}
	sort.Strings(pkgs)
	if len(pkgs) > 0 {
		fmt.Fprintf(&b, "RUN apt-get update && apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n", strings.Join(pkgs, " "))
	}
	locales := map[string]bool{}
	for _, dp := range dumps {
		l := dp.Locale
		if l == nil || (l.Provider != "" && l.Provider != "libc") {
			continue
		}
		for _, name := range []string{l.Collate, l.Ctype} {
			if name != "" && !presentLocales[normLocale(name)] {
				locales[name] = true
			}
		}
	}
	names := make([]string, 0, len(locales))
	for n := range locales {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		lang, charmap, ok := strings.Cut(n, ".")
		if !ok || lang == "" || strings.ContainsAny(n, " '\"\\$`;&|") {
			return "", fmt.Errorf("cannot rebuild locale %q (expected language_TERRITORY.CHARSET)", n)
		}
		charmap, _, _ = strings.Cut(charmap, "@")
		if normLocale(charmap) == "utf8" {
			charmap = "UTF-8"
		}
		fmt.Fprintf(&b, "RUN localedef -i %s -c -f %s -A /usr/share/locale/locale.alias %s\n", lang, charmap, n)
	}
	return b.String(), nil
}

// buildLocalImage builds (or reuses) bracer-drill/<app>:<hash of the Dockerfile>.
func (r *dockerRuntime) buildLocalImage(ctx context.Context, app string, env *pg.Env, dumps []*backup.Dump) (string, error) {
	df, err := LocalDockerfile(env, dumps)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(df))
	tag := localImageRepo + app + ":" + hex.EncodeToString(sum[:])[:12]
	if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"image", "inspect", "-f", "{{.Id}}", tag}}); err == nil {
		return tag, nil
	}
	out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"build", "--progress", "plain", "--label", Label + "=" + app, "-t", tag, "-"}, Stdin: strings.NewReader(df)})
	if err != nil {
		return "", fmt.Errorf("build %s for the local source failed:\n%s", tag, restore.Tail(append(out.Stdout, out.Stderr...), restore.StderrTail))
	}
	return tag, nil
}

// checkOfficial: the drill relies on the official image's entrypoint
// (POSTGRES_USER, POSTGRES_DB, -c passed to postgres).
func (r *dockerRuntime) checkOfficial(ctx context.Context, image string) error {
	if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"image", "inspect", "-f", "{{.Id}}", image}}); err != nil {
		if _, perr := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"pull", "-q", image}}); perr != nil {
			return fmt.Errorf("image %s is not on this machine and cannot be pulled: %v", image, firstLine(perr.Error()))
		}
	}
	out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"image", "inspect", "-f", "{{json .Config.Entrypoint}}\n{{json .Config.Cmd}}", image}})
	if err != nil {
		return fmt.Errorf("inspect %s: %v", image, err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(out.Stdout)), "\n", 2)
	var ep, cmd []string
	if len(lines) == 2 {
		_ = json.Unmarshal([]byte(lines[0]), &ep)
		_ = json.Unmarshal([]byte(lines[1]), &cmd)
	}
	if len(ep) != 1 || !strings.HasSuffix(ep[0], "docker-entrypoint.sh") || len(cmd) != 1 || cmd[0] != "postgres" {
		return fmt.Errorf("drill runs only the official postgres image family; %s is not (entrypoint %v, cmd %v)", image, ep, cmd)
	}
	return nil
}

// imageFiles lists the extension control files and the libraries in the
// image, without starting Postgres.
func (r *dockerRuntime) imageFiles(ctx context.Context, image string) (exts, libs map[string]bool, err error) {
	const script = `s=$(pg_config --sharedir) && l=$(pg_config --pkglibdir) && ls "$s/extension" && echo --- && ls "$l"`
	out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"run", "--rm", "--network", "none", "--entrypoint", "sh", image, "-c", script}})
	if err != nil {
		return nil, nil, fmt.Errorf("list the extensions in %s: %v", image, firstLine(err.Error()))
	}
	before, after, ok := bytes.Cut(out.Stdout, []byte("---\n"))
	if !ok {
		return nil, nil, fmt.Errorf("list the extensions in %s: unexpected output", image)
	}
	exts, libs = map[string]bool{}, map[string]bool{}
	for _, f := range strings.Fields(string(before)) {
		if n, ok := strings.CutSuffix(f, ".control"); ok {
			exts[n] = true
		}
	}
	for _, f := range strings.Fields(string(after)) {
		libs[f] = true
	}
	return exts, libs, nil
}

// checkImage: every extension the databases use has a control file in the
// image; the source's preload libraries that an extension needs are there.
// It returns the preload list to start with and notes on libraries left out.
func checkImage(env *pg.Env, dumps []*backup.Dump, image string, exts, libs map[string]bool) (preload, notes []string, err error) {
	var all []pg.Extension
	var missing []string
	seen := map[string]bool{}
	for _, dp := range dumps {
		for _, e := range dp.Extensions {
			all = append(all, e)
			if !exts[e.Name] && !seen[e.Name] {
				seen[e.Name] = true
				missing = append(missing, e.Name)
			}
		}
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("image %s has no extension %s", image, strings.Join(missing, ", "))
	}
	if env.PreloadLibraries == nil {
		return nil, nil, nil
	}
	needed, _ := pg.PreloadNeeds(*env.PreloadLibraries, all)
	need := map[string]bool{}
	for _, l := range needed {
		need[l] = true
		if !libs[l+".so"] {
			return nil, nil, fmt.Errorf("image %s has no library %s, which extension %s needs loaded at start", image, l, l)
		}
	}
	var left []string
	for _, l := range pg.SplitLibraries(*env.PreloadLibraries) {
		switch {
		case libs[l+".so"]:
			preload = append(preload, l)
		case !need[l]:
			left = append(left, l)
		}
	}
	if len(left) > 0 {
		notes = append(notes, fmt.Sprintf("source preloads %s, which %s does not have and no database's extension needs; left out", strings.Join(left, ", "), image))
	}
	return preload, notes, nil
}
