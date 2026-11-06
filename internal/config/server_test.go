package config

import (
	"strings"
	"testing"
)

// BR-016 item 7: the hosted server gets the pair secret, the app token and
// every report: https, or http:// only to this machine.
func TestBR016CheckServer(t *testing.T) {
	for s, ok := range map[string]bool{
		"https://app.bracer.dev":         true,
		"https://bracer.example.com/api": true,
		"http://localhost:8080":          true,
		"http://127.0.0.1:8080":          true,
		"http://[::1]:8080":              true,
		"http://app.bracer.dev":          false,
		"http://10.0.0.5:8080":           false,
		"app.bracer.dev":                 false,
		"https://":                       false,
		"ftp://app.bracer.dev":           false,
		"https://u:p@app.bracer.dev":     false,
		"http://localhost.:8080":         true,
		"http://LOCALHOST:8080":          true,
		"https://app.bracer.dev?":        false,
		"https://app.bracer.dev#":        false,
		"https://app.bracer.dev?x=1":     false,
	} {
		if err := CheckServer(s); (err == nil) != ok {
			t.Errorf("CheckServer(%q) = %v", s, err)
		}
	}
	a, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	a.Hosted = &Hosted{Server: "http://app.bracer.dev", TokenEnv: "BRACER_TOKEN"}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), `hosted.server: "http://app.bracer.dev": http:// would send the token in cleartext`) || !strings.HasSuffix(err.Error(), "\nFix: bracer remove shop, then bracer init shop --web") {
		t.Errorf("Validate: %v", err)
	}
}
