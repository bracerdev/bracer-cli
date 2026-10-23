package pg

import (
	"strings"
	"testing"
)

func TestDictFilesAndLibraryFile(t *testing.T) {
	for _, c := range []struct{ tmpl, opts, want string }{
		{"ispell", "dictfile = 'english', afffile = 'english', stopwords = 'english'", "english.dict english.affix english.stop"},
		{"thesaurus", "dictfile = 'thesaurus_astro', dictionary = 'english_stem'", "thesaurus_astro.ths"},
		{"synonym", "synonyms = 'MySyn'", "mysyn.syn"},
		{"unaccent", "rules = 'unaccent'", "unaccent.rules"},
		{"snowball", "language = 'swedish'", ""},
	} {
		if got := strings.Join(DictFiles(c.tmpl, c.opts), " "); got != c.want {
			t.Errorf("%s %q: %q", c.tmpl, c.opts, got)
		}
	}
	for in, want := range map[string]string{"$libdir/mylib": "mylib.so", "$libdir/mylib.so": "mylib.so"} {
		if got := LibraryFile(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
