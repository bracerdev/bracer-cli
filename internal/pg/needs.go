package pg

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RestoreNeeds is what the target server must have for a restore of a
// database beyond the dump: files the
// dump does not carry and collations it does not create (spec backup § needs).
// Restore checks the target has them before loading.
type RestoreNeeds struct {
	Libraries  []NeededFile `json:"libraries,omitempty"` // in PKGLIBDIR
	TSFiles    []NeededFile `json:"ts_files,omitempty"`  // in SHAREDIR/tsearch_data
	Collations []Collation  `json:"collations,omitempty"`
}

// Empty reports whether nothing is needed.
func (n *RestoreNeeds) Empty() bool {
	return n == nil || len(n.Libraries) == 0 && len(n.TSFiles) == 0 && len(n.Collations) == 0
}

// NeededFile is one file on the server and what uses it.
type NeededFile struct {
	Name   string   `json:"name"`
	UsedBy []string `json:"used_by"`
}

// Collation is a collation the database uses (columns, indexes, domains) or
// created. Created: it lives outside pg_catalog, so the dump creates it; a
// pg_catalog collation must already be on the target.
type Collation struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Created bool     `json:"created,omitempty"`
	Locale           // provider, collate, ctype, locale, icu_rules
	Version string   `json:"version,omitempty"` // pg_collation_actual_version
	UsedBy  []string `json:"used_by,omitempty"`
}

// QualName is schema.name as SQL writes it.
func (c Collation) QualName() string { return QuoteIdent(c.Schema) + "." + QuoteIdent(c.Name) }

const maxUsedBy = 3

// libsSQL: C functions that no extension owns and load a library from
// $libdir; the system's own (pg_catalog: encoding conversions, snowball) come
// with every server.
var libsSQL = "select p.probin, p.oid::regprocedure::text from pg_proc p join pg_language l on l.oid = p.prolang join pg_namespace n on n.oid = p.pronamespace " +
	"where l.lanname = 'c' and p.probin like '$libdir/%' and n.nspname not in ('pg_catalog','information_schema') and " + notExtensionMember("pg_proc", "p.oid") + " order by 1, 2"

// dictsSQL: full-text dictionaries that no extension owns, with their template.
var dictsSQL = "select td.oid::regdictionary::text, t.tmplname, coalesce(td.dictinitoption, '') from pg_ts_dict td join pg_ts_template t on t.oid = td.dicttemplate " +
	"join pg_namespace n on n.oid = td.dictnamespace where n.nspname <> 'pg_catalog' and " + notExtensionMember("pg_ts_dict", "td.oid") + " order by 1"

// collationUsesSQL: collations other than default used by columns, index
// columns and domains outside the system schemas, and every collation created
// outside pg_catalog that no extension owns. {cols} are the locale columns.
const collationUsesSQL = `with uses as (
  select a.attcollation as coll, format('%s.%s', a.attrelid::regclass, quote_ident(a.attname)) as what
    from pg_attribute a join pg_class r on r.oid = a.attrelid join pg_namespace n on n.oid = r.relnamespace
    where a.attnum > 0 and not a.attisdropped and a.attcollation <> 0 and r.relkind in ('r','p','v','m','f','c')
      and n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^pg_toast'
  union all
  select u.coll, i.indexrelid::regclass::text
    from pg_index i join pg_class r on r.oid = i.indexrelid join pg_namespace n on n.oid = r.relnamespace,
      unnest(i.indcollation::oid[]) as u(coll)
    where u.coll <> 0 and n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^pg_toast'
  union all
  select t.typcollation, t.oid::regtype::text
    from pg_type t join pg_namespace n on n.oid = t.typnamespace
    where t.typtype = 'd' and t.typcollation <> 0 and n.nspname not in ('pg_catalog','information_schema')
)
select n.nspname, c.collname, n.nspname <> 'pg_catalog', {cols}, coalesce(pg_collation_actual_version(c.oid), ''),
  coalesce((select string_agg(what, E'\x1e' order by what) from uses where uses.coll = c.oid), '')
from pg_collation c join pg_namespace n on n.oid = c.collnamespace
where c.oid <> 100 and (c.oid in (select coll from uses)
  or (n.nspname <> 'pg_catalog' and n.nspname <> 'information_schema' and ` + "{notext}" + `))
order by 1, 2`

// collationCols reads provider, collate, ctype, locale and ICU rules by major,
// as DatabaseLocale does: PG < 15 keeps an ICU locale in collcollate.
func collationCols(major int) string {
	switch {
	case major >= 17:
		return "c.collprovider::text, coalesce(c.collcollate, ''), coalesce(c.collctype, ''), coalesce(c.colllocale, ''), coalesce(c.collicurules, '')"
	case major == 16:
		return "c.collprovider::text, coalesce(c.collcollate, ''), coalesce(c.collctype, ''), coalesce(c.colliculocale, ''), coalesce(c.collicurules, '')"
	case major == 15:
		return "c.collprovider::text, coalesce(c.collcollate, ''), coalesce(c.collctype, ''), coalesce(c.colliculocale, ''), ''"
	}
	return "c.collprovider::text, c.collcollate, c.collctype, case when c.collprovider = 'i' then c.collcollate else '' end, ''"
}

// RestoreNeeds reads what a restore of the connected database needs on the
// target server beyond its dump.
func (c *Client) RestoreNeeds(ctx context.Context, major int) (*RestoreNeeds, error) {
	n := &RestoreNeeds{}
	rows, err := c.Query(ctx, libsSQL)
	if err != nil {
		return nil, fmt.Errorf("C functions: %w", err)
	}
	libs := map[string]int{}
	for _, r := range rows {
		if len(r) != 2 {
			return nil, fmt.Errorf("unexpected pg_proc row %q", r)
		}
		n.Libraries = addUse(n.Libraries, libs, LibraryFile(r[0]), r[1])
	}
	rows, err = c.Query(ctx, dictsSQL)
	if err != nil {
		return nil, fmt.Errorf("text search dictionaries: %w", err)
	}
	files := map[string]int{}
	for _, r := range rows {
		if len(r) != 3 {
			return nil, fmt.Errorf("unexpected pg_ts_dict row %q", r)
		}
		for _, f := range DictFiles(r[1], r[2]) {
			n.TSFiles = addUse(n.TSFiles, files, f, r[0])
		}
	}
	sql := strings.Replace(strings.Replace(collationUsesSQL, "{cols}", collationCols(major), 1), "{notext}", notExtensionMember("pg_collation", "c.oid"), 1)
	rows, err = c.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("collations: %w", err)
	}
	for _, r := range rows {
		if len(r) != 10 {
			return nil, fmt.Errorf("unexpected pg_collation row %q", r)
		}
		col := Collation{Schema: r[0], Name: r[1], Created: r[2] == "t", Version: r[8],
			Locale: Locale{Provider: providerNames[r[3]], Collate: r[4], Ctype: r[5], Locale: r[6], ICURules: r[7]}}
		if col.Provider == "libc" {
			col.Provider = ""
		}
		if r[9] != "" {
			col.UsedBy = firstN(strings.Split(r[9], "\x1e"), maxUsedBy)
		}
		n.Collations = append(n.Collations, col)
	}
	return n, nil
}

// LibraryFile is the file a probin of $libdir/<name> loads: Postgres adds the
// platform suffix when the name has none.
func LibraryFile(probin string) string {
	name := strings.TrimPrefix(probin, "$libdir/")
	if !strings.Contains(name, ".") {
		name += ".so"
	}
	return name
}

var reDictOption = regexp.MustCompile(`(?i)(\w+)\s*=\s*'((?:[^']|'')*)'`)

// DictFiles lists the tsearch_data files a dictionary with template tmpl and
// dictinitoption opts reads.
func DictFiles(tmpl, opts string) []string {
	var out []string
	for _, m := range reDictOption.FindAllStringSubmatch(opts, -1) {
		v := strings.ToLower(strings.ReplaceAll(m[2], "''", "'"))
		var suffix string
		switch strings.ToLower(m[1]) {
		case "dictfile":
			suffix = ".dict"
			if tmpl == "thesaurus" {
				suffix = ".ths"
			}
		case "afffile":
			suffix = ".affix"
		case "stopwords":
			suffix = ".stop"
		case "synonyms":
			suffix = ".syn"
		case "rules":
			suffix = ".rules"
		default:
			continue
		}
		out = append(out, v+suffix)
	}
	return out
}

func addUse(list []NeededFile, idx map[string]int, name, user string) []NeededFile {
	if i, ok := idx[name]; ok {
		if len(list[i].UsedBy) < maxUsedBy {
			list[i].UsedBy = append(list[i].UsedBy, user)
		}
		return list
	}
	idx[name] = len(list)
	return append(list, NeededFile{Name: name, UsedBy: []string{user}})
}

func firstN(s []string, n int) []string {
	sort.Strings(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
