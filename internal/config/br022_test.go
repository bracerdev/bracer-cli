package config

import (
	"fmt"
	"strings"
	"testing"
)

// BR-022: a schedule has at most 96 times of day (a backup every 15
// minutes); the hosted server grades no denser one.
func TestBR022ScheduleAtMost96Times(t *testing.T) {
	at := func(n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			m := i * 15
			if i >= 96 {
				m = i - 95
			}
			out = append(out, fmt.Sprintf("%02d:%02d", m/60, m%60))
		}
		return out
	}
	if err := (Schedule{Every: EveryDaily, At: at(96), Keep: 1, Since: "2026-10-01T00:00:00Z"}).Validate(); err != nil {
		t.Errorf("96 times: %v", err)
	}
	err := (Schedule{Every: EveryDaily, At: at(97), Keep: 1, Since: "2026-10-01T00:00:00Z"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "at: 97 times of day; at most 96 (one every 15 minutes)") {
		t.Errorf("97 times: %v", err)
	}
}
