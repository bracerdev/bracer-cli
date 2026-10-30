package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrNoInput: stdin ended while questions remain; init needs an interactive terminal.
var ErrNoInput = errors.New("no more input: bracer init needs an interactive terminal")

// prompter asks and answers on the terminal. secret reads without echoing when
// stdin is a terminal; otherwise (tests, pipes) it reads a plain line.
type prompter struct {
	in     *bufio.Reader
	out    io.Writer
	secret func(prompt string) (string, error)
	// before runs before every question; init --web tells the web the
	// customer is now answering in the terminal (spec init-web § Flow 5).
	before func()
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	p := &prompter{in: bufio.NewReader(in), out: out}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		p.secret = func(prompt string) (string, error) {
			fmt.Fprint(out, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(out)
			return string(b), err
		}
	} else {
		p.secret = func(prompt string) (string, error) {
			fmt.Fprint(out, prompt)
			return p.line()
		}
	}
	return p
}

func (p *prompter) line() (string, error) {
	if p.before != nil {
		p.before()
	}
	s, err := p.in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", ErrNoInput
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func (p *prompter) printf(format string, args ...interface{}) {
	fmt.Fprintf(p.out, format, args...)
}

// ask asks for one line; Enter returns def.
func (p *prompter) ask(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", prompt)
	}
	s, err := p.line()
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// askRequired asks until a value is given.
func (p *prompter) askRequired(prompt string) (string, error) {
	for {
		s, err := p.ask(prompt, "")
		if err != nil || s != "" {
			return s, err
		}
	}
}

func (p *prompter) askSecret(prompt string) (string, error) {
	if p.before != nil {
		p.before()
	}
	return p.secret(prompt + ": ")
}

// yesNo asks [Y/n] or [y/N].
func (p *prompter) yesNo(prompt string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		fmt.Fprintf(p.out, "%s %s: ", prompt, hint)
		s, err := p.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

// choose asks for a number 1..n or one of the extra keys; returns (index, "") or (-1, key).
func (p *prompter) choose(prompt string, n int, extra ...string) (int, string, error) {
	for {
		fmt.Fprintf(p.out, "%s: ", prompt)
		s, err := p.line()
		if err != nil {
			return 0, "", err
		}
		s = strings.ToLower(strings.TrimSpace(s))
		for _, e := range extra {
			if s == e {
				return -1, e, nil
			}
		}
		if i, err := strconv.Atoi(s); err == nil && i >= 1 && i <= n {
			return i - 1, "", nil
		}
		fmt.Fprintf(p.out, "Please enter a number from 1 to %d", n)
		if len(extra) > 0 {
			fmt.Fprintf(p.out, " or %s", strings.Join(extra, "/"))
		}
		fmt.Fprintln(p.out)
	}
}

// chooseMany asks for a comma-separated list of numbers, or `all`.
func (p *prompter) chooseMany(prompt string, n int) (all bool, idx []int, err error) {
	for {
		fmt.Fprintf(p.out, "%s [all]: ", prompt)
		s, err := p.line()
		if err != nil {
			return false, nil, err
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || s == "all" {
			return true, nil, nil
		}
		var out []int
		ok := true
		seen := map[int]bool{}
		for _, part := range strings.Split(s, ",") {
			i, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || i < 1 || i > n {
				ok = false
				break
			}
			if !seen[i-1] {
				seen[i-1] = true
				out = append(out, i-1)
			}
		}
		if ok && len(out) > 0 {
			return false, out, nil
		}
		fmt.Fprintf(p.out, "Please enter numbers from 1 to %d separated by commas, or all\n", n)
	}
}
