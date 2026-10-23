package pg

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Extension is an extension installed in a database (pg_extension), written to
// the manifest so drill can pick an image and drill/restore can check the target
// has them all before restoring (spec backup § Flow 3c, drill § Flow 3a–3b).
type Extension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema,omitempty"` // empty in manifests written before ADR-0015
	// Config: the extension has configuration tables (extconfig), whose rows
	// the dump carries as of Version (spec restore § extensions).
	Config bool `json:"config,omitempty"`
}

func (e Extension) String() string { return e.Name + " " + e.Version }

// Extensions reads pg_extension of the connected database, skipping plpgsql
// (every database has it, every image has it).
func (c *Client) Extensions(ctx context.Context) ([]Extension, error) {
	rows, err := c.Query(ctx, "select extname, extversion, extnamespace::regnamespace::text, extconfig is not null from pg_extension where extname <> 'plpgsql' order by extname")
	if err != nil {
		return nil, err
	}
	out := make([]Extension, 0, len(rows))
	for _, r := range rows {
		if len(r) < 2 || r[0] == "" {
			return nil, fmt.Errorf("unexpected pg_extension row: %q", r)
		}
		e := Extension{Name: r[0], Version: r[1]}
		if len(r) > 2 {
			e.Schema = strings.Trim(r[2], `"`)
		}
		e.Config = len(r) > 3 && r[3] == "t"
		out = append(out, e)
	}
	return out, nil
}

// AvailableExtensions is pg_available_extensions of the connected server:
// name → default_version. The evidence of whether an image/server has an extension.
func (c *Client) AvailableExtensions(ctx context.Context) (map[string]string, error) {
	rows, err := c.Query(ctx, "select name, coalesce(default_version, '') from pg_available_extensions order by name")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if len(r) < 2 || r[0] == "" {
			return nil, fmt.Errorf("unexpected pg_available_extensions row: %q", r)
		}
		out[r[0]] = r[1]
	}
	return out, nil
}

// MissingExtensions compares the needed extensions (manifest) with the available
// ones: missing = absent; differ = present but default_version differs from the manifest.
func MissingExtensions(need []Extension, avail map[string]string) (missing, differ []Extension) {
	for _, e := range need {
		v, ok := avail[e.Name]
		switch {
		case !ok:
			missing = append(missing, e)
		case v != "" && v != e.Version:
			differ = append(differ, Extension{Name: e.Name, Version: v})
		}
	}
	return missing, differ
}

// ImageForExtensions picks the drill image by extension (spec drill § Flow 3a.2):
// the three common extensions have official images, tagged by the manifest version
// to match the source server; none → postgres:N-alpine. Two extensions pointing at
// two different images → error (the user sets drill.image). Extensions outside the
// table have no effect: pg_available_extensions decides later whether the image has them.
func ImageForExtensions(major int, exts []Extension) (string, error) {
	images := map[string]string{} // extension → image
	for _, e := range exts {
		switch e.Name {
		case "postgis":
			images[e.Name] = fmt.Sprintf("postgis/postgis:%d-%s", major, majorMinor(e.Version))
		case "timescaledb":
			images[e.Name] = fmt.Sprintf("timescale/timescaledb:%s-pg%d", e.Version, major)
		case "vector":
			images[e.Name] = fmt.Sprintf("pgvector/pgvector:pg%d", major)
		}
	}
	if len(images) == 0 {
		return ImageForMajor(major), nil
	}
	names := make([]string, 0, len(images))
	for n := range images {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 1 {
		return "", fmt.Errorf("databases use both %s; no single image is known to have them all: set drill.image in app.yaml", strings.Join(names, " and "))
	}
	return images[names[0]], nil
}

// majorMinor extracts "3.4" from "3.4.2" (postgis/postgis tags are N-<major.minor>).
func majorMinor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// PreloadNeeds splits the source's shared_preload_libraries into the libraries
// a restore needs (named like an extension the databases use: timescaledb,
// pg_cron, citus…) and the rest (auto_explain…), which a restore does not need.
func PreloadNeeds(preload string, exts []Extension) (needed, others []string) {
	used := map[string]bool{}
	for _, e := range exts {
		used[e.Name] = true
	}
	for _, lib := range SplitLibraries(preload) {
		if used[lib] {
			needed = append(needed, lib)
		} else {
			others = append(others, lib)
		}
	}
	return needed, others
}
