// Package hostedfilter is, for tests, the hosted server's check of a
// /report body: a body holding a string that looks like a credential is
// refused with 400 and the report is lost. The patterns are copied from
// the server and kept in step by hand, so the tests of the CLI's
// redaction can prove that what it sends is never refused.
package hostedfilter

import (
	"encoding/json"
	"regexp"
	"strings"
)

// patterns are the server's. A match of the password-key pattern whose
// value (group 1) is only * is redacted text and not refused.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/@'"?#]*:[^\s/'"?#]*@`),
	regexp.MustCompile(`(?i)(?:^|[?&;\s])(?:[a-z0-9_]*(?:password|passwd)|(?:[a-z0-9]+_)?pwd)=([^&#\s'"]+)`),
	regexp.MustCompile(`(?i)PASSWORD\s+'`),
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	regexp.MustCompile(`AGE-SECRET-KEY-`),
}

// Refuses reports whether the server refuses the string s.
func Refuses(s string) bool {
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			if len(m) > 1 && strings.Trim(m[1], "*") == "" {
				continue
			}
			return true
		}
	}
	return false
}

// RefusesJSON reports whether the server refuses a /report body: any
// decoded string of it (keys included) that Refuses, or a body that is
// not JSON.
func RefusesJSON(body []byte) bool {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return true
	}
	return refusesValue(v)
}

func refusesValue(v any) bool {
	switch x := v.(type) {
	case string:
		return Refuses(x)
	case []any:
		for _, e := range x {
			if refusesValue(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if Refuses(k) || refusesValue(e) {
				return true
			}
		}
	}
	return false
}
