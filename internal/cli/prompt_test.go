package cli

import (
	"strings"
	"testing"
)

// An answer that is neither y nor n is asked again, with a hint: a stray
// line (a number meant for the web, an Enter) must not pass silently.
func TestYesNoRepeatsWithHint(t *testing.T) {
	var out strings.Builder
	p := newPrompter(strings.NewReader("1\nmaybe\ny\n"), &out)
	ok, err := p.yesNo("Go on?", false)
	if err != nil || !ok {
		t.Fatalf("yesNo = %v, %v", ok, err)
	}
	if got := strings.Count(out.String(), "Please answer y or n\n"); got != 2 {
		t.Fatalf("hint printed %d times:\n%s", got, out.String())
	}
}

// The web hook fires before the question is printed, so the web shows
// "finish in your terminal" while the prompt waits, not after.
func TestPromptHookBeforeQuestion(t *testing.T) {
	var out strings.Builder
	p := newPrompter(strings.NewReader("\n"), &out)
	p.before = func() { out.WriteString("<hook>") }
	if _, err := p.ask("Instance name", "db"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "<hook>Instance name [db]: ") {
		t.Fatalf("output %q", out.String())
	}
}
