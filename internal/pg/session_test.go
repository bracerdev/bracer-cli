package pg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// scriptedPsql fakes psql -f -: reads each statement, prints the result from the given table, then prints the marker.
type scriptedPsql struct {
	answers map[string]string // sql → output (multi-line); "ERR:..." → error
}

func (p scriptedPsql) Describe() string { return "scripted psql" }

func (p scriptedPsql) Run(_ context.Context, _ Conn, tool string, _ []string, stdin io.Reader, stdout io.Writer) error {
	if tool != "psql" {
		return fmt.Errorf("unexpected tool %s", tool)
	}
	sc := bufio.NewScanner(stdin)
	lastErr := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, `\echo `) {
			errFlag := "false"
			if lastErr != "" {
				errFlag = "true"
			}
			fmt.Fprintf(stdout, "%s %s %s\n", sessionMarker, errFlag, lastErr)
			continue
		}
		sql := strings.TrimSuffix(line, ";")
		ans, ok := p.answers[sql]
		switch {
		case !ok:
			lastErr = "ERROR:  unknown statement " + sql
		case strings.HasPrefix(ans, "ERR:"):
			lastErr = strings.TrimPrefix(ans, "ERR:")
		default:
			lastErr = ""
			if ans != "" {
				io.WriteString(stdout, ans+"\n")
			}
		}
	}
	return nil
}

func TestSessionExecAndErrors(t *testing.T) {
	r := scriptedPsql{answers: map[string]string{
		"select 1":                                   "1",
		"begin isolation level repeatable read":      "",
		"select pg_export_snapshot()":                "00000003-0000001B-1",
		"select count(*) from \"public\".\"orders\"": "500",
		"select 1/0":                                 "ERR:ERROR:  division by zero",
		"commit":                                     "",
	}}
	s, err := StartSession(context.Background(), r, Conn{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("begin isolation level repeatable read"); err != nil {
		t.Fatal(err)
	}
	if snap, err := s.Scalar("select pg_export_snapshot()"); err != nil || snap != "00000003-0000001B-1" {
		t.Errorf("snapshot: %q %v", snap, err)
	}
	if n, err := s.Scalar(`select count(*) from "public"."orders";`); err != nil || n != "500" {
		t.Errorf("count: %q %v", n, err)
	}
	if _, err := s.Exec("select 1/0"); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("SQL error must surface: %v", err)
	}
	// The session is still alive after an error.
	if n, err := s.Scalar(`select count(*) from "public"."orders"`); err != nil || n != "500" {
		t.Errorf("after error: %q %v", n, err)
	}
	if _, err := s.Exec("commit"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("select 1"); err == nil {
		t.Error("exec after close must fail")
	}
}
