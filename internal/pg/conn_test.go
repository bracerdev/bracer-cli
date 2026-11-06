package pg

import (
	"strings"
	"testing"
)

// BR-008: url.Parse's error quotes the whole URL, password included; the
// error of ParseURL keeps only the reason, so it can be printed and reported.
func TestBR008ParseURLErrorHasNoURL(t *testing.T) {
	const pw = "fakepw-br008-xyz"
	cases := []struct {
		name, raw, password, reason string
	}{
		{"bad port", "postgres://u:" + pw + "@10.0.0.1:abc/db", pw, "invalid port"},
		{"bad escape in password", "postgres://u:" + pw + "%zz@10.0.0.1/db", pw + "%zz", "invalid URL escape"},
		// The authority ends at the first `/`, so `p@ss:w` reads as a host
		// and `/rd%` as the path; the port url.Parse quotes is password.
		{"password with @ : / %", "postgres://u:p@ss:w/rd%" + pw + "@h/db", "p@ss:w/rd%" + pw, "invalid"},
		{"query password, bad port", "postgres://u@10.0.0.1:abc/db?password=" + pw, pw, "invalid port"},
		{"control character", "postgres://u:" + pw + "@h/db\x7f\n", pw, "invalid control character"},
		{"scheme garbage", "://u:" + pw + "@h/db", pw, "missing protocol scheme"},
	}
	for _, c := range cases {
		_, err := ParseURL(c.raw)
		if err == nil {
			t.Errorf("%s: parsed", c.name)
			continue
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "invalid URL") || !strings.Contains(msg, c.reason) {
			t.Errorf("%s: unreadable error %q", c.name, msg)
		}
		if strings.Contains(msg, "postgres") || strings.Contains(msg, "@") || strings.Contains(msg, `"`) {
			t.Errorf("%s: error quotes the URL: %q", c.name, msg)
		}
		for _, frag := range fragments(c.password, 3) {
			if strings.Contains(msg, frag) {
				t.Errorf("%s: error %q holds %q of the password", c.name, msg, frag)
			}
		}
	}
}

// BR-014: a hand-broken URL turns password material into a query key; the
// error names no key, so no part of the URL is printed, written to
// run.json or reported. A misspelled parameter still gets a readable error.
func TestBR014ParseURLErrorNamesNoKey(t *testing.T) {
	const tail = "Tail-Secret99"
	for _, raw := range []string{
		"postgres://u:?" + tail + "@10.0.0.1/db",
		"postgres://u@h/db?password=hunter2&" + tail,
		"postgres://u@h/db?" + tail + "=x",
	} {
		_, err := ParseURL(raw)
		if err == nil {
			t.Fatalf("%q parsed", raw)
		}
		msg := strings.ToLower(err.Error())
		if !strings.HasPrefix(msg, "unsupported url parameter") {
			t.Errorf("%q: error %q", raw, msg)
		}
		for _, frag := range append(fragments(strings.ToLower(tail), 4), "hunter2", "10.0.0.1") {
			if strings.Contains(msg, frag) {
				t.Errorf("%q: error %q holds %q", raw, msg, frag)
			}
		}
	}
	_, err := ParseURL("postgres://h/db?sslmod=require")
	if err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("misspelled parameter: %v", err)
	}
}

// fragments are the substrings of s at least n bytes long, the shortest
// that could still identify a password.
func fragments(s string, n int) []string {
	var out []string
	for i := 0; i+n <= len(s); i++ {
		out = append(out, s[i:i+n])
	}
	return out
}
